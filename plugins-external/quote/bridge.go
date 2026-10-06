package main

// bridge.go — run the official LyoSU/quote-api renderer (vendored by the
// TeleBox quote plugin) through a node bridge process. The Go side builds
// the JSON payload, spawns `node bridge.mjs` and decodes the framed binary
// reply (magic + len + ext code + bytes). No remote quote service is used:
// the vendor renderer runs locally, exactly like TeleBox's quote plugin.

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	bridgeMagic    = "PVQT" // frame magic
	bridgeHeaderSz = 16     // magic(4) + payloadLen(4) + extCode(4) + reserved(4)
	bridgeTimeout  = 180 * time.Second

	extCodePNG  = 1
	extCodeWebP = 2
	extCodeWebM = 3
)

// quoteMessage is the message model the official generate.js expects
// (field names follow quote-api / TeleBox quote.ts).
type quoteMessage struct {
	ChatID       int64           `json:"chatId"`
	MessageID    int             `json:"message_id"`
	From         *quoteFrom      `json:"from"`
	Text         string          `json:"text"`
	Entities     []bridgeEntity  `json:"entities,omitempty"`
	Caption      string          `json:"caption,omitempty"`
	CaptionEnts  []bridgeEntity  `json:"caption_entities,omitempty"`
	Avatar       bool            `json:"avatar"`
	AvatarBuffer string          `json:"avatarBuffer,omitempty"` // base64 image, optional
	AvatarScale  int             `json:"avatarScale"`
	ReplyMessage *quoteReply     `json:"replyMessage,omitempty"`
	MediaCanvas  string          `json:"mediaCanvas,omitempty"` // base64 image preview
	MediaType    string          `json:"mediaType,omitempty"`
	MediaMaxSize int             `json:"mediaMaxSize,omitempty"`
	MediaCrop    bool            `json:"mediaCrop"`
	MediaDur     int             `json:"mediaDuration,omitempty"`
	Voice        *bridgeVoice    `json:"voice,omitempty"`
	Document     *bridgeDocument `json:"document,omitempty"`
	Audio        *bridgeAudio    `json:"audio,omitempty"`
	GroupPos     string          `json:"groupPos,omitempty"`
}

type quoteFrom struct {
	ID        int64    `json:"id"`
	Name      any      `json:"name"`       // string, or false when hidden
	FirstName any      `json:"first_name"` // string, or false when hidden
	Photo     struct{} `json:"photo"`
}

type quoteReply struct {
	ChatID   int64          `json:"chatId"`
	Name     string         `json:"name"`
	Text     string         `json:"text"`
	Entities []bridgeEntity `json:"entities"`
	From     *quoteFrom     `json:"from"`
}

type bridgeVoice struct {
	Waveform []int `json:"waveform"`
	Duration int   `json:"duration,omitempty"`
}

type bridgeDocument struct {
	FileName string `json:"file_name"`
	FileSize int64  `json:"file_size,omitempty"`
}

type bridgeAudio struct {
	Title     string `json:"title"`
	Performer string `json:"performer,omitempty"`
	Duration  int    `json:"duration,omitempty"`
}

// bridgeEntity is a Telegram message entity in quote-api terms.
type bridgeEntity struct {
	Type     string `json:"type"`
	Offset   int    `json:"offset"`
	Length   int    `json:"length"`
	URL      string `json:"url,omitempty"`
	Language string `json:"language,omitempty"`
}

// bridgeRequest is the stdin payload for bridge.mjs.
type bridgeRequest struct {
	Messages        []json.RawMessage `json:"messages"`
	Type            string            `json:"type"`
	Format          string            `json:"format"`
	Scale           int               `json:"scale"`
	BackgroundColor string            `json:"backgroundColor"`
	EmojiBrand      string            `json:"emojiBrand"`
	AssetsDir       string            `json:"assetsDir"`
}

// bridgeResult is the decoded stdout frame.
type bridgeResult struct {
	Data []byte
	Ext  string // "png" | "webp" | "webm"
}

// nodeRuntime looks up node; returns "" when unavailable.
func nodeRuntime() string {
	for _, name := range []string{"node", "nodejs"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

// dataDirOf returns the plugin data dir, falling back to ./data/quote.
func dataDirOf(host bridgeHost) string {
	if host != nil {
		if dir, err := host.DataDir("quote"); err == nil && dir != "" {
			return dir
		}
	}
	return filepath.Join("data", "quote")
}

// bridgeHost is the slice of plugin.Host the bridge needs.
type bridgeHost interface {
	DataDir(plugin string) (string, error)
}

// ensureBridgeFile materializes the embedded bridge.mjs in dir.
func ensureBridgeFile(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	dst := filepath.Join(dir, "bridge.mjs")
	if cur, err := os.ReadFile(dst); err == nil && string(cur) == bridgeSource {
		return dst, nil
	}
	if err := os.WriteFile(dst, []byte(bridgeSource), 0o644); err != nil {
		return "", err
	}
	return dst, nil
}

// bridgeMu serializes bridge runs: two concurrent quotes must not both run
// the first-run bootstrap (npm install / asset download / .ready write) in
// the same data dir at once.
var bridgeMu sync.Mutex

// runBridge spawns node bridge.mjs, feeds req as JSON on stdin and decodes
// the framed binary output.
func runBridge(ctx context.Context, host bridgeHost, req *bridgeRequest) (*bridgeResult, error) {
	bridgeMu.Lock()
	defer bridgeMu.Unlock()
	node := nodeRuntime()
	if node == "" {
		return nil, errors.New("node not found — quote 渲染需要 Node.js (node)")
	}
	dir := dataDirOf(host)
	script, err := ensureBridgeFile(dir)
	if err != nil {
		return nil, fmt.Errorf("prepare bridge: %w", err)
	}
	// cmd.Dir changes the process cwd — the script argument must be absolute
	// or node resolves it relative to the new cwd.
	if script, err = filepath.Abs(script); err != nil {
		return nil, err
	}

	payload, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	c, cancel := context.WithTimeout(ctx, bridgeTimeout)
	defer cancel()
	cmd := exec.CommandContext(c, node, script)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(string(payload))
	var stderrBuf errBuffer
	cmd.Stderr = &stderrBuf
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderrBuf.String())
		if msg == "" {
			msg = err.Error()
		}
		// surface the tail of the stderr (most informative part),
		// cut on rune boundaries so multibyte text stays valid UTF-8
		if r := []rune(msg); len(r) > 400 {
			msg = string(r[len(r)-400:])
		}
		if i := strings.LastIndexByte(msg, '\n'); i >= 0 && i+1 < len(msg) {
			msg = msg[i+1:]
		}
		if c.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("bridge timeout after %s (first run downloads assets; retry): %s", bridgeTimeout, msg)
		}
		return nil, fmt.Errorf("bridge: %s", msg)
	}
	return parseBridgeFrame(out)
}

// errBuffer keeps the last 8 KiB of stderr, cut on rune boundaries so the
// string stays valid UTF-8.
type errBuffer struct{ b []byte }

func (e *errBuffer) Write(p []byte) (int, error) {
	e.b = append(e.b, p...)
	if len(e.b) > 8192 {
		keep := e.b[len(e.b)-8192:]
		// drop a partial leading rune (leading continuation bytes)
		for len(keep) > 0 && !utf8.RuneStart(keep[0]) {
			keep = keep[1:]
		}
		e.b = keep
	}
	// drop any incomplete trailing rune (a start byte missing some of its
	// continuation bytes) so String() is always valid UTF-8
	for n := len(e.b); n > 0; {
		r, sz := utf8.DecodeLastRune(e.b[:n])
		if r != utf8.RuneError || sz > 1 {
			break
		}
		n-- // drop the incomplete byte (0xe4, 0xe4 0xbd, …)
		e.b = e.b[:n]
	}
	return len(p), nil
}
func (e *errBuffer) String() string { return string(e.b) }

// parseBridgeFrame decodes magic+len+ext+reserved then the payload.
func parseBridgeFrame(out []byte) (*bridgeResult, error) {
	if len(out) < bridgeHeaderSz {
		return nil, fmt.Errorf("bridge output too short: %d bytes", len(out))
	}
	if string(out[:4]) != bridgeMagic {
		return nil, fmt.Errorf("bridge output bad magic: %q", out[:4])
	}
	n := binary.BigEndian.Uint32(out[4:8])
	extCode := int(binary.BigEndian.Uint32(out[8:12]))
	body := out[bridgeHeaderSz:]
	if uint32(len(body)) != n {
		return nil, fmt.Errorf("bridge frame length mismatch: header %d, body %d", n, len(body))
	}
	var ext string
	switch extCode {
	case extCodePNG:
		ext = "png"
	case extCodeWebP:
		ext = "webp"
	case extCodeWebM:
		ext = "webm"
	default:
		return nil, fmt.Errorf("bridge frame unknown ext code %d", extCode)
	}
	return &bridgeResult{Data: body, Ext: ext}, nil
}

// detectImageExt sniffs png/webp magic bytes (defense in depth against a
// mismatched ext in the frame).
func detectImageExt(b []byte) string {
	if len(b) >= 8 && b[0] == 0x89 && b[1] == 0x50 && b[2] == 0x4E && b[3] == 0x47 {
		return "png"
	}
	if len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP" {
		return "webp"
	}
	return ""
}

// frameFor builds a binary frame (used by tests and as documentation of the
// wire format).
func frameFor(data []byte, ext string) []byte {
	code := extCodePNG
	switch ext {
	case "webp":
		code = extCodeWebP
	case "webm":
		code = extCodeWebM
	}
	hdr := make([]byte, bridgeHeaderSz)
	copy(hdr, bridgeMagic)
	binary.BigEndian.PutUint32(hdr[4:8], uint32(len(data)))
	binary.BigEndian.PutUint32(hdr[8:12], uint32(code))
	return append(hdr, data...)
}
