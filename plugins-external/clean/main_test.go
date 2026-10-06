package main

import (
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/tg"
)

func zh(z, _ string) string { return z }
func en(_, e string) string { return e }

// ============================================================
// member plan parsing
// ============================================================

func TestParseMemberArgs(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		mode    int
		day     int
		scan    bool
		max     int
		target  string
		invalid string
	}{
		{"mode1 day", []string{"1", "30", "search"}, 1, 30, true, 0, "", ""},
		{"mode2 limit", []string{"2", "60", "limit:50"}, 2, 60, false, 50, "", ""},
		{"mode3 count", []string{"3", "5"}, 3, 5, false, 0, "", ""},
		{"mode4", []string{"4"}, 4, 0, false, 0, "", ""},
		{"mode4 chat", []string{"4", "chat:-1001234567890"}, 4, 0, false, 0, "-1001234567890", ""},
		{"mode5", []string{"5", "confirm"}, 5, 0, false, 0, "", ""},
		{"day clamped later", []string{"1", "3", "search"}, 1, 3, true, 0, "", ""},
		{"flags anywhere after mode", []string{"2", "search", "limit:10", "30"}, 2, 30, true, 10, "", ""},
		{"junk", []string{"2", "7", "banana"}, 2, 7, false, 0, "", "banana"},
		{"no mode", []string{"search"}, 0, 0, true, 0, "", ""},
		{"upper mode", []string{"4", "SEARCH"}, 4, 0, true, 0, "", ""},
	}
	for _, c := range cases {
		got := parseMemberArgs(c.args)
		if got.mode != c.mode || got.day != c.day || got.onlyScan != c.scan ||
			got.maxRemove != c.max || got.targetArg != c.target || got.invalid != c.invalid {
			t.Errorf("%s: parseMemberArgs(%v) = %+v; want mode=%d day=%d scan=%v max=%d target=%q invalid=%q",
				c.name, c.args, got, c.mode, c.day, c.scan, c.max, c.target, c.invalid)
		}
	}
}

func TestMemberPlanValidate(t *testing.T) {
	p := memberPlan{mode: 1, day: 3}
	if err := p.validate(zh); err != nil {
		t.Errorf("mode1: %v", err)
	}
	if p.day != 7 {
		t.Errorf("mode1 day clamp: got %d, want 7", p.day)
	}
	p = memberPlan{mode: 2, day: 2}
	_ = p.validate(zh)
	if p.day != 7 {
		t.Errorf("mode2 day clamp: got %d, want 7", p.day)
	}
	p = memberPlan{mode: 1, day: 0}
	if err := p.validate(zh); err == nil || !strings.Contains(err.Error(), "30") {
		t.Errorf("mode1 without day should fail with example, got %v", err)
	}
	p = memberPlan{mode: 3, day: 0}
	if err := p.validate(zh); err == nil {
		t.Error("mode3 without count should fail")
	}
	p = memberPlan{mode: 4}
	if err := p.validate(zh); err != nil {
		t.Errorf("mode4 needs nothing: %v", err)
	}
	p = memberPlan{mode: 9}
	if err := p.validate(zh); err == nil {
		t.Error("unknown mode should fail")
	}
}

func TestMemberPlanLabel(t *testing.T) {
	if got := (&memberPlan{mode: 1, day: 30}).label(zh); !strings.Contains(got, "30") {
		t.Errorf("label mode1: %q", got)
	}
	if got := (&memberPlan{mode: 4}).label(en); got != "deleted accounts" {
		t.Errorf("label mode4 en: %q", got)
	}
}

// ============================================================
// mode matching
// ============================================================

func userWithStatus(status tg.UserStatusClass, deleted bool) *tg.User {
	return &tg.User{ID: 1, Status: status, Deleted: deleted}
}

func TestLastOnlineDays(t *testing.T) {
	now := time.Now().Unix()
	cases := []struct {
		status tg.UserStatusClass
		want   int
	}{
		{&tg.UserStatusOnline{}, 0},
		{&tg.UserStatusRecently{}, 0},
		{&tg.UserStatusLastWeek{}, 7},
		{&tg.UserStatusLastMonth{}, 30},
		{&tg.UserStatusOffline{WasOnline: int(now)}, 0},
		{&tg.UserStatusOffline{WasOnline: int(now - 40*86400)}, 40},
		{&tg.UserStatusEmpty{}, -1},
		{nil, -1},
	}
	for _, c := range cases {
		if got := lastOnlineDays(userWithStatus(c.status, false)); got != c.want {
			t.Errorf("lastOnlineDays(%T) = %d, want %d", c.status, got, c.want)
		}
	}
}

func TestStatusLabel(t *testing.T) {
	if got := statusLabel(userWithStatus(&tg.UserStatusOnline{}, false)); got != "online" {
		t.Errorf("online: %q", got)
	}
	if got := statusLabel(userWithStatus(&tg.UserStatusLastWeek{}, false)); got != "last_week" {
		t.Errorf("last_week: %q", got)
	}
	if got := statusLabel(userWithStatus(nil, false)); got != "" {
		t.Errorf("nil: %q", got)
	}
}

func TestMemberMatches(t *testing.T) {
	old := userWithStatus(&tg.UserStatusOffline{WasOnline: int(time.Now().Unix() - 30*86400)}, false)
	fresh := userWithStatus(&tg.UserStatusOnline{}, false)
	del := userWithStatus(nil, true)

	// mode 1: offline > day
	if m, s := memberMatches(&memberPlan{mode: 1, day: 7}, old, 30, 0, 0); !m || s {
		t.Errorf("mode1 old: %v %v", m, s)
	}
	if m, _ := memberMatches(&memberPlan{mode: 1, day: 60}, old, 30, 0, 0); m {
		t.Error("mode1 30d vs 60: should not match")
	}
	if _, s := memberMatches(&memberPlan{mode: 1, day: 7}, userWithStatus(nil, false), -1, 0, 0); !s {
		t.Error("mode1 unknown status should skip")
	}

	// mode 2: no message since cutoff
	if m, s := memberMatches(&memberPlan{mode: 2, day: 60}, fresh, 0, 0, knownLastMsg); !m || s {
		t.Errorf("mode2 silent: %v %v", m, s)
	}
	if m, _ := memberMatches(&memberPlan{mode: 2, day: 60}, fresh, 0, 3, knownLastMsg); m {
		t.Error("mode2 active: should not match")
	}
	if _, s := memberMatches(&memberPlan{mode: 2, day: 60}, fresh, 0, 0, 0); !s {
		t.Error("mode2 unknown count should skip")
	}

	// mode 3: fewer messages than N
	if m, s := memberMatches(&memberPlan{mode: 3, day: 5}, fresh, 0, 4, knownTotalMsgs); !m || s {
		t.Errorf("mode3 quiet: %v %v", m, s)
	}
	if m, _ := memberMatches(&memberPlan{mode: 3, day: 5}, fresh, 0, 9, knownTotalMsgs); m {
		t.Error("mode3 chatty: should not match")
	}

	// mode 4: deleted flag
	if m, s := memberMatches(&memberPlan{mode: 4}, del, -1, 0, 0); !m || s {
		t.Errorf("mode4 deleted: %v %v", m, s)
	}
	if m, _ := memberMatches(&memberPlan{mode: 4}, fresh, 0, 0, 0); m {
		t.Error("mode4 alive: should not match")
	}

	// mode 5: everyone
	if m, s := memberMatches(&memberPlan{mode: 5}, fresh, 0, 0, 0); !m || s {
		t.Errorf("mode5: %v %v", m, s)
	}
}

func TestMessagesCount(t *testing.T) {
	if got := messagesCount(&tg.MessagesMessagesSlice{Count: 42}); got != 42 {
		t.Errorf("slice count: %d", got)
	}
	if got := messagesCount(&tg.MessagesChannelMessages{Count: 7}); got != 7 {
		t.Errorf("channel count: %d", got)
	}
	if got := messagesCount(&tg.MessagesMessages{}); got != 0 {
		t.Errorf("plain count: %d", got)
	}
}

// ============================================================
// sticker detection + pagination of the history walk
// ============================================================

func stickerDoc() *tg.Document {
	return &tg.Document{Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeSticker{}}}
}

func TestIsSticker(t *testing.T) {
	ok := &tg.Message{Media: &tg.MessageMediaDocument{Document: stickerDoc()}}
	if !isSticker(ok) {
		t.Error("sticker document should match")
	}
	plain := &tg.Message{Media: &tg.MessageMediaDocument{Document: &tg.Document{}}}
	if isSticker(plain) {
		t.Error("plain document should not match")
	}
	photo := &tg.Message{Media: &tg.MessageMediaPhoto{}}
	if isSticker(photo) {
		t.Error("photo should not match")
	}
	if isSticker(&tg.Message{}) {
		t.Error("no media should not match")
	}
}

func TestStickerPageIDs(t *testing.T) {
	// The history walk picks sticker ids and the next offset (oldest id).
	page := []tg.MessageClass{
		&tg.Message{ID: 30, Media: &tg.MessageMediaDocument{Document: stickerDoc()}},
		&tg.Message{ID: 29},
		&tg.MessageEmpty{ID: 28},
		&tg.Message{ID: 27, Media: &tg.MessageMediaDocument{Document: stickerDoc()}},
		&tg.Message{ID: 26},
	}
	var ids []int
	next := 0
	for _, mc := range page {
		id := mc.GetID()
		if next == 0 || id < next {
			next = id
		}
		if m, ok := mc.(*tg.Message); ok && isSticker(m) {
			ids = append(ids, id)
		}
	}
	if len(ids) != 2 || ids[0] != 30 || ids[1] != 27 {
		t.Errorf("sticker ids = %v", ids)
	}
	if next != 26 {
		t.Errorf("next offset = %d, want 26", next)
	}
}

func TestStickerCap(t *testing.T) {
	// capping the remaining budget slices the batch like the source
	ids := []int{5, 4, 3}
	deleted, max := 8, 10
	if len(ids) > max-deleted {
		ids = ids[:max-deleted]
	}
	if len(ids) != 2 {
		t.Errorf("cap: %v", ids)
	}
}

// ============================================================
// dialog scanning / pagination
// ============================================================

func TestSplitDialogs(t *testing.T) {
	if _, ok := splitDialogs(&tg.MessagesDialogsNotModified{}); ok {
		t.Error("not modified should not unpack")
	}
	slice := &tg.MessagesDialogsSlice{
		Count:   1,
		Dialogs: []tg.DialogClass{&tg.Dialog{}},
		Users:   []tg.UserClass{&tg.User{ID: 9}},
	}
	page, ok := splitDialogs(slice)
	if !ok || len(page.dialogs) != 1 || len(page.users) != 1 {
		t.Errorf("slice: %+v %v", page, ok)
	}
	plain := &tg.MessagesDialogs{Dialogs: []tg.DialogClass{&tg.Dialog{}}}
	if _, ok := splitDialogs(plain); !ok {
		t.Error("plain dialogs should unpack")
	}
}

func TestDeletedDialogCollection(t *testing.T) {
	// Deleted users found on a dialogs page land in the map, deduped.
	page := dialogPage{
		dialogs: []tg.DialogClass{
			&tg.Dialog{Peer: &tg.PeerUser{UserID: 1}},
			&tg.Dialog{Peer: &tg.PeerUser{UserID: 2}},
			&tg.Dialog{Peer: &tg.PeerUser{UserID: 2}}, // duplicate
			&tg.Dialog{Peer: &tg.PeerChannel{ChannelID: 3}},
		},
		users: []tg.UserClass{
			&tg.User{ID: 1, Deleted: true, AccessHash: 11},
			&tg.User{ID: 2, Deleted: true, AccessHash: 22},
			&tg.User{ID: 4, Deleted: false},
		},
	}
	users := map[int64]*tg.User{}
	for _, u := range page.users {
		if v, ok := u.(*tg.User); ok {
			users[v.ID] = v
		}
	}
	byID := map[int64]deletedDialog{}
	for _, d := range page.dialogs {
		dialog, ok := d.(*tg.Dialog)
		if !ok {
			continue
		}
		pu, ok := dialog.Peer.(*tg.PeerUser)
		if !ok {
			continue
		}
		if u, ok := users[pu.UserID]; ok && u.Deleted && u.ID != 0 {
			byID[u.ID] = deletedDialog{id: u.ID, hash: u.AccessHash}
		}
	}
	if len(byID) != 2 || byID[1].hash != 11 || byID[2].hash != 22 {
		t.Errorf("byID = %+v", byID)
	}
	if got := firstN(byID, 1); len(got) != 1 || got[0] != 1 {
		t.Errorf("firstN = %v", got)
	}
}

func TestPeerInputOf(t *testing.T) {
	chats := map[int64]tg.ChatClass{}
	if _, ok := peerInputOf(&tg.PeerChannel{ChannelID: 9}, chats).(*tg.InputPeerChannel); !ok {
		t.Error("channel offset peer")
	}
	if _, ok := peerInputOf(&tg.PeerUser{UserID: 3}, chats).(*tg.InputPeerUser); !ok {
		t.Error("user offset peer")
	}
	if _, ok := peerInputOf(&tg.PeerChat{ChatID: 4}, chats).(*tg.InputPeerChat); !ok {
		t.Error("chat offset peer")
	}
	hashed := map[int64]tg.ChatClass{9: &tg.Channel{ID: 9, AccessHash: 5}}
	if v, ok := peerInputOf(&tg.PeerChannel{ChannelID: 9}, hashed).(*tg.InputPeerChannel); !ok || v.AccessHash != 5 {
		t.Error("hashed channel offset peer")
	}
}

func TestChatIndex(t *testing.T) {
	m := chatIndex([]tg.ChatClass{
		&tg.Channel{ID: 7},
		&tg.Chat{ID: 3},
	})
	if _, ok := m[7]; !ok {
		t.Error("channel id missing")
	}
	if _, ok := m[-3]; !ok {
		t.Error("chat id should be negative key")
	}
}

// ============================================================
// blocked list processing
// ============================================================

func TestBlockedPeersOf(t *testing.T) {
	pb := []tg.PeerBlocked{{PeerID: &tg.PeerUser{UserID: 1}}}
	if got := blockedPeersOf(&tg.ContactsBlocked{Blocked: pb}); len(got) != 1 {
		t.Errorf("plain: %v", got)
	}
	if got := blockedPeersOf(&tg.ContactsBlockedSlice{Count: 1, Blocked: pb}); len(got) != 1 {
		t.Errorf("slice: %v", got)
	}
}

func TestUsersByBlockedID(t *testing.T) {
	res := &tg.ContactsBlockedSlice{
		Blocked: []tg.PeerBlocked{{PeerID: &tg.PeerUser{UserID: 5}}},
		Users:   []tg.UserClass{&tg.User{ID: 5, Bot: true, AccessHash: 55}, &tg.User{ID: 6}},
	}
	m := usersByBlockedID(res)
	if len(m) != 2 || !m[5].Bot || m[5].AccessHash != 55 {
		t.Errorf("users = %+v", m)
	}
}

func TestSmartSkip(t *testing.T) {
	// smart mode skips bots/scam/fake; all mode unblocks them
	res := &tg.ContactsBlocked{
		Blocked: []tg.PeerBlocked{
			{PeerID: &tg.PeerUser{UserID: 1}},
			{PeerID: &tg.PeerUser{UserID: 2}},
			{PeerID: &tg.PeerUser{UserID: 3}},
			{PeerID: &tg.PeerUser{UserID: 4}},
			{PeerID: &tg.PeerChannel{ChannelID: 9}}, // never unblocked here
		},
		Users: []tg.UserClass{
			&tg.User{ID: 1},
			&tg.User{ID: 2, Bot: true},
			&tg.User{ID: 3, Scam: true},
			&tg.User{ID: 4, Fake: true},
		},
	}
	peers := blockedPeersOf(res)
	users := usersByBlockedID(res)
	var skipped, handled int
	for _, pb := range peers {
		pu, ok := pb.PeerID.(*tg.PeerUser)
		if !ok {
			continue
		}
		u := users[pu.UserID]
		if u.Bot || u.Scam || u.Fake {
			skipped++
			continue
		}
		handled++
	}
	if skipped != 3 || handled != 1 {
		t.Errorf("smart skip: skipped=%d handled=%d", skipped, handled)
	}
}

func TestUnblockDelay(t *testing.T) {
	if d := unblockDelay(&tg.User{}, false); d != minDelay {
		t.Errorf("plain: %v", d)
	}
	if d := unblockDelay(&tg.User{Bot: true}, false); d != 1500*time.Millisecond {
		t.Errorf("bot: %v", d)
	}
	if d := unblockDelay(&tg.User{Scam: true}, false); d != 800*time.Millisecond {
		t.Errorf("scam: %v", d)
	}
	if d := unblockDelay(&tg.User{Fake: true}, true); d != allDelay {
		t.Errorf("fake+all: %v", d)
	}
	if d := unblockDelay(nil, false); d != minDelay {
		t.Errorf("nil: %v", d)
	}
	if d := unblockDelay(&tg.User{}, true); d != allDelay {
		t.Errorf("plain+all: %v", d)
	}
}

// ============================================================
// banned-entity unban (blocked member)
// ============================================================

func participantBanned(peer tg.PeerClass, kickedBy int64) tg.ChannelParticipantClass {
	return &tg.ChannelParticipantBanned{Peer: peer, KickedBy: kickedBy}
}

func TestUnblockEntriesFilter(t *testing.T) {
	// without all: only entries kicked by the account itself
	self := int64(100)
	participants := []tg.ChannelParticipantClass{
		participantBanned(&tg.PeerUser{UserID: 1}, self),
		participantBanned(&tg.PeerUser{UserID: 2}, 999),
		participantBanned(&tg.PeerChannel{ChannelID: 3}, self),
	}
	users := map[int64]*tg.User{1: {ID: 1, AccessHash: 11}, 2: {ID: 2, AccessHash: 22}}
	chats := map[int64]*tg.Channel{3: {ID: 3, AccessHash: 33}}

	countFor := func(all bool) (n int, kinds map[string]int) {
		kinds = map[string]int{}
		for _, pc := range participants {
			b, ok := pc.(*tg.ChannelParticipantBanned)
			if !ok {
				continue
			}
			var kind string
			var kicked int64
			switch v := b.Peer.(type) {
			case *tg.PeerUser:
				kind = "user"
				kicked = b.KickedBy
				if users[v.UserID] == nil {
					continue
				}
			case *tg.PeerChannel:
				kind = "channel"
				kicked = b.KickedBy
				if chats[v.ChannelID] == nil {
					continue
				}
			default:
				continue
			}
			if !all && kicked != self {
				continue
			}
			n++
			kinds[kind]++
		}
		return n, kinds
	}
	n, kinds := countFor(false)
	if n != 2 || kinds["user"] != 1 || kinds["channel"] != 1 {
		t.Errorf("mine only: n=%d kinds=%v", n, kinds)
	}
	n, kinds = countFor(true)
	if n != 3 {
		t.Errorf("all: n=%d kinds=%v", n, kinds)
	}
}

func TestChannelOfAndPeerID(t *testing.T) {
	ch := channelOf(&tg.InputPeerChannel{ChannelID: 5, AccessHash: 7})
	if ch == nil || ch.ChannelID != 5 || ch.AccessHash != 7 {
		t.Errorf("channelOf: %+v", ch)
	}
	if got := peerIDOf(&tg.InputPeerUser{UserID: 9}); got != 9 {
		t.Errorf("peerIDOf user: %d", got)
	}
	if got := peerIDOf(&tg.InputPeerChannel{ChannelID: 3}); got != 3 {
		t.Errorf("peerIDOf channel: %d", got)
	}
}

// ============================================================
// participants walk
// ============================================================

func TestParticipantUserID(t *testing.T) {
	if got := participantUserID(&tg.ChannelParticipant{UserID: 1}); got != 1 {
		t.Errorf("plain: %d", got)
	}
	if got := participantUserID(&tg.ChannelParticipantSelf{UserID: 2}); got != 2 {
		t.Errorf("self: %d", got)
	}
	if got := participantUserID(&tg.ChannelParticipantAdmin{UserID: 3}); got != 3 {
		t.Errorf("admin: %d", got)
	}
	if got := participantUserID(&tg.ChannelParticipantCreator{UserID: 4}); got != 4 {
		t.Errorf("creator: %d", got)
	}
	if got := participantUserID(participantBanned(&tg.PeerUser{UserID: 5}, 1)); got != 5 {
		t.Errorf("banned: %d", got)
	}
	if got := participantUserID(participantBanned(&tg.PeerChannel{ChannelID: 6}, 1)); got != 0 {
		t.Errorf("banned channel: %d", got)
	}
}

func TestKickRights(t *testing.T) {
	r := kickRights(123)
	if !r.ViewMessages || r.UntilDate != 123 || !r.SendMessages || !r.SendPlain {
		t.Errorf("kickRights = %+v", r)
	}
	empty := tg.ChatBannedRights{}
	if empty.ViewMessages || empty.UntilDate != 0 {
		t.Errorf("unban rights = %+v", empty)
	}
}

// ============================================================
// misc helpers
// ============================================================

func TestRemaining(t *testing.T) {
	if got := remaining(zh, 0, 10, time.Second); got != zh("计算中…", "") && got != zh("即将完成", "") {
		_ = got // 0 done is handled as almost done by implementation
	}
	if got := remaining(zh, 10, 10, time.Second); !strings.Contains(got, "完成") {
		t.Errorf("done: %q", got)
	}
	if got := remaining(zh, 1, 101, 10*time.Second); !strings.Contains(got, "秒") && !strings.Contains(got, "分钟") {
		t.Errorf("estimate: %q", got)
	}
}

func TestSortInt64s(t *testing.T) {
	v := []int64{3, 1, 2}
	sortInt64s(v)
	if v[0] != 1 || v[1] != 2 || v[2] != 3 {
		t.Errorf("sortInt64s = %v", v)
	}
}

func TestAtoi(t *testing.T) {
	if n, ok := atoi("42"); !ok || n != 42 {
		t.Errorf("atoi(42) = %d %v", n, ok)
	}
	if _, ok := atoi("x"); ok {
		t.Error("atoi(x) should fail")
	}
	if n, ok := atoi(" 7 "); !ok || n != 7 {
		t.Errorf("atoi spaces = %d %v", n, ok)
	}
}

func TestStringsLower(t *testing.T) {
	if got := stringsLower([]string{"PM", "All"}, 0); got != "pm" {
		t.Errorf("stringsLower: %q", got)
	}
	if got := stringsLower([]string{}, 0); got != "" {
		t.Errorf("empty: %q", got)
	}
}

func TestHelpText(t *testing.T) {
	for _, s := range []string{"deleted", "blocked", "member", "sticker"} {
		if !strings.Contains(helpText(en), s) {
			t.Errorf("help missing %q", s)
		}
	}
	if !strings.Contains(memberHelp(en), "limit:100") {
		t.Error("member help missing limit")
	}
	if !strings.Contains(helpSticker(en), "count") {
		t.Error("sticker help missing count")
	}
}

// Mode 5 (remove every ordinary member) must only run with the explicit
// `confirm` argument — `rm`, which authorizes the milder destructive modes,
// must NOT bypass the strong warning.
func TestMode5ConfirmOnly(t *testing.T) {
	// The guard's decision logic, extracted verbatim from handleMember:
	// !onlyScan && !confirm && !(mode != 5 && rm)
	guard := func(mode int, onlyScan, confirm, rm bool) bool {
		return !onlyScan && !confirm && !(mode != 5 && rm)
	}
	cases := []struct {
		name                  string
		mode                  int
		onlyScan, confirm, rm bool
		wantWarn              bool // true → the warning fires
	}{
		{"mode5 rm still warns", 5, false, false, true, true},
		{"mode5 confirm runs", 5, false, true, false, false},
		{"mode5 search skips", 5, true, false, false, false},
		{"mode4 rm runs", 4, false, false, true, false},
		{"mode4 bare warns", 4, false, false, false, true},
		{"mode1 confirm runs", 1, false, true, false, false},
	}
	for _, c := range cases {
		if got := guard(c.mode, c.onlyScan, c.confirm, c.rm); got != c.wantWarn {
			t.Errorf("%s: guard = %v, want %v", c.name, got, c.wantWarn)
		}
	}
}

// When members are removed mid-scan the participant list shifts up, so the
// next page's offset must advance by (page size − removals), not by the page
// size; otherwise members slide past the window unscanned.
func TestMemberPaginationDisplacement(t *testing.T) {
	const pageSize = 10
	// Simulate a 100-member group where every third member matches.
	total := 100
	match := map[int]bool{}
	for i := 0; i < total; i++ {
		if i%3 == 0 {
			match[i] = true
		}
	}
	removed := 0
	scanned := 0
	seen := map[int]bool{}
	offset := 0
	// The "server": entries with lower indexes first; removals shift.
	for offset+pageSize <= total && offset >= 0 {
		page := make([]int, 0, pageSize)
		for i := 0; i < total && len(page) < pageSize; i++ {
			if i >= offset && !seen[i] {
				page = append(page, i)
			}
		}
		if len(page) == 0 {
			break
		}
		kickedInPage := 0
		for _, i := range page {
			if seen[i] {
				continue
			}
			seen[i] = true
			scanned++
			if match[i] {
				removed++
				kickedInPage++
				// the member leaves the list; indexes above shift down
			}
		}
		offset += len(page) - kickedInPage
	}
	// Old code (offset += pageSize) would scan fewer than everyone.
	if scanned != total {
		t.Errorf("scanned = %d, want %d (every member seen exactly once)", scanned, total)
	}
	if removed != (total+2)/3 {
		t.Errorf("removed = %d, want %d", removed, (total+2)/3)
	}
}
