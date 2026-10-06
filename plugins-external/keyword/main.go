package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

const (
	tasksFile = "tasks.json"
	sendWait  = 30 * time.Second
)

var Metadata = &plugin.PluginMetadata{
	Name:        "keyword",
	Description: "关键词自动回复，支持正则、精确/包含匹配、冷却与继承",
	DescEN:      "Auto-reply to keywords with regex/exact/contains, cooldowns and inheritance",
	Version:     "1.0.0",
	Author:      "PaperValet",
	MinVersion:  "0.1.0",
}

type KeywordPlugin struct {
	mu   sync.Mutex
	dir  string
	host plugin.Host
	log  plugin.Logger
	set  plugin.Settings

	tasks  []*task
	alias  map[int64]int64 // chat -> chat whose rules it also applies
	nextID int
	cool   map[int]time.Time // task id -> earliest next fire (cooldown)
	// inFlight dedupes concurrent fires of one task while its send runs.
	inFlight map[int]bool

	stopListen func()
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
}

func New() *KeywordPlugin { return &KeywordPlugin{} }

func (p *KeywordPlugin) Name() string        { return "keyword" }
func (p *KeywordPlugin) Description() string { return Metadata.Description }
func (p *KeywordPlugin) DescEN() string      { return Metadata.DescEN }

func (p *KeywordPlugin) Init(_ context.Context, mgr plugin.Manager) error {
	p.host = mgr.Host()
	p.log = p.host.Logger(p.Name())
	dir, err := p.host.DataDir(p.Name())
	if err != nil {
		return err
	}
	p.dir = dir
	set, err := p.host.Settings(&plugin.SettingsSpec{
		Plugin: p.Name(), Title: "🔑 关键词回复", TitleEN: "🔑 Keyword replies",
		Settings: []plugin.Setting{
			{Key: "skip_commands", Label: "忽略命令消息", LabelEN: "Skip command messages", Kind: plugin.SettingToggle, Default: true,
				Hint: "不回应以命令前缀开头的消息", HintEN: "Do not react to messages starting with a command prefix"},
			{Key: "process_edits", Label: "处理编辑后的消息", LabelEN: "React to edited messages", Kind: plugin.SettingToggle, Default: false,
				Hint: "开启后消息被编辑也会触发关键词", HintEN: "Also fire when a message is edited"},
		},
	})
	if err != nil {
		return err
	}
	p.set = set
	if err := p.host.Bot(p.Name()).SetPage(&plugin.Page{
		Title: "关键词任务", TitleEN: "Keyword tasks", Handle: p.page,
	}); err != nil && err != plugin.ErrBotNotReady {
		return err
	}
	return mgr.RegisterCommand(&plugin.Command{
		Name:        "keyword",
		Description: "关键词自动回复：匹配关键词/正则后回复，支持删除、封禁、延迟删除、冷却与群继承",
		DescEN:      "Auto-reply to keywords/regex, with delete, ban, delayed delete, cooldown and group inheritance",
		Usage:       "keyword <词>\\n+++\\n<回复>\\n+++\\n[匹配选项]\\n+++\\n[动作] · keyword list [all] · keyword rm <ID,…> · keyword alias [群ID|rm] · keyword help",
		UsageEN:     "keyword <word>\\n+++\\n<reply>\\n+++\\n[match options]\\n+++\\n[actions] · keyword list [all] · keyword rm <ID,…> · keyword alias [groupID|rm] · keyword help",
		Plugin:      p.Name(),
		Category:    "tools",
		OwnerOnly:   true,
		Handler:     p.handle,
	})
}

func (p *KeywordPlugin) Start(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel != nil {
		return nil
	}
	if err := p.load(filepath.Join(p.dir, tasksFile)); err != nil {
		return fmt.Errorf("keyword: load: %w", err)
	}
	p.cool, p.inFlight = map[int]time.Time{}, map[int]bool{}
	p.ctx, p.cancel = context.WithCancel(context.Background())
	p.stopListen = p.host.Listen(p.Name(), p.onMessage)
	return nil
}

// load reads tasks.json into memory; caller holds p.mu.
func (p *KeywordPlugin) load(path string) error {
	var store struct {
		NextID int              `json:"next_id"`
		Tasks  []*task          `json:"tasks"`
		Alias  map[string]int64 `json:"alias"`
	}
	if err := readJSON(path, &store); err != nil {
		return err
	}
	p.tasks = p.tasks[:0]
	maxID := 0
	for _, t := range store.Tasks {
		if t == nil || t.ID <= 0 || t.Key == "" || t.Msg == "" {
			continue
		}
		if t.Regexp && t.compiled() == nil {
			// Broken regexp from an older save: never matches; keep it
			// listed (the owner can fix or remove it) but it stays cold.
			t.re = nil
		}
		p.tasks = append(p.tasks, t)
		if t.ID > maxID {
			maxID = t.ID
		}
	}
	p.nextID = store.NextID
	if p.nextID <= maxID {
		p.nextID = maxID + 1
	}
	p.alias = map[int64]int64{}
	for k, v := range store.Alias {
		var from int64
		fmt.Sscanf(k, "%d", &from)
		if from != 0 && v != 0 {
			p.alias[from] = v
		}
	}
	return nil
}

func (p *KeywordPlugin) Stop(context.Context) error {
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

func (p *KeywordPlugin) saveLocked() error {
	store := struct {
		NextID int              `json:"next_id"`
		Tasks  []*task          `json:"tasks"`
		Alias  map[string]int64 `json:"alias"`
	}{NextID: p.nextID, Tasks: p.tasks, Alias: map[string]int64{}}
	for k, v := range p.alias {
		store.Alias[fmt.Sprintf("%d", k)] = v
	}
	return writeJSON(filepath.Join(p.dir, tasksFile), store)
}

func (p *KeywordPlugin) lifetime() context.Context {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ctx == nil {
		return context.Background()
	}
	return p.ctx
}

// ---------------------------------------------------------------- listener

// onMessage runs on the update path: it only snapshots matching work and
// hands it to a goroutine, so the update pipeline never blocks on sends.
func (p *KeywordPlugin) onMessage(_ context.Context, ev *plugin.MessageEvent, edited bool) {
	if ev == nil || ev.Message == nil {
		return
	}
	if edited && (p.set == nil || !p.set.Bool("process_edits")) {
		return
	}
	if ev.IsOut || ev.Text == "" {
		return
	}
	if p.set != nil && p.set.Bool("skip_commands") && p.host != nil && len(p.host.Prefixes()) > 0 {
		for _, pref := range p.host.Prefixes() {
			if strings.HasPrefix(ev.Text, pref) {
				return
			}
		}
	}

	p.mu.Lock()
	if p.cancel == nil { // stopped
		p.mu.Unlock()
		return
	}
	chatID := ev.ChatID
	ids := p.matchingLocked(ev)
	ctx := p.ctx
	p.mu.Unlock()
	if len(ids) == 0 {
		return
	}

	snapshot := make([]task, len(ids))
	p.mu.Lock() // task pointers are stable; copy values for the goroutine
	for i, id := range ids {
		if t := p.byIDLocked(id); t != nil {
			snapshot[i] = *t
		}
	}
	p.mu.Unlock()

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		for i := range snapshot {
			p.fire(ctx, chatID, &snapshot[i], ev)
		}
	}()
}

// matchingLocked returns the ids of tasks that match now, in list order
// (priority), honouring cooldowns. Must hold p.mu.
func (p *KeywordPlugin) matchingLocked(ev *plugin.MessageEvent) []int {
	text := messageText(ev)
	var ids []int
	now := time.Now()
	for _, chat := range p.chatsLocked(ev.ChatID) {
		for _, t := range p.tasksForChatLocked(chat) {
			if p.inFlight[t.ID] {
				continue
			}
			if at, ok := p.cool[t.ID]; ok && now.Before(at) {
				continue
			}
			if t.IgnoreForward && isForwarded(ev.Message) {
				continue
			}
			if t.matches(text) {
				if t.Cooldown > 0 {
					p.cool[t.ID] = now.Add(time.Duration(t.Cooldown) * time.Second)
				}
				ids = append(ids, t.ID)
			}
		}
	}
	return ids
}

// chatsLocked returns the chat ids whose rules apply: the chat itself plus
// its alias target. Must hold p.mu.
func (p *KeywordPlugin) chatsLocked(chatID int64) []int64 {
	chats := []int64{chatID}
	if to, ok := p.alias[chatID]; ok {
		chats = append(chats, to)
	}
	return chats
}

func (p *KeywordPlugin) tasksForChatLocked(chatID int64) []*task {
	var out []*task
	for _, t := range p.tasks {
		if t.ChatID == chatID {
			out = append(out, t)
		}
	}
	return out
}

func (p *KeywordPlugin) byIDLocked(id int) *task {
	for _, t := range p.tasks {
		if t.ID == id {
			return t
		}
	}
	return nil
}

// messageText returns the message body. In gotd, tg.Message.Message holds
// the caption for media messages too, so ev.Text covers both (the source's
// getMessageText did the same through mtcute's .text).
func messageText(ev *plugin.MessageEvent) string {
	if ev == nil {
		return ""
	}
	return ev.Text
}

func isForwarded(m *tg.Message) bool {
	return m != nil && m.FwdFrom != tg.MessageFwdHeader{}
}

// senderName digs a display name out of the update's users list.
func senderName(ev *plugin.MessageEvent, userID int64) string {
	if ev == nil || userID == 0 {
		return ""
	}
	for _, u := range usersFromUpdate(ev.Update) {
		if user, ok := u.(*tg.User); ok && user.ID == userID {
			name := strings.TrimSpace(user.FirstName + " " + user.LastName)
			if name == "" && user.Username != "" {
				name = "@" + user.Username
			}
			return name
		}
	}
	return ""
}

// usersFromUpdate pulls the users carried by a raw update so names and
// access hashes are available without extra RPCs.
func usersFromUpdate(u tg.UpdatesClass) []tg.UserClass {
	switch v := u.(type) {
	case *tg.Updates:
		return v.Users
	case *tg.UpdatesCombined:
		return v.Users
	case *tg.UpdateShortSentMessage:
		return nil
	}
	return nil
}

// ---------------------------------------------------------------- firing

// fire sends one task's reply and applies its actions for the trigger
// message ev.
func (p *KeywordPlugin) fire(ctx context.Context, chatID int64, t *task, ev *plugin.MessageEvent) {
	p.mu.Lock()
	if p.inFlight == nil {
		p.inFlight = map[int]bool{}
	}
	p.inFlight[t.ID] = true
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.inFlight, t.ID)
		p.mu.Unlock()
	}()

	sctx, cancel := context.WithTimeout(ctx, sendWait)
	defer cancel()

	userID := plugin.SenderID(ev.Message)
	text := t.render(userID, senderName(ev, userID))
	replyTo := 0
	if t.Reply {
		replyTo = ev.Message.ID
	}
	sentID, err := p.host.Send(sctx, chatID, text, replyTo)
	if err != nil {
		if p.log != nil {
			p.log.Warn("keyword: send reply failed", "task", t.ID, "error", err)
		}
		// The cooldown was taken when the task matched; a failed send
		// should not burn it (the next matching message retries).
		if t.Cooldown > 0 {
			p.mu.Lock()
			delete(p.cool, t.ID)
			p.mu.Unlock()
		}
	}

	if t.Delete || t.SourceDelayDelete > 0 {
		p.deleteLater(sctx, chatID, ev.Message.ID, t.SourceDelayDelete)
	}
	if t.DelayDelete > 0 && sentID != 0 {
		p.deleteLater(sctx, chatID, sentID, t.DelayDelete)
	}
	if t.Ban > 0 || t.Restrict > 0 {
		p.moderate(sctx, chatID, ev, t)
	}
}

// moderate applies the ban/restrict action to the sender (supergroups only,
// via channels.editBanned; needs admin rights).
func (p *KeywordPlugin) moderate(ctx context.Context, chatID int64, ev *plugin.MessageEvent, t *task) {
	userID := plugin.SenderID(ev.Message)
	if userID == 0 {
		return
	}
	peer, err := p.host.PeerResolver().ResolveFromChatID(ctx, chatID)
	if err != nil {
		if p.log != nil {
			p.log.Warn("keyword: resolve chat failed", "chat", chatID, "error", err)
		}
		return
	}
	ch, ok := peer.(*tg.InputPeerChannel)
	if !ok {
		if p.log != nil {
			p.log.Debug("keyword: ban/restrict skipped, not a supergroup", "chat", chatID)
		}
		return
	}
	var rights tg.ChatBannedRights
	now := time.Now()
	switch {
	case t.Ban > 0:
		rights = tg.ChatBannedRights{
			ViewMessages: true,
			UntilDate:    int(now.Add(time.Duration(t.Ban) * time.Second).Unix()),
		}
	case t.Restrict > 0:
		rights = fullMuteRights(int(now.Add(time.Duration(t.Restrict) * time.Second).Unix()))
	}
	_, err = p.host.API().ChannelsEditBanned(ctx, &tg.ChannelsEditBannedRequest{
		Channel:      &tg.InputChannel{ChannelID: ch.ChannelID, AccessHash: ch.AccessHash},
		Participant:  &tg.InputPeerUser{UserID: userID},
		BannedRights: rights,
	})
	if err != nil {
		if p.log != nil {
			p.log.Warn("keyword: ban/restrict failed", "chat", chatID, "user", userID, "error", err)
		}
		// Rights problems (admin target, missing rights) must not stay
		// silent: the reply already went out, so warn the chat once.
		if tgerr.Is(err, "USER_ADMIN_INVALID", "CHAT_ADMIN_REQUIRED", "PARTICIPANT_ID_INVALID") {
			verb := "restrict"
			if t.Ban > 0 {
				verb = "ban"
			}
			wctx, wcancel := context.WithTimeout(context.Background(), sendWait)
			defer wcancel()
			_, _ = p.host.Send(wctx, chatID, "⚠️ "+verb+" failed: "+plugin.Escape(err.Error()), 0)
		}
	}
}

// fullMuteRights revokes every send right until the given time.
func fullMuteRights(until int) tg.ChatBannedRights {
	return tg.ChatBannedRights{
		SendMessages: true, SendMedia: true, SendStickers: true, SendGifs: true,
		SendGames: true, SendInline: true, EmbedLinks: true, SendPolls: true,
		SendPhotos: true, SendVideos: true, SendRoundvideos: true, SendAudios: true,
		SendVoices: true, SendDocs: true, SendPlain: true, UntilDate: until,
	}
}

// deleteLater removes msgID right away, or after delay seconds.
func (p *KeywordPlugin) deleteLater(ctx context.Context, chatID int64, msgID int, delay int) {
	if delay > 0 {
		life := p.lifetime()
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			timer := time.NewTimer(time.Duration(delay) * time.Second)
			defer timer.Stop()
			select {
			case <-life.Done():
				return
			case <-timer.C:
			}
			dctx, cancel := context.WithTimeout(context.Background(), sendWait)
			defer cancel()
			p.deleteMessages(dctx, chatID, msgID)
		}()
		return
	}
	p.deleteMessages(ctx, chatID, msgID)
}

func (p *KeywordPlugin) deleteMessages(ctx context.Context, chatID int64, ids ...int) {
	if p.host == nil {
		return
	}
	peer, err := p.host.PeerResolver().ResolveFromChatID(ctx, chatID)
	if err != nil {
		if p.log != nil {
			p.log.Warn("keyword: resolve chat failed", "chat", chatID, "error", err)
		}
		return
	}
	if err := plugin.DeleteMessages(ctx, p.host.API(), peer, ids...); err != nil && p.log != nil {
		p.log.Warn("keyword: delete failed", "chat", chatID, "error", err)
	}
}
