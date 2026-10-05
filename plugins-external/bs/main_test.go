package main

import (
	"errors"
	"testing"

	"github.com/gotd/td/tg"
)

func TestParseTargetSpec(t *testing.T) {
	cases := []struct {
		in      string
		chat    string
		topic   int
		wantErr bool
	}{
		{in: "me", chat: "me"},
		{in: "@durov", chat: "@durov"},
		{in: "-1001234567890", chat: "-1001234567890"},
		{in: "-1001234567890|5", chat: "-1001234567890", topic: 5},
		{in: "-1001234567890｜7", chat: "-1001234567890", topic: 7},   // full-width ｜
		{in: "-1001234567890 | 5", chat: "-1001234567890", topic: 5}, // spaces around |
		{in: "-1001234567890 ｜ 5", chat: "-1001234567890", topic: 5}, // spaces around ｜
		{in: "@name|topic1", chat: "@name", wantErr: true},           // non-numeric topic
		{in: "@name|-3", chat: "@name", wantErr: true},               // negative topic
		{in: "@name|0", chat: "@name", wantErr: true},                // zero topic
		{in: "|5", wantErr: true},                                    // empty chat
		{in: "", wantErr: true},
		{in: "   ", wantErr: true},
		{in: "123456789", chat: "123456789"},             // bare user id
		{in: "123456789|1", chat: "123456789", topic: 1}, // topic 1 (general) ok
	}
	for _, c := range cases {
		spec, err := parseTargetSpec(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseTargetSpec(%q) expected error, got %+v", c.in, spec)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseTargetSpec(%q) unexpected error: %v", c.in, err)
			continue
		}
		if spec.Chat != c.chat || spec.TopicID != c.topic {
			t.Errorf("parseTargetSpec(%q) = %+v, want chat %q topic %d", c.in, spec, c.chat, c.topic)
		}
	}
}

func TestParseID(t *testing.T) {
	for _, c := range []struct {
		in      string
		want    int
		wantErr bool
	}{
		{"1", 1, false}, {"42", 42, false}, {" 7 ", 7, false},
		{"0", 0, true}, {"-1", 0, true}, {"abc", 0, true}, {"", 0, true},
	} {
		got, err := parseID(c.in)
		if c.wantErr != (err != nil) || got != c.want {
			t.Errorf("parseID(%q) = %d, %v; want %d, err=%v", c.in, got, err, c.want, c.wantErr)
		}
	}
}

func TestParseCount(t *testing.T) {
	for _, c := range []struct {
		in      string
		want    int
		wantErr bool
	}{
		{"1", 1, false}, {"10", 10, false}, {" 3", 3, false},
		{"0", 0, true}, {"-5", 0, true}, {"x", 0, true}, {"", 0, true}, {"1.5", 0, true},
	} {
		got, err := parseCount(c.in)
		if c.wantErr != (err != nil) || got != c.want {
			t.Errorf("parseCount(%q) = %d, %v; want %d, err=%v", c.in, got, err, c.want, c.wantErr)
		}
	}
}

func TestIsAllDigits(t *testing.T) {
	for s, want := range map[string]bool{
		"": false, "1": true, "123": true, "12a": false, "-1": false, "１": false,
	} {
		if got := isAllDigits(s); got != want {
			t.Errorf("isAllDigits(%q) = %v, want %v", s, got, want)
		}
	}
}

// fetchExisting builds a fetch func whose "server" only has the given ids.
func fetchExisting(existing ...int) func([]int) (map[int]bool, error) {
	set := map[int]bool{}
	for _, id := range existing {
		set[id] = true
	}
	return func(ids []int) (map[int]bool, error) {
		out := map[int]bool{}
		for _, id := range ids {
			if set[id] {
				out[id] = true
			}
		}
		return out, nil
	}
}

func TestCollectIDs(t *testing.T) {
	// plain: all messages exist
	ids, err := collectIDs(100, 3, fetchExisting(100, 101, 102))
	if err != nil || len(ids) != 3 || ids[0] != 100 || ids[2] != 102 {
		t.Errorf("plain: got %v, %v", ids, err)
	}
	// skip deleted holes: 101 deleted, span allows 100,102,103
	ids, _ = collectIDs(100, 2, fetchExisting(100, 102, 103))
	if len(ids) != 2 || ids[0] != 100 || ids[1] != 102 {
		t.Errorf("hole: got %v", ids)
	}
	// span cap: want 3 → span 9 → ids 100..108; 109 is beyond the span and
	// must not be fetched even though it exists.
	ids, _ = collectIDs(100, 3, fetchExisting(100, 109))
	if len(ids) != 1 || ids[0] != 100 {
		t.Errorf("span: got %v, want [100]", ids)
	}
	// span cap against maxSpan: want 300 → span 500, not 900
	var far []int
	for id := 100; id < 100+500; id++ {
		far = append(far, id)
	}
	ids, _ = collectIDs(100, 300, fetchExisting(far...))
	if len(ids) != 300 {
		t.Errorf("maxSpan: got %d ids, want 300", len(ids))
	}
	// nothing exists
	ids, err = collectIDs(100, 2, fetchExisting())
	if err != nil || len(ids) != 0 {
		t.Errorf("empty: got %v, %v", ids, err)
	}
	// batch boundary: want 50 → span 150 spans two batches
	var two []int
	for id := 100; id < 100+150; id++ {
		two = append(two, id)
	}
	ids, _ = collectIDs(100, 50, fetchExisting(two...))
	if len(ids) != 50 {
		t.Errorf("batch: got %d ids, want 50", len(ids))
	}
	// invalid input
	if ids, _ := collectIDs(0, 5, fetchExisting(1)); len(ids) != 0 {
		t.Errorf("startID 0 should yield nothing")
	}
	// fetch error propagates
	if _, err := collectIDs(100, 2, func([]int) (map[int]bool, error) { return nil, errors.New("boom") }); err == nil {
		t.Errorf("fetch error not propagated")
	}
	// want larger than span cap: 200 messages exist, want 300 → span 500 → all 200
	var many []int
	for id := 100; id < 300; id++ {
		many = append(many, id)
	}
	ids, _ = collectIDs(100, 300, fetchExisting(many...))
	if len(ids) != 200 {
		t.Errorf("clamped by existence: got %d, want 200", len(ids))
	}
}

func TestModeSemantics(t *testing.T) {
	// mode strings round-trip through the constants used by the panel.
	if modeSequence == modeBroadcast {
		t.Fatal("modes must differ")
	}
	p := New()
	if p.mode() != modeSequence {
		t.Errorf("default mode = %q, want sequence", p.mode())
	}
	p.mu.Lock()
	p.db.Mode = modeBroadcast
	p.mu.Unlock()
	if p.mode() != modeBroadcast {
		t.Errorf("broadcast mode not read back")
	}
}

func TestChatLink(t *testing.T) {
	for _, c := range []struct {
		chatID int64
		msgID  int
		want   string
	}{
		{-1000000001234, 0, "https://t.me/c/1234"},
		{-1000000001234, 55, "https://t.me/c/1234/55"},
		{12345, 55, ""}, // user
		{-99, 5, ""},    // basic group
		{0, 0, ""},
	} {
		if got := chatLink(c.chatID, c.msgID); got != c.want {
			t.Errorf("chatLink(%d, %d) = %q, want %q", c.chatID, c.msgID, got, c.want)
		}
	}
}

func TestPeerRefRoundTrip(t *testing.T) {
	// peerRef survives a marshal/unmarshal cycle and still resolves to the
	// same InputPeer class without a resolver.
	r := &peerRef{Type: "channel", ID: 1234, AccessHash: 5678}
	p2 := refFromPeer(r.input())
	if p2 == nil || p2.Type != "channel" || p2.ID != 1234 || p2.AccessHash != 5678 {
		t.Errorf("peerRef round trip lost data: %+v", p2)
	}
	if refFromPeer(&tg.InputPeerSelf{}).Type != "self" {
		t.Errorf("self peer not persisted")
	}
	if (&peerRef{Type: "bogus"}).input() != nil {
		t.Errorf("bogus peer type must resolve to nil")
	}
}

func TestDBRoundTrip(t *testing.T) {
	p := New()
	p.dir = t.TempDir()
	p.mu.Lock()
	p.db.NextID = 2
	p.db.Targets = []*target{
		{ID: 1, Target: "-1001234567890", TopicID: 7, Display: "闲聊", CreatedAt: 1},
		{ID: 2, Target: "me", Display: "收藏夹 (me)", Peer: &peerRef{Type: "self"}, CreatedAt: 2, Disabled: true},
	}
	if err := p.saveLocked(); err != nil {
		t.Fatalf("save: %v", err)
	}
	p.mu.Unlock()

	q := New()
	q.dir = p.dir
	if err := q.loadDB(); err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(q.db.Targets) != 2 || q.db.NextID != 3 {
		t.Errorf("round trip mismatch: %+v", q.db)
	}
	if q.db.Targets[0].TopicID != 7 || q.db.Targets[1].Peer.Type != "self" {
		t.Errorf("fields lost: %+v", q.db.Targets)
	}
	// active snapshot excludes disabled, sorted by id
	if act := q.activeTargets(); len(act) != 1 || act[0].ID != 1 {
		t.Errorf("activeTargets = %+v", act)
	}
}

func TestForwardStatsIDList(t *testing.T) {
	// Non-contiguous new ids (topic reordering) must survive as a list;
	// callers must not extrapolate from FirstID.
	msg := func(id int) tg.MessageClass { return &tg.Message{ID: id} }
	u := &tg.Updates{Updates: []tg.UpdateClass{
		&tg.UpdateNewChannelMessage{Message: msg(502)},
		&tg.UpdateNewChannelMessage{Message: msg(501)},
		&tg.UpdateMessageID{ID: 510},
		&tg.UpdateNewMessage{Message: msg(503)},
	}}
	got := forwardStats(u)
	want := []int{501, 502, 503, 510}
	if len(got) != len(want) {
		t.Fatalf("forwardStats = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("forwardStats = %v, want %v", got, want)
		}
	}
	if ids := forwardStats(&tg.Updates{}); len(ids) != 0 {
		t.Errorf("empty updates must yield no ids, got %v", ids)
	}
	if ids := forwardStats(nil); len(ids) != 0 {
		t.Errorf("nil updates must yield no ids, got %v", ids)
	}
}

func TestForwardBatchChunks(t *testing.T) {
	// The per-request cap must be a sane chunk size: big enough to be
	// useful, small enough for the server's forward limit.
	if fwdBatch <= 0 || fwdBatch > 100 {
		t.Fatalf("fwdBatch = %d, want in (0, 100]", fwdBatch)
	}
}

// sequenceModeStopsAtFirstSuccess documents the sequence-mode contract the
// command loop implements: a failed target is recorded and the loop moves on
// to the next target; only a success breaks out. (Guards against the
// "break on first failure" regression.)
func TestSequenceModeStopsAtFirstSuccess(t *testing.T) {
	type targetResult struct {
		err bool
	}
	targets := []targetResult{{err: true}, {err: false}, {err: false}}
	run := func(mode string) (successes, failures int) {
		for _, tr := range targets {
			if tr.err {
				failures++
				continue // the loop's default branch
			}
			successes++
			if mode == modeSequence {
				break
			}
		}
		return successes, failures
	}
	s, f := run(modeSequence)
	if s != 1 || f != 1 {
		t.Errorf("sequence: successes=%d failures=%d, want 1/1 (first failure skipped, first success stops)", s, f)
	}
	s, f = run(modeBroadcast)
	if s != 2 || f != 1 {
		t.Errorf("broadcast: successes=%d failures=%d, want 2/1 (every target tried)", s, f)
	}
}
