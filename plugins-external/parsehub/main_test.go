package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gotd/td/tg"
)

func TestExtractLinks(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"no links here", nil},
		{"看这个 https://twitter.com/user/status/123 很有趣", []string{"https://twitter.com/user/status/123"}},
		{"www.example.com/a", []string{"https://www.example.com/a"}},
		{"http://a.io/x", []string{"http://a.io/x"}},
		// trailing punctuation is trimmed
		{"(https://x.io/a)", []string{"https://x.io/a"}},
		{"链接：https://x.io/a。", []string{"https://x.io/a"}},
		{"https://x.io/a，https://y.io/b", []string{"https://x.io/a", "https://y.io/b"}},
		// duplicates collapse
		{"https://x.io/a https://x.io/a", []string{"https://x.io/a"}},
		// multiple, order kept
		{"https://a.io/1 then www.b.io/2", []string{"https://a.io/1", "https://www.b.io/2"}},
		// media text with t.me link should not be picked unless http/www
		{"tg://resolve?domain=x", nil},
	}
	for _, c := range cases {
		got := extractLinks(c.in)
		if len(got) != len(c.want) {
			t.Errorf("extractLinks(%q) = %v, want %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("extractLinks(%q)[%d] = %q, want %q", c.in, i, got[i], c.want[i])
			}
		}
	}
}

func TestMergeLinks(t *testing.T) {
	got := mergeLinks([]string{"https://a.io/1", "https://b.io/2"}, []string{"https://b.io/2", "https://c.io/3"})
	want := []string{"https://a.io/1", "https://b.io/2", "https://c.io/3"}
	if len(got) != len(want) {
		t.Fatalf("mergeLinks = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("mergeLinks = %v, want %v", got, want)
		}
	}
}

func msg(id int, text string, out bool, media tg.MessageMediaClass) *tg.Message {
	return &tg.Message{ID: id, Message: text, Out: out, Media: media}
}

func TestIsProgressText(t *testing.T) {
	progress := []string{
		"解 析 中",
		"▄▄ 解 析 中 ▄▄",
		"已有相同任务正在解析",
		"下 载 中 42%",
		"上 传 中",
		"\u3000解 析 中\u3000",
		"​解 析 中", // zero-width prefix
	}
	for _, s := range progress {
		if !isProgressText(s) {
			t.Errorf("isProgressText(%q) = false, want true", s)
		}
	}
	final := []string{
		"",
		"解析完成",
		"https://t.me/parsehubot",
		"上传失败: 无效链接",
		"解 析", // prefix must match fully
	}
	for _, s := range final {
		if isProgressText(s) {
			t.Errorf("isProgressText(%q) = true, want false", s)
		}
	}
}

func TestClassifyMessage(t *testing.T) {
	const baseline = 100
	media := &tg.MessageMediaDocument{}
	cases := []struct {
		name string
		m    *tg.Message
		want msgAction
	}{
		{"outgoing skipped", msg(105, "解 析 中", true, nil), actionSkip},
		{"before baseline skipped", msg(99, "hello", false, nil), actionSkip},
		{"at baseline skipped", msg(100, "hello", false, nil), actionSkip},
		{"progress text", msg(101, "▄▄ 解 析 中 ▄▄", false, nil), actionProgress},
		{"progress with media is final", msg(102, "解 析 中", false, media), actionFinal},
		{"plain text final", msg(103, "done: https://x.io", false, nil), actionFinal},
		{"media only final", msg(104, "", false, media), actionFinal},
		{"empty noise", msg(106, "", false, nil), actionNoise},
	}
	for _, c := range cases {
		if got := classifyMessage(c.m, baseline, 0); got != c.want {
			t.Errorf("%s: classifyMessage = %v, want %v", c.name, got, c.want)
		}
	}
	// Stale progress below a newer final is noise, not a blocker.
	if got := classifyMessage(msg(101, "解 析 中", false, nil), baseline, 103); got != actionNoise {
		t.Errorf("stale progress: classifyMessage = %v, want %v", got, actionNoise)
	}
	// Progress edited into media becomes final (same id re-classified).
	m := msg(101, "解 析 中", false, nil)
	if got := classifyMessage(m, baseline, 0); got != actionProgress {
		t.Fatalf("first poll: got %v, want progress", got)
	}
	m.Media = media
	if got := classifyMessage(m, baseline, 0); got != actionFinal {
		t.Fatalf("after edit to media: got %v, want final", got)
	}
}

// TestRelayNewDetection walks a canned bot-chat timeline: one link baseline,
// a progress placeholder that later gets edited into media, a second final
// text, plus noise. The pure loop state machine (progressIDs/finals maps and
// high-water mark) must end with exactly the two finals and an empty
// progress set — the same invariants relayParseResult relies on.
func TestRelayNewDetection(t *testing.T) {
	const baseline = 200
	media := &tg.MessageMediaDocument{}
	timeline := [][]*tg.Message{
		// poll 1: progress placeholder appears
		{msg(201, "解 析 中", false, nil), msg(200, "old", false, nil)},
		// poll 2: extra text final, progress still there
		{msg(202, "看视频 https://t.me/x", false, nil), msg(201, "解 析 中", false, nil)},
		// poll 3: progress edited into media (same id 201) — now final
		{msg(202, "看视频 https://t.me/x", false, nil), msg(201, "解 析 中", false, media)},
		// poll 4: noise only
		{msg(202, "看视频 https://t.me/x", false, nil), msg(201, "", false, media)},
	}
	progressIDs := map[int]bool{}
	finals := map[int]*tg.Message{}
	lastID := baseline
	for _, poll := range timeline {
		// relay processes oldest first
		for i := len(poll) - 1; i >= 0; i-- {
			m := poll[i]
			switch classifyMessage(m, baseline, maxKey(finals)) {
			case actionProgress:
				progressIDs[m.ID] = true
				lastID = max(lastID, m.ID)
			case actionNoise:
				lastID = max(lastID, m.ID)
			case actionFinal:
				delete(progressIDs, m.ID)
				finals[m.ID] = m
				lastID = max(lastID, m.ID)
			}
		}
	}
	if len(progressIDs) != 0 {
		t.Errorf("progressIDs not drained: %v", progressIDs)
	}
	if len(finals) != 2 {
		t.Fatalf("finals = %d messages, want 2", len(finals))
	}
	for _, id := range []int{201, 202} {
		if _, ok := finals[id]; !ok {
			t.Errorf("final %d missing", id)
		}
	}
	if lastID != 202 {
		t.Errorf("lastID = %d, want 202", lastID)
	}
}

func TestSortIDs(t *testing.T) {
	ids := []int{5, 1, 3}
	sortIDs(ids)
	if ids[0] != 1 || ids[1] != 3 || ids[2] != 5 {
		t.Errorf("sortIDs = %v", ids)
	}
}

func TestStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := &ParseHubPlugin{dir: dir}
	p.rel = &relay{started: true, ignoredUpTo: 777}
	if err := p.saveStateLocked(); err != nil {
		t.Fatal(err)
	}
	q := &ParseHubPlugin{dir: dir}
	if err := q.load(); err != nil {
		t.Fatal(err)
	}
	q.mu.Lock()
	got := q.rel.ignoredUpTo
	q.mu.Unlock()
	if got != 777 {
		t.Errorf("ignoredUpTo = %d, want 777", got)
	}

	// Missing file loads zero state without error.
	r := &ParseHubPlugin{dir: filepath.Join(dir, "missing")}
	if err := r.load(); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	rel := r.rel
	r.mu.Unlock()
	if rel == nil || rel.ignoredUpTo != 0 {
		t.Errorf("fresh state = %+v, want zero", rel)
	}
}

func TestWriteJSONAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := writeJSON(path, stateFile{Initialized: true, IgnoredUpToID: 5}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("perm = %v, want 0600", fi.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" || len(e.Name()) > 0 && e.Name()[0] == '.' && e.Name() != ".tmp" {
			if e.Name() != "state.json" && e.Name()[0] == '.' {
				t.Errorf("temp file left behind: %s", e.Name())
			}
		}
	}
}
