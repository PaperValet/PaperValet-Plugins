package main

// media.go implements pic_to_sticker (reply photo → WebP sticker via ffmpeg)
// and sticker_to_pic (reply sticker → JPG/PNG photo via ffmpeg).

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
	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

const (
	convTimeout = 2 * time.Minute
	maxSticker  = 512 << 10 // 512 KB sticker limit
)

// settings snapshot for conversions, read from the panel with fallbacks.
type convSettings struct {
	emoji   string
	size    int
	quality int
	bg      string
}

func (p *StickerPlugin) convSettings(ctx *plugin.CommandContext) convSettings {
	s := convSettings{emoji: "🙂", size: 512, quality: 90, bg: "transparent"}
	if p.set != nil {
		if v := p.set.String("emoji"); v != "" {
			s.emoji = v
		}
		if v := p.set.Int("size"); v >= 128 && v <= 512 {
			s.size = v
		}
		if v := p.set.Int("quality"); v >= 1 && v <= 100 {
			s.quality = v
		}
		if v := p.set.String("bg"); v != "" {
			s.bg = v
		}
	}
	return s
}

// bgHex maps a background name to an ffmpeg color (RGBA for transparent).
func bgHex(bg string) string {
	switch bg {
	case "white":
		return "white"
	case "black":
		return "black"
	}
	return "0x00000000@0"
}

// ffmpegWebpArgs builds the photo→sticker ffmpeg command:
// square canvas, letterboxed, WebP output.
func ffmpegWebpArgs(in, out string, s convSettings) []string {
	pad := fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2:color=%s,format=yuva420p",
		s.size, s.size, s.size, s.size, bgHex(s.bg))
	return []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-i", in,
		"-vf", pad,
		"-c:v", "libwebp", "-lossless", "0",
		"-q:v", fmt.Sprintf("%d", s.quality),
		"-frames:v", "1",
		out,
	}
}

// ffmpegToPicArgs builds the sticker→photo ffmpeg command. Transparent areas
// are composited over white unless transparency is requested (PNG only).
func ffmpegToPicArgs(in, out string, transparent bool) []string {
	args := []string{"-hide_banner", "-loglevel", "error", "-y", "-i", in}
	if strings.HasSuffix(out, ".png") && transparent {
		args = append(args, "-vf", "scale=iw:ih:force_original_aspect_ratio=decrease")
	} else {
		// Flatten alpha onto white.
		args = append(args,
			"-f", "lavfi", "-i", "color=white",
			"-filter_complex", "[1:v][0:v]scale2ref[bg][fg];[bg][fg]overlay=shortest=1:format=auto,format=rgb24")
	}
	if strings.HasSuffix(out, ".jpg") {
		args = append(args, "-q:v", "2")
	}
	args = append(args, "-frames:v", "1", out)
	return args
}

// runFfmpeg runs ffmpeg with a timeout and returns a trimmed error.
func runFfmpeg(ctx context.Context, args []string) error {
	bin, err := exec.LookPath("ffmpeg")
	if err != nil {
		return errors.New("ffmpeg not found")
	}
	c, cancel := context.WithTimeout(ctx, convTimeout)
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
		return fmt.Errorf("ffmpeg: %s", msg)
	}
	return nil
}

// tempPath returns a unique path in the plugin tmp dir.
func tempPath(prefix, ext string) string {
	_ = os.MkdirAll(tmpDirRoot, 0o755)
	return filepath.Join(tmpDirRoot, fmt.Sprintf("%s_%d_%06d%s", prefix, time.Now().UnixNano(), rand.IntN(1000000), ext))
}

// ---------------------------------------------------------------- photo→sticker

// isPhotoLike reports whether media is a photo or an image document
// (the source accepted photos and image/* documents).
func isPhotoLike(media tg.MessageMediaClass) bool {
	switch m := media.(type) {
	case *tg.MessageMediaPhoto:
		return true
	case *tg.MessageMediaDocument:
		if d, ok := m.Document.(*tg.Document); ok {
			if _, isSticker := stickerAttr(d); isSticker {
				return false
			}
			return strings.HasPrefix(strings.ToLower(d.MimeType), "image/")
		}
	}
	return false
}

// handlePicToSticker implements "sticker pic [emoji|batch]".
func (p *StickerPlugin) handlePicToSticker(ctx *plugin.CommandContext, s sub) error {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return ctx.Edit("❌ " + ctx.Tlocal("未找到 ffmpeg，请先安装后再试", "ffmpeg not found, please install it first"))
	}
	settings := p.convSettings(ctx)
	emoji := settings.emoji
	if s.emoji != "" {
		emoji = s.emoji
	}

	targets, err := p.collectPhotoTargets(ctx, s.kind == subPicBatch)
	if err != nil {
		return tgErrText(ctx, err)
	}
	if len(targets) == 0 {
		return ctx.Edit("❌ " + ctx.Tlocal("请回复包含图片的消息", "Please reply to a message with a photo") + "\n\n" +
			plugin.Code("sticker pic") + " / " + plugin.Code("sticker pic batch"))
	}

	_ = ctx.Edit("🔄 " + ctx.Tlocal("正在转换图片...", "Converting photos..."))
	okCount, failCount := 0, 0
	for i, msg := range targets {
		if err := ctx.Context().Err(); err != nil {
			return err
		}
		if s.kind == subPicBatch {
			_ = ctx.Edit(fmt.Sprintf("🔄 %s %d/%d", ctx.Tlocal("正在转换图片", "Converting photos"), i+1, len(targets)))
		}
		if err := p.picToSticker(ctx, msg, emoji, settings); err != nil {
			if ctx.Context().Err() != nil {
				return err
			}
			failCount++
			continue
		}
		okCount++
		if s.kind == subPicBatch && i < len(targets)-1 {
			sleepCtx(ctx.Context(), 500*time.Millisecond)
		}
	}

	if okCount == 0 {
		return ctx.Edit("❌ " + ctx.Tlocal("图片转换失败", "Photo conversion failed"))
	}
	if s.kind == subPicBatch {
		_ = ctx.Edit(fmt.Sprintf("✅ %s\n\n%s: %d · %s: %d",
			ctx.Tlocal("批量转换完成", "Batch conversion finished"),
			ctx.Tlocal("成功", "ok"), okCount, ctx.Tlocal("失败", "failed"), failCount))
	}
	return ctx.Delete()
}

// collectPhotoTargets gathers the replied message (and its album for batch).
// With no reply, the command message itself may carry the photo (caption).
func (p *StickerPlugin) collectPhotoTargets(ctx *plugin.CommandContext, batch bool) ([]*tg.Message, error) {
	var reply *tg.Message
	if ctx.Message != nil && ctx.Message.ReplyToID != 0 {
		r, err := ctx.ReplyMessage()
		if err != nil {
			return nil, err
		}
		reply = r
	} else if ctx.Message != nil && ctx.Message.Message != nil {
		reply = ctx.Message.Message
	}
	if reply == nil || !isPhotoLike(reply.Media) {
		return nil, nil
	}
	if !batch || reply.GroupedID == 0 {
		return []*tg.Message{reply}, nil
	}
	// Album: fetch ±10 messages around the replied one, keep the group.
	peer, err := ctx.ResolvePeer()
	if err != nil {
		return nil, err
	}
	var ids []int
	for id := reply.ID - 10; id <= reply.ID+10; id++ {
		if id > 0 {
			ids = append(ids, id)
		}
	}
	msgs, _, _, err := plugin.GetMessages(ctx.Context(), ctx.API, peer, ids...)
	if err != nil {
		return []*tg.Message{reply}, nil
	}
	var out []*tg.Message
	for _, m := range msgs {
		if m.GroupedID == reply.GroupedID && isPhotoLike(m.Media) {
			out = append(out, m)
		}
	}
	return out, nil
}

// picToSticker converts one photo message and sends it as a sticker.
func (p *StickerPlugin) picToSticker(ctx *plugin.CommandContext, msg *tg.Message, emoji string, settings convSettings) error {
	src := tempPath("pic_src", ".bin")
	stickerPath := tempPath("sticker", ".webp")
	defer os.Remove(src)
	defer os.Remove(stickerPath)

	if err := downloadTo(ctx, msg.Media, src); err != nil {
		return err
	}
	if err := runFfmpeg(ctx.Context(), ffmpegWebpArgs(src, stickerPath, settings)); err != nil {
		return err
	}
	if st, err := os.Stat(stickerPath); err != nil || st.Size() == 0 {
		return errors.New("ffmpeg produced no output")
	}
	// Enforce the 512 KB sticker limit by re-encoding at lower quality.
	if st, err := os.Stat(stickerPath); err == nil && st.Size() > maxSticker {
		lower := settings
		lower.quality = settings.quality * 7 / 10
		if lower.quality < 20 {
			lower.quality = 20
		}
		retry := stickerPath + ".retry"
		defer os.Remove(retry)
		if err := runFfmpeg(ctx.Context(), ffmpegWebpArgs(src, retry, lower)); err == nil {
			_ = os.Remove(stickerPath)
			_ = os.Rename(retry, stickerPath)
		}
	}
	return sendSticker(ctx, stickerPath, emoji)
}

// mediaLocation extracts a downloadable file location from message media
// (photos and documents).
func mediaLocation(m tg.MessageMediaClass) (tg.InputFileLocationClass, bool) {
	switch v := m.(type) {
	case *tg.MessageMediaPhoto:
		photo, ok := v.Photo.(*tg.Photo)
		if !ok {
			return nil, false
		}
		typ, _, _, _ := largestPhotoSize(photo.Sizes)
		if typ == "" {
			return nil, false
		}
		return &tg.InputPhotoFileLocation{
			ID:            photo.ID,
			AccessHash:    photo.AccessHash,
			FileReference: photo.FileReference,
			ThumbSize:     typ,
		}, true
	case *tg.MessageMediaDocument:
		d, ok := v.Document.(*tg.Document)
		if !ok {
			return nil, false
		}
		return &tg.InputDocumentFileLocation{
			ID:            d.ID,
			AccessHash:    d.AccessHash,
			FileReference: d.FileReference,
		}, true
	}
	return nil, false
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

// downloadTo downloads message media into path (gotd downloader, rev-style).
func downloadTo(ctx *plugin.CommandContext, media tg.MessageMediaClass, path string) error {
	loc, ok := mediaLocation(media)
	if !ok {
		return errors.New("unsupported media type")
	}
	dctx, cancel := context.WithTimeout(ctx.Context(), convTimeout)
	defer cancel()
	_, err := downloader.NewDownloader().Download(ctx.API, loc).ToPath(dctx, path)
	return err
}

// sendSticker uploads a WebP file with a sticker attribute and sends it,
// mirroring the source's DocumentAttributeSticker + InputStickerSetEmpty.
func sendSticker(ctx *plugin.CommandContext, path, emoji string) error {
	peer, err := ctx.ResolvePeer()
	if err != nil {
		return err
	}
	uctx, cancel := context.WithTimeout(ctx.Context(), convTimeout)
	defer cancel()
	file, err := uploader.NewUploader(ctx.API).FromPath(uctx, path)
	if err != nil {
		return err
	}
	media := &tg.InputMediaUploadedDocument{
		File:     file,
		MimeType: "image/webp",
		Attributes: []tg.DocumentAttributeClass{
			&tg.DocumentAttributeFilename{FileName: "sticker.webp"},
			&tg.DocumentAttributeSticker{Alt: emoji, Stickerset: &tg.InputStickerSetEmpty{}},
		},
	}
	req := &tg.MessagesSendMediaRequest{
		Peer:     peer,
		Media:    media,
		RandomID: rand.Int64(),
	}
	if ctx.Message != nil && ctx.Message.Message != nil {
		req.SetReplyTo(&tg.InputReplyToMessage{ReplyToMsgID: ctx.Message.Message.ID})
	}
	_, err = ctx.API.MessagesSendMedia(uctx, req)
	return err
}

// ---------------------------------------------------------------- sticker→photo

// handleStickerToPic implements "sticker topng [png|transparent|doc]".
func (p *StickerPlugin) handleStickerToPic(ctx *plugin.CommandContext, _ sub) error {
	opts, ok := parseToPicArgs(ctx.Args)
	if !ok {
		return ctx.Edit("❌ " + ctx.Tlocal("未知参数", "Unknown argument") + " " + plugin.Code(strings.Join(ctx.Args[1:], " ")) + "\n\n" + helpText(ctx))
	}
	return p.runToPic(ctx, opts)
}

// runToPic converts the replied sticker to a photo and sends it.
func (p *StickerPlugin) runToPic(ctx *plugin.CommandContext, opts toPicOpts) error {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return ctx.Edit("❌ " + ctx.Tlocal("未找到 ffmpeg，请先安装后再试", "ffmpeg not found, please install it first"))
	}
	if ctx.Message == nil || ctx.Message.ReplyToID == 0 {
		return ctx.Edit("❌ " + ctx.Tlocal("请回复一个贴纸消息", "Reply to a sticker message"))
	}
	reply, err := ctx.ReplyMessage()
	if err != nil {
		return tgErrText(ctx, err)
	}
	doc, ok := docOf(reply)
	if !ok {
		return ctx.Edit("❌ " + ctx.Tlocal("回复的消息不是贴纸", "The replied message is not a sticker"))
	}
	if _, isSticker := stickerAttr(doc); !isSticker {
		return ctx.Edit("❌ " + ctx.Tlocal("回复的消息不是贴纸", "The replied message is not a sticker"))
	}
	kind := classifySticker(doc)
	if kind != kindStatic {
		return ctx.Edit("❌ " + ctx.Tlocal(
			"仅支持静态贴纸（image/webp）", "Only static (image/webp) stickers are supported"))
	}

	_ = ctx.Edit("📥 " + ctx.Tlocal("正在下载贴纸...", "Downloading the sticker..."))
	src := tempPath("stp_src", ".webp")
	out := tempPath("stp_out", "."+opts.format)
	defer os.Remove(src)
	defer os.Remove(out)

	if err := downloadTo(ctx, reply.Media, src); err != nil {
		return tgErrText(ctx, err)
	}
	_ = ctx.Edit("🔄 " + ctx.Tlocal(fmt.Sprintf("正在转换为 %s 格式...", strings.ToUpper(opts.format)), fmt.Sprintf("Converting to %s...", strings.ToUpper(opts.format))))
	if err := runFfmpeg(ctx.Context(), ffmpegToPicArgs(src, out, opts.transp)); err != nil {
		return ctx.Edit("❌ " + ctx.Tlocal("贴纸转换失败", "Sticker conversion failed") + "\n" + plugin.Pre(plugin.Escape(err.Error())))
	}
	if st, err := os.Stat(out); err != nil || st.Size() == 0 {
		return ctx.Edit("❌ " + ctx.Tlocal("转换失败：输出文件未生成", "Conversion failed: no output file"))
	}

	_ = ctx.Edit("📤 " + ctx.Tlocal("正在发送图片...", "Sending the photo..."))
	caption := fmt.Sprintf("🖼️ %s", ctx.Tlocal(
		fmt.Sprintf("贴纸已转换为 %s 格式", strings.ToUpper(opts.format)),
		fmt.Sprintf("Sticker converted to %s", strings.ToUpper(opts.format))))
	if opts.transp {
		caption += ctx.Tlocal("（透明背景）", " (transparent background)")
	}
	if err := sendConvertedPhoto(ctx, out, opts, caption); err != nil {
		return tgErrText(ctx, err)
	}
	return ctx.Delete()
}

// sendConvertedPhoto sends the converted image as a photo or a document.
func sendConvertedPhoto(ctx *plugin.CommandContext, path string, opts toPicOpts, caption string) error {
	peer, err := ctx.ResolvePeer()
	if err != nil {
		return err
	}
	uctx, cancel := context.WithTimeout(ctx.Context(), convTimeout)
	defer cancel()
	file, err := uploader.NewUploader(ctx.API).FromPath(uctx, path)
	if err != nil {
		return err
	}
	var media tg.InputMediaClass
	if opts.doc {
		mime := "image/jpeg"
		if opts.format == "png" {
			mime = "image/png"
		}
		media = &tg.InputMediaUploadedDocument{
			File:       file,
			MimeType:   mime,
			ForceFile:  true,
			Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeFilename{FileName: filepath.Base(path)}},
		}
	} else {
		media = &tg.InputMediaUploadedPhoto{File: file}
	}
	req := &tg.MessagesSendMediaRequest{
		Peer:     peer,
		Media:    media,
		Message:  caption,
		RandomID: rand.Int64(),
	}
	if ctx.Message != nil && ctx.Message.Message != nil {
		req.SetReplyTo(&tg.InputReplyToMessage{ReplyToMsgID: ctx.Message.Message.ID})
	}
	_, err = ctx.API.MessagesSendMedia(uctx, req)
	return err
}
