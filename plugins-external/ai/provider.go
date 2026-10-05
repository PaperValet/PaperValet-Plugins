package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// providerKind selects the wire format derived from the base URL host, like
// the source's PROVIDER_HOST_TYPES.
type providerKind int

const (
	providerOpenAI providerKind = iota
	providerGemini
	providerDoubao
)

func detectProvider(baseURL string) providerKind {
	u, err := url.Parse(baseURL)
	if err != nil {
		return providerOpenAI
	}
	switch u.Hostname() {
	case "generativelanguage.googleapis.com":
		return providerGemini
	case "ark.cn-beijing.volces.com", "ark.ap-southeast.volces.com":
		return providerDoubao
	}
	if strings.HasSuffix(u.Hostname(), ".volces.com") {
		return providerDoubao
	}
	return providerOpenAI
}

// chatEndpoint resolves the chat/completions path for OpenAI-style providers.
// The source normalizes odd base URLs (strips known suffixes, forces /v1);
// we accept a plain base and append the standard path.
func chatEndpoint(baseURL string, kind providerKind) string {
	base := strings.TrimRight(baseURL, "/")
	if kind == providerDoubao {
		// Volcengine Ark v3 speaks the OpenAI schema at api/v3.
		if strings.Contains(base, "/api/v3") {
			return base + "/chat/completions"
		}
		return base + "/api/v3/chat/completions"
	}
	for _, s := range []string{"/chat/completions", "/completions", "/responses", "/messages"} {
		if strings.HasSuffix(base, s) {
			return base
		}
	}
	return base + "/chat/completions"
}

type turnRole string

const (
	roleUser      turnRole = "user"
	roleAssistant turnRole = "assistant"
)

type turn struct {
	Role turnRole `json:"role"`
	Text string   `json:"text"`
}

// apiError carries a provider error message to the user card.
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

// openaiChat posts an OpenAI-compatible chat/completions request
// (non-streaming, like the source's default stream:false config).
func openaiChat(ctx context.Context, client *http.Client, cfg chatConfig, hist []turn, endpoint string) (string, error) {
	msgs := []map[string]any{}
	if sys := strings.TrimSpace(cfg.system); sys != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": sys})
	}
	for _, t := range hist {
		msgs = append(msgs, map[string]any{"role": string(t.Role), "content": t.Text})
	}
	if len(msgs) == 0 {
		return "", errors.New("empty request")
	}
	body, _ := json.Marshal(map[string]any{
		"model":    cfg.model,
		"messages": msgs,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.apiKey)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return "", &apiError{status: resp.StatusCode, msg: extractProviderError(raw)}
	}
	var data struct {
		Choices []struct {
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return "", &apiError{msg: "bad JSON: " + err.Error()}
	}
	if data.Error != nil && data.Error.Message != "" {
		return "", &apiError{msg: data.Error.Message}
	}
	if len(data.Choices) == 0 {
		return "", &apiError{msg: "empty choices"}
	}
	text, err := parseContent(data.Choices[0].Message.Content)
	if err != nil {
		return "", &apiError{msg: err.Error()}
	}
	if strings.TrimSpace(text) == "" {
		return "", &apiError{msg: "empty reply"}
	}
	return text, nil
}

// parseContent accepts string content or the array-of-parts shape some
// gateways return.
func parseContent(raw json.RawMessage) (string, error) {
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

// geminiChat posts a generateContent request with systemInstruction.
func geminiChat(ctx context.Context, client *http.Client, cfg chatConfig, hist []turn) (string, error) {
	base := strings.TrimRight(cfg.baseURL, "/")
	endpoint := base + "/models/" + url.PathEscape(cfg.model) + ":generateContent?key=" + url.QueryEscape(cfg.apiKey)

	contents := make([]map[string]any, 0, len(hist))
	for _, t := range hist {
		role := "user"
		if t.Role == roleAssistant {
			role = "model"
		}
		contents = append(contents, map[string]any{
			"role":  role,
			"parts": []map[string]any{{"text": t.Text}},
		})
	}
	body := map[string]any{"contents": contents}
	if sys := strings.TrimSpace(cfg.system); sys != "" {
		body["systemInstruction"] = map[string]any{
			"parts": []map[string]any{{"text": sys}},
		}
	}
	payload, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return "", &apiError{status: resp.StatusCode, msg: extractProviderError(raw)}
	}
	var data struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return "", &apiError{msg: "bad JSON: " + err.Error()}
	}
	var segs []string
	for _, c := range data.Candidates {
		for _, part := range c.Content.Parts {
			if part.Text != "" {
				segs = append(segs, part.Text)
			}
		}
	}
	text := strings.Join(segs, "\n")
	if strings.TrimSpace(text) == "" {
		return "", &apiError{msg: "empty reply"}
	}
	return text, nil
}
