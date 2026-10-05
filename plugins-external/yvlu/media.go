// media.go downloads message media for quote payloads (with tgs/mp4 → webm
// conversion), and sends the generated quote as sticker/photo.

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

const (
	convertTimeout = 3 * time.Minute
	maxMediaBytes  = 30 << 20
	pythonPath     = "python3"
)

// downloadBytes downloads a file location into memory.
func downloadBytes(ctx context.Context, api *tg.Client, loc tg.InputFileLocationClass) ([]byte, error) {
	c, cancel := context.WithTimeout(ctx, convertTimeout)
	defer cancel()
	var buf bytes.Buffer
	if _, err := downloader.NewDownloader().Download(api, loc).Stream(c, &buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// downloadFile is downloadBytes for external callers, kept for clarity.
func downloadFile(ctx context.Context, api *tg.Client, loc tg.InputFileLocationClass) *downloader.Builder {
	return downloader.NewDownloader().Download(api, loc)
}

// largestPhotoSize returns the biggest downloadable photo size.
func largestPhotoSize(sizes []tg.PhotoSizeClass) (typ string, w, h, size int) {
	best := -1
	for _, s := range sizes {
		switch v := s.(type) {
		case *tg.PhotoSize:
			if v.W*v.H > best {
				best, typ, w, h, size = v.W*v.H, v.Type, v.W, v.H, v.Size
			}
		case *tg.PhotoSizeProgressive:
			if v.W*v.H > best {
				sz := 0
				if len(v.Sizes) > 0 {
					sz = v.Sizes[len(v.Sizes)-1]
				}
				best, typ, w, h, size = v.W*v.H, v.Type, v.W, v.H, sz
			}
		}
	}
	return
}

// mediaDoc returns the document of a document message.
func mediaDoc(m tg.MessageMediaClass) (*tg.Document, bool) {
	md, ok := m.(*tg.MessageMediaDocument)
	if !ok {
		return nil, false
	}
	d, ok := md.Document.(*tg.Document)
	return d, ok
}

// docAttr helpers.
func docSticker(d *tg.Document) (*tg.DocumentAttributeSticker, bool) {
	for _, a := range d.Attributes {
		if v, ok := a.(*tg.DocumentAttributeSticker); ok {
			return v, true
		}
	}
	return nil, false
}

func docFileName(d *tg.Document) string {
	for _, a := range d.Attributes {
		if v, ok := a.(*tg.DocumentAttributeFilename); ok {
			return v.FileName
		}
	}
	return ""
}

// thumbLocation picks the biggest document thumbnail as a file location.
func thumbLocation(d *tg.Document) (tg.InputFileLocationClass, bool) {
	typ, _, _, _ := largestPhotoSize(d.Thumbs)
	if typ == "" {
		return nil, false
	}
	return &tg.InputDocumentFileLocation{
		ID: d.ID, AccessHash: d.AccessHash,
		FileReference: d.FileReference, ThumbSize: typ,
	}, true
}

// isGzip reports whether b starts with the gzip magic (a TGS sticker).
func isGzip(b []byte) bool { return len(b) >= 2 && b[0] == 0x1f && b[1] == 0x8b }

// isMp4 reports whether b looks like an MP4 container.
func isMp4(b []byte) bool { return len(b) >= 8 && string(b[4:8]) == "ftyp" }

// runCmd runs an external command with a timeout and trims its error output.
func runCmd(ctx context.Context, name string, args []string) error {
	bin, err := exec.LookPath(name)
	if err != nil {
		return fmt.Errorf("%s not found", name)
	}
	c, cancel := context.WithTimeout(ctx, convertTimeout)
	defer cancel()
	out, err := exec.CommandContext(c, bin, args...).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if i := strings.IndexByte(msg, '\n'); i > 0 {
			msg = msg[:i]
		}
		if msg == "" {
			msg = err.Error()
		}
		if c.Err() != nil {
			msg = "timeout"
		}
		return fmt.Errorf("%s: %s", name, msg)
	}
	return nil
}

// tgsToWebm converts a TGS (gzipped Lottie) sticker to webm via
// rlottie-python + ffmpeg, mirroring the source pipeline.
func tgsToWebm(ctx context.Context, tmpDir string, tgs []byte) ([]byte, error) {
	id := fmt.Sprintf("%d_%06d", time.Now().UnixNano(), rand.IntN(1000000))
	tgsPath := filepath.Join(tmpDir, "tgs_"+id+".tgs")
	gifPath := filepath.Join(tmpDir, "tgs_"+id+".gif")
	webmPath := filepath.Join(tmpDir, "tgs_"+id+".webm")
	defer os.Remove(tgsPath)
	defer os.Remove(gifPath)
	defer os.Remove(webmPath)
	if err := os.WriteFile(tgsPath, tgs, 0o600); err != nil {
		return nil, err
	}
	script := "import sys\nfrom rlottie_python import LottieAnimation\n" +
		"anim = LottieAnimation.from_tgs(sys.argv[1])\nanim.save_animation(sys.argv[2])\n"
	if err := runCmd(ctx, pythonPath, []string{"-c", script, tgsPath, gifPath}); err != nil {
		return nil, fmt.Errorf("tgs: %w", err)
	}
	if err := runCmd(ctx, "ffmpeg", []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-i", gifPath, "-c:v", "libvpx-vp9", "-pix_fmt", "yuva420p",
		"-b:v", "400k", "-auto-alt-ref", "0", "-an", webmPath,
	}); err != nil {
		return nil, fmt.Errorf("tgs: %w", err)
	}
	return os.ReadFile(webmPath)
}

// mp4ToWebm converts an MP4/GIF video to webm with alpha.
func mp4ToWebm(ctx context.Context, tmpDir string, mp4 []byte) ([]byte, error) {
	id := fmt.Sprintf("%d_%06d", time.Now().UnixNano(), rand.IntN(1000000))
	in := filepath.Join(tmpDir, "vid_"+id+".mp4")
	out := filepath.Join(tmpDir, "vid_"+id+".webm")
	defer os.Remove(in)
	defer os.Remove(out)
	if err := os.WriteFile(in, mp4, 0o600); err != nil {
		return nil, err
	}
	if err := runCmd(ctx, "ffmpeg", []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-i", in, "-c:v", "libvpx-vp9", "-pix_fmt", "yuva420p",
		"-b:v", "400k", "-auto-alt-ref", "0", "-an", out,
	}); err != nil {
		return nil, err
	}
	return os.ReadFile(out)
}

// mediaDataURI downloads the message media suited for a quote and returns it
// as a data: URI (animated sticker/video/gif as webm, photos as jpeg,
// documents as their thumbnail — matching the source).
func mediaDataURI(e *msgEnv, msg *tg.Message) (string, bool) {
	if e.api == nil || msg.Media == nil {
		return "", false
	}
	switch m := msg.Media.(type) {
	case *tg.MessageMediaPhoto:
		photo, ok := m.Photo.(*tg.Photo)
		if !ok {
			return "", false
		}
		typ, _, _, _ := largestPhotoSize(photo.Sizes)
		if typ == "" {
			return "", false
		}
		loc := &tg.InputPhotoFileLocation{
			ID: photo.ID, AccessHash: photo.AccessHash,
			FileReference: photo.FileReference, ThumbSize: typ,
		}
		data, err := downloadBytes(e.ctx, e.api, loc)
		if err != nil || len(data) == 0 {
			return "", false
		}
		return "data:image/jpeg;base64," + base64Encode(data), true
	case *tg.MessageMediaDocument:
		d, ok := m.Document.(*tg.Document)
		if !ok {
			return "", false
		}
		if d.Size > maxMediaBytes {
			return "", false
		}
		mime := strings.ToLower(d.MimeType)
		_, isSticker := docSticker(d)
		animated := isSticker && (mime == "video/webm" || mime == "application/x-tgsticker" || mime == "image/webp") ||
			mime == "video/mp4" || mime == "image/gif"

		var loc tg.InputFileLocationClass
		if animated {
			loc = &tg.InputDocumentFileLocation{ID: d.ID, AccessHash: d.AccessHash, FileReference: d.FileReference}
		} else if l, ok := thumbLocation(d); ok {
			loc = l
		} else {
			return "", false
		}
		data, err := downloadBytes(e.ctx, e.api, loc)
		if err != nil || len(data) == 0 {
			return "", false
		}
		finalMime := mime
		if isSticker && mime == "" {
			finalMime = "image/webp"
		}
		switch {
		case isSticker && (mime == "application/x-tgsticker" || isGzip(data)):
			if webm, cerr := tgsToWebm(e.ctx, e.tmp, data); cerr == nil && len(webm) > 0 {
				return "data:video/webm;base64," + base64Encode(webm), true
			}
			// conversion failed: fall back to whatever was downloaded
		case mime == "video/mp4" || mime == "image/gif" || isMp4(data):
			if webm, cerr := mp4ToWebm(e.ctx, e.tmp, data); cerr == nil && len(webm) > 0 {
				return "data:video/webm;base64," + base64Encode(webm), true
			}
		}
		if finalMime == "" {
			finalMime = "application/octet-stream"
		}
		return "data:" + finalMime + ";base64," + base64Encode(data), true
	}
	return "", false
}

func base64Encode(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// ---------------------------------------------------------------- sending

// sendResult sends the generated file: webp/webm as a sticker, png as a
// photo, replying to the quoted message (source behaviour).
func (p *YvluPlugin) sendResult(ctx *plugin.CommandContext, path, kind string, replyTo int) error {
	peer, err := ctx.ResolvePeer()
	if err != nil {
		return err
	}
	uctx, cancel := context.WithTimeout(ctx.Context(), convertTimeout)
	defer cancel()
	file, err := uploader.NewUploader(ctx.API).FromPath(uctx, path)
	if err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	var media tg.InputMediaClass
	switch kind {
	case "webm":
		media = &tg.InputMediaUploadedDocument{
			File:     file,
			MimeType: "video/webm",
			Attributes: []tg.DocumentAttributeClass{
				&tg.DocumentAttributeFilename{FileName: "yvlu.webm"},
				&tg.DocumentAttributeSticker{Alt: "📝", Stickerset: &tg.InputStickerSetEmpty{}},
			},
		}
	case "webp":
		media = &tg.InputMediaUploadedDocument{
			File:     file,
			MimeType: "image/webp",
			Attributes: []tg.DocumentAttributeClass{
				&tg.DocumentAttributeFilename{FileName: "yvlu.webp"},
				&tg.DocumentAttributeSticker{Alt: "📝", Stickerset: &tg.InputStickerSetEmpty{}},
			},
		}
	default: // png
		media = &tg.InputMediaUploadedPhoto{File: file}
	}
	req := &tg.MessagesSendMediaRequest{
		Peer:     peer,
		Media:    media,
		RandomID: rand.Int64(),
	}
	if replyTo > 0 {
		req.SetReplyTo(&tg.InputReplyToMessage{ReplyToMsgID: replyTo})
	}
	_, err = ctx.API.MessagesSendMedia(uctx, req)
	return err
}

// uploadStickerFile uploads a local file as an InputMediaUploadedDocument
// sticker and runs messages.uploadMedia to get a reusable InputDocument.
func uploadStickerDoc(ctx *plugin.CommandContext, path, mime string) (*tg.InputDocument, error) {
	uctx, cancel := context.WithTimeout(ctx.Context(), convertTimeout)
	defer cancel()
	file, err := uploader.NewUploader(ctx.API).FromPath(uctx, path)
	if err != nil {
		return nil, fmt.Errorf("upload: %w", err)
	}
	peer, err := ctx.ResolvePeer()
	if err != nil {
		return nil, err
	}
	media := &tg.InputMediaUploadedDocument{
		File:     file,
		MimeType: mime,
		Attributes: []tg.DocumentAttributeClass{
			&tg.DocumentAttributeFilename{FileName: filepath.Base(path)},
			&tg.DocumentAttributeSticker{Alt: "📝", Stickerset: &tg.InputStickerSetEmpty{}},
		},
	}
	res, err := ctx.API.MessagesUploadMedia(uctx, &tg.MessagesUploadMediaRequest{Peer: peer, Media: media})
	if err != nil {
		return nil, err
	}
	md, ok := res.(*tg.MessageMediaDocument)
	if !ok {
		return nil, fmt.Errorf("unexpected upload result %T", res)
	}
	doc, ok := md.Document.(*tg.Document)
	if !ok {
		return nil, errors.New("upload returned no document")
	}
	return &tg.InputDocument{
		ID:            doc.ID,
		AccessHash:    doc.AccessHash,
		FileReference: doc.FileReference,
	}, nil
}
