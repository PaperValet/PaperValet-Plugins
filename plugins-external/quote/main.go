package main

// quote — turn replied messages into a rendered quote image. Rendering uses
// the official LyoSU/quote-api code (vendored by the TeleBox quote plugin)
// executed locally through a node bridge; the Go side collects messages,
// builds the JSON payload and sends the result. No remote quote service.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

const maxQuoteMessages = 50

type QuotePlugin struct {
	host plugin.Host
}

func New() *QuotePlugin { return &QuotePlugin{} }

var Metadata = &plugin.PluginMetadata{
	Name:        "quote",
	Description: "回复消息生成引用图（quote-api 官方渲染引擎）",
	DescEN:      "Turn replied messages into a quote image, rendered by the official quote-api engine",
	Version:     "2.0.0",
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
			"组合示例：`quote r 3` · `quote stories #231d2b/#372e44`\n\n"+
			"首次使用需联网下载渲染资源（较大，请耐心）。",
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
			"Examples: `quote r 3` · `quote stories #231d2b/#372e44`\n\n"+
			"First use downloads rendering assets (large, be patient).")
}

// ---------------------------------------------------------------- handler

func (p *QuotePlugin) handleQuote(ctx *plugin.CommandContext) error {
	if len(ctx.Args) == 1 && strings.EqualFold(ctx.Args[0], "help") {
		return ctx.Edit(helpText(ctx))
	}
	args := parseArgs(ctx.RawArgs)

	if nodeRuntime() == "" {
		return ctx.Edit("❌ " + ctx.Tlocal(
			"未找到 node，quote 渲染需要 Node.js 运行时",
			"node not found — quote rendering needs Node.js"))
	}

	isReply := ctx.Message != nil && ctx.Message.IsReply && ctx.Message.ReplyToID > 0

	_ = ctx.Edit(ctx.Tlocal("⏳ 正在收集消息…", "⏳ Collecting messages…"))

	msgs, chats, err := p.collectMessages(ctx, args, isReply)
	if err != nil {
		if d, ok := tgerr.AsFloodWait(err); ok {
			return ctx.Edit("❌ " + ctx.Tlocal(fmt.Sprintf("触发限流，请 %d 秒后重试", int(d.Seconds())), fmt.Sprintf("Flood wait, retry in %ds", int(d.Seconds()))))
		}
		return ctx.Edit("❌ " + ctx.Tlocal("获取消息失败: ", "failed to load messages: ") + plugin.Escape(err.Error()))
	}

	qmsgs := p.buildQuoteMessages(ctx, msgs, chats, args)

	rawMsgs := make([]json.RawMessage, 0, len(qmsgs))
	for _, qm := range qmsgs {
		b, err := json.Marshal(qm)
		if err != nil {
			return ctx.Edit("❌ " + ctx.Tlocal("消息序列化失败: ", "message encode failed: ") + plugin.Escape(err.Error()))
		}
		rawMsgs = append(rawMsgs, b)
	}

	// type/format mirror the TeleBox source: quote→webp sticker, png→image,
	// stories→720×1280 png.
	outType := "quote"
	if args.stories {
		outType = "stories"
	} else if args.png {
		outType = "image"
	}
	outFormat := "png"
	if outType == "quote" {
		outFormat = "webp"
	}

	assetsDir, _ := p.host.DataDir("quote")
	_ = ctx.Edit(ctx.Tlocal("⏳ 初始化 quote 资源（首次较慢）…", "⏳ Preparing quote assets (slow on first run)…"))

	res, err := runBridge(ctx.Context(), p.host, &bridgeRequest{
		Messages:        rawMsgs,
		Type:            outType,
		Format:          outFormat,
		Scale:           args.scale,
		BackgroundColor: args.bg,
		EmojiBrand:      "apple",
		AssetsDir:       assetsDir,
	})
	if err != nil {
		return ctx.Edit("❌ " + ctx.Tlocal("渲染失败: ", "render failed: ") + plugin.Escape(err.Error()))
	}

	replyTo := 0
	if isReply {
		replyTo = ctx.Message.ReplyToID
	} else if ctx.Message != nil && ctx.Message.Message != nil {
		replyTo = ctx.Message.Message.ID
	}

	if res.Ext == "webp" {
		webpPath, err := saveTemp(res.Data, ".webp")
		if err != nil {
			return ctx.Edit("❌ " + ctx.Tlocal("编码失败: ", "encode failed: ") + plugin.Escape(err.Error()))
		}
		defer os.Remove(webpPath)
		if err := p.sendSticker(ctx, webpPath, replyTo); err != nil {
			return ctx.Edit("❌ " + ctx.Tlocal("发送失败: ", "send failed: ") + plugin.Escape(err.Error()))
		}
	} else {
		pngPath, err := saveTemp(res.Data, ".png")
		if err != nil {
			return ctx.Edit("❌ " + ctx.Tlocal("编码失败: ", "encode failed: ") + plugin.Escape(err.Error()))
		}
		defer os.Remove(pngPath)
		if err := p.sendPhoto(ctx, pngPath, replyTo); err != nil {
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
// Any chats returned alongside the messages are passed back so channel
// posts can be attributed to the real channel title.
func (p *QuotePlugin) collectMessages(ctx *plugin.CommandContext, args quoteArgs, isReply bool) ([]*tg.Message, []tg.ChatClass, error) {
	api := ctx.API
	if api == nil {
		return nil, nil, fmt.Errorf("no api")
	}
	peer, err := ctx.ResolvePeer()
	if err != nil {
		return nil, nil, err
	}
	count := args.count
	if count == 0 {
		count = 1
	}

	var anchor *tg.Message
	var chats []tg.ChatClass
	if isReply {
		msgs, _, c, err := plugin.GetMessages(ctx.Context(), api, peer, ctx.Message.ReplyToID)
		if err != nil {
			return nil, nil, err
		}
		chats = c
		for _, m := range msgs {
			if m.ID == ctx.Message.ReplyToID {
				anchor = m
			}
		}
		if anchor == nil {
			return nil, nil, fmt.Errorf("reply message not found")
		}
	}

	if abs(count) <= 1 {
		if anchor != nil {
			return []*tg.Message{anchor}, chats, nil
		}
		if ctx.Message != nil && ctx.Message.Message != nil {
			return []*tg.Message{ctx.Message.Message}, nil, nil
		}
		return nil, nil, fmt.Errorf("no message")
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
			return []*tg.Message{anchor}, nil, nil
		}
		return []*tg.Message{ctx.Message.Message}, nil, nil
	}

	var list []*tg.Message
	switch v := res.(type) {
	case *tg.MessagesMessages:
		list, chats = messagesOf(v.Messages), v.Chats
	case *tg.MessagesMessagesSlice:
		list, chats = messagesOf(v.Messages), v.Chats
	case *tg.MessagesChannelMessages:
		list, chats = messagesOf(v.Messages), v.Chats
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
			return []*tg.Message{anchor}, chats, nil
		}
		return []*tg.Message{ctx.Message.Message}, chats, nil
	}
	return out, chats, nil
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

// buildQuoteMessages converts tg messages into the quote-api message model:
// name, avatar (base64), text + entities, media preview, voice/document/
// audio rows and reply preview. Field names follow quote-api/generate.js.
func (p *QuotePlugin) buildQuoteMessages(ctx *plugin.CommandContext, msgs []*tg.Message, chats []tg.ChatClass, args quoteArgs) []*quoteMessage {
	out := make([]*quoteMessage, 0, len(msgs))
	users := p.fetchUsers(ctx, msgs)
	channels := channelNames(chats)
	for _, m := range msgs {
		senderID := plugin.SenderID(m)
		qm := &quoteMessage{
			ChatID:      senderID,
			MessageID:   m.ID,
			AvatarScale: args.scale,
			MediaCrop:   args.crop,
		}
		name := "User"
		if n := userName(users[senderID]); n != "" {
			name = n
		} else if ch, ok := m.FromID.(*tg.PeerChannel); ok && channels[ch.ChannelID] != "" {
			// channel posts: real title from the chats in the response
			name = channels[ch.ChannelID]
			if senderID == 0 {
				qm.ChatID = -ch.ChannelID
			}
		} else if ch := channelName(m); ch != "" {
			name = ch
		}
		if args.hidden {
			qm.From = &quoteFrom{ID: senderID, Name: false, FirstName: false}
		} else {
			qm.From = &quoteFrom{ID: senderID, Name: name, FirstName: name}
			if av := p.fetchAvatarBytes(ctx, senderID, users[senderID]); len(av) > 0 {
				qm.Avatar = true
				qm.AvatarBuffer = base64.StdEncoding.EncodeToString(av)
			}
		}

		// media classification first
		kind, info, _, _ := classifyMedia(m.Media)
		switch kind {
		case mediaVoice:
			wf := make([]int, len(info.waveform))
			for i, b := range info.waveform {
				// waveform bytes are 5-bit amplitudes shifted left by 3
				wf[i] = min(31, int(b)>>3)
			}
			if len(wf) > 0 {
				qm.Voice = &bridgeVoice{Waveform: wf, Duration: info.duration}
			}
			qm.Text = m.Message
		case mediaAudio:
			qm.Audio = &bridgeAudio{Title: audioLabel(info), Performer: info.performer, Duration: info.duration}
			qm.Text = m.Message
		case mediaDocument:
			if shouldFetchPreview(kind, args.media) {
				qm.MediaCanvas = base64If(fetchMediaPreviewBytes(ctx.Context(), ctx.API, m.Media))
				qm.MediaType = "photo"
			}
			if qm.MediaCanvas == "" {
				qm.Document = &bridgeDocument{FileName: info.fileName, FileSize: info.size}
			}
			qm.Text = m.Message
		case mediaPhoto, mediaSticker, mediaAnimation, mediaVideo, mediaRoundVideo:
			if data := fetchMediaPreviewBytes(ctx.Context(), ctx.API, m.Media); len(data) > 0 {
				qm.MediaCanvas = base64.StdEncoding.EncodeToString(data)
				switch kind {
				case mediaSticker:
					qm.MediaType = "sticker"
					qm.MediaMaxSize = 220 * args.scale
					qm.MediaCrop = false
				case mediaAnimation:
					qm.MediaType = "gif"
				case mediaVideo, mediaRoundVideo:
					qm.MediaType = "video"
				default:
					qm.MediaType = "photo"
				}
				if kind == mediaVideo || kind == mediaRoundVideo || kind == mediaAnimation {
					qm.MediaDur = info.duration
				}
			}
			qm.Text = m.Message
		default:
			qm.Text = m.Message
		}
		qm.Caption = qm.Text
		qm.Entities = bridgeEntities(m)
		qm.CaptionEnts = qm.Entities

		// reply preview
		if args.reply {
			if hdr, ok := m.ReplyTo.(*tg.MessageReplyHeader); ok && hdr.ReplyToMsgID > 0 {
				qm.ReplyMessage = p.fetchReplyPreview(ctx, hdr.ReplyToMsgID)
			}
		}
		out = append(out, qm)
	}
	return out
}

func base64If(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return base64.StdEncoding.EncodeToString(b)
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

// bridgeEntities maps tg entities onto the quote-api entity model (types
// follow TeleBox convertEntities).
func bridgeEntities(m *tg.Message) []bridgeEntity {
	var out []bridgeEntity
	for _, e := range m.Entities {
		be := bridgeEntity{
			Type:   entityTypeOf(e),
			Offset: entityOffset(e),
			Length: entityLength(e),
		}
		switch v := e.(type) {
		case *tg.MessageEntityTextURL:
			be.URL = v.URL
		case *tg.MessageEntityPre:
			be.Language = v.Language
		}
		if be.Length > 0 && be.Type != "text" {
			out = append(out, be)
		}
	}
	return out
}

func entityTypeOf(e tg.MessageEntityClass) string {
	switch e.(type) {
	case *tg.MessageEntityBold:
		return "bold"
	case *tg.MessageEntityItalic:
		return "italic"
	case *tg.MessageEntityUnderline:
		return "underline"
	case *tg.MessageEntityStrike:
		return "strikethrough"
	case *tg.MessageEntityBlockquote:
		return "blockquote"
	case *tg.MessageEntitySpoiler:
		return "spoiler"
	case *tg.MessageEntityCode:
		return "code"
	case *tg.MessageEntityPre:
		return "pre"
	case *tg.MessageEntityTextURL:
		return "text_link"
	case *tg.MessageEntityMentionName:
		return "text_mention"
	case *tg.MessageEntityMention:
		return "mention"
	case *tg.MessageEntityHashtag:
		return "hashtag"
	case *tg.MessageEntityCashtag:
		return "cashtag"
	case *tg.MessageEntityBotCommand:
		return "bot_command"
	case *tg.MessageEntityURL:
		return "url"
	case *tg.MessageEntityEmail:
		return "email"
	case *tg.MessageEntityPhone:
		return "phone_number"
	case *tg.MessageEntityCustomEmoji:
		return "custom_emoji"
	}
	return "text"
}

func entityOffset(e tg.MessageEntityClass) int {
	switch v := e.(type) {
	case *tg.MessageEntityBold:
		return v.Offset
	case *tg.MessageEntityItalic:
		return v.Offset
	case *tg.MessageEntityUnderline:
		return v.Offset
	case *tg.MessageEntityStrike:
		return v.Offset
	case *tg.MessageEntityBlockquote:
		return v.Offset
	case *tg.MessageEntitySpoiler:
		return v.Offset
	case *tg.MessageEntityCode:
		return v.Offset
	case *tg.MessageEntityPre:
		return v.Offset
	case *tg.MessageEntityTextURL:
		return v.Offset
	case *tg.MessageEntityMentionName:
		return v.Offset
	case *tg.MessageEntityMention:
		return v.Offset
	case *tg.MessageEntityHashtag:
		return v.Offset
	case *tg.MessageEntityCashtag:
		return v.Offset
	case *tg.MessageEntityBotCommand:
		return v.Offset
	case *tg.MessageEntityURL:
		return v.Offset
	case *tg.MessageEntityEmail:
		return v.Offset
	case *tg.MessageEntityPhone:
		return v.Offset
	case *tg.MessageEntityCustomEmoji:
		return v.Offset
	}
	return 0
}

func entityLength(e tg.MessageEntityClass) int {
	switch v := e.(type) {
	case *tg.MessageEntityBold:
		return v.Length
	case *tg.MessageEntityItalic:
		return v.Length
	case *tg.MessageEntityUnderline:
		return v.Length
	case *tg.MessageEntityStrike:
		return v.Length
	case *tg.MessageEntityBlockquote:
		return v.Length
	case *tg.MessageEntitySpoiler:
		return v.Length
	case *tg.MessageEntityCode:
		return v.Length
	case *tg.MessageEntityPre:
		return v.Length
	case *tg.MessageEntityTextURL:
		return v.Length
	case *tg.MessageEntityMentionName:
		return v.Length
	case *tg.MessageEntityMention:
		return v.Length
	case *tg.MessageEntityHashtag:
		return v.Length
	case *tg.MessageEntityCashtag:
		return v.Length
	case *tg.MessageEntityBotCommand:
		return v.Length
	case *tg.MessageEntityURL:
		return v.Length
	case *tg.MessageEntityEmail:
		return v.Length
	case *tg.MessageEntityPhone:
		return v.Length
	case *tg.MessageEntityCustomEmoji:
		return v.Length
	}
	return 0
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

// fetchAvatarBytes downloads a user's photo (small size) as raw image bytes
// (passed to the renderer as base64; it crops to a circle itself).
func (p *QuotePlugin) fetchAvatarBytes(ctx *plugin.CommandContext, userID int64, uc tg.UserClass) []byte {
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
	return data
}

// fetchReplyPreview loads name+text of the replied message for the preview
// block.
func (p *QuotePlugin) fetchReplyPreview(ctx *plugin.CommandContext, msgID int) *quoteReply {
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
		senderID := plugin.SenderID(m)
		name := userName(users[senderID])
		if name == "" {
			name = "User"
		}
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
		rp := &quoteReply{
			ChatID:   senderID,
			Name:     name,
			Text:     truncVisually(text, 36),
			Entities: []bridgeEntity{},
			From:     &quoteFrom{ID: senderID, Name: name, FirstName: name},
		}
		return rp
	}
	return nil
}

// channelNames extracts channel titles from the chats returned alongside
// messages (channels are named by the chat object, not a user).
func channelNames(chats []tg.ChatClass) map[int64]string {
	out := map[int64]string{}
	for _, c := range chats {
		switch v := c.(type) {
		case *tg.Channel:
			if v.Title != "" {
				out[v.ID] = v.Title
			}
		case *tg.Chat:
			if v.Title != "" {
				out[v.ID] = v.Title
			}
		}
	}
	return out
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

// truncVisually truncates s to maxRunes with an ellipsis.
func truncVisually(s string, maxRunes int) string {
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	if maxRunes <= 1 {
		return "…"
	}
	return string(r[:maxRunes-1]) + "…"
}
