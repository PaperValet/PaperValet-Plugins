package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

// Sentinel errors that abort the whole run (wrapped with the cause).
var (
	errFloodAbort = errors.New("flood")
	errRestricted = errors.New("forwards restricted")
)

// permissionErrs mark per-target failures: this target is skipped, the run
// continues with the next one.
var permissionErrs = []string{
	"CHAT_WRITE_FORBIDDEN",
	"USER_BANNED_IN_CHANNEL",
	"CHAT_ADMIN_REQUIRED",
	"CHANNEL_PRIVATE",
	"CHAT_SEND_GIFS_DISABLED",
	"PEER_FLOOD",
	"USER_IS_BLOCKED",
	"CHAT_SEND_PLAIN_FORBIDDEN",
}

// collectIDs picks up to want message ids starting at startID, skipping
// deleted/missing ones. The search spans min(want*3, maxSpan) ids ahead.
// fetch receives ascending batches and returns the set of ids that exist.
func collectIDs(startID, want int, fetch func(ids []int) (map[int]bool, error)) ([]int, error) {
	if startID <= 0 || want <= 0 {
		return nil, nil
	}
	span := want * 3
	if span > maxSpan {
		span = maxSpan
	}
	existing := map[int]bool{}
	for base := 0; base < span; base += batchSize {
		end := base + batchSize
		if end > span {
			end = span
		}
		ids := make([]int, 0, end-base)
		for id := startID + base; id < startID+end; id++ {
			ids = append(ids, id)
		}
		got, err := fetch(ids)
		if err != nil {
			return nil, err
		}
		for id := range got {
			existing[id] = true
		}
		if len(existing) >= want {
			break
		}
	}
	out := make([]int, 0, len(existing))
	for id := range existing {
		out = append(out, id)
	}
	sort.Ints(out)
	if len(out) > want {
		out = out[:want]
	}
	return out, nil
}

// replyStart returns the message id this command replies to, or 0.
// A plain message inside a forum topic carries a reply header pointing at
// the topic root; that is not a reply.
func replyStart(ev *plugin.MessageEvent) int {
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

// cmdForward implements "bs [N]" as a reply to the first message.
func (p *BsPlugin) cmdForward(ctx *plugin.CommandContext) error {
	tl := ctx.Tlocal
	count := 1
	if len(ctx.Args) > 0 {
		n, err := parseCount(ctx.Args[0])
		if err != nil {
			return ctx.Edit("❌ " + tl("消息数必须是正整数，例如 ", "count must be a positive integer, e.g. ") + plugin.Code("bs 3"))
		}
		count = n
	}
	startID := replyStart(ctx.Message)
	if startID == 0 {
		return ctx.Edit("❌ " + tl("请先回复需要保送的消息", "reply to the message you want to forward first"))
	}
	targets := p.activeTargets()
	if len(targets) == 0 {
		return ctx.Edit("❌ " + tl("暂无可用目标，先用 ", "no targets yet; add one with ") + plugin.Code("bs add") + tl(" 添加", ""))
	}
	fromPeer, err := ctx.ResolvePeer()
	if err != nil {
		return ctx.Edit("❌ " + tl("无法解析当前会话", "cannot resolve the current chat"))
	}
	_ = ctx.Edit("⏳ " + tl("正在收集消息…", "Collecting messages…"))

	ids, err := collectIDs(startID, count, func(batch []int) (map[int]bool, error) {
		msgs, _, _, err := plugin.GetMessages(ctx.Context(), ctx.API, fromPeer, batch...)
		if err != nil {
			return nil, err
		}
		out := map[int]bool{}
		for _, m := range msgs {
			out[m.ID] = true
		}
		return out, nil
	})
	if err != nil {
		return ctx.Edit("❌ **" + tl("获取消息失败", "failed to fetch messages") + "**\n" + plugin.Code(err.Error()))
	}
	if len(ids) == 0 {
		return ctx.Edit("❌ " + tl("未找到可转发的消息，请确认消息未被删除", "no forwardable messages found; make sure they are not deleted"))
	}

	mode := p.mode()
	var (
		successes []*forwardResult
		failures  []string
	)
	for _, t := range targets {
		res, err := p.forwardTo(ctx.Context(), ctx.API, ctx.PeerResolver, ctx.SelfID, fromPeer, t, ids)
		switch {
		case err == nil:
			successes = append(successes, res)
		case errors.Is(err, errFloodAbort):
			return ctx.Edit("❌ **" + tl("保送中止", "forwarding aborted") + "**\n" +
				fmt.Sprintf(tl("操作频繁（%s），请稍后重试", "rate limited (%s); retry later"), plugin.Code(causeOf(err))))
		case errors.Is(err, errRestricted):
			return ctx.Edit("❌ **" + tl("保送中止", "forwarding aborted") + "**\n" +
				tl("该会话的消息不允许被转发", "this chat forbids forwarding its messages") + "  " + plugin.Code(causeOf(err)))
		default:
			if ctx.Context().Err() != nil {
				return ctx.Edit("❌ " + plugin.Code(ctx.Context().Err().Error()))
			}
			failures = append(failures, renderTarget(t, tl)+"\n  "+plugin.Code(err.Error()))
			continue
		}
		if mode == modeSequence {
			break
		}
	}
	if len(successes) == 0 {
		var b strings.Builder
		b.WriteString("❌ **" + tl("保送失败", "forwarding failed") + "**\n")
		if len(failures) > 0 {
			for _, f := range failures {
				b.WriteString("• " + f + "\n")
			}
		} else {
			b.WriteString(tl("未找到可用的目标", "no usable targets"))
		}
		return ctx.Edit(b.String())
	}

	// Feedback in the source chat: edit the command message.
	var parts []string
	for _, s := range successes {
		label := plugin.Escape(s.Display)
		if s.Display == "" {
			label = plugin.Code(s.Target.Target)
		}
		n := len(ids)
		if s.Forwarded > 0 && s.Forwarded < n {
			n = s.Forwarded
		}
		parts = append(parts, fmt.Sprintf(tl("%d 条消息已被保送到 %s", "%d messages were escorted to %s"), n, label))
	}
	_ = ctx.Edit("🚚 " + tl("亲爱的被观察者，您的 ", "Dear observed one, your ") + strings.Join(parts, "\n"))

	// Feedback in each target chat: reply under the first forwarded message.
	p.sendTargetFeedback(ctx, fromPeer, ids, successes)
	return nil
}

// forwardResult reports one successful target.
type forwardResult struct {
	Target    *target
	Display   string
	Forwarded int   // forwarded messages found in updates, 0 if unknown
	FirstID   int   // id of the first forwarded message, 0 if unknown
	NewIDs    []int // all new message ids, ascending (may be non-contiguous)
	Peer      tg.InputPeerClass
	TopicID   int
}

// fwdBatch is the per-request message cap for messages.forwardMessages; the
// server rejects larger requests, so big escorts go out in chunks.
const fwdBatch = 100

// forwardTo forwards ids to one target. FLOOD_WAIT is retried once after
// waiting; a second one aborts the whole run. CHAT_FORWARDS_RESTRICTED
// aborts the run too. Permission errors are returned as plain errors so the
// caller can skip to the next target.
func (p *BsPlugin) forwardTo(ctx context.Context, api *tg.Client, resolver plugin.PeerResolver, selfID int64, fromPeer tg.InputPeerClass, t *target, ids []int) (*forwardResult, error) {
	// call forwards all ids to peer in fwdBatch-sized chunks and returns
	// the new message ids, sorted ascending.
	call := func(peer tg.InputPeerClass) ([]int, error) {
		var all []int
		for base := 0; base < len(ids); base += fwdBatch {
			end := base + fwdBatch
			if end > len(ids) {
				end = len(ids)
			}
			chunk := ids[base:end]
			req := &tg.MessagesForwardMessagesRequest{
				FromPeer: fromPeer,
				ID:       chunk,
				ToPeer:   peer,
				RandomID: randomIDs(len(chunk)),
			}
			if t.TopicID > 0 {
				req.SetTopMsgID(t.TopicID)
			}
			var res tg.UpdatesClass
			var err error
			for attempt := 0; ; attempt++ {
				res, err = api.MessagesForwardMessages(ctx, req)
				if err == nil {
					break
				}
				if d, ok := tgerr.AsFloodWait(err); ok {
					if attempt >= 1 {
						return nil, fmt.Errorf("%w: %s", errFloodAbort, err.Error())
					}
					select {
					case <-time.After(d + time.Second):
					case <-ctx.Done():
						return nil, ctx.Err()
					}
					continue
				}
				if tgerr.Is(err, "CHAT_FORWARDS_RESTRICTED") {
					return nil, fmt.Errorf("%w: %s", errRestricted, err.Error())
				}
				if tgerr.Is(err, permissionErrs...) {
					return nil, fmt.Errorf("no write permission: %s", err.Error())
				}
				return nil, err
			}
			all = append(all, forwardStats(res)...)
		}
		sort.Ints(all)
		return all, nil
	}
	peer := t.Peer.input()
	if peer == nil {
		var err error
		peer, err = resolveTargetInput(ctx, resolver, selfID, t.Target)
		if err != nil {
			return nil, fmt.Errorf("resolve: %w", err)
		}
		p.mu.Lock()
		t.Peer = refFromPeer(peer)
		_ = p.saveLocked()
		p.mu.Unlock()
	}
	res, err := call(peer)
	if err != nil && tgerr.Is(err, "PEER_ID_INVALID", "CHANNEL_INVALID", "USER_ID_INVALID") {
		// The persisted access hash went stale (relogin, remote reset):
		// drop it and resolve from the user-entered target once more.
		p.mu.Lock()
		stale := t.Peer != nil && t.Peer.AccessHash != 0
		if stale {
			t.Peer = nil
			_ = p.saveLocked()
		}
		p.mu.Unlock()
		if stale {
			if peer2, rerr := resolveTargetInput(ctx, resolver, selfID, t.Target); rerr == nil {
				if ids2, cerr := call(peer2); cerr == nil {
					p.mu.Lock()
					t.Peer = refFromPeer(peer2)
					_ = p.saveLocked()
					p.mu.Unlock()
					res, err = ids2, nil
				} else {
					res, err = nil, cerr
				}
			}
		}
	}
	if err != nil {
		return nil, err
	}
	newIDs := res
	first, count := 0, len(newIDs)
	if count > 0 {
		first = newIDs[0]
	}
	return &forwardResult{Target: t, Display: t.Display, Forwarded: count, FirstID: first, NewIDs: newIDs, Peer: peer, TopicID: t.TopicID}, nil
}

// forwardStats extracts the new message ids from a forward Updates result,
// sorted ascending. The ids are not necessarily contiguous (topics, server
// reordering), so callers must use the list instead of extrapolating.
func forwardStats(u tg.UpdatesClass) []int {
	var list []tg.UpdateClass
	switch v := u.(type) {
	case *tg.Updates:
		list = v.Updates
	case *tg.UpdatesCombined:
		list = v.Updates
	}
	var ids []int
	for _, up := range list {
		var id int
		switch x := up.(type) {
		case *tg.UpdateNewMessage:
			id = x.Message.GetID()
		case *tg.UpdateNewChannelMessage:
			id = x.Message.GetID()
		case *tg.UpdateMessageID:
			id = x.ID
		}
		if id > 0 {
			ids = append(ids, id)
		}
	}
	sort.Ints(ids)
	return ids
}

// sendTargetFeedback replies under the first forwarded message in each
// target chat with the source link and tags for original + forwarded ids.
func (p *BsPlugin) sendTargetFeedback(ctx *plugin.CommandContext, fromPeer tg.InputPeerClass, origIDs []int, successes []*forwardResult) {
	tl := ctx.Tlocal
	srcChatID := plugin.ChatIDOfInput(fromPeer)
	srcName, _ := p.describe(ctx.Context(), ctx.API, fromPeer, tl)
	src := plugin.Escape(srcName)
	if l := chatLink(srcChatID, 0); l != "" {
		src = plugin.Link(srcName, l)
	}
	origTags := make([]string, len(origIDs))
	for i, id := range origIDs {
		if l := chatLink(srcChatID, id); l != "" {
			origTags[i] = plugin.Link(fmt.Sprintf("#%d", id), l)
		} else {
			origTags[i] = fmt.Sprintf("#%d", id)
		}
	}
	for _, s := range successes {
		if s.FirstID <= 0 {
			continue
		}
		dstChatID := plugin.ChatIDOfInput(s.Peer)
		var b strings.Builder
		b.WriteString(tl("来源", "Source") + ": " + src + "\n")
		if len(origTags) > 0 {
			b.WriteString(tl("原消息", "Original") + ": " + strings.Join(origTags, " ") + "\n")
		}
		var newTags []string
		for _, id := range s.NewIDs {
			if l := chatLink(dstChatID, id); l != "" {
				newTags = append(newTags, plugin.Link(fmt.Sprintf("#%d", id), l))
			} else {
				newTags = append(newTags, fmt.Sprintf("#%d", id))
			}
		}
		if len(newTags) > 0 {
			b.WriteString(tl("新消息", "Forwarded") + ": " + strings.Join(newTags, " "))
		}
		reply := &tg.InputReplyToMessage{ReplyToMsgID: s.FirstID}
		if s.TopicID > 0 {
			reply.SetTopMsgID(s.TopicID)
		}
		plain, entities := plugin.ParseMarkdown(b.String(), inputUserResolver(ctx))
		_, err := ctx.API.MessagesSendMessage(ctx.Context(), &tg.MessagesSendMessageRequest{
			Peer:     s.Peer,
			Message:  plain,
			Entities: entities,
			ReplyTo:  reply,
			RandomID: randomID(),
		})
		if err != nil && p.logger != nil {
			p.logger.Warn("target feedback failed", "target", s.Target.Target, "err", err)
		}
	}
}

// chatLink builds a message link for chat ids in the PaperValet form.
// msgID 0 links the chat itself; users and basic groups have no links.
func chatLink(chatID int64, msgID int) string {
	switch {
	case chatID > 0:
		return ""
	case chatID <= -1000000000000:
		raw := -chatID - 1000000000000
		if msgID > 0 {
			return fmt.Sprintf("https://t.me/c/%d/%d", raw, msgID)
		}
		return fmt.Sprintf("https://t.me/c/%d", raw)
	default:
		return ""
	}
}

// inputUserResolver resolves mention ids for ParseMarkdown.
func inputUserResolver(ctx *plugin.CommandContext) func(int64) (tg.InputUserClass, error) {
	return func(id int64) (tg.InputUserClass, error) {
		if ctx.API == nil {
			return nil, fmt.Errorf("no api client")
		}
		users, err := ctx.API.UsersGetUsers(ctx.Context(), []tg.InputUserClass{&tg.InputUser{UserID: id}})
		if err != nil || len(users) == 0 {
			return nil, fmt.Errorf("resolve user %d failed", id)
		}
		u, ok := users[0].(*tg.User)
		if !ok {
			return nil, fmt.Errorf("unexpected user type %T", users[0])
		}
		return u.AsInput(), nil
	}
}

// causeOf unwraps one level and returns the cause message.
func causeOf(err error) string {
	if un := errors.Unwrap(err); un != nil {
		return un.Error()
	}
	return err.Error()
}

func randomIDs(n int) []int64 {
	out := make([]int64, n)
	for i := range out {
		out[i] = randomID()
	}
	return out
}

func randomID() int64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err == nil {
		return int64(binary.LittleEndian.Uint64(b[:]))
	}
	return time.Now().UnixNano()
}
