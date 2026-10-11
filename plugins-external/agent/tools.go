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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
			Description: "列出目录下的文件与子目录，快速了解项目结构。",
			DescEN:      "List files and subdirectories under a path.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":      map[string]any{"type": "string", "description": "目录路径，默认工作区 / directory, defaults to the workspace"},
					"recursive": map[string]any{"type": "boolean", "description": "是否递归 / recurse"},
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
					"start_line": map[string]any{"type": "integer", "description": "起始行（1-based）"},
					"end_line":   map[string]any{"type": "integer", "description": "结束行"},
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
					"path":       map[string]any{"type": "string", "description": "搜索目录，默认工作区"},
					"glob":       map[string]any{"type": "string", "description": "可选 glob，如 *.go"},
					"files_only": map[string]any{"type": "boolean", "description": "只返回文件名 / names only"},
				},
				"required": []string{"pattern"},
			},
		},
		{
			Name:        "write_file",
			Description: "创建新文件或完整重写文件。",
			DescEN:      "Create or overwrite a file.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":    map[string]any{"type": "string", "description": "文件路径 / file path"},
					"content": map[string]any{"type": "string", "description": "文件内容 / contents"},
				},
				"required": []string{"path", "content"},
			},
		},
		{
			Name:        "run_command",
			Description: "运行终端命令，返回 exit_code、stdout、stderr。",
			DescEN:      "Run a shell command; returns exit_code, stdout and stderr.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": map[string]any{"type": "string", "description": "要执行的 shell 命令 / shell command"},
					"cwd":     map[string]any{"type": "string", "description": "工作目录，默认工作区 / working directory"},
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
		return toolResult{Output: "参数解析失败: " + err.Error(), IsError: true}
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
		msg, _ := args["message"].(string)
		return toolResult{Output: msg}
	default:
		return toolResult{Output: "未知工具 / unknown tool: " + name, IsError: true}
	}
}

// ============================================================
// Argument helpers
// ============================================================

func strArg(args map[string]any, key string) string {
	if v, ok := args[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func boolArg(args map[string]any, key string) bool {
	if v, ok := args[key].(bool); ok {
		return v
	}
	return false
}

func intArg(args map[string]any, key string) int {
	if v, ok := args[key].(float64); ok {
		return int(v)
	}
	return 0
}

// resolvePath turns a tool path into an absolute one: relative paths resolve
// against the workspace, absolute paths pass through, and $workspace is
// substituted so the model can echo paths it was shown.
func (p *AgentPlugin) resolvePath(raw string) string {
	ws := p.config().Workspace
	if raw == "" {
		return ws
	}
	raw = strings.ReplaceAll(raw, "$workspace", ws)
	if filepath.IsAbs(raw) {
		return filepath.Clean(raw)
	}
	return filepath.Join(ws, raw)
}

// ============================================================
// Tools
// ============================================================

const maxListEntries = 500

func (p *AgentPlugin) toolListFiles(args map[string]any) toolResult {
	dir := p.resolvePath(strArg(args, "path"))
	recursive := boolArg(args, "recursive")

	var sb strings.Builder
	count, truncated := 0, false
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil || rel == "." {
			return nil
		}
		if info.IsDir() {
			if !recursive && filepath.Dir(path) != dir {
				return filepath.SkipDir
			}
			if count < maxListEntries {
				fmt.Fprintf(&sb, "%s/\n", rel)
			}
			return nil
		}
		count++
		if count > maxListEntries {
			truncated = true
			return filepath.SkipAll
		}
		fmt.Fprintf(&sb, "%-60s %8d\n", rel, info.Size())
		return nil
	})
	if sb.Len() == 0 {
		return toolResult{Output: "(empty)"}
	}
	if truncated {
		sb.WriteString(fmt.Sprintf("\n…truncated at %d entries", maxListEntries))
	}
	if _, err := os.Stat(dir); err != nil {
		return toolResult{Output: "读取目录失败 / cannot read directory: " + err.Error(), IsError: true}
	}
	return toolResult{Output: sb.String()}
}

const maxReadBytes = 32000

func (p *AgentPlugin) toolReadFile(args map[string]any) toolResult {
	path := p.resolvePath(strArg(args, "path"))
	data, err := os.ReadFile(path)
	if err != nil {
		return toolResult{Output: "读取失败 / read failed: " + err.Error(), IsError: true}
	}
	lines := strings.Split(string(data), "\n")
	start, end := intArg(args, "start_line"), intArg(args, "end_line")
	if start > 0 || end > 0 {
		if start <= 0 {
			start = 1
		}
		if start > len(lines) {
			return toolResult{Output: fmt.Sprintf("起始行 %d 超出文件总行数 %d", start, len(lines))}
		}
		if end <= 0 || end > len(lines) {
			end = len(lines)
		}
		lines = lines[start-1 : end]
	}
	out := strings.Join(lines, "\n")
	if len(out) > maxReadBytes {
		out = out[:maxReadBytes] + "\n…(truncated)"
	}
	return toolResult{Output: out}
}

const maxSearchLines = 100

func (p *AgentPlugin) toolSearchFiles(ctx context.Context, args map[string]any) toolResult {
	pattern := strArg(args, "pattern")
	if pattern == "" {
		return toolResult{Output: "pattern 不能为空 / pattern is required", IsError: true}
	}
	dir := p.resolvePath(strArg(args, "path"))
	glob := strArg(args, "glob")

	regArgs := []string{"-n", "--no-heading", "--color=never"}
	if boolArg(args, "files_only") {
		regArgs = []string{"-l", "--color=never"}
	}
	if glob != "" {
		regArgs = append(regArgs, "-g", glob)
	}
	regArgs = append(regArgs, "-e", pattern, dir)

	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	res := runCapture(cctx, "rg", regArgs...)
	if strings.TrimSpace(res) == "" {
		// Fall back to grep when ripgrep is absent.
		res = runCapture(cctx, "grep", "-rn", "-E", pattern, dir)
	}
	res = strings.TrimSpace(res)
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

func (p *AgentPlugin) toolWriteFile(args map[string]any) toolResult {
	path := p.resolvePath(strArg(args, "path"))
	content := strArg(args, "content")
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

	out, code, timedOut := runShell(cctx, command, cwd)
	var sb strings.Builder
	fmt.Fprintf(&sb, "exit_code=%d\n", code)
	if s := strings.TrimSpace(out); s != "" {
		fmt.Fprintf(&sb, "output:\n%s\n", clipBytes(s, 20000))
	}
	if timedOut {
		sb.WriteString("(命令超时被终止 / command timed out)")
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
	return toolResult{
		Output:   fmt.Sprintf("ready to send %s (%d bytes)", path, info.Size()),
		SendFile: path,
	}
}

// ============================================================
// Process helpers
// ============================================================

// agentPath is prepended to PATH so tools resolve even when the host was
// started with a minimal environment (systemd units often lack /usr/bin).
const agentPath = "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

func runCapture(ctx context.Context, name string, args ...string) string {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), agentPath)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	_ = cmd.Run()
	return out.String()
}

// runShell executes a command and returns its combined output plus the exit
// code; timedOut reports a deadline hit so the model can tell the difference
// between a failure and a timeout.
func runShell(ctx context.Context, command, dir string) (output string, code int, timedOut bool) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		dir = ""
	}
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), agentPath)

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()

	if err != nil {
		var ee *exec.ExitError
		if ok := asExitError(err, &ee); ok {
			code = ee.ExitCode()
		} else {
			code = -1
		}
	}
	timedOut = ctx.Err() == context.DeadlineExceeded
	return buf.String(), code, timedOut
}

func asExitError(err error, target **exec.ExitError) bool {
	if ee, ok := err.(*exec.ExitError); ok {
		*target = ee
		return true
	}
	return false
}

func clipBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n…(truncated)"
}
