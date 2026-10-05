package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// recOpenAI records the last request body and serves one canned choice.
func recOpenAI(t *testing.T, status int, resp string, saw *map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		*saw = m
		w.WriteHeader(status)
		_, _ = w.Write([]byte(resp))
	}))
}

func TestOpenAIChatSendsHistory(t *testing.T) {
	var saw map[string]any
	srv := recOpenAI(t, 200, `{"choices":[{"message":{"content":"pong"}}]}`, &saw)
	defer srv.Close()

	cfg := chatConfig{apiKey: "sk-test", model: "gpt-4o-mini", system: "be nice"}
	hist := []turn{
		{roleUser, "q1"}, {roleAssistant, "a1"}, {roleUser, "ping"},
	}
	out, err := openaiChat(context.Background(), srv.Client(), cfg, hist, srv.URL+"/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	if out != "pong" {
		t.Fatalf("got %q", out)
	}
	if saw["model"] != "gpt-4o-mini" {
		t.Fatalf("model = %v", saw["model"])
	}
	msgs, ok := saw["messages"].([]any)
	if !ok || len(msgs) != 4 {
		t.Fatalf("messages = %v", saw["messages"])
	}
	first := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "be nice" {
		t.Fatalf("system message = %v", first)
	}
	last := msgs[3].(map[string]any)
	if last["role"] != "user" || last["content"] != "ping" {
		t.Fatalf("last message = %v", last)
	}
}

func TestOpenAIChatErrors(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"401", 401, `{"error":{"message":"bad key"}}`, "bad key"},
		{"empty", 200, `{"choices":[]}`, "empty choices"},
		{"blank", 200, `{"choices":[{"message":{"content":"  "}}]}`, "empty reply"},
	}
	for _, c := range cases {
		var saw map[string]any
		srv := recOpenAI(t, c.status, c.body, &saw)
		_, err := openaiChat(context.Background(), srv.Client(), chatConfig{apiKey: "k", model: "m"}, []turn{{roleUser, "q"}}, srv.URL)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: err = %v, want %q", c.name, err, c.want)
		}
		srv.Close()
	}
}

func TestOpenAIChatPartsContent(t *testing.T) {
	var saw map[string]any
	body := `{"choices":[{"message":{"content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}}]}`
	srv := recOpenAI(t, 200, body, &saw)
	defer srv.Close()
	out, err := openaiChat(context.Background(), srv.Client(), chatConfig{apiKey: "k", model: "m"}, []turn{{roleUser, "q"}}, srv.URL)
	if err != nil || out != "a\nb" {
		t.Fatalf("out=%q err=%v", out, err)
	}
}

func TestGeminiChat(t *testing.T) {
	var sawPath, sawKey string
	var sawBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		sawKey = r.URL.Query().Get("key")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &sawBody)
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"hello "},{"text":"there"}]}}]}`))
	}))
	defer srv.Close()

	cfg := chatConfig{apiKey: "g-key", baseURL: srv.URL, model: "gemini-2.0-flash", system: "sys"}
	hist := []turn{{roleUser, "hi"}, {roleAssistant, "yo"}, {roleUser, "again"}}
	out, err := geminiChat(context.Background(), srv.Client(), cfg, hist)
	if err != nil {
		t.Fatal(err)
	}
	if out != "hello \nthere" {
		t.Fatalf("out=%q", out)
	}
	if !strings.Contains(sawPath, "/models/gemini-2.0-flash:generateContent") {
		t.Fatalf("path=%q", sawPath)
	}
	if sawKey != "g-key" {
		t.Fatalf("key=%q", sawKey)
	}
	sysi, ok := sawBody["systemInstruction"].(map[string]any)
	if !ok {
		t.Fatalf("systemInstruction missing: %v", sawBody)
	}
	parts := sysi["parts"].([]any)[0].(map[string]any)
	if parts["text"] != "sys" {
		t.Fatalf("sys parts = %v", parts)
	}
	contents := sawBody["contents"].([]any)
	if len(contents) != 3 {
		t.Fatalf("contents = %v", contents)
	}
	if contents[1].(map[string]any)["role"] != "model" {
		t.Fatalf("assistant role mapping wrong: %v", contents[1])
	}
}

func TestTimeoutContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := openaiChat(ctx, srv.Client(), chatConfig{apiKey: "k", model: "m"}, []turn{{roleUser, "q"}}, srv.URL)
	if err == nil {
		t.Fatal("expected timeout error")
	}
}
