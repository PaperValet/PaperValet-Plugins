package main

import (
	"context"
	"fmt"
	"hash/fnv"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

const (
	dbFile       = "db.json"
	reactTimeout = 15 * time.Second
	premiumTTL   = 10 * time.Minute
)

var Metadata = &plugin.PluginMetadata{
	Name:        "trace",
	Description: "自动回应指定用户或关键字的消息",
	DescEN:      "Auto-react to chosen users or keywords",
	Version:     "1.0.0",
	Author:      "PaperValet",
	MinVersion:  "0.1.0",
}

type userEntry struct {
	Name      string   `json:"name,omitempty"`
	Reactions []string `json:"reactions"`
}

type traceDB struct {
	Users    map[string]*userEntry `json:"users"`
	Keywords map[string][]string   `json:"keywords"`
}

type TracePlugin struct {
	mu   sync.Mutex
	db   traceDB
	dir  string
	host plugin.Host
	set  plugin.Settings
	log  plugin.Logger

	premium    bool
	premiumAt  time.Time
	stopListen func()
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
}

func New() *TracePlugin {
	return &TracePlugin{db: traceDB{Users: map[string]*userEntry{}, Keywords: map[string][]string{}}}
}

func (p *TracePlugin) Name() string        { return "trace" }
func (p *TracePlugin) Description() string { return Metadata.Description }
func (p *TracePlugin) DescEN() string      { return Metadata.DescEN }

func (p *TracePlugin) Init(_ context.Context, mgr plugin.Manager) error {
	p.host = mgr.Host()
	p.log = p.host.Logger(p.Name())
	dir, err := p.host.DataDir(p.Name())
	if err != nil {
		return err
	}
	p.dir = dir
	set, err := p.host.Settings(&plugin.SettingsSpec{
		Plugin: p.Name(), Title: "🎯 自动回应", TitleEN: "🎯 Trace",
		Settings: []plugin.Setting{
			{Key: "keep_log", Label: "保留操作回执", LabelEN: "Keep receipts", Kind: plugin.SettingToggle, Default: true,
				Hint: "关闭后命令回执 10 秒后自动删除", HintEN: "When off, command receipts are deleted after 10 seconds"},
			{Key: "big", Label: "大号表情动画", LabelEN: "Big reaction animation", Kind: plugin.SettingToggle, Default: true},
		},
	})
	if err != nil {
		return err
	}
	p.set = set
	if err := p.host.Bot(p.Name()).SetPage(&plugin.Page{Title: "追踪列表", TitleEN: "Traces", Handle: p.page}); err != nil && err != plugin.ErrBotNotReady {
		return err
	}
	return mgr.RegisterCommand(&plugin.Command{
		Name:        "trace",
		Description: "自动给指定用户或含关键字的消息贴表情回应",
		DescEN:      "Automatically react to messages from chosen users or containing keywords",
		Usage:       "回复消息: trace <表情…> 追踪 · 回复消息: trace 取消 · trace kw add <词> <表情…> · trace kw del <词> · trace status · trace clean",
		UsageEN:     "reply: trace <emoji…> to trace · reply: trace to untrace · trace kw add <word> <emoji…> · trace kw del <word> · trace status · trace clean",
		Plugin:      p.Name(),
		Category:    "tools",
		OwnerOnly:   true,
		Handler:     p.handle,
	})
}

func (p *TracePlugin) Start(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel != nil {
		return nil
	}
	var db traceDB
	if err := readJSON(filepath.Join(p.dir, dbFile), &db); err != nil {
		return fmt.Errorf("trace: load: %w", err)
	}
	if db.Users == nil {
		db.Users = map[string]*userEntry{}
	}
	if db.Keywords == nil {
		db.Keywords = map[string][]string{}
	}
	p.db = db
	p.ctx, p.cancel = context.WithCancel(context.Background())
	p.stopListen = p.host.Listen(p.Name(), p.onMessage)
	return nil
}

func (p *TracePlugin) Stop(context.Context) error {
	p.mu.Lock()
	stop, cancel := p.stopListen, p.cancel
	p.stopListen, p.cancel = nil, nil
	p.mu.Unlock()
	if stop != nil {
		stop()
	}
	if cancel != nil {
		cancel()
	}
	p.wg.Wait()
	return nil
}

func (p *TracePlugin) saveLocked() error {
	return writeJSON(filepath.Join(p.dir, dbFile), p.db)
}

func (p *TracePlugin) lifetime() context.Context {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ctx == nil {
		return context.Background()
	}
	return p.ctx
}

// senderKey identifies who sent msg: the from peer, or the chat itself for
// private chats and channel posts.
func senderKey(msg *tg.Message) int64 {
	if msg == nil {
		return 0
	}
	if msg.FromID != nil {
		return plugin.ChatIDOf(msg.FromID)
	}
	return plugin.ChatIDOf(msg.PeerID)
}

// replyMsgID returns the id of the message the command actually replies
// to, or 0. A plain message inside a forum topic carries a reply header
// pointing at the topic root; that is not a reply (host re.go has the same
// distinction).
func replyMsgID(ev *plugin.MessageEvent) int {
	if ev == nil || ev.Message == nil {
		return 0
	}
	hdr, ok := ev.Message.ReplyTo.(*tg.MessageReplyHeader)
	if !ok {
		return 0
	}
	id, has := hdr.GetReplyToMsgID()
	if !has || id <= 0 {
		return 0
	}
	if hdr.ForumTopic {
		if _, hasTop := hdr.GetReplyToTopID(); !hasTop {
			return 0
		}
	}
	return id
}

// onMessage runs on the update path; the reaction is sent in a goroutine.
func (p *TracePlugin) onMessage(_ context.Context, ev *plugin.MessageEvent, edited bool) {
	if edited || ev == nil || ev.IsOut || ev.Message == nil {
		return
	}
	key := senderKey(ev.Message)
	if key == 0 || key == p.host.SelfID() {
		return
	}
	p.mu.Lock()
	if p.cancel == nil {
		p.mu.Unlock()
		return
	}
	var reactions []string
	if u, ok := p.db.Users[strconv.FormatInt(key, 10)]; ok {
		reactions = u.Reactions
	} else if kw, ok := matchKeyword(ev.Text, p.db.Keywords); ok {
		reactions = p.db.Keywords[kw]
	}
	ctx := p.ctx
	p.mu.Unlock()
	if len(reactions) == 0 {
		return
	}
	reactions = append([]string(nil), reactions...)
	chatID, msgID := ev.ChatID, ev.Message.ID
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		if err := p.react(ctx, chatID, msgID, reactions); err != nil {
			p.log.Debug("trace: reaction failed", "chat", chatID, "msg", msgID, "error", err)
		}
	}()
}

func (p *TracePlugin) react(ctx context.Context, chatID int64, msgID int, reactions []string) error {
	ctx, cancel := context.WithTimeout(ctx, reactTimeout)
	defer cancel()
	peer, err := p.host.PeerResolver().ResolveFromChatID(ctx, chatID)
	if err != nil {
		return err
	}
	return p.sendReaction(ctx, peer, msgID, reactions)
}

func (p *TracePlugin) sendReaction(ctx context.Context, peer tg.InputPeerClass, msgID int, reactions []string) error {
	req := &tg.MessagesSendReactionRequest{Peer: peer, MsgID: msgID, Reaction: toTG(reactions)}
	if p.set == nil || p.set.Bool("big") {
		req.SetBig(true)
	}
	_, err := p.host.API().MessagesSendReaction(ctx, req)
	return err
}

// isPremium reports (cached) whether the account has Telegram Premium.
func (p *TracePlugin) isPremium(ctx context.Context, api *tg.Client) bool {
	p.mu.Lock()
	if !p.premiumAt.IsZero() && time.Since(p.premiumAt) < premiumTTL {
		v := p.premium
		p.mu.Unlock()
		return v
	}
	p.mu.Unlock()
	users, err := api.UsersGetUsers(ctx, []tg.InputUserClass{&tg.InputUserSelf{}})
	v := false
	if err == nil && len(users) > 0 {
		if u, ok := users[0].(*tg.User); ok {
			v = u.Premium
		}
	}
	p.mu.Lock()
	p.premium, p.premiumAt = v, time.Now()
	p.mu.Unlock()
	return v
}

// ---------------------------------------------------------------- commands

func (p *TracePlugin) handle(ctx *plugin.CommandContext) error {
	if ctx.Message == nil || ctx.Message.Message == nil {
		return plugin.ErrNoMessage
	}
	sub := strings.ToLower(ctx.GetArg(0))
	if sub == "help" {
		return ctx.Edit(helpText(ctx))
	}
	if reply := replyMsgID(ctx.Message); reply != 0 {
		if len(ctx.Args) == 0 {
			return p.untraceUser(ctx)
		}
		if sub != "kw" && sub != "status" && sub != "clean" {
			return p.traceUser(ctx)
		}
	}
	switch sub {
	case "kw":
		action := strings.ToLower(ctx.GetArg(1))
		word := ctx.GetArg(2)
		switch {
		case action == "add" && word != "" && ctx.ArgCount() > 3:
			return p.traceKeyword(ctx, word)
		case action == "del" && word != "":
			return p.untraceKeyword(ctx, word)
		}
	case "status":
		return ctx.Edit(p.statusText(ctx.Tlocal))
	case "clean":
		return p.clean(ctx)
	}
	return ctx.Edit(helpText(ctx))
}

func helpText(ctx *plugin.CommandContext) string {
	tl := ctx.Tlocal
	line := func(cmd, zh, en string) string { return plugin.Code(cmd) + "  " + tl(zh, en) + "\n" }
	var b strings.Builder
	b.WriteString("🎯 **" + tl("自动回应 (Trace)", "Auto-react (Trace)") + "**\n\n")
	b.WriteString("**" + tl("用户追踪", "Users") + "**\n")
	b.WriteString(line(tl("回复消息 + trace 👍👎🥰", "reply + trace 👍👎🥰"), "用这些表情追踪该用户", "trace that user with these reactions"))
	b.WriteString(line(tl("回复消息 + trace", "reply + trace"), "取消追踪该用户", "stop tracing that user"))
	b.WriteString("\n**" + tl("关键字追踪", "Keywords") + "**\n")
	b.WriteString(line(tl("trace kw add <词> 👍👎", "trace kw add <word> 👍👎"), "含关键字的消息自动回应", "react to messages containing the word"))
	b.WriteString(line(tl("trace kw del <词>", "trace kw del <word>"), "删除关键字追踪", "remove a keyword"))
	b.WriteString("\n**" + tl("管理", "Manage") + "**\n")
	b.WriteString(line("trace status", "查看追踪列表", "show traces"))
	b.WriteString(line("trace clean", "清除所有追踪", "remove all traces"))
	b.WriteString("\n💡 " + tl("标准表情无需会员，自定义表情需要 Premium；回执保留与大号动画在机器人面板设置",
		"Standard reactions need no Premium, custom emoji do; receipt keeping and big animation are set in the bot panel") + "\n")
	b.WriteString(tl("可用表情：", "Reactions: ") + strings.Join(standardReactions, ""))
	return b.String()
}

// receipt edits the command message and, when receipts are not kept,
// deletes it after a while.
func (p *TracePlugin) receipt(ctx *plugin.CommandContext, text string, seconds int) error {
	err := ctx.Edit(text)
	if p.set != nil && !p.set.Bool("keep_log") {
		api, resolver, chatID, id := ctx.API, ctx.PeerResolver, ctx.Message.ChatID, ctx.Message.Message.ID
		life := p.lifetime()
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			t := time.NewTimer(time.Duration(seconds) * time.Second)
			defer t.Stop()
			select {
			case <-life.Done():
				return
			case <-t.C:
			}
			dctx, cancel := context.WithTimeout(context.Background(), reactTimeout)
			defer cancel()
			if peer, err := resolver.ResolveFromChatID(dctx, chatID); err == nil {
				_ = plugin.DeleteMessages(dctx, api, peer, id)
			}
		}()
	}
	return err
}

func errText(tl func(string, string) string, err error) string {
	if d, ok := tgerr.AsFloodWait(err); ok {
		return fmt.Sprintf(tl("请求过于频繁，请 %d 秒后重试", "Flood wait, retry in %d seconds"), int(d.Seconds())+1)
	}
	if e, ok := tgerr.As(err); ok {
		switch e.Type {
		case "REACTION_INVALID":
			return tl("该聊天不允许这个表情回应", "This chat does not allow that reaction")
		case "PREMIUM_ACCOUNT_REQUIRED":
			return tl("需要 Premium 会员", "Telegram Premium is required")
		case "CHAT_WRITE_FORBIDDEN":
			return tl("无权在此聊天回应", "No permission to react in this chat")
		}
		return e.Type
	}
	return err.Error()
}

func peerName(key int64, users []tg.UserClass, chats []tg.ChatClass) string {
	for _, u := range users {
		if v, ok := u.(*tg.User); ok && v.ID == key {
			name := strings.TrimSpace(v.FirstName + " " + v.LastName)
			if v.Username != "" {
				name = strings.TrimSpace(name + " @" + v.Username)
			}
			return name
		}
	}
	for _, c := range chats {
		switch v := c.(type) {
		case *tg.Channel:
			if plugin.ChannelChatID(v.ID) == key {
				return v.Title
			}
		case *tg.Chat:
			if -v.ID == key {
				return v.Title
			}
		}
	}
	return ""
}

func userLabel(key string, e *userEntry) string {
	name := ""
	if e != nil {
		name = e.Name
	}
	id, _ := strconv.ParseInt(key, 10, 64)
	idText := plugin.Code(key)
	if id > 0 {
		if name == "" {
			name = key
		}
		return plugin.Mention(name, id) + " " + idText
	}
	if name != "" {
		return plugin.Escape(name) + " " + idText
	}
	return idText
}

func (p *TracePlugin) traceUser(ctx *plugin.CommandContext) error {
	tl := ctx.Tlocal
	peer, err := ctx.ResolvePeer()
	if err != nil {
		return ctx.Edit("❌ " + tl("会话解析失败: ", "Failed to resolve chat: ") + plugin.Escape(err.Error()))
	}
	msgs, users, chats, err := plugin.GetMessages(ctx.Context(), ctx.API, peer, ctx.Message.ReplyToID)
	if err != nil || len(msgs) == 0 {
		return p.receipt(ctx, "❌ "+tl("无法获取被回复的消息", "Cannot fetch the replied message"), 5)
	}
	replied := msgs[0]
	key := senderKey(replied)
	if key == 0 {
		return p.receipt(ctx, "❌ "+tl("无法获取用户信息", "Cannot identify the sender"), 5)
	}
	full := ctx.Message.Message.Message
	start := argsStart(full, 1)
	reactions := parseReactions(full, max(start, 0), ctx.Message.Message.Entities, p.isPremium(ctx.Context(), ctx.API))
	if start < 0 || len(reactions) == 0 {
		return p.receipt(ctx, "❌ "+tl("未找到有效的表情，可用列表见 ", "No valid reaction found, see ")+plugin.Code("trace help"), 5)
	}
	k := strconv.FormatInt(key, 10)
	entry := &userEntry{Name: peerName(key, users, chats), Reactions: reactions}
	p.mu.Lock()
	p.db.Users[k] = entry
	err = p.saveLocked()
	n := len(p.db.Users)
	p.mu.Unlock()
	if err != nil {
		return ctx.Edit("❌ " + tl("保存失败: ", "Save failed: ") + plugin.Escape(err.Error()))
	}
	var b strings.Builder
	b.WriteString("✅ **" + tl("追踪成功", "Tracing") + "**\n")
	b.WriteString(tl("用户", "User") + "  " + userLabel(k, entry) + "\n")
	b.WriteString(tl("表情", "Reactions") + "  " + displayReactions(reactions) + "\n")
	if c := countCustom(reactions); c > 0 {
		b.WriteString(fmt.Sprintf(tl("表情数  %d（含 %d 个会员表情）", "Count  %d (%d custom)"), len(reactions), c) + "\n")
	}
	b.WriteString(fmt.Sprintf(tl("当前追踪  %d 个用户", "Tracing  %d users"), n))
	if err := p.sendReaction(ctx.Context(), peer, replied.ID, reactions); err != nil {
		b.WriteString("\n⚠️ " + tl("回应失败: ", "Reaction failed: ") + plugin.Escape(errText(tl, err)))
	}
	return p.receipt(ctx, b.String(), 10)
}

func (p *TracePlugin) untraceUser(ctx *plugin.CommandContext) error {
	tl := ctx.Tlocal
	peer, err := ctx.ResolvePeer()
	if err != nil {
		return ctx.Edit("❌ " + tl("会话解析失败: ", "Failed to resolve chat: ") + plugin.Escape(err.Error()))
	}
	msgs, _, _, err := plugin.GetMessages(ctx.Context(), ctx.API, peer, ctx.Message.ReplyToID)
	if err != nil || len(msgs) == 0 {
		return p.receipt(ctx, "❌ "+tl("无法获取被回复的消息", "Cannot fetch the replied message"), 5)
	}
	k := strconv.FormatInt(senderKey(msgs[0]), 10)
	p.mu.Lock()
	entry, ok := p.db.Users[k]
	if ok {
		delete(p.db.Users, k)
		err = p.saveLocked()
	}
	n := len(p.db.Users)
	p.mu.Unlock()
	if !ok {
		return p.receipt(ctx, "ℹ️ "+tl("该用户未被追踪", "That user is not traced"), 5)
	}
	if err != nil {
		return ctx.Edit("❌ " + tl("保存失败: ", "Save failed: ") + plugin.Escape(err.Error()))
	}
	return p.receipt(ctx, "🗑 **"+tl("取消追踪", "Untraced")+"**\n"+
		tl("用户", "User")+"  "+userLabel(k, entry)+"\n"+
		tl("原表情", "Reactions")+"  "+displayReactions(entry.Reactions)+"\n"+
		fmt.Sprintf(tl("剩余追踪  %d 个用户", "Still tracing  %d users"), n), 10)
}

func (p *TracePlugin) traceKeyword(ctx *plugin.CommandContext, word string) error {
	tl := ctx.Tlocal
	full := ctx.Message.Message.Message
	start := argsStart(full, 4)
	reactions := parseReactions(full, max(start, 0), ctx.Message.Message.Entities, p.isPremium(ctx.Context(), ctx.API))
	if start < 0 || len(reactions) == 0 {
		return p.receipt(ctx, "❌ "+tl("未找到有效的表情，可用列表见 ", "No valid reaction found, see ")+plugin.Code("trace help"), 5)
	}
	p.mu.Lock()
	_, update := p.db.Keywords[word]
	p.db.Keywords[word] = reactions
	err := p.saveLocked()
	n := len(p.db.Keywords)
	p.mu.Unlock()
	if err != nil {
		return ctx.Edit("❌ " + tl("保存失败: ", "Save failed: ") + plugin.Escape(err.Error()))
	}
	title := tl("已添加关键字追踪", "Keyword added")
	if update {
		title = tl("已更新关键字追踪", "Keyword updated")
	}
	return p.receipt(ctx, "✅ **"+title+"**\n"+
		tl("关键字", "Keyword")+"  "+plugin.Code(word)+"\n"+
		tl("表情", "Reactions")+"  "+displayReactions(reactions)+"\n"+
		fmt.Sprintf(tl("当前追踪  %d 个关键字", "Tracing  %d keywords"), n), 10)
}

func (p *TracePlugin) untraceKeyword(ctx *plugin.CommandContext, word string) error {
	tl := ctx.Tlocal
	p.mu.Lock()
	old, ok := p.db.Keywords[word]
	var err error
	if ok {
		delete(p.db.Keywords, word)
		err = p.saveLocked()
	}
	n := len(p.db.Keywords)
	p.mu.Unlock()
	if !ok {
		return p.receipt(ctx, "ℹ️ "+tl("关键字未被追踪: ", "Keyword not traced: ")+plugin.Code(word), 5)
	}
	if err != nil {
		return ctx.Edit("❌ " + tl("保存失败: ", "Save failed: ") + plugin.Escape(err.Error()))
	}
	return p.receipt(ctx, "🗑 **"+tl("已删除关键字追踪", "Keyword removed")+"**\n"+
		tl("关键字", "Keyword")+"  "+plugin.Code(word)+"\n"+
		tl("原表情", "Reactions")+"  "+displayReactions(old)+"\n"+
		fmt.Sprintf(tl("剩余追踪  %d 个关键字", "Still tracing  %d keywords"), n), 10)
}

func (p *TracePlugin) clean(ctx *plugin.CommandContext) error {
	tl := ctx.Tlocal
	p.mu.Lock()
	nu, nk := len(p.db.Users), len(p.db.Keywords)
	p.db.Users, p.db.Keywords = map[string]*userEntry{}, map[string][]string{}
	err := p.saveLocked()
	p.mu.Unlock()
	if err != nil {
		return ctx.Edit("❌ " + tl("保存失败: ", "Save failed: ") + plugin.Escape(err.Error()))
	}
	return p.receipt(ctx, "🗑 **"+tl("清理完成", "Cleared")+"**\n"+
		fmt.Sprintf(tl("清除用户  %d 个\n清除关键字  %d 个", "Users removed  %d\nKeywords removed  %d"), nu, nk), 10)
}

func (p *TracePlugin) statusText(tl func(string, string) string) string {
	p.mu.Lock()
	users := make(map[string]userEntry, len(p.db.Users))
	for k, v := range p.db.Users {
		users[k] = *v
	}
	kws := make(map[string][]string, len(p.db.Keywords))
	for k, v := range p.db.Keywords {
		kws[k] = v
	}
	p.mu.Unlock()
	var b strings.Builder
	b.WriteString("📊 **" + tl("追踪状态", "Trace status") + "**\n")
	b.WriteString(fmt.Sprintf(tl("用户 %d 个 · 关键字 %d 个", "%d users · %d keywords"), len(users), len(kws)) + "\n")
	b.WriteString("\n👤 **" + tl("追踪的用户", "Users") + "**\n")
	if len(users) == 0 {
		b.WriteString(tl("暂无", "none") + "\n")
	}
	for _, k := range sortedKeys(users) {
		e := users[k]
		b.WriteString("> " + userLabel(k, &e) + "\n> " + displayReactions(e.Reactions) + "\n")
	}
	b.WriteString("\n🔑 **" + tl("追踪的关键字", "Keywords") + "**\n")
	if len(kws) == 0 {
		b.WriteString(tl("暂无", "none") + "\n")
	}
	for _, k := range sortedKeys(kws) {
		b.WriteString("> " + plugin.Code(k) + "  " + displayReactions(kws[k]) + "\n")
	}
	if p.set != nil {
		onOff := func(v bool) string {
			if v {
				return "✅"
			}
			return "❌"
		}
		b.WriteString("\n⚙️ " + tl("保留回执 ", "Keep receipts ") + onOff(p.set.Bool("keep_log")) + " · " + tl("大号动画 ", "Big animation ") + onOff(p.set.Bool("big")))
	}
	return clipLines(strings.TrimRight(b.String(), "\n"), 3900)
}

// clipLines cuts s to at most n runes at a line boundary, so no Markdown
// span is split (a split span makes the whole message render raw).
func clipLines(s string, n int) string {
	if len([]rune(s)) <= n {
		return s
	}
	var b strings.Builder
	size := 0
	for _, l := range strings.Split(s, "\n") {
		ln := len([]rune(l)) + 1
		if size+ln > n-2 {
			break
		}
		b.WriteString(l + "\n")
		size += ln
	}
	return b.String() + "…"
}

// kwToken is a short stable id for a keyword in callback data, which is
// capped at 64 bytes and must not shift when the list changes.
func kwToken(k string) string {
	h := fnv.New32a()
	h.Write([]byte(k))
	return strconv.FormatUint(uint64(h.Sum32()), 36)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// page lists traces with delete buttons.
func (p *TracePlugin) page(c *plugin.BotContext) (*plugin.View, error) {
	tl := c.Tlocal
	if op, arg, ok := strings.Cut(c.Data, ":"); ok {
		p.mu.Lock()
		found := false
		switch op {
		case "u":
			_, found = p.db.Users[arg]
			delete(p.db.Users, arg)
		case "k":
			for k := range p.db.Keywords {
				if kwToken(k) == arg {
					delete(p.db.Keywords, k)
					found = true
				}
			}
		}
		var err error
		if found {
			err = p.saveLocked()
		}
		p.mu.Unlock()
		switch {
		case err != nil:
			c.Alert(tl("保存失败: ", "Save failed: ") + err.Error())
		case found:
			c.Toast(tl("已删除", "Deleted"))
		default:
			c.Toast(tl("已经不在列表里了", "Already gone"))
		}
	}
	v := &plugin.View{Text: p.statusText(tl)}
	p.mu.Lock()
	for _, k := range sortedKeys(p.db.Users) {
		name := p.db.Users[k].Name
		if name == "" {
			name = k
		}
		v.Buttons = append(v.Buttons, plugin.Row(plugin.Btn("🗑 👤 "+truncate(name, 24), "u:"+k).Danger()))
	}
	for _, k := range sortedKeys(p.db.Keywords) {
		v.Buttons = append(v.Buttons, plugin.Row(plugin.Btn("🗑 🔑 "+truncate(k, 24), "k:"+kwToken(k)).Danger()))
	}
	p.mu.Unlock()
	return v, nil
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
