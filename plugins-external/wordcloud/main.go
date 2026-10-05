// Plugin wordcloud: word cloud from recent chat history (ported from
// TeleBox cy). Generate on demand or on a schedule, rendered in pure Go.
package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

const (
	defaultLimit = 500
	minLimit     = 50
	maxLimit     = 2000

	historyPage  = 100
	fetchTimeout = 5 * time.Minute
	maxFloodWait = 2 * time.Minute
)

var Metadata = &plugin.PluginMetadata{
	Name:        "wordcloud",
	Description: "从群聊天记录生成词云，可定时推送",
	DescEN:      "Word cloud from chat history, schedulable",
	Version:     "1.0.0",
	Author:      "PaperValet",
	MinVersion:  "0.1.0",
}

type WordCloudPlugin struct {
	host   plugin.Host
	logger plugin.Logger
	set    plugin.Settings

	mu       sync.Mutex
	cancel   context.CancelFunc
	done     chan struct{}
	lastRuns map[string]bool // "2006-01-02 15:04" → succeeded
	running  map[string]bool // in-flight generation guard
	lastOrd  []string        // insertion order of lastRuns

	peerMu   sync.Mutex
	peerChat int64  // resolved chat id of the @username target
	peerName string // which @name the cache belongs to
}

func New() *WordCloudPlugin {
	return &WordCloudPlugin{lastRuns: map[string]bool{}, running: map[string]bool{}}
}

func (p *WordCloudPlugin) Name() string        { return "wordcloud" }
func (p *WordCloudPlugin) Description() string { return Metadata.Description }
func (p *WordCloudPlugin) DescEN() string      { return Metadata.DescEN }

func (p *WordCloudPlugin) Init(_ context.Context, mgr plugin.Manager) error {
	if mgr != nil && mgr.Host() != nil {
		p.host = mgr.Host()
		p.logger = p.host.Logger(p.Name())
		set, err := p.host.Settings(&plugin.SettingsSpec{
			Plugin:  p.Name(),
			Title:   "☁️ 词云",
			TitleEN: "☁️ Word cloud",
			Settings: []plugin.Setting{
				{Key: "schedule_enabled", Label: "启用定时", LabelEN: "Schedule enabled", Kind: plugin.SettingToggle, Default: false,
					Hint: "按设定时间自动生成并发送到目标", HintEN: "Auto-generate and send to the target at the set times"},
				{Key: "schedule_target", Label: "定时目标", LabelEN: "Schedule target", Kind: plugin.SettingText, Default: "",
					Hint: "@username 或数字 chat id", HintEN: "@username or numeric chat id", Validate: validTarget},
				{Key: "schedule_times", Label: "定时时间", LabelEN: "Schedule times", Kind: plugin.SettingText, Default: "",
					Hint: "逗号分隔 HH:MM，如 09:00,21:30，最多 12 个", HintEN: "HH:MM comma separated, e.g. 09:00,21:30, max 12", Validate: validTimes},
				{Key: "schedule_limit", Label: "统计条数", LabelEN: "Message limit", Kind: plugin.SettingNumber, Default: defaultLimit, Min: minLimit, Max: maxLimit,
					Hint: "定时生成时统计的最近消息数", HintEN: "Recent messages counted when scheduled"},
			},
		})
		if err != nil {
			return err
		}
		p.set = set
		if err := p.host.Bot(p.Name()).SetPage(&plugin.Page{
			Title: "词云", TitleEN: "Word cloud",
			Handle: p.page,
		}); err != nil && err != plugin.ErrBotNotReady {
			return err
		}
	}
	return mgr.RegisterCommand(&plugin.Command{
		Name:        "wordcloud",
		Description: "从群聊天记录生成词云，可定时推送",
		DescEN:      "Word cloud from chat history, schedulable",
		Usage:       "wordcloud [N] · wordcloud send [N] · wordcloud help",
		UsageEN:     "wordcloud [N] · wordcloud send [N] · wordcloud help",
		Plugin:      p.Name(),
		Category:    "fun",
		OwnerOnly:   true,
		Handler:     p.handle,
	})
}

// Start launches the scheduler goroutine: sleep until the next fire time,
// capped at one minute so clock jumps and panel edits are re-evaluated.
func (p *WordCloudPlugin) Start(_ context.Context) error {
	p.mu.Lock()
	if p.cancel != nil {
		p.mu.Unlock()
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel, p.done = cancel, make(chan struct{})
	done := p.done
	p.mu.Unlock()

	go p.loop(ctx, done)
	return nil
}

func (p *WordCloudPlugin) Stop(ctx context.Context) error {
	p.mu.Lock()
	cancel, done := p.cancel, p.done
	p.cancel, p.done = nil, nil
	p.mu.Unlock()
	if cancel != nil {
		cancel()
		select {
		case <-done:
		case <-ctx.Done():
		}
	}
	return nil
}

func (p *WordCloudPlugin) loop(ctx context.Context, done chan struct{}) {
	defer close(done)
	for {
		timer := time.NewTimer(p.nextDelay(time.Now()))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		p.tick(ctx)
	}
}

func (p *WordCloudPlugin) nextDelay(now time.Time) time.Duration {
	if p.set == nil || !p.set.Bool("schedule_enabled") {
		return schedTick
	}
	at, ok := nextFire(parseTimes(p.set.String("schedule_times")), now)
	if !ok {
		return schedTick
	}
	if d := at.Sub(now); d > 0 && d < schedTick {
		return d
	}
	return schedTick
}

// tick runs a due generation, deduped per day+time.
func (p *WordCloudPlugin) tick(ctx context.Context) {
	if p.set == nil || p.host == nil || !p.set.Bool("schedule_enabled") {
		return
	}
	target := strings.TrimSpace(p.set.String("schedule_target"))
	if target == "" {
		return
	}
	now := time.Now()
	cur := now.Format("15:04")
	due := false
	for _, t := range parseTimes(p.set.String("schedule_times")) {
		if t == cur {
			due = true
			break
		}
	}
	if !due {
		return
	}
	key := dayKey(now) + " " + cur
	p.mu.Lock()
	if p.lastRuns[key] || p.running[key] {
		p.mu.Unlock()
		return
	}
	p.running[key] = true
	p.mu.Unlock()

	limit := clampLimit(p.set.Int("schedule_limit"))
	err := p.generateAndSend(ctx, p.host.API(), p.host.PeerResolver(), target, limit, 0)
	p.mu.Lock()
	delete(p.running, key)
	// Only mark done on success so a failure retries on the next tick
	// (source adds lastRuns after the send succeeds).
	if err == nil {
		p.lastRuns[key] = true
		p.lastOrd = append(p.lastOrd, key)
		if len(p.lastOrd) > 200 {
			keep := p.lastOrd[len(p.lastOrd)-lastRunsKeep:]
			live := map[string]bool{}
			for _, k := range keep {
				live[k] = true
			}
			for k := range p.lastRuns {
				if !live[k] {
					delete(p.lastRuns, k)
				}
			}
			p.lastOrd = keep
		}
	}
	p.mu.Unlock()
	if err != nil && p.logger != nil {
		p.logger.Warn("wordcloud: scheduled generation failed", "error", err)
	}
}

// ---------------------------------------------------------------- history

// messageText mirrors cy.ts: sticker messages contribute nothing.
func messageText(m *tg.Message) string {
	if doc, ok := m.Media.(*tg.MessageMediaDocument); ok {
		if d, ok := doc.Document.(*tg.Document); ok {
			for _, a := range d.Attributes {
				if _, ok := a.(*tg.DocumentAttributeSticker); ok {
					return ""
				}
			}
		}
	}
	return strings.TrimSpace(m.Message)
}

// isCommand checks the text against every configured prefix.
func (p *WordCloudPlugin) isCommand(text string) bool {
	if p.host == nil || text == "" {
		return false
	}
	for _, pre := range p.host.Prefixes() {
		if strings.HasPrefix(text, pre) {
			return true
		}
	}
	return false
}

// fetchHistory walks the chat history newest→oldest collecting up to limit
// message texts (commands skipped).
func (p *WordCloudPlugin) fetchHistory(ctx context.Context, api *tg.Client, peer tg.InputPeerClass, limit int) ([]string, error) {
	var out []string
	offsetID := 0
	for len(out) < limit {
		pageSize := historyPage
		if r := limit - len(out); r < pageSize {
			pageSize = r
		}
		var page tg.MessagesMessagesClass
		err := retry(ctx, func() error {
			var err error
			page, err = api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
				Peer: peer, OffsetID: offsetID, Limit: pageSize,
			})
			return err
		})
		if err != nil {
			return nil, err
		}
		mod, ok := page.AsModified()
		if !ok {
			break
		}
		msgs := mod.GetMessages()
		if len(msgs) == 0 {
			break
		}
		next := 0
		for _, mc := range msgs {
			m, ok := mc.(*tg.Message)
			if !ok {
				continue
			}
			if next == 0 || m.ID < next {
				next = m.ID
			}
			text := messageText(m)
			if text == "" || p.isCommand(text) {
				continue
			}
			out = append(out, text)
			if len(out) >= limit {
				break
			}
		}
		if next == 0 || next >= offsetID && offsetID != 0 {
			break
		}
		offsetID = next
	}
	return out, nil
}

func retry(ctx context.Context, fn func() error) error {
	for {
		err := fn()
		d, ok := tgerr.AsFloodWait(err)
		if !ok || d > maxFloodWait {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d + time.Second):
		}
	}
}

// resolveTarget turns the settings target (@username or numeric id) into an
// input peer. Usernames are resolved once and the chat id cached.
func (p *WordCloudPlugin) resolveTarget(ctx context.Context, resolver plugin.PeerResolver, target string) (tg.InputPeerClass, error) {
	if resolver == nil {
		return nil, errors.New("no peer resolver")
	}
	if !strings.HasPrefix(target, "@") {
		id, err := strconv.ParseInt(target, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("bad target %q", target)
		}
		return resolver.ResolveFromChatID(ctx, id)
	}
	name := strings.TrimPrefix(target, "@")
	p.peerMu.Lock()
	cached, cachedName := p.peerChat, p.peerName
	p.peerMu.Unlock()
	if cached != 0 && cachedName == name {
		if peer, err := resolver.ResolveFromChatID(ctx, cached); err == nil {
			return peer, nil
		}
	}
	peer, err := resolver.ResolveUsername(ctx, name)
	if err != nil {
		return nil, err
	}
	if id := plugin.ChatIDOfInput(peer); id != 0 {
		p.peerMu.Lock()
		p.peerChat, p.peerName = id, name
		p.peerMu.Unlock()
	}
	return peer, nil
}

// generateAndSend builds the cloud for target and sends the PNG (replyTo 0 =
// plain message).
func (p *WordCloudPlugin) generateAndSend(ctx context.Context, api *tg.Client, resolver plugin.PeerResolver, target string, limit, replyTo int) error {
	if api == nil {
		return errors.New("API client not ready")
	}
	peer, err := p.resolveTarget(ctx, resolver, target)
	if err != nil {
		return fmt.Errorf("resolve target: %w", err)
	}
	fctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	texts, err := p.fetchHistory(fctx, api, peer, limit)
	if err != nil {
		return fmt.Errorf("fetch history: %w", err)
	}
	counts := newWordCounts()
	for _, t := range texts {
		collectWords(t, counts)
	}
	words := buildWordItems(counts)
	if len(words) == 0 {
		return errors.New("没有统计到足够的热词 / not enough hot words")
	}
	png, err := renderCloud(words, limit, len(texts))
	if err != nil {
		return fmt.Errorf("render: %w", err)
	}
	file, err := uploader.NewUploader(api).FromBytes(ctx, "wordcloud.png", png)
	if err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	req := &tg.MessagesSendMediaRequest{
		Peer:     peer,
		Media:    &tg.InputMediaUploadedPhoto{File: file},
		RandomID: rand.Int64(),
	}
	if replyTo > 0 {
		req.SetReplyTo(&tg.InputReplyToMessage{ReplyToMsgID: replyTo})
	}
	_, err = api.MessagesSendMedia(ctx, req)
	return err
}

// ---------------------------------------------------------------- command

func (p *WordCloudPlugin) handle(ctx *plugin.CommandContext) error {
	if ctx.Message == nil || ctx.Message.Message == nil {
		return plugin.ErrNoMessage
	}
	sub := strings.ToLower(ctx.GetArg(0))
	switch sub {
	case "help", "?":
		return ctx.Edit(helpText(ctx))
	case "send":
		return p.cmdSend(ctx)
	}
	limit := defaultLimit
	if sub != "" {
		limit = parseLimitArg(sub)
	}
	return p.cmdNow(ctx, limit)
}

// cmdNow generates for the current chat. Replying to a message replies the
// image to that message; the command message is deleted afterwards.
func (p *WordCloudPlugin) cmdNow(ctx *plugin.CommandContext, limit int) error {
	tl := ctx.Tlocal
	_ = ctx.Edit(fmt.Sprintf(tl("⏳ 正在统计最近 %d 条消息…", "⏳ Counting the last %d messages…"), limit))
	replyTo := ctx.Message.Message.ID
	if ctx.Message.IsReply && ctx.Message.ReplyToID > 0 {
		replyTo = ctx.Message.ReplyToID
	}
	err := p.generateAndSend(ctx.Context(), ctx.API, ctx.PeerResolver, strconv.FormatInt(ctx.Message.ChatID, 10), limit, replyTo)
	if err != nil {
		return ctx.Edit("❌ " + plugin.Escape(err.Error()))
	}
	_ = ctx.Delete()
	return nil
}

// cmdSend generates for the configured schedule target and sends it there.
func (p *WordCloudPlugin) cmdSend(ctx *plugin.CommandContext) error {
	tl := ctx.Tlocal
	if p.set == nil {
		return ctx.Edit("❌ " + tl("设置不可用", "settings unavailable"))
	}
	target := strings.TrimSpace(p.set.String("schedule_target"))
	if target == "" {
		return ctx.Edit("❌ " + tl("请先在机器人面板设置定时目标 (schedule_target)", "Set the schedule target in the bot panel first"))
	}
	limit := clampLimit(p.set.Int("schedule_limit"))
	if a := ctx.GetArg(1); a != "" {
		limit = parseLimitArg(a)
	}
	_ = ctx.Edit(fmt.Sprintf(tl("⏳ 正在为 %s 生成词云…", "⏳ Generating the cloud for %s…"), plugin.Code(target)))
	if err := p.generateAndSend(ctx.Context(), ctx.API, ctx.PeerResolver, target, limit, 0); err != nil {
		return ctx.Edit("❌ " + plugin.Escape(err.Error()))
	}
	return ctx.Edit("✅ " + tl("词云已发送", "Cloud sent"))
}

func helpText(ctx *plugin.CommandContext) string {
	tl := ctx.Tlocal
	var b strings.Builder
	b.WriteString("☁️ **" + tl("词云", "Word Cloud") + "**\n\n")
	b.WriteString(plugin.Code("wordcloud [N]") + "  " + tl("为本群最近 N 条消息生成词云（默认 500，50–2000）", "cloud from this chat's last N messages (default 500, 50–2000)") + "\n")
	b.WriteString(plugin.Code("wordcloud send [N]") + "  " + tl("立即为定时目标生成并发送", "generate for the schedule target and send now") + "\n")
	b.WriteString(plugin.Code("wordcloud help") + "  " + tl("查看帮助", "show help") + "\n\n")
	b.WriteString("💡 " + tl(
		"回复一条消息使用 `wordcloud` 会把词云回复到那条消息；定时开关、目标、时间在机器人面板里设置",
		"Reply to a message to attach the cloud to it; schedule toggle, target and times live in the bot panel"))
	return b.String()
}

// ---------------------------------------------------------------- bot page

func (p *WordCloudPlugin) page(c *plugin.BotContext) (*plugin.View, error) {
	tl := c.Tlocal
	if c.Data == "run" {
		c.Toast(tl("⏳ 正在生成…", "⏳ Generating…"))
		go p.pageRun()
	}
	var b strings.Builder
	b.WriteString("☁️ **" + tl("词云定时", "Word Cloud Schedule") + "**\n\n")
	if p.set == nil {
		b.WriteString("❌ " + tl("设置不可用", "settings unavailable"))
		return &plugin.View{Text: b.String()}, nil
	}
	enabled := p.set.Bool("schedule_enabled")
	target := strings.TrimSpace(p.set.String("schedule_target"))
	times := parseTimes(p.set.String("schedule_times"))
	limit := clampLimit(p.set.Int("schedule_limit"))

	status := tl("🔴 关闭", "🔴 off")
	if enabled {
		status = tl("🟢 开启", "🟢 on")
	}
	b.WriteString(tl("状态", "Status") + "  " + status + "\n")
	if target != "" {
		b.WriteString(tl("目标", "Target") + "  " + plugin.Code(target) + "\n")
	} else {
		b.WriteString(tl("目标", "Target") + "  " + tl("未设置", "not set") + "\n")
	}
	if len(times) > 0 {
		sorted := append([]string(nil), times...)
		sort.Strings(sorted)
		b.WriteString(tl("时间", "Times") + "  " + plugin.Code(strings.Join(sorted, ", ")) + "\n")
	} else {
		b.WriteString(tl("时间", "Times") + "  " + tl("未设置", "not set") + "\n")
	}
	b.WriteString(tl("条数", "Limit") + "  " + plugin.Code(limit) + "\n")
	if enabled && len(times) > 0 {
		if at, ok := nextFire(times, time.Now()); ok {
			b.WriteString(tl("下次", "Next") + "  " + plugin.Code(at.Format("2006-01-02 15:04")) + "\n")
		}
	}
	return &plugin.View{
		Text: b.String(),
		Buttons: [][]plugin.Button{
			plugin.Row(plugin.Btn(tl("🖼 立即生成", "🖼 Generate now"), "run").Primary()),
		},
	}, nil
}

// pageRun generates for the schedule target and notifies the owner.
func (p *WordCloudPlugin) pageRun() {
	if p.set == nil || p.host == nil {
		return
	}
	tl := p.tlOwner
	target := strings.TrimSpace(p.set.String("schedule_target"))
	if target == "" {
		p.notifyOwner(tl("❌ 请先设置定时目标", "❌ Set the schedule target first"))
		return
	}
	limit := clampLimit(p.set.Int("schedule_limit"))
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()
	if err := p.generateAndSend(ctx, p.host.API(), p.host.PeerResolver(), target, limit, 0); err != nil {
		if p.logger != nil {
			p.logger.Warn("wordcloud: manual page run failed", "error", err)
		}
		p.notifyOwner("❌ " + plugin.Escape(err.Error()))
		return
	}
	p.notifyOwner(tl("✅ 词云已发送", "✅ Cloud sent"))
}

// tlOwner picks a string by the owner's global language.
func (p *WordCloudPlugin) tlOwner(zh, en string) string {
	if p.host != nil {
		if id := p.host.SelfID(); id != 0 && p.host.Lang(id) == "en-US" {
			return en
		}
	}
	return zh
}

func (p *WordCloudPlugin) notifyOwner(text string) {
	if p.host == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, _ = p.host.Bot(p.Name()).Notify(ctx, &plugin.View{Text: text})
}
