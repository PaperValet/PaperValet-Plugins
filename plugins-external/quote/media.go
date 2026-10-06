package main

// media.go — media download for the quote renderer: photos, stickers,
// animations/videos (first frame via ffmpeg when needed), returned as raw
// bytes for the node renderer (base64 over the bridge).

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/tg"
)

const maxDLSize = 30 << 20 // 30 MB cap for preview downloads

// limitedWriter accepts up to N bytes, then errors so the download stream
// aborts instead of buffering a file larger than the cap.
type limitedWriter struct {
	w   *bytes.Buffer
	n   int64 // bytes written so far
	max int64
}

var errTooLarge = errors.New("file exceeds download cap")

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.n+int64(len(p)) > l.max {
		return 0, errTooLarge
	}
	n, err := l.w.Write(p)
	l.n += int64(n)
	return n, err
}

// downloadLocation downloads an input file location to memory (bounded).
func downloadLocation(ctx context.Context, api *tg.Client, loc tg.InputFileLocationClass, size int64) ([]byte, error) {
	if size > maxDLSize {
		return nil, fmt.Errorf("file too large (%d MB)", size>>20)
	}
	var buf bytes.Buffer
	lw := &limitedWriter{w: &buf, max: maxDLSize + 1<<20} // small slack for misreported sizes
	if _, err := downloader.NewDownloader().Download(api, loc).Stream(ctx, lw); err != nil {
		if errors.Is(err, errTooLarge) {
			return nil, fmt.Errorf("file too large (over %d MB)", maxDLSize>>20)
		}
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

// fetchMediaPreviewBytes downloads the visual preview of a message's media
// as raw image/video bytes (the node renderer decodes and crops), or nil
// when it has none / on failure.
func fetchMediaPreviewBytes(ctx context.Context, api *tg.Client, m tg.MessageMediaClass) []byte {
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
		return data
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
				return png
			}
			return nil
		}
		return data
	case mediaAnimation, mediaVideo, mediaRoundVideo:
		ext := ".mp4"
		if strings.Contains(info.mime, "webm") {
			ext = ".webm"
		}
		loc := &tg.InputDocumentFileLocation{ID: doc.ID, AccessHash: doc.AccessHash, FileReference: doc.FileReference}
		data, err := downloadLocation(ctx, api, loc, doc.Size)
		if err != nil {
			return nil
		}
		png, err := ffmpegFirstFrame(ctx, data, ext)
		if err != nil {
			return nil
		}
		return png
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
			if png, err := ffmpegFirstFrame(ctx, data, ".gif"); err == nil {
				return png
			}
			return nil
		}
		return data
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
