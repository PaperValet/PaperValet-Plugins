package main

// quote — turn replied messages into a rendered quote image (TeleBox port).

import (
	"context"
	"fmt"
	"image"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

const maxQuoteMessages = 50

type QuotePlugin struct {
	host     plugin.Host
	fontOnce sync.Once
	fontErr  error
}

func New() *QuotePlugin { return &QuotePlugin{} }

var Metadata = &plugin.PluginMetadata{
	Name:        "quote",
	Description: "回复消息生成引用图，本地纯 Go 渲染",
	DescEN:      "Turn replied messages into a quote image, rendered locally in pure Go",
	Version:     "1.0.0",
	Author:      "PaperValet",
	MinVersion:  "0.1.0",
}

func (p *QuotePlugin) Name() string        { return "quote" }
func (p *QuotePlugin) Description() string { return Metadata.Description }
func (p *QuotePlugin) DescEN() string      { return Metadata.DescEN }

func (p *QuotePlugin) Init(ctx context.Context, mgr plugin.Manager) error {
	p.host = mgr.Host()
	return mgr.RegisterCommand(&plugin.Command{
		Name:        "quote",
		Description: "回复消息生成引用图（气泡贴纸/图片）",
		DescEN:      "Reply to a message to render it as a quote image",
		Usage:       "quote [N] [r] [png|stories|webp] [hidden] [media] [crop] [scale N] [bg #hex|#hex/#hex|random]",
		UsageEN:     "quote [N] [r] [png|stories|webp] [hidden] [media] [crop] [scale N] [bg #hex|#hex/#hex|random]",
		Plugin:      p.Name(),
		Category:    "fun",
		OwnerOnly:   true,
		Handler:     p.handleQuote,
	})
}

func (p *QuotePlugin) Start(ctx context.Context) error { return nil }
func (p *QuotePlugin) Stop(ctx context.Context) error  { return nil }

// ---------------------------------------------------------------- args

type quoteArgs struct {
	count   int
	reply   bool
	png     bool
	stories bool
	hidden  bool
	media   bool
	crop    bool
	scale   int
	bg      string
}

func parseArgs(text string) quoteArgs {
	out := quoteArgs{count: 1, scale: 2, bg: defaultBackground}
	fields := strings.Fields(text)
	for i := 0; i < len(fields); i++ {
		arg := fields[i]
		lower := strings.ToLower(arg)
		switch {
		case lower == "r" || lower == "reply":
			out.reply = true
		case lower == "png" || lower == "image" || lower == "img":
			out.png = true
		case lower == "stories" || lower == "story":
			out.stories = true
			out.png = true
		case lower == "webp" || lower == "quote":
			out.png = false
			out.stories = false
		case lower == "hidden" || lower == "hide" || lower == "anonymous":
			out.hidden = true
		case lower == "media" || lower == "m":
			out.media = true
		case lower == "crop":
			out.crop = true
		case lower == "scale" || lower == "s":
			if i+1 < len(fields) {
				if n, err := strconv.Atoi(fields[i+1]); err == nil && n > 0 {
					out.scale = min(20, max(1, n))
					i++
				}
			}
		case strings.HasPrefix(lower, "scale=") || strings.HasPrefix(lower, "s="):
			if n, err := strconv.Atoi(lower[strings.IndexByte(lower, '=')+1:]); err == nil && n > 0 {
				out.scale = min(20, max(1, n))
			}
		case lower == "bg" || lower == "color" || lower == "background":
			if i+1 < len(fields) && isColorToken(fields[i+1]) {
				out.bg = normalizeColorToken(fields[i+1])
				i++
			}
		case strings.HasPrefix(lower, "bg=") || strings.HasPrefix(lower, "color=") || strings.HasPrefix(lower, "background="):
			val := lower[strings.IndexByte(lower, '=')+1:]
			if isColorToken(val) {
				out.bg = normalizeColorToken(val)
			}
		case isPureInt(arg):
			n, _ := strconv.Atoi(arg)
			out.count = max(-maxQuoteMessages, min(maxQuoteMessages, n))
		case isColorToken(arg):
			out.bg = normalizeColorToken(arg)
		}
	}
	return out
}

// isPureInt reports whether arg is a plain (optionally signed) integer —
// counts must win over 3/6-digit hex color tokens like "100".
func isPureInt(arg string) bool {
	if arg == "" {
		return false
	}
	_, err := strconv.Atoi(arg)
	return err == nil
}

func helpText(ctx *plugin.CommandContext) string {
	return ctx.Tlocal(
		"💬 **引用图**\n\n"+
			"• 回复消息 `quote` — 生成引用贴纸\n"+
			"• `quote N` — 连续引用 N 条（最多 50，负数向前）\n"+
			"• `quote r` — 气泡内显示被回复内容\n"+
			"• `quote png` / `quote stories` — 背景大图 / 720×1280 故事\n"+
			"• `quote hidden` — 隐藏头像昵称\n"+
			"• `quote media` — 强制附带媒体预览\n"+
			"• `quote crop` — 媒体按比例裁剪\n"+
			"• `quote scale N` — 缩放 1–20（默认 2）\n"+
			"• `quote bg #1b1429` / `bg #111/#222` / `bg random` — 背景\n\n"+
			"组合示例：`quote r 3` · `quote stories #231d2b/#372e44`",
		"💬 **Quote**\n\n"+
			"• Reply with `quote` — render a quote sticker\n"+
			"• `quote N` — quote N consecutive messages (max 50, negative goes back)\n"+
			"• `quote r` — include the replied-to preview in the bubble\n"+
			"• `quote png` / `quote stories` — wallpaper PNG / 720×1280 story\n"+
			"• `quote hidden` — hide avatar and name\n"+
			"• `quote media` — force media preview\n"+
			"• `quote crop` — crop media to ratio\n"+
			"• `quote scale N` — scale 1–20 (default 2)\n"+
			"• `quote bg #1b1429` / `bg #111/#222` / `bg random` — background\n\n"+
			"Examples: `quote r 3` · `quote stories #231d2b/#372e44`")
}

// ---------------------------------------------------------------- handler

func (p *QuotePlugin) handleQuote(ctx *plugin.CommandContext) error {
	if len(ctx.Args) == 1 && strings.EqualFold(ctx.Args[0], "help") {
		return ctx.Edit(helpText(ctx))
	}
	args := parseArgs(ctx.RawArgs)

	// resources
	if err := p.loadFonts(); err != nil {
		return ctx.Edit("❌ " + ctx.Tlocal("字体加载失败: ", "font load failed: ") + plugin.Escape(err.Error()))
	}
	emoji, _ := loadEmojiFont() // optional

	isReply := ctx.Message != nil && ctx.Message.IsReply && ctx.Message.ReplyToID > 0

	_ = ctx.Edit(ctx.Tlocal("⏳ 正在生成引用图…", "⏳ Rendering quote…"))

	msgs, err := p.collectMessages(ctx, args, isReply)
	if err != nil {
		if d, ok := tgerr.AsFloodWait(err); ok {
			return ctx.Edit("❌ " + ctx.Tlocal(fmt.Sprintf("触发限流，请 %d 秒后重试", int(d.Seconds())), fmt.Sprintf("Flood wait, retry in %ds", int(d.Seconds()))))
		}
		return ctx.Edit("❌ " + ctx.Tlocal("获取消息失败: ", "failed to load messages: ") + plugin.Escape(err.Error()))
	}

	rmsgs := p.buildRenderMsgs(ctx, msgs, args, emoji)

	opt := renderOptions{scale: args.scale, stories: args.stories, pngMode: args.png, media: args.media, crop: args.crop, emojiFont: emoji}
	bg1, bg2, _ := backgroundColor(args.bg)
	img := newRenderer(opt).renderQuoteImage(rmsgs, bg1, bg2)

	// encode + send
	pngPath, err := savePNG(img)
	if err != nil {
		return ctx.Edit("❌ " + ctx.Tlocal("编码失败: ", "encode failed: ") + plugin.Escape(err.Error()))
	}
	defer os.Remove(pngPath)

	replyTo := 0
	if isReply {
		replyTo = ctx.Message.ReplyToID
	} else if ctx.Message != nil && ctx.Message.Message != nil {
		replyTo = ctx.Message.Message.ID
	}

	if args.stories || args.png {
		if err := p.sendPhoto(ctx, pngPath, replyTo); err != nil {
			return ctx.Edit("❌ " + ctx.Tlocal("发送失败: ", "send failed: ") + plugin.Escape(err.Error()))
		}
	} else {
		webpData, err := ffmpegWebp(ctx.Context(), mustReadFile(pngPath))
		if err != nil {
			// fallback: send as a photo when webp encoding is unavailable
			if err := p.sendPhoto(ctx, pngPath, replyTo); err != nil {
				return ctx.Edit("❌ " + ctx.Tlocal("发送失败: ", "send failed: ") + plugin.Escape(err.Error()))
			}
			return nil
		}
		webpPath, err := saveTemp(webpData, ".webp")
		if err != nil {
			return ctx.Edit("❌ " + ctx.Tlocal("编码失败: ", "encode failed: ") + plugin.Escape(err.Error()))
		}
		defer os.Remove(webpPath)
		if err := p.sendSticker(ctx, webpPath, replyTo); err != nil {
			return ctx.Edit("❌ " + ctx.Tlocal("发送失败: ", "send failed: ") + plugin.Escape(err.Error()))
		}
	}

	_ = ctx.Delete()
	return nil
}

// saveTemp writes bytes to a new temp file with the given suffix.
func saveTemp(data []byte, ext string) (string, error) {
	f, err := os.CreateTemp("", "quote-*"+ext)
	if err != nil {
		return "", err
	}
	name := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(name)
		return "", err
	}
	f.Close()
	return name, nil
}

func mustReadFile(path string) []byte {
	b, _ := os.ReadFile(path)
	return b
}

func savePNG(img image.Image) (string, error) {
	dir := os.TempDir()
	f, err := os.CreateTemp(dir, "quote-*.png")
	if err != nil {
		return "", err
	}
	name := f.Name()
	if err := encodePNG(f, img); err != nil {
		f.Close()
		os.Remove(name)
		return "", err
	}
	f.Close()
	return name, nil
}

// sendPhoto uploads the PNG as a photo reply.
func (p *QuotePlugin) sendPhoto(ctx *plugin.CommandContext, path string, replyTo int) error {
	file, err := uploader.NewUploader(ctx.API).FromPath(ctx.Context(), path)
	if err != nil {
		return err
	}
	peer, err := ctx.ResolvePeer()
	if err != nil {
		return err
	}
	req := &tg.MessagesSendMediaRequest{
		Peer:    peer,
		Media:   &tg.InputMediaUploadedPhoto{File: file},
		Message: "",
	}
	if replyTo > 0 {
		req.SetReplyTo(&tg.InputReplyToMessage{ReplyToMsgID: replyTo})
	}
	_, err = ctx.API.MessagesSendMedia(ctx.Context(), req)
	return err
}

// sendSticker uploads the webp as a sticker reply.
func (p *QuotePlugin) sendSticker(ctx *plugin.CommandContext, path string, replyTo int) error {
	file, err := uploader.NewUploader(ctx.API).FromPath(ctx.Context(), path)
	if err != nil {
		return err
	}
	peer, err := ctx.ResolvePeer()
	if err != nil {
		return err
	}
	req := &tg.MessagesSendMediaRequest{
		Peer: peer,
		Media: &tg.InputMediaUploadedDocument{
			File:     file,
			MimeType: "image/webp",
			Attributes: []tg.DocumentAttributeClass{
				&tg.DocumentAttributeFilename{FileName: "quote.webp"},
				&tg.DocumentAttributeSticker{Alt: "💜", Stickerset: &tg.InputStickerSetEmpty{}},
				&tg.DocumentAttributeImageSize{W: 512, H: 512},
			},
		},
	}
	if replyTo > 0 {
		req.SetReplyTo(&tg.InputReplyToMessage{ReplyToMsgID: replyTo})
	}
	_, err = ctx.API.MessagesSendMedia(ctx.Context(), req)
	return err
}

// ---------------------------------------------------------------- collect

// collectMessages gathers the messages to quote: the replied message plus
// (count-1) following ones, or N before the command when not replying.
func (p *QuotePlugin) collectMessages(ctx *plugin.CommandContext, args quoteArgs, isReply bool) ([]*tg.Message, error) {
	api := ctx.API
	if api == nil {
		return nil, fmt.Errorf("no api")
	}
	peer, err := ctx.ResolvePeer()
	if err != nil {
		return nil, err
	}
	count := args.count
	if count == 0 {
		count = 1
	}

	var anchor *tg.Message
	if isReply {
		msgs, _, _, err := plugin.GetMessages(ctx.Context(), api, peer, ctx.Message.ReplyToID)
		if err != nil {
			return nil, err
		}
		for _, m := range msgs {
			if m.ID == ctx.Message.ReplyToID {
				anchor = m
			}
		}
		if anchor == nil {
			return nil, fmt.Errorf("reply message not found")
		}
	}

	if abs(count) <= 1 {
		if anchor != nil {
			return []*tg.Message{anchor}, nil
		}
		if ctx.Message != nil && ctx.Message.Message != nil {
			return []*tg.Message{ctx.Message.Message}, nil
		}
		return nil, fmt.Errorf("no message")
	}

	limit := min(abs(count), maxQuoteMessages)
	var (
		res      tg.MessagesMessagesClass
		innerErr error
	)
	c, cancel := context.WithTimeout(ctx.Context(), 30*time.Second)
	defer cancel()
	if count > 0 {
		anchorID := ctx.Message.Message.ID
		if anchor != nil {
			anchorID = anchor.ID
		}
		res, innerErr = api.MessagesGetHistory(c, &tg.MessagesGetHistoryRequest{
			Peer:      peer,
			OffsetID:  anchorID,
			AddOffset: -limit,
			Limit:     limit,
			MinID:     anchorID,
		})
	} else {
		cmdID := ctx.Message.Message.ID
		res, innerErr = api.MessagesGetHistory(c, &tg.MessagesGetHistoryRequest{
			Peer:     peer,
			OffsetID: cmdID,
			Limit:    limit,
		})
	}
	if innerErr != nil {
		// fall back to the single anchor/command message
		if anchor != nil {
			return []*tg.Message{anchor}, nil
		}
		return []*tg.Message{ctx.Message.Message}, nil
	}

	var list []*tg.Message
	switch v := res.(type) {
	case *tg.MessagesMessages:
		list = messagesOf(v.Messages)
	case *tg.MessagesMessagesSlice:
		list = messagesOf(v.Messages)
	case *tg.MessagesChannelMessages:
		list = messagesOf(v.Messages)
	case *tg.MessagesMessagesNotModified:
	}
	sort.Slice(list, func(a, b int) bool { return list[a].ID < list[b].ID })
	if anchor != nil && count > 0 {
		list = append([]*tg.Message{anchor}, list...)
	}
	// dedupe by ID
	seen := map[int]bool{}
	var out []*tg.Message
	for _, m := range list {
		if m == nil || seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		out = append(out, m)
	}
	if len(out) > limit {
		if count > 0 {
			out = out[:limit]
		} else {
			out = out[len(out)-limit:]
		}
	}
	if len(out) == 0 {
		if anchor != nil {
			return []*tg.Message{anchor}, nil
		}
		return []*tg.Message{ctx.Message.Message}, nil
	}
	return out, nil
}

func messagesOf(in []tg.MessageClass) []*tg.Message {
	var out []*tg.Message
	for _, m := range in {
		if msg, ok := m.(*tg.Message); ok {
			out = append(out, msg)
		}
	}
	return out
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// ---------------------------------------------------------------- build

// buildRenderMsgs converts tg messages into render models (with avatar,
// reply preview, media, voice/file rows).
func (p *QuotePlugin) buildRenderMsgs(ctx *plugin.CommandContext, msgs []*tg.Message, args quoteArgs, emoji *emojiFont) []*renderMsg {
	out := make([]*renderMsg, 0, len(msgs))
	users := p.fetchUsers(ctx, msgs)
	for _, m := range msgs {
		rm := &renderMsg{senderID: plugin.SenderID(m), nameHidden: args.hidden}
		if name := userName(users[rm.senderID]); name != "" {
			rm.name = name
		} else if ch := channelName(m); ch != "" {
			rm.name = ch
		} else {
			rm.name = "User"
		}
		rm.bgColor = paletteColor(rm.senderID)
		if !args.hidden {
			rm.avatar = p.fetchAvatar(ctx, rm.senderID, users[rm.senderID])
		}

		// media classification first
		kind, info, _, _ := classifyMedia(m.Media)
		switch kind {
		case mediaVoice:
			rm.voice = &voiceInfo{waveform: info.waveform, duration: info.duration}
			rm.text = m.Message
		case mediaAudio:
			rm.fileRow = &fileRow{name: audioLabel(info), size: info.size}
			rm.text = m.Message
		case mediaDocument:
			if shouldFetchPreview(kind, args.media) {
				rm.media = fetchMediaPreview(ctx.Context(), ctx.API, m.Media)
			}
			if rm.media == nil {
				rm.fileRow = &fileRow{name: info.fileName, size: info.size}
			}
			rm.text = m.Message
		case mediaPhoto, mediaSticker, mediaAnimation, mediaVideo, mediaRoundVideo:
			rm.media = fetchMediaPreview(ctx.Context(), ctx.API, m.Media)
			rm.text = m.Message
		default:
			rm.text = messageText(m)
		}
		if strings.TrimSpace(rm.text) == "" && rm.media == nil && rm.voice == nil && rm.fileRow == nil {
			rm.text = fallbackText(kind)
		}
		rm.entities = entitiesOf(m)

		// reply preview
		if args.reply {
			if hdr, ok := m.ReplyTo.(*tg.MessageReplyHeader); ok && hdr.ReplyToMsgID > 0 {
				rm.reply = p.fetchReplyPreview(ctx, hdr.ReplyToMsgID)
			}
		}
		out = append(out, rm)
	}
	return out
}

// userName returns a display name for a user entity.
func userName(u tg.UserClass) string {
	v, ok := u.(*tg.User)
	if !ok || v == nil {
		return ""
	}
	name := strings.TrimSpace(v.FirstName + " " + v.LastName)
	if name == "" {
		name = v.Username
	}
	if name == "" {
		name = "User"
	}
	return name
}

func audioLabel(info *docInfo) string {
	if info.title != "" {
		return info.title
	}
	if info.fileName != "" {
		return info.fileName
	}
	return "Audio"
}

func messageText(m *tg.Message) string { return m.Message }

func fallbackText(kind mediaKind) string {
	switch kind {
	case mediaVoice:
		return "[语音] Voice"
	case mediaAudio:
		return "[音频] Audio"
	case mediaOther:
		return "[媒体] Media"
	}
	return ""
}

func entitiesOf(m *tg.Message) []entityRef {
	var out []entityRef
	for _, e := range m.Entities {
		out = append(out, entityRef{kind: entityKindOf(e), offset: entityOffset(e), length: entityLength(e)})
	}
	return out
}

// fetchUsers resolves sender names for all messages in one call.
func (p *QuotePlugin) fetchUsers(ctx *plugin.CommandContext, msgs []*tg.Message) map[int64]tg.UserClass {
	out := map[int64]tg.UserClass{}
	if ctx.API == nil {
		return out
	}
	seen := map[int64]bool{}
	var ids []tg.InputUserClass
	for _, m := range msgs {
		id := plugin.SenderID(m)
		if id == 0 || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, &tg.InputUser{UserID: id})
	}
	if len(ids) == 0 {
		return out
	}
	c, cancel := context.WithTimeout(ctx.Context(), 20*time.Second)
	defer cancel()
	users, err := ctx.API.UsersGetUsers(c, ids)
	if err != nil {
		return out
	}
	for _, u := range users {
		if v, ok := u.(*tg.User); ok {
			out[v.ID] = v
		}
	}
	return out
}

// fetchAvatar downloads a user's photo (small size) as a square image.
func (p *QuotePlugin) fetchAvatar(ctx *plugin.CommandContext, userID int64, uc tg.UserClass) image.Image {
	u, _ := uc.(*tg.User)
	if ctx.API == nil || userID == 0 || u == nil {
		return nil
	}
	c, cancel := context.WithTimeout(ctx.Context(), 20*time.Second)
	defer cancel()
	res, err := ctx.API.PhotosGetUserPhotos(c, &tg.PhotosGetUserPhotosRequest{
		UserID: &tg.InputUser{UserID: userID, AccessHash: u.AccessHash},
		Limit:  1,
	})
	if err != nil {
		return nil
	}
	var photo *tg.Photo
	switch v := res.(type) {
	case *tg.PhotosPhotos:
		if len(v.Photos) > 0 {
			photo, _ = v.Photos[0].(*tg.Photo)
		}
	case *tg.PhotosPhotosSlice:
		if len(v.Photos) > 0 {
			photo, _ = v.Photos[0].(*tg.Photo)
		}
	}
	if photo == nil {
		return nil
	}
	loc, size, _, _, ok := photoLocation(photo)
	if !ok {
		return nil
	}
	data, err := downloadLocation(ctx.Context(), ctx.API, loc, size)
	if err != nil {
		return nil
	}
	img, err := decodeImage(data)
	if err != nil {
		return nil
	}
	return squareCrop(img)
}

// fetchReplyPreview loads name+text of the replied message for the preview
// block.
func (p *QuotePlugin) fetchReplyPreview(ctx *plugin.CommandContext, msgID int) *renderReply {
	if ctx.API == nil {
		return nil
	}
	peer, err := ctx.ResolvePeer()
	if err != nil {
		return nil
	}
	msgs, users, _, err := plugin.GetMessages(ctx.Context(), ctx.API, peer, msgID)
	if err != nil {
		return nil
	}
	for _, m := range msgs {
		if m.ID != msgID {
			continue
		}
		name := userName(users[plugin.SenderID(m)])
		if name == "" {
			name = "User"
		}
		id := plugin.SenderID(m)
		text := m.Message
		if strings.TrimSpace(text) == "" {
			kind, info, _, _ := classifyMedia(m.Media)
			switch kind {
			case mediaVoice:
				text = "[语音] Voice"
			case mediaAudio:
				text = "[音频] " + audioLabel(info)
			case mediaDocument:
				text = "[文件] " + info.fileName
			case mediaPhoto:
				text = "[照片] Photo"
			case mediaSticker:
				text = "[贴纸] Sticker"
			case mediaAnimation:
				text = "[动画] GIF"
			case mediaVideo, mediaRoundVideo:
				text = "[视频] Video"
			}
		}
		return &renderReply{name: name, text: truncVisually(text, 36), color: paletteColor(id)}
	}
	return nil
}

func channelName(m *tg.Message) string {
	if m == nil || m.FromID == nil {
		return ""
	}
	if ch, ok := m.FromID.(*tg.PeerChannel); ok {
		return fmt.Sprintf("Channel %d", ch.ChannelID)
	}
	return ""
}

// loadFonts parses the wqy TTC once.
func (p *QuotePlugin) loadFonts() error {
	p.fontOnce.Do(func() {
		f, err := openWQY()
		if err != nil {
			p.fontErr = err
			return
		}
		faceCache.ttf = f
	})
	return p.fontErr
}

// squareCrop center-crops an image to a square (for avatars).
func squareCrop(img image.Image) image.Image {
	b := img.Bounds()
	side := min(b.Dx(), b.Dy())
	x0 := b.Min.X + (b.Dx()-side)/2
	y0 := b.Min.Y + (b.Dy()-side)/2
	type subImager interface {
		SubImage(r image.Rectangle) image.Image
	}
	if si, ok := img.(subImager); ok {
		return si.SubImage(image.Rect(x0, y0, x0+side, y0+side))
	}
	dst := image.NewNRGBA(image.Rect(0, 0, side, side))
	for y := 0; y < side; y++ {
		for x := 0; x < side; x++ {
			dst.Set(x, y, img.At(x0+x, y0+y))
		}
	}
	return dst
}
