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

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
)

const (
	tempDir      = "data/rev/tmp"
	maxMediaSize = 50 << 20 // refuse to download anything bigger
)

type RevPlugin struct{}

func New() *RevPlugin { return &RevPlugin{} }

var Metadata = &plugin.PluginMetadata{
	Name:        "rev",
	Description: "反转文字（保留格式）或翻转/反色媒体",
	DescEN:      "Reverse text (keeps formatting) or flip/invert media",
	Version:     "1.1.0",
	Author:      "PaperValet",
	MinVersion:  "0.1.0",
}

func (p *RevPlugin) Name() string { return "rev" }
func (p *RevPlugin) Description() string {
	return "反转文字（保留格式）或翻转/反色媒体"
}
func (p *RevPlugin) DescEN() string { return "Reverse text (keeps formatting) or flip/invert media" }

func (p *RevPlugin) Init(ctx context.Context, mgr plugin.Manager) error {
	return mgr.RegisterCommand(&plugin.Command{
		Name:        "rev",
		Description: "反转文字（逐行，保留格式）；回复图片/GIF/贴纸可翻转或反色",
		DescEN:      "Reverse text per line keeping formatting; reply to image/GIF/sticker to flip or invert",
		Usage:       "rev [文字] | 回复消息 rev [h|v] [c]",
		UsageEN:     "rev [text] | reply: rev [h|v] [c]",
		Plugin:      p.Name(),
		Category:    "fun",
		Handler:     p.handleRev,
	})
}

func (p *RevPlugin) Start(ctx context.Context) error { return nil }
func (p *RevPlugin) Stop(ctx context.Context) error  { return nil }

func helpText(ctx *plugin.CommandContext) string {
	return ctx.Tlocal(
		"🔄 **反转插件**\n\n"+
			"**文字反转**\n"+
			"• `rev [文字]` — 逐行反转文字（支持 emoji，保留格式）\n"+
			"• 回复文字消息 `rev` — 反转回复的文字\n\n"+
			"**媒体反转**（图片 / GIF / WebP / WebM 贴纸，需要 ffmpeg）\n"+
			"• 回复媒体 `rev` 或 `rev h` — 水平翻转\n"+
			"• `rev v` — 垂直翻转\n"+
			"• `rev c` — 颜色反转（负片）\n"+
			"• `rev h c` — 组合使用\n\n"+
			"💡 媒体的说明文字会一并反转",
		"🔄 **Reverse**\n\n"+
			"**Text**\n"+
			"• `rev [text]` — reverse each line (emoji-safe, keeps formatting)\n"+
			"• reply to a text message with `rev` — reverse it\n\n"+
			"**Media** (image / GIF / WebP / WebM sticker, needs ffmpeg)\n"+
			"• reply with `rev` or `rev h` — horizontal flip\n"+
			"• `rev v` — vertical flip\n"+
			"• `rev c` — invert colours (negative)\n"+
			"• `rev h c` — combine\n\n"+
			"💡 Media captions are reversed too")
}

func (p *RevPlugin) handleRev(ctx *plugin.CommandContext) error {
	isReply := ctx.Message != nil && ctx.Message.IsReply && ctx.Message.ReplyToID > 0
	if len(ctx.Args) == 1 && strings.EqualFold(ctx.Args[0], "help") && !isReply {
		return ctx.Edit(helpText(ctx))
	}

	opts := extractMediaOptions(ctx.Args)

	// Direct text: reverse it (with any formatting of the command message).
	if len(opts.remaining) > 0 {
		text, ents := p.commandText(ctx, opts.remaining)
		return p.editReversed(ctx, text, ents)
	}

	if isReply {
		reply, err := p.replyMessage(ctx)
		if err != nil {
			return ctx.Edit("❌ " + ctx.Tlocal("获取回复消息失败: ", "Failed to load replied message: ") + plugin.Escape(err.Error()))
		}
		if info, ok := classifyMedia(reply.Media); ok {
			if err := p.transformMedia(ctx, reply, info, opts); err != nil {
				return ctx.Edit("❌ " + ctx.Tlocal("处理失败: ", "Failed: ") + plugin.Escape(err.Error()))
			}
			return nil
		}
		if strings.TrimSpace(reply.Message) != "" {
			return p.editReversed(ctx, reply.Message, reply.Entities)
		}
	}

	return ctx.Edit("❌ " + ctx.Tlocal("请提供文本内容或回复一条支持的消息", "Give some text or reply to a supported message") +
		"\n\n" + helpText(ctx))
}

// commandText returns the text to reverse from the command itself: the
// verbatim tail of the message (keeps newlines) plus its entities, or the
// space-joined args when the tail cannot be located.
func (p *RevPlugin) commandText(ctx *plugin.CommandContext, remaining []string) (string, []tg.MessageEntityClass) {
	if ctx.Message != nil && ctx.Message.Message != nil {
		full := ctx.Message.Message.Message
		if sub, base, ok := rawRemainder(full, remaining); ok {
			return sub, shiftEntities(ctx.Message.Message.Entities, base, utf16Len(sub))
		}
	}
	return strings.Join(remaining, " "), nil
}

// editReversed edits the command message to the reversed text, keeping
// entities when possible; falls back to plain text.
func (p *RevPlugin) editReversed(ctx *plugin.CommandContext, text string, ents []tg.MessageEntityClass) error {
	text, ents = trimWithEntities(text, ents)
	if text == "" {
		return ctx.Edit("❌ " + ctx.Tlocal("没有可反转的文字", "Nothing to reverse"))
	}
	reversed := reverseText(text)
	revEnts := reverseEntities(text, ents)
	if ctx.API == nil || ctx.Message == nil || ctx.Message.Message == nil {
		return plugin.ErrNoMessage
	}
	peer, err := ctx.ResolvePeer()
	if err != nil {
		return err
	}
	req := &tg.MessagesEditMessageRequest{Peer: peer, ID: ctx.Message.Message.ID, Message: reversed}
	if len(revEnts) > 0 {
		req.SetEntities(revEnts)
		if _, err := ctx.API.MessagesEditMessage(ctx.Context(), req); err == nil {
			return nil
		} else if ctx.Logger != nil {
			ctx.Logger.Debug("rev: rich edit failed, falling back to plain", "err", err)
		}
		req = &tg.MessagesEditMessageRequest{Peer: peer, ID: ctx.Message.Message.ID, Message: reversed}
	}
	_, err = ctx.API.MessagesEditMessage(ctx.Context(), req)
	return err
}

// replyMessage loads the replied-to message.
func (p *RevPlugin) replyMessage(ctx *plugin.CommandContext) (*tg.Message, error) {
	if ctx.API == nil {
		return nil, errors.New("no api")
	}
	peer, err := ctx.ResolvePeer()
	if err != nil {
		return nil, err
	}
	id := ctx.Message.ReplyToID
	ref := []tg.InputMessageClass{&tg.InputMessageID{ID: id}}
	var res tg.MessagesMessagesClass
	if ch, ok := peer.(*tg.InputPeerChannel); ok {
		res, err = ctx.API.ChannelsGetMessages(ctx.Context(), &tg.ChannelsGetMessagesRequest{
			Channel: &tg.InputChannel{ChannelID: ch.ChannelID, AccessHash: ch.AccessHash},
			ID:      ref,
		})
	} else {
		res, err = ctx.API.MessagesGetMessages(ctx.Context(), ref)
	}
	if err != nil {
		return nil, err
	}
	var msgs []tg.MessageClass
	switch m := res.(type) {
	case *tg.MessagesMessages:
		msgs = m.Messages
	case *tg.MessagesMessagesSlice:
		msgs = m.Messages
	case *tg.MessagesChannelMessages:
		msgs = m.Messages
	}
	for _, m := range msgs {
		if msg, ok := m.(*tg.Message); ok && msg.ID == id {
			return msg, nil
		}
	}
	return nil, fmt.Errorf("message %d not found", id)
}

// ---------------------------------------------------------------- media

type mediaKind int

const (
	kindPhoto     mediaKind = iota // MessageMediaPhoto
	kindImage                      // image/* document (png/jpg/bmp)
	kindGif                        // real image/gif document
	kindAnimation                  // Telegram GIF (mp4, .gif.mp4 / animated attr)
	kindSticker                    // static webp sticker / image/webp
	kindWebm                       // video sticker / video/webm
)

type mediaInfo struct {
	kind     mediaKind
	ext      string
	location tg.InputFileLocationClass
	size     int64
	w, h     int
}

func docFileName(d *tg.Document) string {
	for _, a := range d.Attributes {
		if fn, ok := a.(*tg.DocumentAttributeFilename); ok {
			return fn.FileName
		}
	}
	return ""
}

func docHas[T tg.DocumentAttributeClass](d *tg.Document) (T, bool) {
	for _, a := range d.Attributes {
		if v, ok := a.(T); ok {
			return v, true
		}
	}
	var zero T
	return zero, false
}

func extFromMime(mime string) string {
	switch {
	case strings.Contains(mime, "png"):
		return ".png"
	case strings.Contains(mime, "webp"):
		return ".webp"
	case strings.Contains(mime, "bmp"):
		return ".bmp"
	case strings.Contains(mime, "gif"):
		return ".gif"
	case strings.Contains(mime, "webm"):
		return ".webm"
	}
	return ".jpg"
}

// classifyMedia mirrors the reference's isSupportedMedia: photos, image/*
// documents, webm and .gif.mp4 animations. Plain videos are not handled.
func classifyMedia(m tg.MessageMediaClass) (mediaInfo, bool) {
	switch v := m.(type) {
	case *tg.MessageMediaPhoto:
		photo, ok := v.Photo.(*tg.Photo)
		if !ok {
			return mediaInfo{}, false
		}
		typ, w, h, size := largestPhotoSize(photo.Sizes)
		if typ == "" {
			return mediaInfo{}, false
		}
		return mediaInfo{
			kind: kindPhoto, ext: ".jpg", size: int64(size), w: w, h: h,
			location: &tg.InputPhotoFileLocation{ID: photo.ID, AccessHash: photo.AccessHash, FileReference: photo.FileReference, ThumbSize: typ},
		}, true
	case *tg.MessageMediaDocument:
		d, ok := v.Document.(*tg.Document)
		if !ok {
			return mediaInfo{}, false
		}
		mime := strings.ToLower(d.MimeType)
		name := strings.ToLower(docFileName(d))
		info := mediaInfo{
			size:     d.Size,
			location: &tg.InputDocumentFileLocation{ID: d.ID, AccessHash: d.AccessHash, FileReference: d.FileReference},
		}
		if va, ok := docHas[*tg.DocumentAttributeVideo](d); ok {
			info.w, info.h = va.W, va.H
		} else if ia, ok := docHas[*tg.DocumentAttributeImageSize](d); ok {
			info.w, info.h = ia.W, ia.H
		}
		_, animated := docHas[*tg.DocumentAttributeAnimated](d)
		switch {
		case strings.HasSuffix(name, ".gif.mp4") || (animated && strings.HasPrefix(mime, "video/mp4")):
			info.kind, info.ext = kindAnimation, ".gif.mp4"
		case strings.Contains(mime, "webm"):
			info.kind, info.ext = kindWebm, ".webm"
		case strings.Contains(mime, "gif"):
			info.kind, info.ext = kindGif, ".gif"
		case strings.Contains(mime, "webp"):
			info.kind, info.ext = kindSticker, ".webp"
		case strings.HasPrefix(mime, "image/"):
			info.kind, info.ext = kindImage, extFromMime(mime)
		default:
			return mediaInfo{}, false
		}
		return info, true
	}
	return mediaInfo{}, false
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

// buildFilters returns the ffmpeg -vf chain for the options.
func buildFilters(o mediaOptions) []string {
	var f []string
	switch o.flip {
	case "v":
		f = append(f, "vflip")
	case "h":
		f = append(f, "hflip")
	}
	if o.invert {
		// negate leaves alpha alone by default, so transparent stickers
		// stay transparent.
		f = append(f, "negate")
	}
	return f
}

// buildFfmpegArgs builds the ffmpeg command line for one transformation.
func buildFfmpegArgs(in, out string, kind mediaKind, filters []string) []string {
	args := []string{"-hide_banner", "-loglevel", "error", "-y"}
	if kind == kindWebm {
		// libvpx decoder is needed to read the VP9 alpha channel.
		args = append(args, "-c:v", "libvpx-vp9")
	}
	args = append(args, "-i", in)
	chain := strings.Join(filters, ",")
	switch kind {
	case kindGif:
		base := chain
		if base == "" {
			base = "null"
		}
		args = append(args, "-filter_complex",
			"[0:v]"+base+"[flip];[flip]split[a][b];[a]palettegen=stats_mode=diff[p];[b][p]paletteuse=dither=bayer",
			"-loop", "0")
	case kindAnimation:
		// Telegram GIFs are H.264 mp4: keep that so they stay animations.
		vf := "scale=trunc(iw/2)*2:trunc(ih/2)*2"
		if chain != "" {
			vf = chain + "," + vf
		}
		args = append(args, "-vf", vf, "-an", "-c:v", "libx264", "-pix_fmt", "yuv420p",
			"-preset", "veryfast", "-crf", "23", "-movflags", "+faststart", "-f", "mp4")
	case kindWebm:
		if chain != "" {
			args = append(args, "-vf", chain)
		}
		args = append(args, "-an", "-c:v", "libvpx-vp9", "-pix_fmt", "yuva420p",
			"-b:v", "0", "-crf", "32", "-auto-alt-ref", "0")
	case kindSticker:
		if chain != "" {
			args = append(args, "-vf", chain)
		}
		args = append(args, "-c:v", "libwebp", "-lossless", "0", "-q:v", "90", "-frames:v", "1")
	default: // photo / image
		if chain != "" {
			args = append(args, "-vf", chain)
		}
		args = append(args, "-frames:v", "1")
		if strings.HasSuffix(out, ".jpg") {
			args = append(args, "-q:v", "2")
		}
	}
	return append(args, out)
}

func runFfmpeg(ctx context.Context, args []string) error {
	bin, err := exec.LookPath("ffmpeg")
	if err != nil {
		return errors.New("未找到 ffmpeg，请先安装后再试 / ffmpeg not found")
	}
	c, cancel := context.WithTimeout(ctx, 3*time.Minute)
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

func (p *RevPlugin) transformMedia(ctx *plugin.CommandContext, reply *tg.Message, info mediaInfo, opts mediaOptions) error {
	if info.size > maxMediaSize {
		return fmt.Errorf(ctx.Tlocal("文件过大（%d MB），上限 %d MB", "file too large (%d MB), limit %d MB"), info.size>>20, maxMediaSize>>20)
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return errors.New(ctx.Tlocal("未找到 ffmpeg，请先安装后再试", "ffmpeg not found, please install it first"))
	}
	_ = ctx.Edit(ctx.Tlocal("🔄 正在处理媒体，请稍候...", "🔄 Processing media, please wait..."))

	if err := os.MkdirAll(tempDir, 0o755); err != nil {
		return err
	}
	id := fmt.Sprintf("%d_%06d", time.Now().UnixNano(), rand.IntN(1000000))
	in := filepath.Join(tempDir, "rev_src_"+id+info.ext)
	out := filepath.Join(tempDir, "rev_flip_"+id+info.ext)
	defer os.Remove(in)
	defer os.Remove(out)

	dctx, cancel := context.WithTimeout(ctx.Context(), 3*time.Minute)
	_, err := downloader.NewDownloader().Download(ctx.API, info.location).ToPath(dctx, in)
	cancel()
	if err != nil {
		return fmt.Errorf("%s: %w", ctx.Tlocal("下载媒体失败", "download failed"), err)
	}

	if err := runFfmpeg(ctx.Context(), buildFfmpegArgs(in, out, info.kind, buildFilters(opts))); err != nil {
		return err
	}
	if st, err := os.Stat(out); err != nil || st.Size() == 0 {
		return errors.New(ctx.Tlocal("ffmpeg 未生成输出文件", "ffmpeg produced no output"))
	}

	caption, capEnts := trimWithEntities(reply.Message, reply.Entities)
	if caption != "" {
		capEnts = reverseEntities(caption, capEnts)
		caption = reverseText(caption)
	}
	if err := sendMedia(ctx, out, info, caption, capEnts, reply.ID); err != nil {
		return fmt.Errorf("%s: %w", ctx.Tlocal("发送失败", "send failed"), err)
	}
	if err := ctx.Delete(); err != nil {
		_ = ctx.Edit(ctx.Tlocal("✅ 媒体已处理完成", "✅ Media processed"))
	}
	return nil
}

func sendMedia(ctx *plugin.CommandContext, path string, info mediaInfo, caption string, ents []tg.MessageEntityClass, replyTo int) error {
	peer, err := ctx.ResolvePeer()
	if err != nil {
		return err
	}
	uctx, cancel := context.WithTimeout(ctx.Context(), 3*time.Minute)
	defer cancel()
	file, err := uploader.NewUploader(ctx.API).FromPath(uctx, path)
	if err != nil {
		return err
	}
	base := "rev" + info.ext
	var media tg.InputMediaClass
	switch info.kind {
	case kindPhoto:
		media = &tg.InputMediaUploadedPhoto{File: file}
	case kindSticker:
		media = &tg.InputMediaUploadedDocument{
			File: file, MimeType: "image/webp",
			Attributes: []tg.DocumentAttributeClass{
				&tg.DocumentAttributeFilename{FileName: "sticker.webp"},
				&tg.DocumentAttributeSticker{Alt: "🔄", Stickerset: &tg.InputStickerSetEmpty{}},
			},
		}
		if info.w > 0 && info.h > 0 {
			d := media.(*tg.InputMediaUploadedDocument)
			d.Attributes = append(d.Attributes, &tg.DocumentAttributeImageSize{W: info.w, H: info.h})
		}
	case kindWebm:
		media = &tg.InputMediaUploadedDocument{
			File: file, MimeType: "video/webm",
			Attributes: []tg.DocumentAttributeClass{
				&tg.DocumentAttributeFilename{FileName: "sticker.webm"},
				&tg.DocumentAttributeSticker{Alt: "🔄", Stickerset: &tg.InputStickerSetEmpty{}},
				&tg.DocumentAttributeVideo{W: info.w, H: info.h},
			},
		}
	case kindAnimation:
		media = &tg.InputMediaUploadedDocument{
			File: file, MimeType: "video/mp4", NosoundVideo: true,
			Attributes: []tg.DocumentAttributeClass{
				&tg.DocumentAttributeFilename{FileName: "rev.gif.mp4"},
				&tg.DocumentAttributeAnimated{},
				&tg.DocumentAttributeVideo{W: info.w, H: info.h, SupportsStreaming: true},
			},
		}
	case kindGif:
		media = &tg.InputMediaUploadedDocument{
			File: file, MimeType: "image/gif",
			Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeFilename{FileName: base}},
		}
	default: // image document → send back as a document, like the source
		mt := map[string]string{".png": "image/png", ".bmp": "image/bmp", ".jpg": "image/jpeg"}[info.ext]
		if mt == "" {
			mt = "application/octet-stream"
		}
		media = &tg.InputMediaUploadedDocument{
			File: file, MimeType: mt, ForceFile: true,
			Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeFilename{FileName: base}},
		}
	}

	// Stickers can't carry captions.
	if info.kind == kindSticker || info.kind == kindWebm {
		caption, ents = "", nil
	}
	send := func(withEnts bool) error {
		req := &tg.MessagesSendMediaRequest{
			Peer: peer, Media: media, Message: caption, RandomID: rand.Int64(),
		}
		if withEnts && len(ents) > 0 {
			req.SetEntities(ents)
		}
		if replyTo > 0 {
			req.SetReplyTo(&tg.InputReplyToMessage{ReplyToMsgID: replyTo})
		}
		_, err := ctx.API.MessagesSendMedia(uctx, req)
		return err
	}
	if err := send(true); err != nil {
		if len(ents) == 0 {
			return err
		}
		return send(false)
	}
	return nil
}
