package main

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/tgerr"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

func TestSteps(t *testing.T) {
	cases := []struct {
		in  string
		raw []string // expected sequence "text|cursor"
	}{
		{"", []string{"|1"}},
		{"a", []string{"|1", "a|1", "a|0"}},
		{"ab", []string{"|1", "a|1", "a|0", "ab|1", "ab|0"}},
		// runes, not bytes: one frame per code point, like the source's for..of
		{"你", []string{"|1", "你|1", "你|0"}},
		{"👍🏽", []string{"|1", "👍|1", "👍|0", "👍🏽|1", "👍🏽|0"}},
	}
	for _, c := range cases {
		var got []string
		for _, f := range steps(c.in) {
			got = append(got, f.text+"|"+map[bool]string{true: "1", false: "0"}[f.cursor])
		}
		if !reflect.DeepEqual(got, c.raw) {
			t.Errorf("steps(%q) = %v, want %v", c.in, got, c.raw)
		}
	}
}

func TestStepsLastFrameIsFullText(t *testing.T) {
	for _, in := range []string{"hello", "你好世界", "a b", strings.Repeat("长", 50)} {
		fr := steps(in)
		last := fr[len(fr)-1]
		if last.text != in || last.cursor {
			t.Errorf("steps(%q) last frame = %+v, want clean full text", in, last)
		}
	}
}

func TestFrameMD(t *testing.T) {
	cases := []struct {
		f    frame
		want string
	}{
		{frame{cursor: true}, "█"},
		{frame{text: "hi"}, "hi"},
		{frame{text: "hi", cursor: true}, "hi█"},
		{frame{text: "a*b"}, `a\*b`},
		{frame{text: "a*b", cursor: true}, `a\*b█`},
		{frame{text: "你好_世界", cursor: true}, `你好\_世界█`},
		{frame{text: "`code`", cursor: true}, "\\`code\\`█"},
		{frame{text: "[a](b)", cursor: true}, `\[a\](b)█`},
	}
	for _, c := range cases {
		if got := frameMD(c.f); got != c.want {
			t.Errorf("frameMD(%+v) = %q, want %q", c.f, got, c.want)
		}
	}
}

func TestTooLong(t *testing.T) {
	if tooLong(strings.Repeat("x", maxRunes)) {
		t.Error("exactly maxRunes runes must be allowed")
	}
	if !tooLong(strings.Repeat("x", maxRunes+1)) {
		t.Error("maxRunes+1 must be rejected")
	}
	if tooLong(strings.Repeat("你", maxRunes)) {
		t.Error("CJK runes count as one each")
	}
	if !tooLong(strings.Repeat("你", maxRunes+1)) {
		t.Error("CJK over the limit must be rejected")
	}
}

func TestIntervalMs(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"50", 50 * time.Millisecond},
		{"25", 25 * time.Millisecond},
		{"200", 200 * time.Millisecond},
		{"", 50 * time.Millisecond},       // unset → source default
		{"abc", 50 * time.Millisecond},    // garbage → default
		{"0", 50 * time.Millisecond},      // zero → default
		{"-5", 50 * time.Millisecond},     // negative → default
		{"1", minInterval},                // clamped to the floor
		{" 100 ", 100 * time.Millisecond}, // trimmed
		{"999999", 999999 * time.Millisecond},
	}
	for _, c := range cases {
		if got := intervalMs(c.in); got != c.want {
			t.Errorf("intervalMs(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestAutoPick(t *testing.T) {
	prefixes := []string{".", "。", "!"}
	cases := []struct {
		name string
		sig  autoSignal
		want bool
	}{
		{"plain own message", autoSignal{out: true, text: "hello"}, true},
		{"cjk two runes", autoSignal{out: true, text: "你好"}, true},
		{"single char skipped", autoSignal{out: true, text: "a"}, false},
		{"single cjk skipped", autoSignal{out: true, text: "你"}, false},
		{"space-padded single char", autoSignal{out: true, text: "  a  "}, false},
		{"edited skipped", autoSignal{edited: true, out: true, text: "hello"}, false},
		{"incoming skipped", autoSignal{out: false, text: "hello"}, false},
		{"media skipped", autoSignal{out: true, media: true, text: "hello"}, false},
		{"command skipped", autoSignal{out: true, text: ".status"}, false},
		{"cjk dot command", autoSignal{out: true, text: "。status"}, false},
		{"bang command", autoSignal{out: true, text: "!ping"}, false},
		{"empty skipped", autoSignal{out: true, text: ""}, false},
		{"spaces skipped", autoSignal{out: true, text: "   "}, false},
	}
	for _, c := range cases {
		if got := autoPick(c.sig, prefixes); got != c.want {
			t.Errorf("%s: autoPick(%+v) = %v, want %v", c.name, c.sig, got, c.want)
		}
	}
}

func TestMetadata(t *testing.T) {
	if Metadata.Name != "teletype" || New().Name() != "teletype" {
		t.Fatalf("name mismatch: %q vs %q", Metadata.Name, New().Name())
	}
}

func TestSleepCtx(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	if err := sleepCtx(ctx, time.Millisecond); err != nil {
		t.Errorf("short sleep = %v, want nil", err)
	}
	cancel()
	if err := sleepCtx(ctx, time.Minute); err != context.Canceled {
		t.Errorf("cancelled sleep = %v, want context.Canceled", err)
	}
}

// recEdit records every frame edit and feeds the run a script of errors,
// one per edit; once the script runs dry every edit succeeds.
type recEdit struct {
	mu     sync.Mutex
	got    []string
	script []error
}

func (r *recEdit) edit(_ context.Context, md string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, md)
	if i := len(r.got) - 1; i < len(r.script) {
		return r.script[i]
	}
	return nil
}

func (r *recEdit) edits() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.got...)
}

// noSleep makes every wait return instantly; a passed d is recorded.
func noSleep(d *time.Duration) func(context.Context, time.Duration) error {
	return func(_ context.Context, got time.Duration) error {
		*d = got
		return nil
	}
}

func TestRunFramesHappyPath(t *testing.T) {
	r := &recEdit{}
	if err := runFrames(context.Background(), r.edit, steps("ab"), 0, noSleep(new(time.Duration))); err != nil {
		t.Fatalf("runFrames: %v", err)
	}
	want := []string{"█", "a█", "a", "ab█", "ab"}
	if got := r.edits(); !reflect.DeepEqual(got, want) {
		t.Errorf("edits = %v, want %v", got, want)
	}
}

func TestRunFramesRetriesShortFloodWait(t *testing.T) {
	// First edit of frame 1 floods for 5s; the run must sleep it out and
	// retry the same frame, then finish normally.
	r := &recEdit{script: []error{tgerr.New(420, "FLOOD_WAIT_5")}}
	var mu sync.Mutex
	var sleeps []time.Duration
	sleep := func(_ context.Context, d time.Duration) error {
		mu.Lock()
		defer mu.Unlock()
		sleeps = append(sleeps, d)
		return nil
	}
	if err := runFrames(context.Background(), r.edit, steps("ab"), 0, sleep); err != nil {
		t.Fatalf("runFrames: %v", err)
	}
	got := r.edits()
	if len(got) != 6 || got[1] != "█" || got[2] != "a█" {
		t.Errorf("edits = %v, want the flooded frame retried", got)
	}
	mu.Lock()
	defer mu.Unlock()
	floodSlept := false
	for _, d := range sleeps {
		if d == 6*time.Second { // d + 1s
			floodSlept = true
		}
	}
	if !floodSlept {
		t.Errorf("sleeps = %v, want one 6s flood wait", sleeps)
	}
}

func TestRunFramesFloodBudgetExhausted(t *testing.T) {
	// Endless short floods: after floodBudget sleeps the run gives up and
	// closes the message out with the full text.
	flood := tgerr.New(420, "FLOOD_WAIT_2")
	var mu sync.Mutex
	var got []string
	ed := func(_ context.Context, md string) error {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, md)
		return flood
	}
	err := runFrames(context.Background(), ed, steps("abc"), 0, noSleep(new(time.Duration)))
	if err == nil {
		t.Fatal("expected error after the flood budget ran out")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) == 0 || got[len(got)-1] != "abc" {
		t.Errorf("last edit = %v, want the complete text as closeout", got)
	}
	// The first frame edit retries floodBudget times (each sleep followed
	// by one more failed try), then one closeout attempt on the final text
	// (which also floods — that error is what surfaces).
	if len(got) != floodBudget+2 {
		t.Errorf("edits = %d, want %d (budget retries + closeout)", len(got), floodBudget+2)
	}
}

func TestRunFramesLongFloodAbortsAndClosesOut(t *testing.T) {
	// A FLOOD_WAIT over maxFloodWait is not slept out; the run aborts but
	// still lands the final frame.
	r := &recEdit{script: []error{nil, tgerr.New(420, "FLOOD_WAIT_600"), nil}}
	err := runFrames(context.Background(), r.edit, steps("ab"), 0, noSleep(new(time.Duration)))
	if err == nil {
		t.Fatal("expected the long flood wait to abort the run")
	}
	got := r.edits()
	if len(got) == 0 || got[len(got)-1] != "ab" {
		t.Errorf("last edit = %v, want the complete text as closeout", got)
	}
}

func TestRunFramesHardErrorClosesOut(t *testing.T) {
	r := &recEdit{script: []error{nil, nil, tgerr.New(400, "MESSAGE_ID_INVALID"), nil}}
	err := runFrames(context.Background(), r.edit, steps("ab"), 0, noSleep(new(time.Duration)))
	if err == nil {
		t.Fatal("expected the edit error to surface")
	}
	got := r.edits()
	if len(got) == 0 || got[len(got)-1] != "ab" {
		t.Errorf("last edit = %v, want the complete text as closeout", got)
	}
}

func TestRunFramesMessageNotModifiedContinues(t *testing.T) {
	// Duplicate frames are skipped without an edit; a MESSAGE_NOT_MODIFIED
	// answer is ignored and the run still finishes with the full text.
	r := &recEdit{script: []error{tgerr.New(400, "MESSAGE_NOT_MODIFIED")}}
	if err := runFrames(context.Background(), r.edit, steps("ab"), 0, noSleep(new(time.Duration))); err != nil {
		t.Fatalf("runFrames: %v", err)
	}
	got := r.edits()
	if len(got) == 0 || got[len(got)-1] != "ab" {
		t.Errorf("last edit = %v, want the complete text", got)
	}
}

func TestRunFramesContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := &recEdit{}
	cancel()
	if err := runFrames(ctx, r.edit, steps("ab"), 0, func(context.Context, time.Duration) error {
		return context.Canceled
	}); err == nil {
		t.Fatal("expected cancellation to surface")
	}
}

func TestHelpBilingual(t *testing.T) {
	p := New()
	zh := p.help(&plugin.CommandContext{Lang: "zh-CN"})
	en := p.help(&plugin.CommandContext{Lang: "en-US"})
	if !strings.Contains(zh, "打字机") || !strings.Contains(zh, "自动模式") {
		t.Errorf("zh help missing: %q", zh)
	}
	if !strings.Contains(en, "Typewriter") || !strings.Contains(en, "Auto mode") {
		t.Errorf("en help missing: %q", en)
	}
}
