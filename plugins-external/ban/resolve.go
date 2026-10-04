package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

// errNoTarget means no ban target was given or usable.
var errNoTarget = errors.New("NO_TARGET")

// target is a fully resolved ban target: the user id plus, when known, the
// access hash Telegram handed back with it.
type target struct {
	id   int64
	hash int64
}

func (t target) peer() tg.InputPeerClass { return &tg.InputPeerUser{UserID: t.id, AccessHash: t.hash} }

// resolveTarget resolves the user to act on: reply first, then @username,
// then numeric id; true/confirm never counts as a target. wantHash is false
// for basic-group remove, where a bare id is accepted.
func (p *BanPlugin) resolveTarget(ctx *plugin.CommandContext, wantHash bool) (t target, name string, err error) {
	tl := ctx.Tlocal
	msg := ctx.Message
	if msg == nil || ctx.Message.Message == nil {
		return target{}, "", errNoTarget
	}
	// 1. Reply (forum-topic aware).
	if replyID := realReplyID(msg.Message); replyID != 0 {
		if peer, rerr := ctx.ResolvePeer(); rerr == nil {
			msgs, users, _, gerr := plugin.GetMessages(ctx.Context(), ctx.API, peer, replyID)
			if gerr == nil {
				for _, m := range msgs {
					if m.ID != replyID {
						continue
					}
					if from, ok := m.FromID.(*tg.PeerUser); ok {
						for _, us := range users {
							if user, ok := us.(*tg.User); ok && user.ID == from.UserID {
								// The hash returned with the message is
								// valid even for min users.
								t := target{id: user.ID, hash: user.AccessHash}
								return t, p.markPeer(ctx, t, user), nil
							}
						}
						// Min user with no hash: try the message resolver,
						// then a cross-group probe, else address by bare id.
						if u, rerr := ctx.PeerResolver.ResolveUserFromMessage(ctx.Context(), peer, m.ID, from.UserID); rerr == nil {
							if pu, ok := u.(*tg.InputPeerUser); ok && (pu.AccessHash != 0 || !wantHash) {
								t := target{id: pu.UserID, hash: pu.AccessHash}
								return t, p.markPeer(ctx, t, nil), nil
							}
						}
						if wantHash {
							if t, rerr := p.resolveUserAcross(ctx, from.UserID); rerr == nil {
								return t, p.markPeer(ctx, t, nil), nil
							}
						}
						return target{id: from.UserID}, plugin.Mention(fmt.Sprintf("%d", from.UserID), from.UserID), nil
					}
					// Anonymous admin or channel identity: not a user.
					return target{}, "", errors.New(tl(
						"被回复的消息不是普通用户发的（匿名管理员或频道身份），无法操作",
						"The replied message was sent by an anonymous admin or a channel identity"))
				}
			}
		}
	}
	// 2. @username.
	for _, a := range ctx.Args {
		if isMetaFlag(a) || !strings.HasPrefix(a, "@") || len(a) == 1 {
			continue
		}
		peer, err := ctx.PeerResolver.ResolveUsername(ctx.Context(), a[1:])
		if err != nil {
			return target{}, "", err
		}
		if u, ok := peer.(*tg.InputPeerUser); ok {
			t := target{id: u.UserID, hash: u.AccessHash}
			return t, p.markPeer(ctx, t, nil), nil
		}
		return target{}, "", errors.New(tl("这个用户名不是个人用户", "That username is not a personal user"))
	}
	// 3. Numeric id: peer store first, then a getParticipant accessHash=0
	// backfill across managed supergroups (target need not be a member).
	for _, a := range ctx.Args {
		if isMetaFlag(a) {
			continue
		}
		id, err := strconv.ParseInt(strings.TrimSpace(a), 10, 64)
		if err != nil {
			continue
		}
		if peer, err := ctx.PeerResolver.ResolveFromChatID(ctx.Context(), id); err == nil {
			if u, ok := peer.(*tg.InputPeerUser); ok && (u.AccessHash != 0 || !wantHash) {
				t := target{id: u.UserID, hash: u.AccessHash}
				return t, p.markPeer(ctx, t, nil), nil
			}
		}
		if t, err := p.resolveUserAcross(ctx, id); err == nil {
			return t, p.markPeer(ctx, t, nil), nil
		}
	}
	return target{}, "", errNoTarget
}

// markPeer feeds a resolved hash back into the bot's peer store so later
// commands (and other plugins) can use it, and renders a display name.
func (p *BanPlugin) markPeer(ctx *plugin.CommandContext, t target, u *tg.User) string {
	if t.hash != 0 {
		if res, ok := ctx.PeerResolver.(interface{ RegisterPeer(int64, int64, string) }); ok {
			res.RegisterPeer(t.id, t.hash, "user")
		}
	}
	name := ""
	if u != nil {
		name = strings.TrimSpace(u.FirstName + " " + u.LastName)
		if name == "" && u.Username != "" {
			name = "@" + u.Username
		}
	}
	if name == "" {
		name = strconv.FormatInt(t.id, 10)
	}
	return name
}

// resolveUserAcross walks the managed supergroups with a getParticipant
// accessHash=0 probe, which makes Telegram return the target's full user
// (with hash) even when they are not a member of that group.
func (p *BanPlugin) resolveUserAcross(ctx *plugin.CommandContext, userID int64) (target, error) {
	groups, err := p.loadGroups(ctx.Context(), false)
	if err != nil || len(groups) == 0 {
		return target{}, errUnresolved
	}
	for _, g := range groups {
		if g.Kind != "channel" {
			continue
		}
		res, err := ctx.API.ChannelsGetParticipant(ctx.Context(), &tg.ChannelsGetParticipantRequest{
			Channel: g.channel(), Participant: &tg.InputPeerUser{UserID: userID},
		})
		if err != nil {
			continue
		}
		for _, u := range res.GetUsers() {
			if user, ok := u.(*tg.User); ok && user.ID == userID && user.AccessHash != 0 {
				return target{id: user.ID, hash: user.AccessHash}, nil
			}
		}
	}
	return target{}, errUnresolved
}

var errUnresolved = errors.New("TARGET_UNRESOLVED")

// realReplyID returns the replied message id, ignoring the implicit
// reply-to-topic-root header every forum topic message carries.
func realReplyID(msg *tg.Message) int {
	h, ok := msg.ReplyTo.(*tg.MessageReplyHeader)
	if !ok || h.ReplyToMsgID == 0 {
		return 0
	}
	if h.ForumTopic {
		if _, has := h.GetReplyToTopID(); !has {
			return 0
		}
	}
	return h.ReplyToMsgID
}

// isMetaFlag filters the true/confirm confirmation flag out of target args.
func isMetaFlag(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "confirm":
		return true
	}
	return false
}

// ============================================================
// Managed-group cache (data/ban/groups.json)
// ============================================================

type managedGroup struct {
	ID         int64  `json:"id"`          // raw positive id
	AccessHash int64  `json:"access_hash"` // channels only
	Kind       string `json:"kind"`        // "channel" | "chat"
	Title      string `json:"title"`
}

func (m managedGroup) channel() *tg.InputChannel {
	return &tg.InputChannel{ChannelID: m.ID, AccessHash: m.AccessHash}
}

// groupsPath returns data/ban/groups.json.
func (p *BanPlugin) groupsPath() (string, error) {
	dir, err := p.host.DataDir(p.Name())
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, groupsFile), nil
}

// loadGroups returns the cached managed groups, refreshing them when the
// cache is empty or force is set. scan errors keep the last good cache.
func (p *BanPlugin) loadGroups(ctx context.Context, force bool) ([]managedGroup, error) {
	path, err := p.groupsPath()
	if err != nil {
		return nil, err
	}
	cached := readGroups(path)
	if !force && len(cached) > 0 {
		return cached, nil
	}
	fresh, err := p.scanGroups(ctx)
	if err != nil {
		if len(cached) > 0 {
			if p.log != nil {
				p.log.Warn("ban: scan failed, keeping cache", "error", err)
			}
			return cached, nil
		}
		return nil, err
	}
	if werr := writeGroups(path, fresh); werr != nil && p.log != nil {
		p.log.Warn("ban: write groups cache failed", "error", werr)
	}
	return fresh, nil
}

// scanGroups enumerates every dialog (main list + archived folder 1) and
// keeps the chats where the account can ban: creator, or admin with
// ban/delete rights.
func (p *BanPlugin) scanGroups(ctx context.Context) ([]managedGroup, error) {
	chats := map[int64]tg.ChatClass{}
	for _, folder := range []int{0, 1} {
		offsetDate, offsetID := 0, 0
		var offsetPeer tg.InputPeerClass = &tg.InputPeerEmpty{}
		for {
			req := &tg.MessagesGetDialogsRequest{
				OffsetDate: offsetDate,
				OffsetID:   offsetID,
				OffsetPeer: offsetPeer,
				Limit:      dialogsPerPage,
				Hash:       0,
			}
			if folder != 0 {
				req.SetFolderID(folder) // archived chats live in folder 1
			}
			res, err := p.host.API().MessagesGetDialogs(ctx, req)
			if err != nil {
				return nil, fmt.Errorf("folder %d: %w", folder, err)
			}
			page, ok := splitDialogs(res)
			if !ok || len(page.dialogs) == 0 {
				break
			}
			for _, c := range page.chats {
				switch ch := c.(type) {
				case *tg.Channel:
					chats[ch.ID] = ch
				case *tg.Chat:
					chats[-ch.ID] = ch
				}
			}
			if len(page.dialogs) < dialogsPerPage {
				break
			}
			last := page.dialogs[len(page.dialogs)-1]
			if d, okd := last.(*tg.Dialog); okd {
				offsetID = d.TopMessage
				offsetPeer = peerInputOf(d.Peer, chats)
			}
			offsetDate = 0
			if len(page.msgs) > 0 {
				if m, okm := page.msgs[len(page.msgs)-1].(*tg.Message); okm && m.Date > 0 {
					offsetDate = m.Date
				}
			}
		}
	}
	var out []managedGroup
	for _, c := range chats {
		if g, ok := manageable(c, p.host.SelfID()); ok {
			out = append(out, g)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Title < out[j].Title })
	return out, nil
}

// dialogPage holds the concrete lists one dialogs result carries.
type dialogPage struct {
	dialogs []tg.DialogClass
	chats   []tg.ChatClass
	msgs    []tg.MessageClass
}

// splitDialogs unpacks a dialogs result into its concrete lists.
func splitDialogs(res tg.MessagesDialogsClass) (page dialogPage, ok bool) {
	switch v := res.(type) {
	case *tg.MessagesDialogs:
		page = dialogPage{v.Dialogs, v.Chats, v.Messages}
		return page, true
	case *tg.MessagesDialogsSlice:
		page = dialogPage{v.Dialogs, v.Chats, v.Messages}
		return page, true
	}
	return dialogPage{}, false
}

// peerInputOf builds the next-page offset peer from a dialog peer.
func peerInputOf(peer tg.PeerClass, chats map[int64]tg.ChatClass) tg.InputPeerClass {
	switch v := peer.(type) {
	case *tg.PeerUser:
		return &tg.InputPeerUser{UserID: v.UserID}
	case *tg.PeerChat:
		return &tg.InputPeerChat{ChatID: v.ChatID}
	case *tg.PeerChannel:
		if c, ok := chats[v.ChannelID].(*tg.Channel); ok && c.AccessHash != 0 {
			return &tg.InputPeerChannel{ChannelID: v.ChannelID, AccessHash: c.AccessHash}
		}
		return &tg.InputPeerChannel{ChannelID: v.ChannelID}
	}
	return &tg.InputPeerEmpty{}
}

// manageable reports whether the account can ban in chat c — creator, or
// admin with ban/delete rights — and returns its cache entry. self is the
// logged-in account id.
func manageable(c tg.ChatClass, self int64) (managedGroup, bool) {
	switch v := c.(type) {
	case *tg.Channel:
		if v.Broadcast || v.Left {
			return managedGroup{}, false
		}
		g := managedGroup{ID: v.ID, Kind: "channel", Title: v.Title, AccessHash: v.AccessHash}
		if v.Creator {
			return g, true
		}
		if r, ok := v.GetAdminRights(); ok && (r.BanUsers || r.DeleteMessages) {
			return g, true
		}
		return managedGroup{}, false
	case *tg.Chat:
		if v.Deactivated || v.Left || v.MigratedTo != nil {
			return managedGroup{}, false
		}
		// The dialog snapshot carries no admin rights for basic groups;
		// only groups the account knows it can admin are kept.
		return managedGroup{ID: v.ID, Kind: "chat", Title: v.Title}, false
	}
	return managedGroup{}, false
}

func readGroups(path string) []managedGroup {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var g []managedGroup
	if json.Unmarshal(data, &g) != nil {
		return nil
	}
	return g
}

func writeGroups(path string, g []managedGroup) error {
	data, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ============================================================
// Cross-group helpers
// ============================================================

// adminEverywhere counts the managed supergroups where the target is an
// admin, for the true-confirmation guard.
func (p *BanPlugin) adminEverywhere(ctx *plugin.CommandContext, t target, groups []managedGroup) int {
	n := 0
	for _, g := range groups {
		if g.Kind != "channel" {
			continue
		}
		res, err := ctx.API.ChannelsGetParticipant(ctx.Context(), &tg.ChannelsGetParticipantRequest{
			Channel: g.channel(), Participant: t.peer(),
		})
		if err != nil {
			continue
		}
		switch res.Participant.(type) {
		case *tg.ChannelParticipantCreator, *tg.ChannelParticipantAdmin:
			n++
		}
	}
	return n
}

// batchOutcome aggregates per-group results for sb/unsb.
type batchOutcome struct {
	ok, skipped, failed int
	reasons             map[string]int
}

func newOutcome() *batchOutcome { return &batchOutcome{reasons: map[string]int{}} }

func (o *batchOutcome) fail(reason string) {
	o.failed++
	o.reasons[reason]++
}

// topReasons renders the most common failure reasons.
func (o *batchOutcome) topReasons(n int) string {
	type kv struct {
		r string
		c int
	}
	var list []kv
	for r, c := range o.reasons {
		list = append(list, kv{r, c})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].c > list[j].c })
	var out []string
	for i, kv := range list {
		if i >= n {
			break
		}
		out = append(out, fmt.Sprintf("%s×%d", kv.r, kv.c))
	}
	return strings.Join(out, "、")
}

// errCode extracts a short error text for the batch summary.
func errCode(err error) string {
	var e *tgerr.Error
	if errors.As(err, &e) {
		return e.Type
	}
	msg := err.Error()
	if len(msg) > 60 {
		msg = msg[:60]
	}
	return msg
}
