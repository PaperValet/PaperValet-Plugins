package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/tg"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

const (
	maxMediaSize = 10 << 20 // vision APIs reject much bigger payloads anyway
	tgsMime      = "application/x-tgsticker"
	webmMime     = "video/webm"
	convertCmds  = 3 * time.Minute
)

// convertError marks a sticker whose first frame could not be rendered; kind
// names the missing tooling so the user gets the install hint.
type convertError struct{ tgs bool }

func (e *convertError) Error() string { return "sticker conversion failed" }

func convertErrText(ctx *plugin.CommandContext, e *convertError) string {
	if e.tgs {
		return "❌ " + ctx.Tlocal(
			"TGS 贴纸转换失败\n\n需要安装: "+plugin.Code("pip3 install rlottie-python")+" 和 "+plugin.Code("ffmpeg"),
			"TGS sticker conversion failed\n\nNeeds: "+plugin.Code("pip3 install rlottie-python")+" and "+plugin.Code("ffmpeg"))
	}
	return "❌ " + ctx.Tlocal(
		"WebM 贴纸转换失败\n\n需要安装: "+plugin.Code("ffmpeg"),
		"WebM sticker conversion failed\n\nNeeds: "+plugin.Code("ffmpeg"))
}

// detectImageMime sniffs jpeg/png/gif/webp from magic bytes, like the source.
// Buffers shorter than 4 bytes are rejected outright, like the source.
func detectImageMime(b []byte) string {
	if len(b) < 4 {
		return ""
	}
	switch {
	case b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return "image/jpeg"
	case b[0] == 0x89 && b[1] == 0x50 && b[2] == 0x4E && b[3] == 0x47:
		return "image/png"
	case b[0] == 0x47 && b[1] == 0x49 && b[2] == 0x46 && b[3] == 0x38:
		return "image/gif"
	case b[0] == 0x52 && b[1] == 0x49 && b[2] == 0x46 && b[3] == 0x46 &&
		len(b) >= 12 && b[8] == 0x57 && b[9] == 0x45 && b[10] == 0x42 && b[11] == 0x50:
		return "image/webp"
	}
	return ""
}

// stickerKind classifies a message's media for image extraction.
type stickerKind int

const (
	kindNone stickerKind = iota
	kindPhoto
	kindTgs
	kindWebm
	kindImageDoc
)

type stickerInfo struct {
	kind     stickerKind
	location tg.InputFileLocationClass
	size     int64
}

// classifyReplyMedia mirrors the source's extractMediaInfo routing: photos,
// tgs/webm stickers and image documents. Everything else is not usable.
func classifyReplyMedia(m tg.MessageMediaClass) stickerInfo {
	switch v := m.(type) {
	case *tg.MessageMediaPhoto:
		photo, ok := v.Photo.(*tg.Photo)
		if !ok {
			return stickerInfo{}
		}
		typ, _, _, size := largestPhotoSize(photo.Sizes)
		if typ == "" {
			return stickerInfo{}
		}
		return stickerInfo{
			kind: kindPhoto, size: int64(size),
			location: &tg.InputPhotoFileLocation{
				ID: photo.ID, AccessHash: photo.AccessHash,
				FileReference: photo.FileReference, ThumbSize: typ,
			},
		}
	case *tg.MessageMediaDocument:
		d, ok := v.Document.(*tg.Document)
		if !ok {
			return stickerInfo{}
		}
		mime := strings.ToLower(d.MimeType)
		info := stickerInfo{size: d.Size, location: &tg.InputDocumentFileLocation{
			ID: d.ID, AccessHash: d.AccessHash, FileReference: d.FileReference,
		}}
		switch {
		case mime == tgsMime:
			info.kind = kindTgs
		case mime == webmMime:
			info.kind = kindWebm
		case strings.HasPrefix(mime, "image/"):
			info.kind = kindImageDoc
		default:
			return stickerInfo{}
		}
		return info
	}
	return stickerInfo{}
}

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

// mediaFromMessage downloads the replied message's media and returns it as a
// vision-ready image: photos directly, webm/tgs stickers reduced to their
// first PNG frame, image documents as-is. nil (with nil error) when the media
// type is not usable.
func (p *XmslPlugin) mediaFromMessage(ctx *plugin.CommandContext, msg *tg.Message, dir string) (*mediaInput, error) {
	info := classifyReplyMedia(msg.Media)
	if info.kind == kindNone {
		return nil, nil
	}
	if info.size > maxMediaSize {
		return nil, fmt.Errorf(ctx.Tlocal(
			"文件过大（%d MB），上限 %d MB", "file too large (%d MB), limit %d MB"),
			info.size>>20, maxMediaSize>>20)
	}

	tmp := filepath.Join(dir, "tmp")
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return nil, err
	}
	id := fmt.Sprintf("%d_%06d", time.Now().UnixNano(), rand.IntN(1000000))

	var (
		src     string
		out     string
		convert bool
	)
	switch info.kind {
	case kindPhoto:
		src = filepath.Join(tmp, "xmsl_"+id+".jpg")
	case kindImageDoc:
		src = filepath.Join(tmp, "xmsl_"+id+".bin")
	case kindTgs:
		src = filepath.Join(tmp, "xmsl_"+id+".tgs")
		out = filepath.Join(tmp, "xmsl_"+id+".png")
		convert = true
	case kindWebm:
		src = filepath.Join(tmp, "xmsl_"+id+".webm")
		out = filepath.Join(tmp, "xmsl_"+id+".png")
		convert = true
	}
	defer os.Remove(src)
	defer os.Remove(out)

	dctx, cancel := context.WithTimeout(ctx.Context(), convertCmds)
	defer cancel()
	if _, err := downloader.NewDownloader().Download(ctx.API, info.location).ToPath(dctx, src); err != nil {
		return nil, fmt.Errorf("%s: %w", ctx.Tlocal("下载媒体失败", "media download failed"), err)
	}

	if convert {
		if info.kind == kindTgs {
			if err := renderTgsFirstFrame(ctx.Context(), src, out); err != nil {
				return nil, &convertError{tgs: true}
			}
		} else {
			if err := extractVideoFirstFrame(ctx.Context(), src, out); err != nil {
				return nil, &convertError{tgs: false}
			}
		}
		src = out
	}

	data, err := os.ReadFile(src)
	if err != nil {
		return nil, err
	}
	mime := detectImageMime(data)
	if mime == "" {
		return nil, errors.New(ctx.Tlocal("无法识别媒体格式（仅支持 jpeg/png/gif/webp）", "unrecognized media format (jpeg/png/gif/webp only)"))
	}
	return &mediaInput{mimeType: mime, data: data}, nil
}

// renderTgsFirstFrame renders a Lottie TGS sticker to GIF with
// rlottie-python, then takes its first frame, like the source.
func renderTgsFirstFrame(ctx context.Context, tgsPath, pngPath string) error {
	bin, err := exec.LookPath("python3")
	if err != nil {
		return err
	}
	// Convert in a scratch dir so the intermediate GIF is cleaned up with it.
	dir := filepath.Dir(tgsPath)
	scratch, err := os.MkdirTemp(dir, "tgs-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)
	gifPath := filepath.Join(scratch, "frame.gif")

	script := "import sys\nfrom rlottie_python import LottieAnimation\n" +
		"anim = LottieAnimation.from_tgs(sys.argv[1])\nanim.save_animation(sys.argv[2])\n"
	c, cancel := context.WithTimeout(ctx, convertCmds)
	defer cancel()
	if out, err := exec.CommandContext(c, bin, "-c", script, tgsPath, gifPath).CombinedOutput(); err != nil {
		msg := firstLine(out, err)
		return fmt.Errorf("rlottie: %s", msg)
	}
	return extractVideoFirstFrame(ctx, gifPath, pngPath)
}

// extractVideoFirstFrame dumps frame 0 to PNG with ffmpeg.
func extractVideoFirstFrame(ctx context.Context, in, out string) error {
	bin, err := exec.LookPath("ffmpeg")
	if err != nil {
		return err
	}
	c, cancel := context.WithTimeout(ctx, convertCmds)
	defer cancel()
	args := []string{"-hide_banner", "-loglevel", "error", "-y", "-i", in,
		"-vf", "select=eq(n\\,0)", "-vframes", "1", out}
	if out2, err := exec.CommandContext(c, bin, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("ffmpeg: %s", firstLine(out2, err))
	}
	if st, err := os.Stat(out); err != nil || st.Size() == 0 {
		return errors.New("no frame extracted")
	}
	return nil
}

func firstLine(out []byte, err error) string {
	msg := strings.TrimSpace(string(out))
	if msg == "" {
		msg = err.Error()
	}
	if i := strings.IndexByte(msg, '\n'); i > 0 {
		msg = msg[:i]
	}
	return msg
}
