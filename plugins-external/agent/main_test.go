package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ============================================================
// Path resolution
// ============================================================

func TestResolvePath(t *testing.T) {
	p := New()
	p.set = nil
	// newTestPlugin gives a fixed workspace via a fake settings source.
	p2 := &AgentPlugin{set: stubSettings{"workspace": "/ws"}}

	cases := []struct {
		in   string
		want string
	}{
		{"", "/ws"},
		{"a.txt", "/ws/a.txt"},
		{"sub/a.txt", "/ws/sub/a.txt"},
		{"/etc/hosts", "/etc/hosts"},
		{"$workspace/x", "/ws/x"},
	}
	for _, c := range cases {
		if got := p2.resolvePath(c.in); got != c.want {
			t.Errorf("resolvePath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	_ = p
}

// ============================================================
// Argument parsing
// ============================================================

func TestStrArgTrimsAndIgnoresNonString(t *testing.T) {
	args := map[string]any{"a": "  x  ", "b": 12, "c": true}
	if got := strArg(args, "a"); got != "x" {
		t.Errorf("strArg(a) = %q, want x", got)
	}
	if got := strArg(args, "b"); got != "" {
		t.Errorf("strArg(b) = %q, want empty for non-string", got)
	}
	if got := strArg(args, "missing"); got != "" {
		t.Errorf("strArg(missing) = %q, want empty", got)
	}
}

func TestBoolAndIntArgs(t *testing.T) {
	args := map[string]any{"t": true, "n": float64(7), "s": "7"}
	if !boolArg(args, "t") {
		t.Error("boolArg should read true")
	}
	if boolArg(args, "missing") {
		t.Error("boolArg should default false")
	}
	if got := intArg(args, "n"); got != 7 {
		t.Errorf("intArg(n) = %d, want 7", got)
	}
	if got := intArg(args, "s"); got != 7 {
		t.Errorf("intArg on a quoted string = %d, want 7 (quoted numbers must parse)", got)
	}
}

// ============================================================
// Plan parsing
// ============================================================

func TestParsePlan(t *testing.T) {
	raw := `{"plan":[{"step":"a","status":"completed"},{"step":"b","status":"in_progress"}]}`
	plan := parsePlan(raw)
	if len(plan) != 2 {
		t.Fatalf("got %d steps, want 2", len(plan))
	}
	if plan[0].Text != "a" || plan[0].Status != "completed" {
		t.Errorf("step 0 = %+v", plan[0])
	}
	if plan[1].Status != "in_progress" {
		t.Errorf("step 1 = %+v", plan[1])
	}
}

func TestParsePlanRejectsGarbage(t *testing.T) {
	if got := parsePlan("not json"); got != nil {
		t.Errorf("garbage should yield nil, got %+v", got)
	}
	if got := parsePlan("{}"); len(got) != 0 {
		t.Errorf("empty plan should yield no steps, got %+v", got)
	}
}

func TestPlanTextMarks(t *testing.T) {
	got := planText([]planStep{
		{Text: "one", Status: "completed"},
		{Text: "two", Status: "in_progress"},
		{Text: "three", Status: "pending"},
	})
	want := "✓ one\n→ two\n· three"
	if got != want {
		t.Errorf("planText = %q, want %q", got, want)
	}
}

// ============================================================
// URL joining
// ============================================================

func TestChatURL(t *testing.T) {
	cases := map[string]string{
		"https://api.deepseek.com/v1":          "https://api.deepseek.com/v1/chat/completions",
		"https://api.deepseek.com/v1/":         "https://api.deepseek.com/v1/chat/completions",
		"https://example.com/chat/completions": "https://example.com/chat/completions",
	}
	for in, want := range cases {
		if got := chatURL(in); got != want {
			t.Errorf("chatURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// ============================================================
// Text helpers
// ============================================================

func TestClipRunesIsRuneSafe(t *testing.T) {
	got := clipRunes("你好世界", 2)
	if got != "你好…" {
		t.Errorf("clipRunes = %q", got)
	}
	if got := clipRunes("abc", 10); got != "abc" {
		t.Errorf("clipRunes should not pad, got %q", got)
	}
}

func TestQuoteLinesPrefixesEveryLine(t *testing.T) {
	got := quoteLines("a\nb")
	if got != "> a\n> b" {
		t.Errorf("quoteLines = %q", got)
	}
}

func TestElapsedFormats(t *testing.T) {
	if got := elapsed(time.Now()); !strings.HasSuffix(got, "ms") {
		t.Errorf("fresh timestamp should read in ms, got %q", got)
	}
	if got := elapsed(time.Now().Add(-90 * time.Second)); got != "1m30s" {
		t.Errorf("90s should read 1m30s, got %q", got)
	}
}

// ============================================================
// Shell execution
// ============================================================

func TestRunShellExitCodeAndOutput(t *testing.T) {
	env := buildEnv(os.Environ(), "")
	out, code, timedOut := runShell(t.Context(), "echo hello", t.TempDir(), env)
	if code != 0 {
		t.Errorf("exit code = %d, want 0 (output %q)", code, out)
	}
	if !strings.Contains(out, "hello") {
		t.Errorf("output = %q, want it to contain hello", out)
	}
	if timedOut {
		t.Error("should not report a timeout")
	}
}

func TestRunShellReportsNonZero(t *testing.T) {
	env := buildEnv(os.Environ(), "")
	_, code, _ := runShell(t.Context(), "exit 3", t.TempDir(), env)
	if code != 3 {
		t.Errorf("exit code = %d, want 3", code)
	}
}

func TestRunShellMissingCwdFallsBack(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope")
	env := buildEnv(os.Environ(), "")
	out, code, _ := runShell(t.Context(), "echo ok", missing, env)
	if code != 0 || !strings.Contains(out, "ok") {
		t.Errorf("should fall back to the process cwd, got code=%d out=%q", code, out)
	}
	// usableDir must not invent directories for the model.
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Errorf("a missing cwd must not be created, stat err = %v", err)
	}
}

// ============================================================
// File tools
// ============================================================

func TestWriteThenReadFile(t *testing.T) {
	dir := t.TempDir()
	p := &AgentPlugin{set: stubSettings{"workspace": dir}}

	res := p.toolWriteFile(map[string]any{"path": "note.txt", "content": "line1\nline2\nline3"})
	if res.IsError {
		t.Fatalf("write failed: %s", res.Output)
	}
	if _, err := os.Stat(filepath.Join(dir, "note.txt")); err != nil {
		t.Fatalf("file was not created: %v", err)
	}

	got := p.toolReadFile(map[string]any{"path": "note.txt", "start_line": float64(2), "end_line": float64(2)})
	if got.IsError {
		t.Fatalf("read failed: %s", got.Output)
	}
	if strings.TrimSpace(got.Output) != "line2" {
		t.Errorf("line range read = %q, want line2", got.Output)
	}
}

func TestReadFileMissing(t *testing.T) {
	p := &AgentPlugin{set: stubSettings{"workspace": t.TempDir()}}
	if res := p.toolReadFile(map[string]any{"path": "nope.txt"}); !res.IsError {
		t.Error("reading a missing file should be an error result")
	}
}

func TestSendFileRejectsDirectory(t *testing.T) {
	dir := t.TempDir()
	p := &AgentPlugin{set: stubSettings{"workspace": dir}}
	if res := p.toolSendFile(map[string]any{"path": "."}); !res.IsError {
		t.Error("send_file on a directory should fail")
	}
}

func TestSendFileMarksPath(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := &AgentPlugin{set: stubSettings{"workspace": dir}}
	res := p.toolSendFile(map[string]any{"path": "f.txt"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if res.SendFile == "" {
		t.Error("send_file should report the path for the loop to send")
	}
}

func TestListFilesSkipsTruncationMarker(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := &AgentPlugin{set: stubSettings{"workspace": dir}}
	res := p.toolListFiles(map[string]any{"path": ""})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if !strings.Contains(res.Output, "a.txt") {
		t.Errorf("listing = %q, want it to mention a.txt", res.Output)
	}
}

// ============================================================
// Dispatch
// ============================================================

func TestRunToolUnknown(t *testing.T) {
	p := New()
	res := p.runTool(t.Context(), "nope", "{}")
	if !res.IsError {
		t.Error("unknown tool should be an error result")
	}
}

func TestRunToolBadJSON(t *testing.T) {
	p := New()
	res := p.runTool(t.Context(), "list_files", "{not json")
	if !res.IsError {
		t.Error("bad JSON should be an error result")
	}
}

func TestRunToolFinishReturnsMessage(t *testing.T) {
	p := New()
	res := p.runTool(t.Context(), "finish", `{"message":"done"}`)
	if res.Output != "done" {
		t.Errorf("finish output = %q, want done", res.Output)
	}
}

// ============================================================
// Session bookkeeping
// ============================================================

func TestSessionHistoryIsCapped(t *testing.T) {
	p := New()
	for i := 0; i < 60; i++ {
		p.appendHistory(1, chatMessage{Role: "user", Content: "x"})
	}
	if n := p.historyLen(1); n > maxHistory {
		t.Errorf("history = %d messages, want at most %d", n, maxHistory)
	}
}

func TestSessionHistoryDropsLeadingToolMessages(t *testing.T) {
	p := New()
	for i := 0; i < maxHistory; i++ {
		p.appendHistory(1, chatMessage{Role: "user", Content: "x"})
	}
	// Push a burst ending on tool messages so the trim has to advance past them.
	for i := 0; i < 5; i++ {
		p.appendHistory(1, chatMessage{Role: "tool", ToolCallID: "1", Content: "y"})
	}
	hist := p.historySnapshot(1)
	if len(hist) > 0 && hist[0].Role == "tool" {
		t.Error("history must not start with a tool message")
	}
	if len(hist) > maxHistory {
		t.Errorf("history grew to %d", len(hist))
	}
}

func TestClearSession(t *testing.T) {
	p := New()
	p.appendHistory(1, chatMessage{Role: "user", Content: "x"})
	p.clearSession(1)
	if p.historySnapshot(1) != nil {
		t.Error("session should be gone")
	}
}

func TestSingleRunPerChat(t *testing.T) {
	p := New()
	if !p.tryStart(1, func() {}) {
		t.Fatal("first start should win")
	}
	if p.tryStart(1, func() {}) {
		t.Error("second start in the same chat must be refused")
	}
	p.finishRun(1)
	if !p.tryStart(1, func() {}) {
		t.Error("after finishing, a new run should start")
	}
}

// ============================================================
// Config
// ============================================================

func TestConfigDefaultsWhenUnset(t *testing.T) {
	p := New()
	cfg := p.config()
	if cfg.BaseURL != defaultBaseURL || cfg.Model != defaultModel {
		t.Errorf("defaults not applied: %+v", cfg)
	}
	if cfg.MaxSteps != defaultMaxSteps || cfg.CmdTimeout != defaultCmdTimeout {
		t.Errorf("numeric defaults not applied: %+v", cfg)
	}
}

func TestConfigOverrides(t *testing.T) {
	p := &AgentPlugin{set: stubSettings{
		"api_key":     "k",
		"base_url":    "https://example.com/v1/",
		"model":       "m",
		"workspace":   "/tmp/x",
		"max_steps":   3,
		"cmd_timeout": 9,
	}}
	cfg := p.config()
	if cfg.APIKey != "k" || cfg.Model != "m" || cfg.Workspace != "/tmp/x" {
		t.Errorf("string overrides failed: %+v", cfg)
	}
	if cfg.BaseURL != "https://example.com/v1" {
		t.Errorf("base URL should be trimmed, got %q", cfg.BaseURL)
	}
	if cfg.MaxSteps != 3 || cfg.CmdTimeout != 9 {
		t.Errorf("numeric overrides failed: %+v", cfg)
	}
}

// ============================================================
// Argument shapes providers send
// ============================================================

func TestArgsTextHandlesProviderShapes(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"string-encoded object", `"{\"path\":\"a.txt\"}"`, `{"path":"a.txt"}`},
		{"plain object", `{"path":"a.txt"}`, `{"path":"a.txt"}`},
		{"empty string", `""`, `{}`},
		{"null", `null`, `{}`},
		{"missing", ``, `{}`},
	}
	for _, c := range cases {
		call := toolCall{Function: struct {
			Name string `json:"name"`
			// Arguments is raw because providers disagree about its shape: most
			// send a JSON-encoded string, some send the object itself. Decoding
			// it as a plain string makes the whole response fail on the latter.
			Arguments json.RawMessage `json:"arguments"`
		}{Name: "t", Arguments: json.RawMessage(c.raw)}}
		if got := call.argsText(); got != c.want {
			t.Errorf("%s: argsText(%s) = %q, want %q", c.name, c.raw, got, c.want)
		}
	}
}

// ============================================================
// History sanitization
// ============================================================

func TestSanitizeHistoryDropsOrphanToolReplies(t *testing.T) {
	in := []chatMessage{
		{Role: "user", Content: "hi"},
		{Role: "tool", ToolCallID: "gone", Content: "orphan"},
		{Role: "assistant", Content: "answer"},
	}
	out := sanitizeHistory(in)
	if len(out) != 2 || out[0].Role != "user" || out[1].Role != "assistant" {
		t.Fatalf("orphan tool reply must be dropped, got %+v", out)
	}
}

func TestSanitizeHistoryDropsIncompleteCallSets(t *testing.T) {
	in := []chatMessage{
		{Role: "user", Content: "hi"},
		{Role: "assistant", ToolCalls: []toolCall{{ID: "a"}, {ID: "b"}}},
		{Role: "tool", ToolCallID: "a", Content: "first"},
		{Role: "user", Content: "next"},
	}
	out := sanitizeHistory(in)
	// The half-answered call set and its only reply are dropped together.
	if len(out) != 2 || out[0].Role != "user" || out[1].Role != "user" {
		t.Fatalf("incomplete call set must be dropped, got %+v", out)
	}
}

func TestSanitizeHistoryKeepsCompletePairs(t *testing.T) {
	in := []chatMessage{
		{Role: "user", Content: "hi"},
		{Role: "assistant", ToolCalls: []toolCall{{ID: "a"}, {ID: "b"}}},
		{Role: "tool", ToolCallID: "a", Content: "first"},
		{Role: "tool", ToolCallID: "b", Content: "second"},
		{Role: "assistant", Content: "done"},
	}
	out := sanitizeHistory(in)
	if len(out) != 5 {
		t.Fatalf("complete pairs must survive, got %d messages", len(out))
	}
}

// ============================================================
// Child environment
// ============================================================

func TestBuildEnvFillsMissingPathAndHome(t *testing.T) {
	env := buildEnv(nil, "/data/agent")
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "PATH="+stdPath) {
		t.Errorf("missing PATH should be filled, got %q", joined)
	}
	if !strings.Contains(joined, "HOME=") {
		t.Errorf("missing HOME should be filled, got %q", joined)
	}
}

func TestBuildEnvExtendsAndKeepsExisting(t *testing.T) {
	env := buildEnv([]string{"PATH=/opt/bin", "HOME=/root", "K=V"}, "")
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "PATH=/opt/bin:"+stdPath) {
		t.Errorf("existing PATH should be kept and extended, got %q", joined)
	}
	if !strings.Contains(joined, "HOME=/root") {
		t.Errorf("existing HOME must survive verbatim, got %q", joined)
	}
	if !strings.Contains(joined, "K=V") {
		t.Errorf("unrelated entries must survive, got %q", joined)
	}
}

func TestExtendPathDeduplicates(t *testing.T) {
	got := extendPath("/bin:/opt/bin:/bin")
	want := "/bin:/opt/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin"
	if got != want {
		t.Errorf("extendPath = %q, want %q", got, want)
	}
}

func TestUsableDir(t *testing.T) {
	dir := t.TempDir()
	if got := usableDir(dir); got != dir {
		t.Errorf("usableDir(existing) = %q, want %q", got, dir)
	}
	missing := filepath.Join(dir, "nope")
	if got := usableDir(missing); got != "" {
		t.Errorf("usableDir(missing) = %q, want empty", got)
	}
	if got := usableDir(""); got != "" {
		t.Errorf("usableDir(empty) = %q, want empty", got)
	}
	file := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := usableDir(file); got != "" {
		t.Errorf("usableDir(file) = %q, want empty", got)
	}
}

func TestExpandHome(t *testing.T) {
	t.Setenv("HOME", "/home/tester")
	cases := map[string]string{
		"~/x":      "/home/tester/x",
		"~":        "/home/tester",
		"/abs":     "/abs",
		"rel":      "rel",
		"~other/x": "~other/x", // only ~ and ~/ expand
	}
	for in, want := range cases {
		if got := expandHome(in); got != want {
			t.Errorf("expandHome(%q) = %q, want %q", in, got, want)
		}
	}
}

// ============================================================
// Card clipping
// ============================================================

func TestClipCardCutsAtLineBoundary(t *testing.T) {
	s := strings.Repeat("line\n", 100)
	got := clipCard(s, 12)
	if !strings.HasSuffix(got, "\n…") {
		t.Errorf("clipCard should end with a newline plus ellipsis, got %q", got)
	}
	body := strings.TrimSuffix(got, "\n…")
	if !strings.HasSuffix(body, "line") {
		t.Errorf("cut must land after a complete line, got %q", body)
	}
}

func TestClipMiddleKeepsHeadAndTail(t *testing.T) {
	s := strings.Repeat("a", 1000) + "MIDDLE" + strings.Repeat("b", 1000)
	got := clipMiddle(s, 100)
	if !strings.HasPrefix(got, strings.Repeat("a", 50)) {
		t.Errorf("head must be kept, got %q", got[:60])
	}
	if !strings.HasSuffix(got, strings.Repeat("b", 50)) {
		t.Errorf("tail must be kept, got %q", got[len(got)-60:])
	}
	if strings.Contains(got, "MIDDLE") {
		t.Error("the middle must be elided")
	}
}

func TestClipMiddleShortStringsPassThrough(t *testing.T) {
	if got := clipMiddle("short", 100); got != "short" {
		t.Errorf("clipMiddle = %q, want short", got)
	}
}

// ============================================================
// Output buffer
// ============================================================

func TestLimitedBufferCapsButKeepsDraining(t *testing.T) {
	var buf limitedBuffer
	buf.limit = 16
	chunk := strings.Repeat("x", 64)
	for i := 0; i < 8; i++ { // 512 bytes through a 16-byte window
		if n, err := buf.Write([]byte(chunk)); err != nil || n != len(chunk) {
			t.Fatalf("Write must swallow everything: n=%d err=%v", n, err)
		}
	}
	got := buf.String()
	if len(got) == 0 || len(got) > 16+20 {
		t.Errorf("buffer should hold about the cap, got %d bytes", len(got))
	}
	if !strings.Contains(got, "output capped") {
		t.Errorf("cap should be reported, got %q", got)
	}
}

// ============================================================
// Tool edge cases
// ============================================================

func TestReadFileReversedRangeIsError(t *testing.T) {
	dir := t.TempDir()
	p := &AgentPlugin{set: stubSettings{"workspace": dir}}
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("1\n2\n3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := p.toolReadFile(map[string]any{"path": "f.txt", "start_line": float64(3), "end_line": float64(1)})
	if !res.IsError {
		t.Errorf("a reversed range must be an error, got %+v", res)
	}
}

func TestReadFileRejectsBinary(t *testing.T) {
	dir := t.TempDir()
	p := &AgentPlugin{set: stubSettings{"workspace": dir}}
	if err := os.WriteFile(filepath.Join(dir, "bin.dat"), []byte("ab\x00cd"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := p.toolReadFile(map[string]any{"path": "bin.dat"})
	if !res.IsError || !strings.Contains(res.Output, "binary") {
		t.Errorf("binary content must be refused, got %+v", res)
	}
}

func TestListFilesMissingDirIsError(t *testing.T) {
	p := &AgentPlugin{set: stubSettings{"workspace": t.TempDir()}}
	if res := p.toolListFiles(map[string]any{"path": "nope"}); !res.IsError {
		t.Errorf("a missing directory must be an error, got %+v", res)
	}
}

func TestWriteFileKeepsContentVerbatim(t *testing.T) {
	dir := t.TempDir()
	p := &AgentPlugin{set: stubSettings{"workspace": dir}}
	content := "\n  indented\n\n"
	res := p.toolWriteFile(map[string]any{"path": "f.txt", "content": content})
	if res.IsError {
		t.Fatalf("write failed: %s", res.Output)
	}
	data, err := os.ReadFile(filepath.Join(dir, "f.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != content {
		t.Errorf("content changed: wrote %q, got %q", content, data)
	}
}

// ============================================================
// Test doubles
// ============================================================

// stubSettings stands in for the host settings store.
type stubSettings map[string]any

func (s stubSettings) Bool(k string) bool {
	v, _ := s[k].(bool)
	return v
}

func (s stubSettings) String(k string) string {
	v, _ := s[k].(string)
	return v
}

func (s stubSettings) Int(k string) int {
	switch v := s[k].(type) {
	case int:
		return v
	case float64:
		return int(v)
	}
	return 0
}

func (s stubSettings) Set(string, any) error { return nil }
