package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

const (
	firstReplyWait = 40 * time.Second  // until the bot answers the query
	audioWait      = 150 * time.Second // until the audio arrives (lossless files are slow)
	listTTL        = 15 * time.Minute  // how long "music <n>" can pick from a search list
)

var Metadata = &plugin.PluginMetadata{
	Name:        "music",
	Description: "通过音乐机器人搜歌和发歌",
	DescEN:      "Search and send songs through music bots",
	Version:     "1.0.0",
	Author:      "PaperValet",
	MinVersion:  "0.1.0",
}

// lastList is a search result the owner can pick from with "music <n>".
type lastList struct {
	src     *source
	msgID   int
	titles  []string
	buttons []*tg.KeyboardButtonCallback
	at      time.Time
}

type MusicPlugin struct {
	host plugin.Host
	set  plugin.Settings
	log  plugin.Logger

	mu       sync.Mutex
	lists    map[int64]*lastList // chat id → last search
	locks    map[string]*sync.Mutex
	prepared map[string]bool
	peers    map[string]*tg.InputPeerUser
	ctx      context.Context
	cancel   context.CancelFunc
}

func New() *MusicPlugin {
	return &MusicPlugin{lists: map[int64]*lastList{}, locks: map[string]*sync.Mutex{}, prepared: map[string]bool{}, peers: map[string]*tg.InputPeerUser{}}
}

func (p *MusicPlugin) Name() string        { return "music" }
func (p *MusicPlugin) Description() string { return Metadata.Description }
func (p *MusicPlugin) DescEN() string      { return Metadata.DescEN }

func (p *MusicPlugin) Init(_ context.Context, mgr plugin.Manager) error {
	p.host = mgr.Host()
	p.log = p.host.Logger(p.Name())
	choices := make([]plugin.Choice, 0, len(sources))
	for _, s := range sources {
		choices = append(choices, plugin.Choice{Value: s.Key, Label: "@" + s.Bot + " · " + s.Name, LabelEN: "@" + s.Bot + " · " + s.NameEN})
	}
	set, err := p.host.Settings(&plugin.SettingsSpec{
		Plugin: p.Name(), Title: "🎵 音乐", TitleEN: "🎵 Music",
		Settings: []plugin.Setting{
			{Key: "source", Label: "默认音源机器人", LabelEN: "Default bot", Kind: plugin.SettingChoice, Default: "down", Choices: choices,
				Hint: "命令里不写 @音源 时用它；指定的平台它不支持时自动换一个", HintEN: "Used when no @source is given; another bot is picked if it lacks the platform"},
			{Key: "platform", Label: "默认平台", LabelEN: "Default platform", Kind: plugin.SettingChoice, Default: "", Choices: platformChoices(),
				Hint: "命令里不写平台时用它；网易云翻唱多，找原唱选 QQ 音乐或酷我更准", HintEN: "Used when no platform is given; QQ Music or Kuwo find originals more reliably"},
			{Key: "quality", Label: "音质", LabelEN: "Quality", Kind: plugin.SettingChoice, Default: "",
				Hint: "只对 @Music163DownBot 生效", HintEN: "Only applies to @Music163DownBot",
				Choices: []plugin.Choice{
					{Value: "", Label: "机器人默认", LabelEN: "Bot default"},
					{Value: "low", Label: "标准", LabelEN: "Standard"},
					{Value: "high", Label: "较高", LabelEN: "High"},
					{Value: "lossless", Label: "无损", LabelEN: "Lossless"},
					{Value: "hires", Label: "Hi-Res", LabelEN: "Hi-Res"},
				}},
			{Key: "caption", Label: "附带歌名说明", LabelEN: "Caption with title", Kind: plugin.SettingToggle, Default: true},
		},
	})
	if err != nil {
		return err
	}
	p.set = set
	return mgr.RegisterCommand(&plugin.Command{
		Name:        "music",
		Description: "让音乐机器人找歌并把音频发到当前聊天，支持多平台和先搜后选",
		DescEN:      "Have a music bot find a song and post the audio here, across platforms, with search-then-pick",
		Usage:       "music [@音源] [平台] <歌名|链接> · music search … · music <序号>",
		UsageEN:     "music [@source] [platform] <song|link> · music search … · music <n>",
		Plugin:      p.Name(),
		Category:    "media",
		OwnerOnly:   true,
		Handler:     p.handle,
	})
}

func (p *MusicPlugin) Start(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel == nil {
		p.ctx, p.cancel = context.WithCancel(context.Background())
	}
	return nil
}

func (p *MusicPlugin) Stop(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
	}
	return nil
}

func (p *MusicPlugin) help(tl func(string, string) string) string {
	var src []string
	for _, s := range sources {
		src = append(src, plugin.Code("@"+s.Key)+" @"+plugin.Escape(s.Bot))
	}
	var plats []string
	for _, n := range platformNames {
		plats = append(plats, plugin.Code(n))
	}
	return tl(
		"🎵 **音乐**\n\n"+
			plugin.Code("music 晴天 周杰伦")+" 直接发歌\n"+
			plugin.Code("music qq 晴天")+" 指定平台\n"+
			plugin.Code("music @vmomo 晴天")+" 指定音源机器人\n"+
			plugin.Code("music search 晴天")+" 先列出结果\n"+
			plugin.Code("music 3")+" 从上次列表里选第 3 首\n"+
			"回复某条消息发命令，歌曲也回复那条消息\n\n"+
			"**音源**\n"+strings.Join(src, "\n")+"\n\n"+
			"**平台** "+strings.Join(plats, " ")+"\n\n"+
			"默认音源、平台和音质在机器人面板里设置",
		"🎵 **Music**\n\n"+
			plugin.Code("music Shape of You")+" send a song right away\n"+
			plugin.Code("music qq 晴天")+" pick a platform\n"+
			plugin.Code("music @vk Shape of You")+" pick a bot\n"+
			plugin.Code("music search Shape of You")+" list results first\n"+
			plugin.Code("music 3")+" take #3 from the last list\n"+
			"Send it as a reply and the song replies to the same message\n\n"+
			"**Bots**\n"+strings.Join(src, "\n")+"\n\n"+
			"**Platforms** "+strings.Join(plats, " ")+"\n\n"+
			"Default bot, platform and quality are set in the bot panel")
}

func (p *MusicPlugin) lifetime() context.Context {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ctx == nil {
		return context.Background()
	}
	return p.ctx
}

func (p *MusicPlugin) handle(ctx *plugin.CommandContext) error {
	if len(ctx.Args) == 0 || (len(ctx.Args) == 1 && (strings.EqualFold(ctx.Args[0], "help") || strings.EqualFold(ctx.Args[0], "h"))) {
		return ctx.Edit(p.help(ctx.Tlocal))
	}
	req := parseArgs(ctx.Args)
	replyTo := 0
	if ctx.Message.IsReply {
		replyTo = ctx.Message.ReplyToID
	}
	if req.Pick > 0 {
		return p.pick(ctx, req.Pick, replyTo)
	}
	if req.Query == "" {
		return ctx.Edit("❌ " + ctx.Tlocal("要搜什么歌？", "Which song?") + "\n\n" + p.help(ctx.Tlocal))
	}
	def := sourceByKey(p.set.String("source"))
	if def == nil {
		def = sources[0]
	}
	src, plat, ok := route(req, def)
	if ok && req.Plat == "" {
		// The default platform applies only where the chosen bot has it.
		if tok, has := src.platforms[p.set.String("platform")]; has {
			plat = tok
		}
	}
	if !ok {
		if req.Source != nil {
			return ctx.Edit("❌ @" + plugin.Escape(req.Source.Bot) + ctx.Tlocal(" 不支持平台 ", " does not support ") + plugin.Code(req.Plat))
		}
		return ctx.Edit("❌ " + ctx.Tlocal("没有音源支持平台 ", "No bot supports ") + plugin.Code(req.Plat))
	}

	run, cancel := context.WithTimeout(p.lifetime(), firstReplyWait+audioWait)
	defer cancel()
	unlock, err := p.lockBot(run, src)
	if err != nil {
		return ctx.Edit("❌ " + ctx.Tlocal("上一个请求还没完成", "The previous request is still running"))
	}
	defer unlock()

	bot, q := plugin.Escape(src.Bot), plugin.Code(req.Query)
	_ = ctx.Edit("🔎 " + ctx.Tlocal("正在用 @"+bot+" 搜索 "+q+"…", "Searching "+q+" with @"+bot+"…"))
	text := src.get(req.Query, plat, p.set.String("quality"))
	if req.Search {
		text = src.search(req.Query, plat)
	}
	sess, err := p.open(run, src)
	if err != nil {
		return ctx.Edit("❌ " + p.errText(ctx.Tlocal, src, err))
	}
	defer sess.close()
	if err := sess.send(text); err != nil {
		return ctx.Edit("❌ " + p.errText(ctx.Tlocal, src, err))
	}

	res := sess.wait(firstReplyWait, audioWait)
	switch {
	case res.err != nil:
		return ctx.Edit("❌ " + p.errText(ctx.Tlocal, src, res.err))
	case res.audio != nil:
		return p.deliver(ctx, res.audio, replyTo)
	case len(res.buttons) == 0:
		return ctx.Edit("❌ @" + plugin.Escape(src.Bot) + ctx.Tlocal(" 回复：", " says: ") + "\n> " + plugin.Escape(clip(res.text, 300)))
	}

	list := &lastList{src: src, msgID: res.msg.ID, titles: listTitles(res.text), buttons: res.buttons, at: time.Now()}
	p.mu.Lock()
	p.lists[ctx.Message.ChatID] = list
	p.mu.Unlock()
	if req.Search {
		return ctx.Edit(renderList(ctx.Tlocal, list))
	}
	title := ""
	if len(list.titles) > 0 {
		title = list.titles[0]
	}
	return p.fetchPick(ctx, sess, list, 1, title, replyTo)
}

// pick takes item n from the chat's last search list.
func (p *MusicPlugin) pick(ctx *plugin.CommandContext, n, replyTo int) error {
	p.mu.Lock()
	list := p.lists[ctx.Message.ChatID]
	p.mu.Unlock()
	if list == nil || time.Since(list.at) > listTTL {
		return ctx.Edit("❌ " + ctx.Tlocal("没有可选的列表，先发 ", "Nothing to pick from, send ") + plugin.Code(ctx.Tlocal("music search <歌名>", "music search <song>")))
	}
	if n < 1 || n > len(list.buttons) {
		return ctx.Edit(fmt.Sprintf("❌ "+ctx.Tlocal("序号要在 1 到 %d 之间", "Pick a number from 1 to %d"), len(list.buttons)))
	}
	run, cancel := context.WithTimeout(p.lifetime(), audioWait+10*time.Second)
	defer cancel()
	unlock, err := p.lockBot(run, list.src)
	if err != nil {
		return ctx.Edit("❌ " + ctx.Tlocal("上一个请求还没完成", "The previous request is still running"))
	}
	defer unlock()
	sess, err := p.open(run, list.src)
	if err != nil {
		return ctx.Edit("❌ " + p.errText(ctx.Tlocal, list.src, err))
	}
	defer sess.close()
	sess.after = list.msgID
	title := ""
	if n <= len(list.titles) {
		title = list.titles[n-1]
	}
	return p.fetchPick(ctx, sess, list, n, title, replyTo)
}

func (p *MusicPlugin) fetchPick(ctx *plugin.CommandContext, sess *session, list *lastList, n int, title string, replyTo int) error {
	shown := plugin.Code(fmt.Sprint(n))
	if title != "" {
		shown = plugin.Escape(title)
	}
	_ = ctx.Edit("⬇️ " + ctx.Tlocal("已选 ", "Picked ") + shown + "\n" + ctx.Tlocal("等待 @", "Waiting for @") + plugin.Escape(list.src.Bot) + ctx.Tlocal(" 发送音频…", " to send the audio…"))
	sess.click(list.msgID, list.buttons[n-1].Data)
	res := sess.wait(audioWait, audioWait)
	switch {
	case res.err != nil:
		return ctx.Edit("❌ " + p.errText(ctx.Tlocal, list.src, res.err))
	case res.audio == nil:
		return ctx.Edit("❌ @" + plugin.Escape(list.src.Bot) + ctx.Tlocal(" 回复：", " says: ") + "\n> " + plugin.Escape(clip(res.text, 300)))
	}
	return p.deliver(ctx, res.audio, replyTo)
}

func renderList(tl func(string, string) string, l *lastList) string {
	var b strings.Builder
	b.WriteString("🎵 **" + tl("搜索结果", "Results") + "** · @" + plugin.Escape(l.src.Bot) + "\n\n")
	for i := range l.buttons {
		t := fmt.Sprintf(tl("第 %d 首", "Track %d"), i+1)
		if i < len(l.titles) {
			t = l.titles[i]
		}
		b.WriteString(fmt.Sprintf("%d. %s\n", i+1, plugin.Escape(t)))
	}
	b.WriteString("\n" + tl("发 ", "Send ") + plugin.Code("music <n>") + tl(" 选歌", " to pick"))
	return b.String()
}

// deliver re-sends the bot's audio by reference into the command chat,
// then removes the command message.
func (p *MusicPlugin) deliver(ctx *plugin.CommandContext, m *tg.Message, replyTo int) error {
	doc, au := audioDoc(m)
	if doc == nil {
		return ctx.Edit("❌ " + ctx.Tlocal("机器人发来的不是音频", "The bot did not send audio"))
	}
	peer, err := ctx.ResolvePeer()
	if err != nil {
		return ctx.Edit("❌ " + plugin.Escape(err.Error()))
	}
	req := &tg.MessagesSendMediaRequest{
		Peer:     peer,
		Media:    &tg.InputMediaDocument{ID: &tg.InputDocument{ID: doc.ID, AccessHash: doc.AccessHash, FileReference: doc.FileReference}},
		RandomID: time.Now().UnixNano(),
	}
	if p.set.Bool("caption") {
		if c := captionFor(au); c != "" {
			plain, ents := plugin.ParseMarkdown(c, nil)
			req.Message = plain
			if len(ents) > 0 {
				req.SetEntities(ents)
			}
		}
	}
	if replyTo != 0 {
		req.SetReplyTo(&tg.InputReplyToMessage{ReplyToMsgID: replyTo})
	}
	if _, err := ctx.API.MessagesSendMedia(ctx.Context(), req); err != nil {
		return ctx.Edit("❌ " + ctx.Tlocal("发送音频失败：", "Could not send the audio: ") + tgText(ctx.Tlocal, err))
	}
	return ctx.Delete()
}

func captionFor(au *tg.DocumentAttributeAudio) string {
	if au == nil || au.Title == "" {
		return ""
	}
	s := "🎵 **" + plugin.Escape(au.Title) + "**"
	if au.Performer != "" {
		s += " - " + plugin.Escape(au.Performer)
	}
	return s
}

// lockBot serializes requests per bot: replies are matched by chat, so two
// requests to one bot at once would steal each other's answers.
func (p *MusicPlugin) lockBot(ctx context.Context, s *source) (func(), error) {
	p.mu.Lock()
	l := p.locks[s.Key]
	if l == nil {
		l = &sync.Mutex{}
		p.locks[s.Key] = l
	}
	p.mu.Unlock()
	t := time.NewTicker(200 * time.Millisecond)
	defer t.Stop()
	deadline := time.Now().Add(20 * time.Second)
	for !l.TryLock() {
		if time.Now().After(deadline) {
			return nil, errors.New("busy")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-t.C:
		}
	}
	return l.Unlock, nil
}

var errTimeout = errors.New("timeout")

func (p *MusicPlugin) errText(tl func(string, string) string, s *source, err error) string {
	if errors.Is(err, errTimeout) || errors.Is(err, context.DeadlineExceeded) {
		return "@" + plugin.Escape(s.Bot) + tl(" 没有回应，稍后再试或换个音源", " did not answer; retry later or use another bot")
	}
	return tgText(tl, err)
}

func tgText(tl func(string, string) string, err error) string {
	if d, ok := tgerr.AsFloodWait(err); ok {
		return fmt.Sprintf(tl("请求过于频繁，请 %d 秒后重试", "flood wait, retry in %d seconds"), int(d.Seconds())+1)
	}
	if e, ok := tgerr.As(err); ok {
		switch e.Type {
		case "CHAT_SEND_AUDIOS_FORBIDDEN", "CHAT_SEND_MEDIA_FORBIDDEN", "CHAT_SEND_DOCS_FORBIDDEN":
			return tl("这个聊天不允许发音频", "audio is not allowed in this chat")
		case "USERNAME_NOT_OCCUPIED", "USERNAME_INVALID":
			return tl("音源机器人不存在了", "the bot no longer exists")
		}
		return plugin.Code(e.Type)
	}
	return plugin.Escape(err.Error())
}
