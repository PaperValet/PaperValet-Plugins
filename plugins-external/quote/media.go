package main

// media.go — media download (photos, stickers, videos via ffmpeg first
// frame) and decoding (jpeg/png/webp stdlib + x/image/webp).

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	_ "golang.org/x/image/webp"

	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/tg"
)

const maxDLSize = 30 << 20 // 30 MB cap for preview downloads

// downloadLocation downloads an input file location to memory (bounded).
func downloadLocation(ctx context.Context, api *tg.Client, loc tg.InputFileLocationClass, size int64) ([]byte, error) {
	if size > maxDLSize {
		return nil, fmt.Errorf("file too large (%d MB)", size>>20)
	}
	var buf bytes.Buffer
	if _, err := downloader.NewDownloader().Download(api, loc).Stream(ctx, &buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ffmpegFirstFrame extracts the first frame of a video as PNG bytes.
func ffmpegFirstFrame(ctx context.Context, input []byte, ext string) ([]byte, error) {
	bin, err := exec.LookPath("ffmpeg")
	if err != nil {
		return nil, errors.New("ffmpeg not found")
	}
	dir, err := os.MkdirTemp("", "quote-ffmpeg-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	in := filepath.Join(dir, "in"+ext)
	out := filepath.Join(dir, "out.png")
	if err := os.WriteFile(in, input, 0o600); err != nil {
		return nil, err
	}
	c, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	args := []string{"-hide_banner", "-loglevel", "error", "-y"}
	if ext == ".webm" {
		args = append(args, "-c:v", "libvpx-vp9")
	}
	args = append(args, "-i", in, "-frames:v", "1", "-vf",
		"scale=512:512:force_original_aspect_ratio=decrease:flags=lanczos", "-f", "image2", out)
	if out, err := exec.CommandContext(c, bin, args...).CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		if i := strings.IndexByte(msg, '\n'); i > 0 {
			msg = msg[:i]
		}
		return nil, fmt.Errorf("ffmpeg: %s", msg)
	}
	return os.ReadFile(out)
}

// decodeImage decodes jpeg/png/webp (static) bytes.
func decodeImage(data []byte) (image.Image, error) {
	img, _, err := image.Decode(bytes.NewReader(data))
	return img, err
}

// ffmpegWebp converts PNG bytes to lossless webp (512px cap) for stickers.
func ffmpegWebp(ctx context.Context, pngData []byte) ([]byte, error) {
	bin, err := exec.LookPath("ffmpeg")
	if err != nil {
		return nil, errors.New("ffmpeg not found")
	}
	dir, err := os.MkdirTemp("", "quote-webp-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	in := filepath.Join(dir, "in.png")
	out := filepath.Join(dir, "out.webp")
	if err := os.WriteFile(in, pngData, 0o600); err != nil {
		return nil, err
	}
	c, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	args := []string{"-hide_banner", "-loglevel", "error", "-y", "-i", in,
		"-vf", "scale=512:512:force_original_aspect_ratio=decrease",
		"-c:v", "libwebp", "-lossless", "1", "-frames:v", "1", out}
	if outB, err := exec.CommandContext(c, bin, args...).CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(outB))
		if msg == "" {
			msg = err.Error()
		}
		if i := strings.IndexByte(msg, '\n'); i > 0 {
			msg = msg[:i]
		}
		return nil, fmt.Errorf("ffmpeg: %s", msg)
	}
	return os.ReadFile(out)
}

// docAttributes extracts handy attributes from a document.
type docInfo struct {
	fileName   string
	mime       string
	size       int64
	sticker    bool
	animated   bool
	voice      bool
	audio      bool
	video      bool
	roundVideo bool
	w, h       int
	duration   int
	waveform   []byte
	title      string
	performer  string
}

func parseDoc(d *tg.Document) docInfo {
	info := docInfo{mime: strings.ToLower(d.MimeType), size: d.Size}
	for _, a := range d.Attributes {
		switch v := a.(type) {
		case *tg.DocumentAttributeFilename:
			info.fileName = v.FileName
		case *tg.DocumentAttributeSticker:
			info.sticker = true
		case *tg.DocumentAttributeAnimated:
			info.animated = true
		case *tg.DocumentAttributeAudio:
			if v.Voice {
				info.voice = true
			} else {
				info.audio = true
			}
			info.duration = int(v.Duration)
			info.waveform = v.Waveform
			info.title = v.Title
			info.performer = v.Performer
		case *tg.DocumentAttributeVideo:
			info.video = true
			info.w, info.h = v.W, v.H
			info.duration = int(v.Duration)
			if v.RoundMessage {
				info.roundVideo = true
			}
		case *tg.DocumentAttributeImageSize:
			info.w, info.h = v.W, v.H
		}
	}
	return info
}

// photoLocationAndSize returns the biggest photo size location.
func photoLocation(p *tg.Photo) (tg.InputFileLocationClass, int64, int, int, bool) {
	bestType, bestW, bestH, bestSize := "", 0, 0, -1
	for _, s := range p.Sizes {
		switch v := s.(type) {
		case *tg.PhotoSize:
			if v.Size > bestSize {
				bestType, bestW, bestH, bestSize = v.Type, v.W, v.H, v.Size
			}
		case *tg.PhotoSizeProgressive:
			sz := 0
			if n := len(v.Sizes); n > 0 {
				sz = v.Sizes[n-1]
			}
			if sz > bestSize {
				bestType, bestW, bestH, bestSize = v.Type, v.W, v.H, sz
			}
		}
	}
	if bestType == "" {
		return nil, 0, 0, 0, false
	}
	return &tg.InputPhotoFileLocation{
		ID:            p.ID,
		AccessHash:    p.AccessHash,
		FileReference: p.FileReference,
		ThumbSize:     bestType,
	}, int64(bestSize), bestW, bestH, true
}

// mediaKindOf classifies a message's media for rendering purposes.
type mediaKind int

const (
	mediaNone mediaKind = iota
	mediaPhoto
	mediaSticker
	mediaVideo
	mediaRoundVideo
	mediaAnimation
	mediaDocument
	mediaVoice
	mediaAudio
	mediaOther
)

func classifyMedia(m tg.MessageMediaClass) (mediaKind, *docInfo, *tg.Photo, *tg.Document) {
	switch v := m.(type) {
	case *tg.MessageMediaPhoto:
		if p, ok := v.Photo.(*tg.Photo); ok {
			return mediaPhoto, nil, p, nil
		}
		return mediaOther, nil, nil, nil
	case *tg.MessageMediaDocument:
		if d, ok := v.Document.(*tg.Document); ok {
			info := parseDoc(d)
			switch {
			case info.sticker:
				return mediaSticker, &info, nil, d
			case info.animated && info.video:
				return mediaAnimation, &info, nil, d
			case info.voice:
				return mediaVoice, &info, nil, d
			case info.audio:
				return mediaAudio, &info, nil, d
			case info.roundVideo:
				return mediaRoundVideo, &info, nil, d
			case info.video:
				return mediaVideo, &info, nil, d
			default:
				return mediaDocument, &info, nil, d
			}
		}
	}
	return mediaNone, nil, nil, nil
}

// fetchMediaPreview downloads and decodes the visual preview of a message's
// media, or returns nil when it has none / on failure.
func fetchMediaPreview(ctx context.Context, api *tg.Client, m tg.MessageMediaClass) image.Image {
	kind, info, photo, doc := classifyMedia(m)
	switch kind {
	case mediaPhoto:
		loc, size, _, _, ok := photoLocation(photo)
		if !ok {
			return nil
		}
		data, err := downloadLocation(ctx, api, loc, size)
		if err != nil {
			return nil
		}
		img, err := decodeImage(data)
		if err != nil {
			return nil
		}
		return img
	case mediaSticker:
		ext := ".webp"
		if strings.Contains(info.mime, "webm") {
			ext = ".webm"
		}
		loc := &tg.InputDocumentFileLocation{ID: doc.ID, AccessHash: doc.AccessHash, FileReference: doc.FileReference}
		data, err := downloadLocation(ctx, api, loc, doc.Size)
		if err != nil {
			return nil
		}
		if ext == ".webm" {
			if png, err := ffmpegFirstFrame(ctx, data, ext); err == nil {
				if img, err := decodeImage(png); err == nil {
					return img
				}
			}
			return nil
		}
		img, err := decodeImage(data)
		if err != nil {
			return nil
		}
		return img
	case mediaAnimation, mediaVideo, mediaRoundVideo:
		ext := ".mp4"
		if strings.Contains(info.mime, "webm") {
			ext = ".webm"
		}
		loc := &tg.InputDocumentFileLocation{ID: doc.ID, AccessHash: doc.AccessHash, FileReference: doc.FileReference}
		data, err := downloadLocation(ctx, api, loc, min(doc.Size, int64(maxDLSize)))
		if err != nil {
			return nil
		}
		png, err := ffmpegFirstFrame(ctx, data, ext)
		if err != nil {
			return nil
		}
		img, err := decodeImage(png)
		if err != nil {
			return nil
		}
		return img
	case mediaDocument:
		// image documents get a preview
		if !strings.HasPrefix(info.mime, "image/") {
			return nil
		}
		loc := &tg.InputDocumentFileLocation{ID: doc.ID, AccessHash: doc.AccessHash, FileReference: doc.FileReference}
		data, err := downloadLocation(ctx, api, loc, doc.Size)
		if err != nil {
			return nil
		}
		if strings.Contains(info.mime, "gif") {
			// ffmpeg for gif first frame (stdlib has no gif decoder
			// registered by default in this plugin)
			if png, err := ffmpegFirstFrame(ctx, data, ".gif"); err == nil {
				if img, err := decodeImage(png); err == nil {
					return img
				}
			}
			return nil
		}
		img, err := decodeImage(data)
		if err != nil {
			return nil
		}
		return img
	}
	return nil
}

// shouldFetchPreview decides whether a visual preview is downloaded.
func shouldFetchPreview(kind mediaKind, force bool) bool {
	switch kind {
	case mediaPhoto, mediaSticker, mediaAnimation:
		return true
	case mediaVideo, mediaRoundVideo:
		return true
	case mediaDocument:
		return force
	}
	return false
}
