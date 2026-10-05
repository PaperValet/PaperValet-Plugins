package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf16"

	"github.com/gotd/td/tg"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

// systemPrompt is the TeleBox source's instruction, verbatim.
const systemPrompt = `你的任务是对用户的内容（文字或图片）做出一句"羡慕 + 调侃式的称呼或短语"的回复。

规则（调侃版本）：

1. 输出永远只有一句话："羡慕XXX"。
2. XXX 必须是来自用户内容的"可以被轻松调侃"的点。
3. 如果用户发送图片，请识别图片内容并找到可以调侃的点。
4. 不要书面语言，不要抽象词汇，用口语、俚语、小坏笑的风格，比如：
   - "富哥"
   - "狠人"
   - "老整活"
   - "会玩"
   - "大聪明"
   - "神仙操作"
   - "小日子"
5. 回复越短越好，2～4 个字优先。
6. 回复带点调侃，不要太认真，也不要太过火。
7. 负面内容也可以轻轻调侃：
   - 倒霉 → "霉神"
   - 加班 → "打工魂"
   - 心情差 → "情绪达人"
8. 不要解释，不要分析，不要问问题，不要重复用户原句。

示例（只是风格参考）：
用户：我今天吃寿司。
你：羡慕会享受

用户：我下午要加班。
你：羡慕打工魂

用户：我今天心情不好。
你：羡慕情绪达人

用户：我买新手机了。
你：羡慕富哥

用户：[一张豪华跑车图片]
你：羡慕富哥

用户：[一张可爱猫咪贴纸]
你：羡慕猫奴

用户：[一张美食图片]
你：羡慕会吃`

const (
	maxResponseTokens = 4000
	// UTF-16 code units: Telegram's own message metric.
	tokenUnit = 4
	// How much of an oversized reply is kept, like the source.
	truncKeepRunes = 1000
)

// mediaInput is one image for the vision request.
type mediaInput struct {
	mimeType string
	data     []byte
}

// ask runs one Q&A round and edits the command message with the reply.
func (p *XmslPlugin) ask(ctx *plugin.CommandContext, question string, img *mediaInput) error {
	cfg := p.config()
	if cfg.key == "" {
		return ctx.Edit("❌ " + ctx.Tlocal("还没有设置 API 密钥", "No API key configured yet") + "\n> " +
			ctx.Tlocal("请在机器人面板 /menu → 羡慕死了 里设置", "Set it in the bot panel: /menu → Envy me"))
	}

	waiting := "🔄 " + ctx.Tlocal("处理中…", "Thinking…")
	if img != nil {
		waiting = "🔄 " + ctx.Tlocal("正在识别图片…", "Recognizing the image…")
	}
	_ = ctx.Edit(waiting)

	var (
		answer string
		err    error
	)
	if cfg.mode == "gemini" {
		answer, err = geminiChat(ctx.Context(), p.http, cfg, question, img)
	} else {
		answer, err = openaiChat(ctx.Context(), p.http, cfg, question, img)
	}
	if err != nil {
		return ctx.Edit(apiErrCard(ctx, err))
	}

	answer = removeThinkTags(answer)
	answer, tokens, truncated := clampAnswer(answer, maxResponseTokens, truncKeepRunes)
	if truncated {
		return ctx.Edit("⚠️ " + ctx.Tlocal(
			fmt.Sprintf("回复过长（%d tokens，超过限制 %d），已截断", tokens, maxResponseTokens),
			fmt.Sprintf("reply too long (%d tokens, limit %d), truncated", tokens, maxResponseTokens)) +
			"\n\n" + plugin.Escape(answer))
	}
	if strings.TrimSpace(answer) == "" {
		return ctx.Edit("❌ " + ctx.Tlocal("AI 没有返回内容", "The AI returned nothing"))
	}
	return ctx.Edit(plugin.Escape(answer))
}

// ---------------------------------------------------------------- helpers

// removeThinkTags drops a leading <think>…</think> block, keeping whatever
// follows the closing tag (DeepSeek-style reasoning models).
func removeThinkTags(text string) string {
	if !strings.Contains(text, "<think>") || !strings.Contains(text, "</think>") {
		return text
	}
	if i := strings.LastIndex(text, "</think>"); i >= 0 {
		return strings.TrimSpace(text[i+len("</think>"):])
	}
	return text
}

// clampAnswer reports the estimated token count (len/4 like the source) and,
// when over the limit, truncates to keep runes.
func clampAnswer(answer string, maxTokens, keepRunes int) (string, int, bool) {
	estimated := (utf16Len(answer) + tokenUnit - 1) / tokenUnit
	if estimated <= maxTokens {
		return answer, estimated, false
	}
	r := []rune(answer)
	if len(r) > keepRunes {
		r = r[:keepRunes]
	}
	return string(r) + "...", estimated, true
}

// utf16Len counts UTF-16 code units, the metric both the source's length/4
// estimate and Telegram use.
func utf16Len(s string) int {
	return len(utf16.Encode([]rune(s)))
}

// normalizeBaseURL validates and normalizes a typed base URL: http(s) scheme
// required, trailing slashes stripped.
func normalizeBaseURL(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
		return "", plugin.Invalid("必须是 http(s) 开头的完整地址", "must be a full http(s) URL")
	}
	return strings.TrimRight(s, "/"), nil
}

// apiError carries an HTTP status plus the provider's error text.
type apiError struct {
	status int
	msg    string
}

func (e *apiError) Error() string {
	if e.status > 0 && e.msg != "" {
		return fmt.Sprintf("HTTP %d: %s", e.status, e.msg)
	}
	if e.status > 0 {
		return fmt.Sprintf("HTTP %d", e.status)
	}
	return e.msg
}

// apiErrCard maps an API failure to the bilingual error the source produced
// (400 with provider detail, 401 invalid key, 429 rate limit, connection
// refused, timeout, and a generic tail).
func apiErrCard(ctx *plugin.CommandContext, err error) string {
	var ae *apiError
	switch {
	case errors.As(err, &ae):
		switch ae.status {
		case http.StatusBadRequest:
			return "❌ " + ctx.Tlocal("API 请求错误：", "API request error: ") + plugin.Escape(ae.msg)
		case http.StatusUnauthorized:
			return "❌ " + ctx.Tlocal("API 密钥无效", "Invalid API key")
		case http.StatusTooManyRequests:
			return "❌ " + ctx.Tlocal("请求过于频繁，请稍后重试", "Rate limited, try again soon")
		}
		return "❌ " + ctx.Tlocal("API 调用失败：", "API call failed: ") + plugin.Escape(ae.Error())
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "❌ " + ctx.Tlocal("请求超时，请稍后重试", "Request timed out, try again soon")
	}
	s := err.Error()
	if isConnectionRefused(s) {
		return "❌ " + ctx.Tlocal("无法连接到 API 服务器", "Cannot reach the API server")
	}
	if r := []rune(s); len(r) > 200 {
		s = string(r[:200]) + "..."
	}
	return "❌ " + ctx.Tlocal("API 调用失败：", "API call failed: ") + plugin.Escape(s)
}

func isConnectionRefused(s string) bool {
	s = strings.ToLower(s)
	return strings.Contains(s, "connection refused") || strings.Contains(s, "no such host")
}

// ---------------------------------------------------------------- providers

// openaiChat posts a chat/completions request with the question plus an
// inline data-URL image part, temperature 0.7 like the source.
func openaiChat(ctx context.Context, client *http.Client, cfg apiConfig, question string, img *mediaInput) (string, error) {
	endpoint := strings.TrimRight(cfg.baseURL, "/") + "/chat/completions"

	user := []map[string]any{}
	text := strings.TrimSpace(question)
	if text == "" {
		text = "请识别这张图片/贴纸的内容"
	}
	if img != nil {
		user = append(user,
			map[string]any{"type": "text", "text": text},
			map[string]any{
				"type": "image_url",
				"image_url": map[string]any{
					"url": "data:" + img.mimeType + ";base64," + base64.StdEncoding.EncodeToString(img.data),
				},
			},
		)
	} else {
		user = append(user, map[string]any{"type": "text", "text": text})
	}

	body, _ := json.Marshal(map[string]any{
		"model":       cfg.model,
		"temperature": 0.7,
		"messages": []map[string]any{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": user},
		},
	})
	data, err := postJSON(ctx, client, endpoint, body, map[string]string{
		"Authorization": "Bearer " + cfg.key,
	})
	if err != nil {
		return "", err
	}
	var resp struct {
		Choices []struct {
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", &apiError{msg: "bad JSON: " + err.Error()}
	}
	if len(resp.Choices) == 0 {
		return "", &apiError{msg: "empty choices"}
	}
	content, err := parseContentParts(resp.Choices[0].Message.Content)
	if err != nil {
		return "", &apiError{msg: err.Error()}
	}
	return content, nil
}

// parseContentParts accepts string content or the array-of-parts shape some
// gateways return.
func parseContentParts(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", errors.New("no content")
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var segs []string
		for _, p := range parts {
			if p.Text != "" && (p.Type == "" || p.Type == "text" || p.Type == "output_text") {
				segs = append(segs, p.Text)
			}
		}
		return strings.Join(segs, "\n"), nil
	}
	return "", errors.New("unsupported content shape")
}

// geminiChat posts a generateContent request with systemInstruction and the
// image as inlineData, temperature 0.7. The key travels in the
// x-goog-api-key header, not the query string, so it stays out of proxy and
// server access logs.
func geminiChat(ctx context.Context, client *http.Client, cfg apiConfig, question string, img *mediaInput) (string, error) {
	base := strings.TrimRight(cfg.baseURL, "/")
	endpoint := base + "/models/" + url.PathEscape(cfg.model) + ":generateContent"

	parts := []map[string]any{}
	if text := strings.TrimSpace(question); text != "" {
		parts = append(parts, map[string]any{"text": text})
	} else if img != nil {
		parts = append(parts, map[string]any{"text": "请识别这张图片/贴纸的内容"})
	}
	if img != nil {
		parts = append(parts, map[string]any{
			"inlineData": map[string]any{
				"mimeType": img.mimeType,
				"data":     base64.StdEncoding.EncodeToString(img.data),
			},
		})
	}
	body := map[string]any{
		"contents":         []map[string]any{{"parts": parts}},
		"generationConfig": map[string]any{"temperature": 0.7},
		"systemInstruction": map[string]any{
			"parts": []map[string]any{{"text": systemPrompt}},
		},
	}
	payload, _ := json.Marshal(body)
	data, err := postJSON(ctx, client, endpoint, payload, map[string]string{"x-goog-api-key": cfg.key})
	if err != nil {
		return "", err
	}
	var resp struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", &apiError{msg: "bad JSON: " + err.Error()}
	}
	var segs []string
	for _, c := range resp.Candidates {
		for _, part := range c.Content.Parts {
			if part.Text != "" {
				segs = append(segs, part.Text)
			}
		}
	}
	return strings.Join(segs, ""), nil
}

// postJSON sends the request and returns the body, or an apiError carrying
// the provider's error message.
func postJSON(ctx context.Context, client *http.Client, endpoint string, body []byte, headers map[string]string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &apiError{status: resp.StatusCode, msg: extractProviderError(raw)}
	}
	return raw, nil
}

func extractProviderError(raw []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &e) == nil {
		if e.Error.Message != "" {
			return e.Error.Message
		}
		if e.Message != "" {
			return e.Message
		}
	}
	if len(raw) > 300 {
		raw = raw[:300]
	}
	return strings.TrimSpace(string(raw))
}

// ---------------------------------------------------------------- reply media

// mediaFromReply extracts an image from the replied message: photos as their
// largest size, stickers by kind (tgs via rlottie+ffmpeg, webm via ffmpeg,
// static images directly), documents when their bytes sniff as an image.
func (p *XmslPlugin) mediaFromReply(ctx *plugin.CommandContext, reply *tg.Message) (*mediaInput, error) {
	if reply.Media == nil {
		return nil, nil
	}
	dir, err := p.host.DataDir(p.Name())
	if err != nil {
		return nil, err
	}
	return p.mediaFromMessage(ctx, reply, dir)
}
