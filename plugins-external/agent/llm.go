package main

// llm.go — the model call, the agent loop, and the live progress card.
//
// The loop is a plain ReAct cycle: send messages + tool schemas, run whatever
// tools come back, feed the results in, repeat until the model calls finish or
// the step budget runs out. Progress is rendered into one edited message.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

// ============================================================
// HTTP client
// ============================================================

type httpDoer struct{ c *http.Client }

func newHTTPDoer() *httpDoer {
	return &httpDoer{c: &http.Client{Timeout: 180 * time.Second}}
}

// ============================================================
// Wire types (OpenAI chat completions)
// ============================================================

type chatMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type apiRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Tools       []apiTool     `json:"tools,omitempty"`
	Temperature float64       `json:"temperature"`
	Stream      bool          `json:"stream"`
}

type apiTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

type apiResponse struct {
	Choices []struct {
		Message struct {
			Content   string     `json:"content"`
			ToolCalls []toolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		TotalTokens int `json:"total_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// chatURL joins the configured base URL with the chat completions path.
func chatURL(base string) string {
	base = strings.TrimRight(base, "/")
	if strings.HasSuffix(base, "/chat/completions") {
		return base
	}
	return base + "/chat/completions"
}

// ============================================================
// One model call
// ============================================================

func (p *AgentPlugin) callModel(ctx context.Context, msgs []chatMessage) (chatMessage, int, error) {
	cfg := p.config()

	tools := make([]apiTool, 0, len(p.tools))
	for _, t := range p.tools {
		var at apiTool
		at.Type = "function"
		at.Function.Name = t.Name
		at.Function.Description = t.Description + " / " + t.DescEN
		at.Function.Parameters = t.Parameters
		tools = append(tools, at)
	}

	body := apiRequest{Model: cfg.Model, Messages: msgs, Tools: tools, Temperature: 0.3}
	raw, err := json.Marshal(body)
	if err != nil {
		return chatMessage{}, 0, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, chatURL(cfg.BaseURL), bytes.NewReader(raw))
	if err != nil {
		return chatMessage{}, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}

	resp, err := p.http.c.Do(req)
	if err != nil {
		return chatMessage{}, 0, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return chatMessage{}, 0, err
	}
	if resp.StatusCode != http.StatusOK {
		return chatMessage{}, 0, fmt.Errorf("HTTP %d: %s", resp.StatusCode, clipBytes(string(data), 300))
	}

	var ar apiResponse
	if err := json.Unmarshal(data, &ar); err != nil {
		return chatMessage{}, 0, fmt.Errorf("响应解析失败 / bad response: %w (%s)", err, clipBytes(string(data), 200))
	}
	if ar.Error != nil {
		return chatMessage{}, 0, fmt.Errorf("%s", ar.Error.Message)
	}
	if len(ar.Choices) == 0 {
		return chatMessage{}, 0, fmt.Errorf("接口无返回 / empty response")
	}
	tokens := 0
	if ar.Usage != nil {
		tokens = ar.Usage.TotalTokens
	}
	m := ar.Choices[0].Message
	return chatMessage{Role: "assistant", Content: m.Content, ToolCalls: m.ToolCalls}, tokens, nil
}

// ============================================================
// System prompt
// ============================================================

func (p *AgentPlugin) systemPrompt() string {
	cfg := p.config()
	ws := cfg.Workspace
	return fmt.Sprintf(`You are a capable assistant that receives tasks over Telegram and carries them out with tools.

Workspace: %s
Local time: %s

How to work:
1. Carry the task out with tools; never guess at file contents or paths.
2. For multi-step tasks, call update_plan first and keep the step statuses current: mark one step in_progress, mark it completed right after it succeeds, then move on. Simple tasks need no plan.
3. Locate files with search_files or list_files before reading them.
4. Use run_command for shell work (it runs as the bot's user).
5. When the user asks for a file, send it with send_file.
6. When the task is done you MUST call finish with a concise final answer.

Rules:
- Never invent tool output; report what actually came back.
- On command failure read the output and adapt instead of retrying blindly.
- Answer in the same language the user wrote in.
- Keep the final answer short: what you did and what the outcome was.`, ws, time.Now().Format("2006-01-02 15:04:05"))
}

// ============================================================
// The agent loop
// ============================================================

const runTimeout = 10 * time.Minute

func (p *AgentPlugin) runTask(chatID int64, msgID int, task string) {
	defer p.finishRun(chatID)
	defer func() {
		if r := recover(); r != nil {
			p.logger.Error("agent panic", "err", fmt.Sprint(r))
			p.edit(chatID, msgID, "❌ 内部错误 / internal error: "+fmt.Sprint(r))
		}
	}()

	cfg := p.config()
	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()

	view := newProgressView(cfg.Model, cfg.Workspace, task)

	msgs := []chatMessage{{Role: "system", Content: p.systemPrompt()}}
	if s := p.getSession(chatID); s != nil {
		msgs = append(msgs, s.Messages...)
	}
	msgs = append(msgs, chatMessage{Role: "user", Content: task})
	p.appendHistory(chatID, chatMessage{Role: "user", Content: task})

	view.status("🤔 正在思考 / thinking")
	view.flushNow()
	p.edit(chatID, msgID, view.render())

	var final string
	var failure string
	var sent []string

	for step := 1; step <= cfg.MaxSteps; step++ {
		if ctx.Err() != nil {
			failure = "任务超时（10 分钟）/ task timed out"
			break
		}

		assistant, tokens, err := p.callModel(ctx, msgs)
		view.addTokens(tokens)
		if err != nil {
			p.logger.Error("model call failed", "err", err.Error())
			failure = "AI 调用失败 / model call failed: " + err.Error()
			break
		}

		msgs = append(msgs, assistant)
		p.appendHistory(chatID, assistant)

		if len(assistant.ToolCalls) == 0 {
			if s := strings.TrimSpace(assistant.Content); s != "" {
				final = s
			}
			break
		}

		done := false
		for _, call := range assistant.ToolCalls {
			name := call.Function.Name
			args := normalizeArgs(call.Function.Arguments)

			view.status(statusText(name, args))
			view.addStep(stepLine(name, args))
			view.flushNow()
			p.edit(chatID, msgID, view.render())

			if name == "update_plan" {
				if plan := parsePlan(args); len(plan) > 0 {
					view.setPlan(plan)
					view.flushNow()
					p.edit(chatID, msgID, view.render())
				}
			}

			res := p.runTool(ctx, name, args)

			if name == "finish" {
				final = res.Output
				done = true
				break
			}

			if name == "send_file" && res.SendFile != "" {
				if err := p.host.Media().SendFile(ctx, chatID, res.SendFile, "", 0); err != nil {
					p.logger.Warn("send file failed", "err", err.Error(), "path", res.SendFile)
					res.Output = "发送失败 / send failed: " + err.Error()
					res.IsError = true
				} else {
					sent = append(sent, res.SendFile)
					res.Output = "sent: " + res.SendFile
				}
			}

			view.observe(res.Output)
			toolMsg := chatMessage{Role: "tool", ToolCallID: call.ID, Name: name, Content: res.Output}
			msgs = append(msgs, toolMsg)
			p.appendHistory(chatID, toolMsg)

			view.status("")
			view.flushNow()
			p.edit(chatID, msgID, view.render())
		}
		if done {
			break
		}
		if step == cfg.MaxSteps {
			failure = fmt.Sprintf("达到最大步数 %d，任务可能未完成 / step budget exhausted", cfg.MaxSteps)
		}
	}

	switch {
	case failure != "":
		p.appendHistory(chatID, chatMessage{Role: "assistant", Content: failure})
		p.edit(chatID, msgID, view.renderFailure(failure))
	case final == "":
		p.edit(chatID, msgID, view.renderAnswer("已结束本轮任务。\nTask finished."))
	default:
		if len(sent) > 0 {
			final += "\n\n📎 " + strings.Join(sent, "\n📎 ")
		}
		p.appendHistory(chatID, chatMessage{Role: "assistant", Content: final})
		p.edit(chatID, msgID, view.renderAnswer(final))
	}
}

// edit swaps the progress message for new text.
func (p *AgentPlugin) edit(chatID int64, msgID int, text string) {
	if msgID == 0 {
		_, _ = p.host.Send(context.Background(), chatID, text, 0)
		return
	}
	peer, err := p.host.PeerResolver().ResolveFromChatID(context.Background(), chatID)
	if err != nil {
		return
	}
	plain, ents := plugin.ParseMarkdown(text, nil)
	req := &tg.MessagesEditMessageRequest{Peer: peer, ID: msgID, Message: plain, NoWebpage: true}
	if len(ents) > 0 {
		req.SetEntities(ents)
	}
	if _, err := p.host.API().MessagesEditMessage(context.Background(), req); err != nil {
		p.logger.Debug("edit failed", "err", err.Error())
	}
}

// ============================================================
// Progress card
// ============================================================

type planStep struct {
	Text   string
	Status string
}

type progressView struct {
	model     string
	workspace string
	request   string
	started   time.Time
	tokens    int

	status string
	latest string
	steps  []string
	plan   []planStep

	lastEdit time.Time
}

func newProgressView(model, workspace, request string) *progressView {
	return &progressView{model: model, workspace: workspace, request: request, started: time.Now()}
}

func (v *progressView) status(s string)      { v.status = s }
func (v *progressView) addTokens(n int)      { v.tokens += n }
func (v *progressView) setPlan(p []planStep) { v.plan = p }

func (v *progressView) addStep(line string) {
	v.steps = append(v.steps, line)
	if len(v.steps) > 6 {
		v.steps = v.steps[len(v.steps)-6:]
	}
}

func (v *progressView) observe(out string) { v.latest = firstLines(out, 300) }

// flushNow resets the edit throttle so the next render definitely lands.
func (v *progressView) flushNow() { v.lastEdit = time.Time{} }

const (
	cardLimit = 3600
)

func (v *progressView) render() string {
	var b strings.Builder
	b.WriteString(v.header())
	if v.request != "" {
		b.WriteString("\n\n" + plugin.Bold("思考 / Request") + "\n" + quoteLines(clipRunes(v.request, 300)))
	}
	if v.status != "" {
		b.WriteString("\n\n" + plugin.Bold("状态 / Status") + "\n" + quoteLines(v.status))
	}
	if v.latest != "" {
		b.WriteString("\n\n" + plugin.Bold("最近观察 / Last result") + "\n" + quoteLines(v.latest))
	}
	if len(v.steps) > 0 {
		b.WriteString("\n\n" + plugin.Bold(fmt.Sprintf("工具调用 / Tools (%d)", len(v.steps))) + "\n" +
			quoteLines(strings.Join(v.steps, "\n")))
	}
	if len(v.plan) > 0 {
		b.WriteString("\n\n" + plugin.Bold("执行计划 / Plan") + "\n" + quoteLines(planText(v.plan)))
	}
	return clipRunes(b.String(), cardLimit)
}

func (v *progressView) renderAnswer(answer string) string {
	var b strings.Builder
	b.WriteString(v.header())
	if v.request != "" {
		b.WriteString("\n\n" + plugin.Bold("思考 / Request") + "\n" + quoteLines(clipRunes(v.request, 300)))
	}
	if len(v.plan) > 0 {
		b.WriteString("\n\n" + plugin.Bold("执行计划 / Plan") + "\n" + quoteLines(planText(v.plan)))
	}
	if s := strings.TrimSpace(answer); s != "" {
		b.WriteString("\n\n" + plugin.Bold("回复 / Answer") + "\n" + quoteLines(s))
	}
	return clipRunes(b.String(), cardLimit)
}

func (v *progressView) renderFailure(msg string) string {
	var b strings.Builder
	b.WriteString(v.header())
	b.WriteString("\n\n" + plugin.Bold("出错 / Failed") + "\n" + quoteLines(msg))
	return clipRunes(b.String(), cardLimit)
}

func (v *progressView) header() string {
	return fmt.Sprintf("%s | %s | %s | %s",
		plugin.Bold("模型")+" "+plugin.Code(v.model),
		plugin.Bold("token")+" "+plugin.Code(strconv.Itoa(v.tokens)),
		plugin.Bold("耗时")+" "+plugin.Code(elapsed(v.started)),
		plugin.Bold("工作区")+" "+plugin.Code(v.workspace))
}

func planText(plan []planStep) string {
	var lines []string
	for _, s := range plan {
		switch s.Status {
		case "completed":
			lines = append(lines, "✓ "+s.Text)
		case "in_progress":
			lines = append(lines, "→ "+s.Text)
		default:
			lines = append(lines, "· "+s.Text)
		}
	}
	return strings.Join(lines, "\n")
}

// ============================================================
// Small helpers
// ============================================================

// quoteLines prefixes every line so Telegram renders a blockquote.
func quoteLines(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i, l := range lines {
		lines[i] = "> " + l
	}
	return strings.Join(lines, "\n")
}

func normalizeArgs(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return "{}"
	}
	return raw
}

func parsePlan(argsJSON string) []planStep {
	var args struct {
		Plan []struct {
			Step   string `json:"step"`
			Status string `json:"status"`
		} `json:"plan"`
	}
	if json.Unmarshal([]byte(argsJSON), &args) != nil {
		return nil
	}
	out := make([]planStep, 0, len(args.Plan))
	for _, x := range args.Plan {
		out = append(out, planStep{Text: x.Step, Status: x.Status})
	}
	return out
}

// statusText mirrors the tool call as a one-line action label.
func statusText(name, argsJSON string) string {
	a := firstLines(strings.ReplaceAll(argsJSON, "\n", " "), 90)
	switch name {
	case "list_files":
		return "📂 列出目录 / listing " + a
	case "read_file":
		return "📄 读取文件 / reading " + a
	case "search_files":
		return "🔍 搜索 / searching " + a
	case "write_file":
		return "✍️ 写入文件 / writing " + a
	case "run_command":
		return "⚡ 执行命令 / running " + a
	case "send_file":
		return "📤 发送文件 / sending " + a
	case "finish":
		return "✅ 整理结论 / wrapping up"
	default:
		return "🔧 " + name + " " + a
	}
}

// stepLine is the compact record kept in the tools section.
func stepLine(name, argsJSON string) string {
	return fmt.Sprintf("%s: %s", name, firstLines(strings.ReplaceAll(argsJSON, "\n", " "), 80))
}

func elapsed(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.0fs", d.Seconds())
	default:
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	}
}

func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// firstLines trims s to at most n runes, collapsing the rest.
func firstLines(s string, n int) string {
	return clipRunes(strings.TrimSpace(s), n)
}
