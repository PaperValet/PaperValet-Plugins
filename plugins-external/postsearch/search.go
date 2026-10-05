package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

// chatRef identifies the chat a found message belongs to, enough to build
// a t.me link.
type chatRef struct {
	Username string
	ChatID   int64 // PaperValet -100… form; 0 unknown
}

func (r chatRef) link(msgID int) string {
	if r.Username != "" {
		return fmt.Sprintf("https://t.me/%s/%d", strings.TrimPrefix(r.Username, "@"), msgID)
	}
	if raw := -r.ChatID - 1000000000000; r.ChatID != 0 && raw > 0 {
		return fmt.Sprintf("https://t.me/c/%d/%d", raw, msgID)
	}
	return ""
}

// hit is one search result.
type hit struct {
	Msg      *tg.Message
	Chat     chatRef // chat the message was found in
	FileName string  // document filename, for matching/scoring
	Score    int
}

// service is the ported SearchService.
type service struct {
	store *store
	host  plugin.Host

	// peerCache caches handle → resolved chat info, so a search over many
	// channels does not re-run contacts.resolve + getChannels (2 API calls
	// each) on every command.
	mu        sync.Mutex
	peerCache map[string]peerCacheEntry
}

// peerTTL bounds how long a cached resolve/lookup is trusted.
const peerTTL = 6 * time.Hour

type peerCacheEntry struct {
	peer tg.InputPeerClass
	info chatInfo
	at   time.Time
}

// resolveCached resolves a handle and reads its chat info, using the
// service cache; ok=false means the caller should resolve the hard way.
// Errors are not cached.
func (s *service) resolveCached(ctx context.Context, handle string) (tg.InputPeerClass, chatInfo, bool) {
	s.mu.Lock()
	e, ok := s.peerCache[handle]
	s.mu.Unlock()
	if ok && time.Since(e.at) < peerTTL {
		return e.peer, e.info, true
	}
	peer, err := s.resolve(ctx, handle)
	if err != nil {
		return nil, chatInfo{}, false
	}
	info, err := s.lookup(ctx, peer)
	if err != nil {
		return nil, chatInfo{}, false
	}
	if s.peerCache == nil {
		s.peerCache = map[string]peerCacheEntry{}
	}
	s.mu.Lock()
	s.peerCache[handle] = peerCacheEntry{peer: peer, info: info, at: time.Now()}
	s.mu.Unlock()
	return peer, info, true
}

// dropCached forgets a handle's cached peer, e.g. after it went away.
func (s *service) dropCached(handle string) {
	s.mu.Lock()
	delete(s.peerCache, handle)
	s.mu.Unlock()
}

func newService(host plugin.Host) *service {
	return &service{store: newStore(host), host: host}
}

func (s *service) load() error { return s.store.load() }

// ---------------------------------------------------------------- fetch

// resolve turns a configured handle (@name, link, or raw id) into an input
// peer, following save/links.go conventions.
func (s *service) resolve(ctx context.Context, handle string) (tg.InputPeerClass, error) {
	if s.host == nil {
		return nil, fmt.Errorf("no host")
	}
	un := strings.TrimSpace(handle)
	un = strings.TrimPrefix(un, "@")
	if id, ok := parseTME(un); ok {
		un = id
	}
	if strings.HasPrefix(un, "@") {
		return s.host.PeerResolver().ResolveUsername(ctx, strings.TrimPrefix(un, "@"))
	}
	if id, err := parseChatID(un); err == nil {
		return s.host.PeerResolver().ResolveFromChatID(ctx, id)
	}
	return s.host.PeerResolver().ResolveUsername(ctx, un)
}

// parseTME unwraps t.me/<name> and t.me/c/<id> and invite/joinchat links.
// For joinchat links it returns the handle unchanged (not resolvable here).
func parseTME(s string) (string, bool) {
	for _, p := range []string{"https://t.me/", "http://t.me/", "https://telegram.me/", "telegram.me/", "t.me/"} {
		if strings.HasPrefix(s, p) {
			rest := strings.TrimPrefix(s, p)
			if i := strings.IndexAny(rest, "?/#"); i >= 0 {
				rest = rest[:i]
			}
			if rest == "" {
				return s, false
			}
			return rest, true
		}
	}
	return s, false
}

// parseChatID accepts -100…, bare channel ids and bare basic-group ids.
// The whole token must be a number: Sscanf("%d") would happily read the
// "123" out of "123abc" and turn a mistyped handle into PEER_ID_INVALID.
func parseChatID(s string) (int64, error) {
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, err
	}
	return id, nil
}

// chatInfoFromPeer reads title/username/flags from the resolve result.
type chatInfo struct {
	Peer      tg.InputPeerClass
	Title     string
	Username  string
	ChatID    int64
	Megagroup bool
	Broadcast bool
	Basic     bool
}

// lookup fetches the full channel/chat object so links and linked groups
// resolve. peer must have been produced by resolve().
func (s *service) lookup(ctx context.Context, peer tg.InputPeerClass) (chatInfo, error) {
	info := chatInfo{Peer: peer, ChatID: plugin.ChatIDOfInput(peer)}
	api := s.api()
	switch p := peer.(type) {
	case *tg.InputPeerChannel:
		ch := &tg.InputChannel{ChannelID: p.ChannelID, AccessHash: p.AccessHash}
		res, err := api.ChannelsGetChannels(ctx, []tg.InputChannelClass{ch})
		if err != nil {
			return info, err
		}
		for _, c := range res.GetChats() {
			if v, ok := c.(*tg.Channel); ok && v.ID == p.ChannelID {
				info.Title, info.Username = v.Title, v.Username
				info.Megagroup, info.Broadcast = v.Megagroup, v.Broadcast
				return info, nil
			}
			if v, ok := c.(*tg.ChannelForbidden); ok && v.ID == p.ChannelID {
				info.Title = v.Title
				return info, nil
			}
		}
		return info, fmt.Errorf("channel %d not found", p.ChannelID)
	case *tg.InputPeerChat:
		res, err := api.MessagesGetChats(ctx, []int64{p.ChatID})
		if err != nil {
			return info, err
		}
		for _, c := range res.GetChats() {
			if v, ok := c.(*tg.Chat); ok && v.ID == p.ChatID {
				info.Title, info.Basic = v.Title, true
				return info, nil
			}
		}
		return info, fmt.Errorf("chat %d not found", p.ChatID)
	}
	return info, fmt.Errorf("not a channel or group")
}

func (s *service) api() *tg.Client { return s.host.API() }

// discoverLinkedGroup returns the discussion group of a broadcast channel,
// as @username or an invite link, mirroring the source.
func (s *service) discoverLinkedGroup(ctx context.Context, peer tg.InputPeerClass) (string, error) {
	p, ok := peer.(*tg.InputPeerChannel)
	if !ok {
		return "", nil
	}
	full, err := s.api().ChannelsGetFullChannel(ctx, &tg.InputChannel{ChannelID: p.ChannelID, AccessHash: p.AccessHash})
	if err != nil {
		return "", err
	}
	chFull, _ := full.GetFullChat().(*tg.ChannelFull)
	if chFull == nil {
		return "", nil
	}
	linked := chFull.LinkedChatID
	if linked == 0 {
		return "", nil
	}
	res, err := s.api().ChannelsGetChannels(ctx, []tg.InputChannelClass{&tg.InputChannel{ChannelID: linked}})
	if err != nil {
		return "", err
	}
	for _, c := range res.GetChats() {
		if v, ok := c.(*tg.Channel); ok && v.ID == linked && v.Megagroup {
			if v.Username != "" {
				return "@" + v.Username, nil
			}
			break
		}
	}
	// No public username: try exporting an invite link (needs membership).
	inv, err := s.api().MessagesExportChatInvite(ctx, &tg.MessagesExportChatInviteRequest{
		Peer: &tg.InputPeerChannel{ChannelID: linked},
	})
	if err != nil {
		return "", nil // unresolvable private group; source also gives up
	}
	if l, ok := inv.(*tg.ChatInviteExported); ok && l.Link != "" {
		return l.Link, nil
	}
	return "", nil
}

// ---------------------------------------------------------------- search

// searchOnce runs messages.search on one peer with flood-wait retry.
func (s *service) searchOnce(ctx context.Context, req *tg.MessagesSearchRequest) (tg.MessagesMessagesClass, error) {
	for attempt := 0; ; attempt++ {
		res, err := s.api().MessagesSearch(ctx, req)
		if d, ok := tgerr.AsFloodWait(err); ok && attempt < 2 && d <= 30*time.Second {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(d + time.Second):
			}
			continue
		}
		return res, err
	}
}

// getReplies fetches a post's comments with the same flood-wait retry as
// searchOnce, so a rate-limited multi-post crawl does not silently drop
// results.
func (s *service) getReplies(ctx context.Context, peer tg.InputPeerClass, msgID int) (tg.MessagesMessagesClass, error) {
	for attempt := 0; ; attempt++ {
		res, err := s.api().MessagesGetReplies(ctx, &tg.MessagesGetRepliesRequest{
			Peer: peer, MsgID: msgID, Limit: 100,
		})
		d, ok := tgerr.AsFloodWait(err)
		if !ok || attempt >= 2 || d > 30*time.Second {
			return res, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(d + time.Second):
		}
	}
}

// messagesOf flattens a search result into messages plus a chat lookup map.
func messagesOf(res tg.MessagesMessagesClass) (msgs []*tg.Message, chats []tg.ChatClass) {
	mod, ok := res.AsModified()
	if !ok {
		return nil, nil
	}
	for _, m := range mod.GetMessages() {
		if msg, ok := m.(*tg.Message); ok {
			msgs = append(msgs, msg)
		}
	}
	return msgs, mod.GetChats()
}

func chatUsername(chats []tg.ChatClass, id int64) string {
	for _, c := range chats {
		if v, ok := c.(*tg.Channel); ok && v.ID == id {
			return v.Username
		}
	}
	return ""
}

// videoOf returns the video document attribute when msg carries a real
// video (not a webpage preview).
func videoOf(m *tg.Message) (*tg.Document, *tg.DocumentAttributeVideo, bool) {
	if _, web := m.Media.(*tg.MessageMediaWebPage); web || m.Media == nil {
		return nil, nil, false
	}
	md, ok := m.Media.(*tg.MessageMediaDocument)
	if !ok {
		return nil, nil, false
	}
	doc, ok := md.Document.(*tg.Document)
	if !ok {
		return nil, nil, false
	}
	for _, a := range doc.Attributes {
		if v, ok := a.(*tg.DocumentAttributeVideo); ok && !v.RoundMessage {
			return doc, v, true
		}
	}
	return nil, nil, false
}

// fileOf returns the filename attribute of a document message.
func fileOf(m *tg.Message) string {
	md, ok := m.Media.(*tg.MessageMediaDocument)
	if !ok {
		return ""
	}
	doc, ok := md.Document.(*tg.Document)
	if !ok {
		return ""
	}
	for _, a := range doc.Attributes {
		if f, ok := a.(*tg.DocumentAttributeFilename); ok {
			return f.FileName
		}
	}
	return ""
}

// textOf returns message text plus filename, the matching sources of the
// source plugin.
func textOf(m *tg.Message) []string {
	out := []string{m.Message}
	if f := fileOf(m); f != "" {
		out = append(out, f)
	}
	return out
}

// isAd mirrors SearchService.isAdContent.
func isAd(filters []string, m *tg.Message) bool {
	t := strings.ToLower(m.Message + " " + fileOf(m))
	for _, f := range filters {
		if f != "" && strings.Contains(t, f) {
			return true
		}
	}
	return false
}

// normalize and fuzzyMatch port the source's matching helpers.
func normalize(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	space := false
	for _, r := range s {
		switch r {
		case '-', '_', ' ', '.', '|', '/', '#', '\\':
			space = true
		default:
			if space && b.Len() > 0 {
				b.WriteRune(' ')
			}
			space = false
			b.WriteRune(r)
		}
	}
	return b.String()
}

func fuzzyMatch(text, query string) bool {
	if query == "" {
		return false
	}
	if strings.Contains(text, query) {
		return true
	}
	qParts := strings.Fields(query)
	tParts := strings.Fields(text)
	if len(qParts) == 1 && lettersDigits(query) {
		squash := func(s string) string { return strings.ReplaceAll(s, " ", "") }
		if strings.Contains(squash(text), squash(query)) {
			return true
		}
	}
	for _, q := range qParts {
		found := false
		for _, t := range tParts {
			if strings.Contains(t, q) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// lettersDigits reports whether s looks like letters followed by digits
// (source regex [a-z]+\s*\d+).
func lettersDigits(s string) bool {
	letters := 0
	i := 0
	for i < len(s) && isASCIILetter(s[i]) {
		letters++
		i++
	}
	if letters == 0 {
		return false
	}
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	digits := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		digits++
		i++
	}
	return digits > 0 && i == len(s)
}

func isASCIILetter(b byte) bool { return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') }

func matches(m *tg.Message, query string) bool {
	nq := normalize(query)
	for _, src := range textOf(m) {
		if src != "" && fuzzyMatch(normalize(src), nq) {
			return true
		}
	}
	return false
}

// scoreOf ports the source's relevance scoring.
func scoreOf(m *tg.Message, query string) int {
	nq := normalize(query)
	score := 0
	if f := fileOf(m); f != "" && strings.Contains(normalize(f), nq) {
		score += 100
	}
	if m.Message != "" && strings.Contains(normalize(m.Message), nq) {
		score += 50
	}
	return score
}

// durationOf returns the video duration in seconds, or 0.
func durationOf(m *tg.Message) int {
	if _, v, ok := videoOf(m); ok {
		return int(v.Duration)
	}
	return 0
}

// searchChannel runs one channel's keyword search, expanding albums so a
// media group counts once, like the source.
func (s *service) searchChannel(ctx context.Context, ch channel, query string, filters []string) ([]hit, error) {
	peer, info, ok := s.resolveCached(ctx, ch.Handle)
	if !ok {
		return nil, fmt.Errorf("cannot resolve %s", ch.Handle)
	}
	req := &tg.MessagesSearchRequest{
		Peer:   peer,
		Q:      query,
		Limit:  queryFetch,
		Filter: &tg.InputMessagesFilterEmpty{},
	}
	res, err := s.searchOnce(ctx, req)
	if err != nil {
		return nil, err
	}
	msgs, chats := messagesOf(res)
	ref := chatRef{Username: info.Username, ChatID: info.ChatID}
	if ref.Username == "" {
		ref.Username = ch.Username
	}
	var out []hit
	seenGroups := map[int64]bool{}
	for _, m := range msgs {
		if isAd(filters, m) || !matches(m, query) {
			continue
		}
		gid, hasGroup := m.GetGroupedID()
		if hasGroup && gid != 0 {
			if seenGroups[gid] {
				continue
			}
			seenGroups[gid] = true
		}
		hr := hit{Msg: m, Chat: ref, FileName: fileOf(m), Score: scoreOf(m, query)}
		if hr.Chat.Username == "" {
			hr.Chat.Username = chatUsername(chats, info.ChatID)
		}
		out = append(out, hr)
	}
	return out, nil
}

// randomChannel runs the kkp search: recent video posts, 20–180s long.
func (s *service) randomChannel(ctx context.Context, ch channel, filters []string) ([]hit, error) {
	peer, info, ok := s.resolveCached(ctx, ch.Handle)
	if !ok {
		return nil, fmt.Errorf("cannot resolve %s", ch.Handle)
	}
	limit := kkpFetch
	if info.Megagroup {
		limit = kkpFetchGroup
	}
	req := &tg.MessagesSearchRequest{
		Peer:   peer,
		Q:      "",
		Limit:  limit,
		Filter: &tg.InputMessagesFilterVideo{},
	}
	res, err := s.searchOnce(ctx, req)
	if err != nil {
		return nil, err
	}
	msgs, _ := messagesOf(res)
	ref := chatRef{Username: info.Username, ChatID: info.ChatID}
	if ref.Username == "" {
		ref.Username = ch.Username
	}
	var out []hit
	for _, m := range msgs {
		if _, v, ok := videoOf(m); ok && !isAd(filters, m) && v.Duration >= 20 && v.Duration <= 180 {
			out = append(out, hit{Msg: m, Chat: ref, FileName: fileOf(m)})
		}
	}
	return out, nil
}

// searchLinked inspects a broadcast channel's discussion group for
// matching comments carrying videos, like searchInChannelWithLinkedGroup.
func (s *service) searchLinked(ctx context.Context, ch channel, query string, filters []string) ([]hit, error) {
	if ch.LinkedGroup == "" {
		return nil, nil
	}
	peer, info, ok := s.resolveCached(ctx, ch.LinkedGroup)
	if !ok {
		return nil, fmt.Errorf("cannot resolve %s", ch.LinkedGroup)
	}
	// find matching posts with replies
	res, err := s.searchOnce(ctx, &tg.MessagesSearchRequest{
		Peer: peer, Q: query, Limit: 100, Filter: &tg.InputMessagesFilterEmpty{},
	})
	if err != nil {
		return nil, err
	}
	msgs, _ := messagesOf(res)
	var out []hit
	for _, m := range msgs {
		if !matches(m, query) {
			continue
		}
		reps, ok := m.GetReplies()
		if !ok || reps.Replies == 0 {
			continue
		}
		comments, err := s.getReplies(ctx, peer, m.ID)
		if err != nil {
			continue
		}
		cmsgs, cchats := messagesOf(comments)
		ref := chatRef{Username: info.Username, ChatID: info.ChatID}
		for _, c := range cmsgs {
			if _, v, ok := videoOf(c); ok && v != nil && !isAd(filters, c) {
				hr := hit{Msg: c, Chat: ref, FileName: fileOf(c)}
				if hr.Chat.Username == "" {
					hr.Chat.Username = chatUsername(cchats, info.ChatID)
				}
				out = append(out, hr)
				break // first video comment per post, like the source
			}
		}
		if len(out) > 0 {
			return out, nil // source returns on the first post with videos
		}
	}
	// fallback: video posts in the group itself
	res, err = s.searchOnce(ctx, &tg.MessagesSearchRequest{
		Peer: peer, Q: query, Limit: 100, Filter: &tg.InputMessagesFilterVideo{},
	})
	if err != nil {
		return nil, err
	}
	msgs, _ = messagesOf(res)
	ref := chatRef{Username: info.Username, ChatID: info.ChatID}
	for _, m := range msgs {
		if _, _, ok := videoOf(m); ok && !isAd(filters, m) {
			out = append(out, hit{Msg: m, Chat: ref, FileName: fileOf(m)})
		}
	}
	return out, nil
}

// dedupeHits keeps one hit per message id per chat, first occurrence wins.
func dedupeHits(hits []hit) []hit {
	type key struct {
		chat int64
		id   int
	}
	seen := map[key]bool{}
	var out []hit
	for _, h := range hits {
		k := key{h.Chat.ChatID, h.Msg.ID}
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, h)
	}
	return out
}

// sortHits orders by score then duration, like the source's exact mode.
func sortHits(hits []hit) {
	for i := 1; i < len(hits); i++ {
		for j := i; j > 0 && hitLess(hits[j], hits[j-1]); j-- {
			hits[j], hits[j-1] = hits[j-1], hits[j]
		}
	}
}

func hitLess(a, b hit) bool {
	if a.Score != b.Score {
		return a.Score > b.Score
	}
	return durationOf(a.Msg) > durationOf(b.Msg)
}
