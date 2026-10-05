package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

const (
	progressEvery   = 3 * time.Second
	resultTTL       = 10 * time.Second // most results self-delete
	memberResultTTL = 15 * time.Second // member summaries live a little longer
	maxFloodWait    = time.Minute      // retry through flood waits up to this
)

var Metadata = &plugin.PluginMetadata{
	Name:        "clean",
	Description: "批量清理消息/成员消息/贴纸状态，支持确认",
	DescEN:      "Bulk-clean messages, a member's messages or sticker state, with confirm",
	Version:     "1.0.0",
	Author:      "PaperValet",
	MinVersion:  "0.1.0",
}

// CleanPlugin merges the TeleBox clean, clean_member and clear_sticker
// plugins behind one `clean` command.
type CleanPlugin struct {
	mu      sync.Mutex
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	host    plugin.Host
	log     plugin.Logger
	running map[int64]bool // one background job per chat
}

func New() *CleanPlugin { return &CleanPlugin{running: map[int64]bool{}} }

func (p *CleanPlugin) Name() string        { return "clean" }
func (p *CleanPlugin) Description() string { return Metadata.Description }
func (p *CleanPlugin) DescEN() string      { return Metadata.DescEN }

func (p *CleanPlugin) Init(_ context.Context, mgr plugin.Manager) error {
	p.host = mgr.Host()
	p.log = mgr.Host().Logger("clean")
	return mgr.RegisterCommand(&plugin.Command{
		Name:        "clean",
		Description: "批量清理消息、群成员或贴纸消息，支持确认",
		DescEN:      "Bulk-clean messages, group members or sticker messages, with confirm",
		Usage: "clean deleted pm [rm] · clean deleted member [rm] · clean blocked pm [all] · " +
			"clean blocked member [all] · clean member <模式 mode> … · clean sticker [数量 count]",
		UsageEN: "clean deleted pm [rm] · clean deleted member [rm] · clean blocked pm [all] · " +
			"clean blocked member [all] · clean member <mode> … · clean sticker [count]",
		Plugin:    p.Name(),
		Category:  "group",
		OwnerOnly: true,
		Handler:   p.handle,
	})
}

func (p *CleanPlugin) Start(context.Context) error {
	p.lifetime()
	return nil
}

func (p *CleanPlugin) Stop(context.Context) error {
	p.mu.Lock()
	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
	}
	p.mu.Unlock()
	p.wg.Wait()
	return nil
}

// lifetime returns the plugin-scoped context background jobs run on.
func (p *CleanPlugin) lifetime() context.Context {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel == nil {
		p.ctx, p.cancel = context.WithCancel(context.Background())
	}
	return p.ctx
}

func (p *CleanPlugin) claim(chat int64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.running[chat] {
		return false
	}
	p.running[chat] = true
	return true
}

func (p *CleanPlugin) release(chat int64) {
	p.mu.Lock()
	delete(p.running, chat)
	p.mu.Unlock()
}

func (p *CleanPlugin) handle(ctx *plugin.CommandContext) error {
	if ctx.Message == nil || ctx.Message.Message == nil || ctx.API == nil {
		return plugin.ErrNoMessage
	}
	if len(ctx.Args) == 0 || strings.EqualFold(ctx.Args[0], "help") {
		return ctx.Edit(helpText(ctx.Tlocal))
	}
	sub := strings.ToLower(ctx.Args[0])
	rest := ctx.Args[1:]
	switch sub {
	case "deleted":
		return p.handleDeleted(ctx, rest)
	case "blocked":
		return p.handleBlocked(ctx, rest)
	case "member":
		return p.handleMember(ctx, rest)
	case "sticker":
		return p.handleSticker(ctx, rest)
	}
	return p.deny(ctx, "❌ "+ctx.Tlocal("未知子命令：", "Unknown subcommand: ")+plugin.Code(sub)+"\n\n"+
		ctx.Tlocal("可用：", "Available: ")+plugin.Code("deleted")+" / "+plugin.Code("blocked")+" / "+
		plugin.Code("member")+" / "+plugin.Code("sticker"))
}

// deny shows an error in the command message and deletes it soon after.
func (p *CleanPlugin) deny(ctx *plugin.CommandContext, text string) error {
	if err := ctx.Edit(text); err != nil {
		return err
	}
	p.autoDelete(ctx, resultTTL)
	return nil
}

// finish shows a job's final message and deletes it after ttl.
func (p *CleanPlugin) finish(ctx *plugin.CommandContext, text string, ttl time.Duration) {
	_ = ctx.Edit(text)
	p.autoDelete(ctx, ttl)
}

// finishErr renders a job's fatal error and deletes it after resultTTL.
func (p *CleanPlugin) finishErr(ctx *plugin.CommandContext, err error) {
	if ctx.Context().Err() != nil {
		return // plugin stopped; keep the last progress message
	}
	p.finish(ctx, "❌ "+errText(ctx.Tlocal, err), resultTTL)
}

// job is a background clean: the command context bound to the plugin
// lifetime, with the chat peer it works on and progress throttling.
type job struct {
	*plugin.CommandContext
	peer tg.InputPeerClass
	last time.Time // last progress edit
}

// progress edits the status message at most every progressEvery.
func (j *job) progress(text string) {
	if !j.last.IsZero() && time.Since(j.last) < progressEvery {
		return
	}
	j.last = time.Now()
	_ = j.Edit(text)
}

// run claims the chat and starts body in the background; it reports false
// when another clean already runs there.
func (p *CleanPlugin) run(ctx *plugin.CommandContext, body func(*job)) (bool, error) {
	peer, err := ctx.ResolvePeer()
	if err != nil {
		return false, err
	}
	chat := ctx.Message.ChatID
	if !p.claim(chat) {
		return false, nil
	}
	rc := *ctx
	rc.Ctx = p.lifetime()
	j := &job{CommandContext: &rc, peer: peer}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer p.release(chat)
		body(j)
	}()
	return true, nil
}

// autoDelete removes the command message after d on the lifetime context.
func (p *CleanPlugin) autoDelete(ctx *plugin.CommandContext, d time.Duration) {
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		select {
		case <-p.lifetime().Done():
			return
		case <-time.After(d):
		}
		dctx, cancel := context.WithTimeout(p.lifetime(), 15*time.Second)
		defer cancel()
		del := *ctx
		del.Ctx = dctx
		if err := del.Delete(); err != nil && ctx.Logger != nil &&
			!tgerr.Is(err, "MESSAGE_ID_INVALID", "MESSAGE_DELETE_FORBIDDEN") {
			ctx.Logger.Warn("clean: delete result failed", "error", err)
		}
	}()
}

// retry runs fn, sleeping through flood waits up to maxFloodWait.
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

// fatalErr reports errors that mean the rights or the chat are gone, so
// going on would only fail again.
func fatalErr(err error) bool {
	if _, ok := tgerr.AsFloodWait(err); ok {
		return true
	}
	return tgerr.Is(err, "CHAT_ADMIN_REQUIRED", "CHANNEL_PRIVATE", "CHAT_FORBIDDEN",
		"PEER_ID_INVALID", "CHANNEL_INVALID", "CHAT_ID_INVALID")
}

func atoi(s string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	return n, err == nil
}

func helpText(tl func(string, string) string) string {
	return "🧹 **" + tl("清理工具", "Cleaning tools") + "**\n\n" +
		"> " + plugin.Code("clean deleted pm [rm]") + " " + tl("扫描 / 移除已注销账号的私聊", "scan / remove deleted-account private chats") + "\n" +
		"> " + plugin.Code("clean deleted member [rm]") + " " + tl("扫描 / 清理群里的已注销账号", "scan / clean deleted accounts in this group") + "\n" +
		"> " + plugin.Code("clean blocked pm [all]") + " " + tl("清理拉黑名单（智能 / 全量）", "clear the blocklist (smart / all)") + "\n" +
		"> " + plugin.Code("clean blocked member [all]") + " " + tl("解封群里的封禁实体", "unban banned entities in this group") + "\n" +
		"> " + plugin.Code("clean member <1-5> …") + " " + tl("按未上线 / 未发言等模式清理群成员", "clean members by inactivity and other modes") + "\n" +
		"> " + plugin.Code("clean sticker [数量 count]") + " " + tl("清理群里的贴纸消息", "clear sticker messages in this group") + "\n\n" +
		tl("破坏性操作需要确认参数：先扫描预览，再加 `rm` / `all` / `confirm` 执行。发送 ", "Destructive steps need a confirm argument: scan first, then add `rm` / `all` / `confirm` to execute. Send ") +
		plugin.Code("clean member help") + tl(" 查看成员清理的详细用法。", " for the member-clean details.")
}

// errText turns an API error into a bilingual line.
func errText(tl func(string, string) string, err error) string {
	if err == nil {
		return ""
	}
	if d, ok := tgerr.AsFloodWait(err); ok {
		return fmt.Sprintf(tl("请求过于频繁，请 %d 秒后重试", "Flood wait, retry in %d seconds"), int(d.Seconds())+1)
	}
	var e *tgerr.Error
	if errors.As(err, &e) {
		switch e.Type {
		case "CHAT_ADMIN_REQUIRED", "RIGHT_FORBIDDEN", "CHAT_WRITE_FORBIDDEN":
			return tl("需要管理员权限", "Admin rights are needed")
		case "USER_NOT_PARTICIPANT", "PARTICIPANT_ID_INVALID":
			return tl("对方已不在群里", "They are no longer in the group")
		case "CHANNEL_PRIVATE", "CHAT_FORBIDDEN", "CHANNEL_INVALID", "CHAT_ID_INVALID":
			return tl("无法访问该群组", "Cannot access this group")
		case "USER_ID_INVALID", "PEER_ID_INVALID", "INPUT_USER_DEACTIVATED":
			return tl("找不到这个用户", "Cannot find that user")
		case "SEARCH_QUERY_EMPTY":
			return tl("搜索参数无效", "Invalid search parameters")
		}
		return plugin.Code(e.Type)
	}
	return plugin.Escape(err.Error())
}
