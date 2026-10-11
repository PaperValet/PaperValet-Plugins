package main

// tools.go — the tool set the agent can call, and their local execution.
//
// Every tool is a plain function on AgentPlugin; the model picks one by name
// and passes JSON arguments. Nothing here talks to Telegram except send_file,
// which returns the path so the loop can hand it to the host's media sender.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ============================================================
// Tool schema (OpenAI function-calling shape)
// ============================================================

type toolSpec struct {
	Name        string
	Description string
	DescEN      string
	Parameters  map[string]any
}

// toolResult is what a tool hands back to the loop.
type toolResult struct {
	Output   string
	IsError  bool
	SendFile string // non-empty: the loop must send this path
}

func buildTools() []toolSpec {
	return []toolSpec{
		{
			Name: "update_plan",
			Description: "创建或更新执行计划。适用于多步骤复杂任务：先列出所有步骤，" +
				"执行时逐步把当前步骤标记 in_progress、完成后标记 completed。",
			DescEN: "Create or update the step plan for multi-step tasks: mark the " +
				"current step in_progress and completed when done.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"plan": map[string]any{
						"type":        "array",
						"description": "完整步骤列表 / the full step list",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"step":   map[string]any{"type": "string", "description": "步骤描述 / step text"},
								"status": map[string]any{"type": "string", "enum": []string{"pending", "in_progress", "completed"}},
							},
							"required": []string{"step", "status"},
						},
					},
				},
				"required": []string{"plan"},
			},
		},
		{
			Name:        "list_files",
			Description: "列出目录下的文件与子目录，快速了解项目结构。默认只列一层，加 recursive 才递归。",
			DescEN:      "List files and subdirectories under a path. One level by default; set recursive to descend.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":      map[string]any{"type": "string", "description": "目录路径，默认工作区 / directory, defaults to the workspace"},
					"recursive": map[string]any{"type": "boolean", "description": "是否递归 / recurse into subdirectories"},
				},
			},
		},
		{
			Name:        "read_file",
			Description: "读取文本文件内容，可用 start_line/end_line 截取行范围。",
			DescEN:      "Read a text file, optionally a line range.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":       map[string]any{"type": "string", "description": "文件路径 / file path"},
					"start_line": map[string]any{"type": "integer", "description": "起始行（1-based）/ first line, 1-based"},
					"end_line":   map[string]any{"type": "integer", "description": "结束行（含）/ last line, inclusive"},
				},
				"required": []string{"path"},
			},
		},
		{
			Name:        "search_files",
			Description: "用 ripgrep 在文件中搜索文本或正则，返回 文件:行号:内容。",
			DescEN:      "Search file contents with ripgrep; returns file:line:text.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"pattern":    map[string]any{"type": "string", "description": "正则 / pattern"},
					"path":       map[string]any{"type": "string", "description": "搜索目录，默认工作区 / directory, defaults to the workspace"},
					"glob":       map[string]any{"type": "string", "description": "可选 glob，如 *.go / optional glob, e.g. *.go"},
					"files_only": map[string]any{"type": "boolean", "description": "只返回文件名 / names only"},
				},
				"required": []string{"pattern"},
			},
		},
		{
			Name:        "write_file",
			Description: "创建新文件或完整重写文件，内容原样写入。",
			DescEN:      "Create or overwrite a file, writing the content verbatim.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":    map[string]any{"type": "string", "description": "文件路径 / file path"},
					"content": map[string]any{"type": "string", "description": "文件内容 / contents, written verbatim"},
				},
				"required": []string{"path", "content"},
			},
		},
		{
			Name:        "run_command",
			Description: "运行终端命令，返回 exit_code 和输出。",
			DescEN:      "Run a shell command; returns the exit code and its output.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": map[string]any{"type": "string", "description": "要执行的 shell 命令 / shell command"},
					"cwd":     map[string]any{"type": "string", "description": "工作目录，默认工作区 / working directory, defaults to the workspace"},
				},
				"required": []string{"command"},
			},
		},
		{
			Name:        "send_file",
			Description: "把文件发送到当前 Telegram 对话。找到用户要的文件后用它发送。",
			DescEN:      "Send a file to the current chat.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{"type": "string", "description": "要发送的文件路径 / file path"},
				},
				"required": []string{"path"},
			},
		},
		{
			Name:        "finish",
			Description: "任务完成时调用，给出最终答复文本。完成后必须调用它。",
			DescEN:      "Call when the task is done, with the final answer text.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"message": map[string]any{"type": "string", "description": "给用户的最终答复 / final answer"},
				},
				"required": []string{"message"},
			},
		},
	}
}

// ============================================================
// Dispatch
// ============================================================

func (p *AgentPlugin) runTool(ctx context.Context, name, argsJSON string) toolResult {
	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return toolResult{Output: "参数解析失败 / bad arguments: " + err.Error(), IsError: true}
	}
	if args == nil {
		args = map[string]any{}
	}
	switch name {
	case "update_plan":
		return toolResult{Output: "plan updated"}
	case "list_files":
		return p.toolListFiles(args)
	case "read_file":
		return p.toolReadFile(args)
	case "search_files":
		return p.toolSearchFiles(ctx, args)
	case "write_file":
		return p.toolWriteFile(args)
	case "run_command":
		return p.toolRunCommand(ctx, args)
	case "send_file":
		return p.toolSendFile(args)
	case "finish":
		// Not trimmed: this is the answer the owner reads.
		return toolResult{Output: rawStrArg(args, "message")}
	default:
		return toolResult{Output: "未知工具 / unknown tool: " + name, IsError: true}
	}
}

// ============================================================
// Argument helpers
// ============================================================

func strArg(args map[string]any, key string) string {
	return strings.TrimSpace(rawStrArg(args, key))
}

// rawStrArg reads a string argument verbatim. Trimming is right for a path and
// wrong for file content, where it would eat the indentation of the first line
// and the trailing newline of the last.
func rawStrArg(args map[string]any, key string) string {
	if v, ok := args[key].(string); ok {
		return v
	}
	return ""
}

func boolArg(args map[string]any, key string) bool {
	switch v := args[key].(type) {
	case bool:
		return v
	case string:
		// Models do quote booleans; a quoted "true" must not read as false.
		b, err := strconv.ParseBool(strings.TrimSpace(v))
		return err == nil && b
	}
	return false
}

// intArg reads a numeric argument. JSON numbers decode to float64, but a
// quoted "5" must not silently degrade to 0 and read the whole file.
func intArg(args map[string]any, key string) int {
	switch v := args[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return 0
		}
		return int(n)
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0
		}
		return n
	}
	return 0
}

// resolvePath turns a tool path into an absolute one: relative paths resolve
// against the workspace, absolute paths pass through, and $workspace is
// substituted so the model can echo paths it was shown.
//
// This is a convenience, not a sandbox: run_command gives the model a shell
// with the host account's privileges, so containing the file tools alone would
// only be theatre. The command is OwnerOnly for that reason.
func (p *AgentPlugin) resolvePath(raw string) string {
	ws := p.config().Workspace
	if raw == "" {
		return ws
	}
	raw = expandHome(strings.ReplaceAll(raw, "$workspace", ws))
	if filepath.IsAbs(raw) {
		return filepath.Clean(raw)
	}
	return filepath.Join(ws, raw)
}

// ============================================================
// Tools
// ============================================================

const maxListEntries = 500

// errListLimit stops the walk once the entry budget is spent.
var errListLimit = errors.New("list limit reached")

func (p *AgentPlugin) toolListFiles(args map[string]any) toolResult {
	dir := p.resolvePath(strArg(args, "path"))
	info, err := os.Stat(dir)
	if err != nil {
		// Checked before the walk: a missing directory used to report
		// "(empty)", which sends the model looking in the wrong place.
		return toolResult{Output: "读取目录失败 / cannot read directory: " + err.Error(), IsError: true}
	}
	if !info.IsDir() {
		return toolResult{Output: "不是目录 / not a directory: " + dir, IsError: true}
	}
	recursive := boolArg(args, "recursive")

	var sb strings.Builder
	count, truncated := 0, false
	walkErr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == dir {
				return err
			}
			return nil // an unreadable subtree shows up as missing entries
		}
		if path == dir {
			return nil
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil || rel == "." {
			return nil
		}
		if count >= maxListEntries {
			truncated = true
			return errListLimit
		}
		count++
		if d.IsDir() {
			fmt.Fprintf(&sb, "%s/\n", rel)
			if !recursive {
				// List the directory but do not descend into it, so a
				// non-recursive call really stays one level deep.
				return filepath.SkipDir
			}
			return nil
		}
		if fi, fiErr := d.Info(); fiErr == nil {
			fmt.Fprintf(&sb, "%s (%d bytes)\n", rel, fi.Size())
		} else {
			fmt.Fprintf(&sb, "%s\n", rel)
		}
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, errListLimit) {
		return toolResult{Output: "读取目录失败 / cannot read directory: " + walkErr.Error(), IsError: true}
	}
	if sb.Len() == 0 {
		return toolResult{Output: "(empty)"}
	}
	if truncated {
		sb.WriteString(fmt.Sprintf("\n…truncated at %d entries", maxListEntries))
	}
	return toolResult{Output: sb.String()}
}

const (
	// maxReadRunes is what the model gets from one read.
	maxReadRunes = 32000
	// maxReadFileBytes bounds how much of a file is pulled into memory to
	// produce that window. Reading a multi-gigabyte log whole would exhaust
	// a small host long before the truncation ever applied.
	maxReadFileBytes = 4 << 20
)

func (p *AgentPlugin) toolReadFile(args map[string]any) toolResult {
	path := p.resolvePath(strArg(args, "path"))
	info, err := os.Stat(path)
	if err != nil {
		return toolResult{Output: "读取失败 / read failed: " + err.Error(), IsError: true}
	}
	if info.IsDir() {
		return toolResult{Output: "这是目录 / not a file: " + path, IsError: true}
	}
	f, err := os.Open(path)
	if err != nil {
		return toolResult{Output: "读取失败 / read failed: " + err.Error(), IsError: true}
	}
	defer f.Close()

	data, err := io.ReadAll(io.LimitReader(f, maxReadFileBytes+1))
	if err != nil {
		return toolResult{Output: "读取失败 / read failed: " + err.Error(), IsError: true}
	}
	fileCapped := len(data) > maxReadFileBytes
	if fileCapped {
		data = data[:maxReadFileBytes]
	}
	if bytes.IndexByte(data, 0) >= 0 {
		// Dumping binary bytes would burn the context window on noise.
		return toolResult{Output: "二进制文件，内容未显示 / binary file, contents not shown: " + path, IsError: true}
	}

	lines := strings.Split(string(data), "\n")
	start, end := intArg(args, "start_line"), intArg(args, "end_line")
	if start < 0 {
		start = 0
	}
	if end < 0 {
		end = 0
	}
	if start > 0 || end > 0 {
		if start == 0 {
			start = 1
		}
		if start > len(lines) {
			return toolResult{
				Output: fmt.Sprintf("起始行 %d 超出总行数 %d / start line %d beyond %d lines",
					start, len(lines), start, len(lines)),
				IsError: true,
			}
		}
		if end == 0 || end > len(lines) {
			end = len(lines)
		}
		if end < start {
			// A reversed range used to panic on the slice bounds and take
			// the whole run down with it.
			return toolResult{
				Output:  fmt.Sprintf("行范围无效 / invalid range: start %d is after end %d", start, end),
				IsError: true,
			}
		}
		lines = lines[start-1 : end]
	}

	out := strings.Join(lines, "\n")
	if fileCapped {
		out += fmt.Sprintf("\n…(file is larger than %d bytes, only the head was read)", maxReadFileBytes)
	}
	return toolResult{Output: clipRunes(out, maxReadRunes)}
}

const (
	maxSearchLines  = 100
	searchTimeout   = 30 * time.Second
	maxCaptureBytes = 1 << 20
)

func (p *AgentPlugin) toolSearchFiles(ctx context.Context, args map[string]any) toolResult {
	pattern := strArg(args, "pattern")
	if pattern == "" {
		return toolResult{Output: "pattern 不能为空 / pattern is required", IsError: true}
	}
	dir := p.resolvePath(strArg(args, "path"))
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return toolResult{Output: "搜索目录不可用 / cannot search: " + dir, IsError: true}
	}

	cctx, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()

	out, code, err := searchRun(cctx, p.toolEnv(), pattern, dir, strArg(args, "glob"), boolArg(args, "files_only"))
	if err != nil {
		return toolResult{Output: "搜索失败 / search failed: " + err.Error(), IsError: true}
	}
	// rg and grep both use exit code 1 for "no matches" and 2+ for "the
	// search itself broke", e.g. an invalid regex. Reporting the latter as a
	// result would hand the model an error message disguised as hits.
	if code >= 2 {
		msg := strings.TrimSpace(out)
		if msg == "" {
			msg = fmt.Sprintf("exit code %d", code)
		}
		return toolResult{Output: "搜索失败 / search failed: " + firstLines(msg, 400), IsError: true}
	}

	res := strings.TrimSpace(out)
	if res == "" {
		return toolResult{Output: "未找到匹配 / no matches"}
	}
	lines := strings.Split(res, "\n")
	if len(lines) > maxSearchLines {
		res = strings.Join(lines[:maxSearchLines], "\n") +
			fmt.Sprintf("\n…(%d lines total)", len(lines))
	}
	return toolResult{Output: res}
}

// searchRun prefers ripgrep and falls back to grep only when ripgrep is not
// installed. Deciding on "the output was empty" instead would re-scan the
// whole tree on every legitimate no-match result. Both engines get the same
// options so the two paths mean the same thing.
func searchRun(ctx context.Context, env []string, pattern, dir, glob string, filesOnly bool) (string, int, error) {
	var args []string
	if rg, err := exec.LookPath("rg"); err == nil {
		args = []string{"--color=never", "--no-messages"}
		if filesOnly {
			args = append(args, "-l")
		} else {
			args = append(args, "-n", "--no-heading")
		}
		if glob != "" {
			args = append(args, "-g", glob)
		}
		// -e keeps a pattern that starts with "-" from being read as a flag,
		// and -- keeps a directory that does the same from being read as one.
		args = append(args, "-e", pattern, "--", dir)
		return runCapture(ctx, env, rg, args...)
	}
	args = []string{"-r", "-I", "-s", "-E", "--colour=never"}
	if filesOnly {
		args = append(args, "-l")
	} else {
		args = append(args, "-n")
	}
	if glob != "" {
		args = append(args, "--include="+glob)
	}
	args = append(args, "-e", pattern, "--", dir)
	return runCapture(ctx, env, "grep", args...)
}

func (p *AgentPlugin) toolWriteFile(args map[string]any) toolResult {
	path := p.resolvePath(strArg(args, "path"))
	if strings.TrimSpace(path) == "" {
		return toolResult{Output: "path 不能为空 / path is required", IsError: true}
	}
	content := rawStrArg(args, "content")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return toolResult{Output: "创建目录失败 / mkdir failed: " + err.Error(), IsError: true}
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return toolResult{Output: "写入失败 / write failed: " + err.Error(), IsError: true}
	}
	return toolResult{Output: fmt.Sprintf("wrote %s (%d bytes)", path, len(content))}
}

func (p *AgentPlugin) toolRunCommand(ctx context.Context, args map[string]any) toolResult {
	command := strArg(args, "command")
	if command == "" {
		return toolResult{Output: "command 不能为空 / command is required", IsError: true}
	}
	cwd := p.resolvePath(strArg(args, "cwd"))

	timeout := time.Duration(p.config().CmdTimeout) * time.Second
	if timeout <= 0 {
		timeout = defaultCmdTimeout * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	out, code, timedOut := runShell(cctx, command, cwd, p.toolEnv())
	var sb strings.Builder
	fmt.Fprintf(&sb, "exit_code=%d\n", code)
	if s := strings.TrimSpace(out); s != "" {
		fmt.Fprintf(&sb, "output:\n%s\n", clipMiddle(s, 20000))
	}
	if timedOut {
		sb.WriteString(fmt.Sprintf("(命令在 %d 秒后超时被终止 / command killed after %ds)",
			int(timeout.Seconds()), int(timeout.Seconds())))
	}
	return toolResult{Output: sb.String(), IsError: code != 0}
}

func (p *AgentPlugin) toolSendFile(args map[string]any) toolResult {
	path := p.resolvePath(strArg(args, "path"))
	info, err := os.Stat(path)
	if err != nil {
		return toolResult{Output: "文件不存在 / no such file: " + err.Error(), IsError: true}
	}
	if info.IsDir() {
		return toolResult{Output: "这是目录，不能发送 / not a file: " + path, IsError: true}
	}
	if info.Size() == 0 {
		return toolResult{Output: "文件是空的 / the file is empty: " + path, IsError: true}
	}
	return toolResult{
		Output:   fmt.Sprintf("ready to send %s (%d bytes)", path, info.Size()),
		SendFile: path,
	}
}

// ============================================================
// Process helpers
// ============================================================

// stdPath lists the directories a minimal systemd environment leaves out.
const stdPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// toolEnv builds the child environment. A systemd unit starts with no HOME and
// a bare PATH, which makes git, npm and friends abort or write to the wrong
// place, so both get a fallback without discarding what the host did set.
func (p *AgentPlugin) toolEnv() []string {
	return buildEnv(os.Environ(), p.dataDir)
}

// buildEnv is toolEnv split out so the fallbacks can be tested directly.
func buildEnv(base []string, dataDir string) []string {
	out := make([]string, 0, len(base)+2)
	var havePath, haveHome bool
	for _, kv := range base {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			havePath = true
			out = append(out, "PATH="+extendPath(v))
			continue
		}
		if v, ok := strings.CutPrefix(kv, "HOME="); ok {
			if v == "" {
				continue // an empty HOME is the same as no HOME
			}
			haveHome = true
		}
		out = append(out, kv)
	}
	if !havePath {
		out = append(out, "PATH="+stdPath)
	}
	if !haveHome {
		if home := fallbackHome(dataDir); home != "" {
			out = append(out, "HOME="+home)
		}
	}
	return out
}

// extendPath keeps the inherited PATH, because the owner's own toolchain lives
// in it, and adds the standard directories it may be missing.
func extendPath(p string) string {
	if strings.TrimSpace(p) == "" {
		return stdPath
	}
	seen := make(map[string]bool)
	var parts []string
	for _, d := range filepath.SplitList(p) {
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		parts = append(parts, d)
	}
	for _, d := range filepath.SplitList(stdPath) {
		if !seen[d] {
			parts = append(parts, d)
		}
	}
	return strings.Join(parts, string(os.PathListSeparator))
}

// fallbackHome resolves a HOME for a child process when the host has none.
func fallbackHome(dataDir string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return home
	}
	if dataDir == "" {
		return ""
	}
	if abs, err := filepath.Abs(dataDir); err == nil {
		return abs
	}
	return dataDir
}

// usableDir reports dir only when it already exists. A cwd the model invented
// must never be created: run_command has the host account's privileges, and
// mkdir-ing whatever path arrives would litter the filesystem.
func usableDir(dir string) string {
	if dir == "" {
		return ""
	}
	if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
		return dir
	}
	return ""
}

// runShell executes a command and returns its output plus the exit code;
// timedOut reports a deadline hit so the model can tell the difference between
// a failure and a timeout.
func runShell(ctx context.Context, command, dir string, env []string) (output string, code int, timedOut bool) {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	if d := usableDir(dir); d != "" {
		cmd.Dir = d
	}
	cmd.Env = env
	setProcGroup(cmd)

	var buf limitedBuffer
	buf.limit = maxCaptureBytes
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()

	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else {
			code = -1
		}
	}
	timedOut = errors.Is(ctx.Err(), context.DeadlineExceeded)
	return buf.String(), code, timedOut
}

// runCapture runs a helper and reports its output, its exit code and any spawn
// failure. Exit code 1 from rg/grep means "no matches", which callers must not
// mistake for a missing binary or a broken pattern.
func runCapture(ctx context.Context, env []string, name string, args ...string) (string, int, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = env
	setProcGroup(cmd)

	var buf limitedBuffer
	buf.limit = maxCaptureBytes
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return buf.String(), ee.ExitCode(), nil
		}
		return buf.String(), -1, err
	}
	return buf.String(), 0, nil
}

// limitedBuffer collects process output up to a cap and keeps draining past
// it, so a command that spews gigabytes cannot exhaust memory before its
// timeout fires and a blocked child cannot deadlock on a full pipe.
type limitedBuffer struct {
	mu    sync.Mutex
	b     []byte
	limit int
	over  bool
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.limit <= 0 {
		l.limit = maxCaptureBytes
	}
	if room := l.limit - len(l.b); room > 0 {
		if len(p) > room {
			p = p[:room]
		}
		l.b = append(l.b, p...)
	}
	if l.b != nil && len(l.b) >= l.limit && n > 0 {
		l.over = true
	}
	return n, nil
}

func (l *limitedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := string(l.b)
	if l.over {
		s += "\n…(output capped)"
	}
	return s
}

// clipMiddle keeps the head and the tail of s, which is what command output
// needs: the start shows what ran, the end shows why it failed.
func clipMiddle(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	half := n / 2
	if half < 1 {
		half = 1
	}
	return string(r[:half]) + "\n…(truncated)……(truncated)…" + string(r[len(r)-half:])
}
