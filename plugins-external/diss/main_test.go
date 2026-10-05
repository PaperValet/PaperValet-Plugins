package main

import (
	"errors"
	"net/url"
	"strings"
	"testing"
)

func TestClean(t *testing.T) {
	if s, err := clean("\ufeff  你好 \n"); err != nil || s != "你好" {
		t.Fatalf("got %q %v", s, err)
	}
	for _, in := range []string{"", "  \n", "<html>502</html>"} {
		if _, err := clean(in); err == nil {
			t.Fatalf("%q should fail", in)
		}
	}
}

func TestRetryable(t *testing.T) {
	// Definitive server answers: no retry.
	for _, err := range []error{
		&httpErr{status: 400},
		&httpErr{status: 403},
		&httpErr{status: 404},
		&httpErr{status: 429},
		errors.New("empty response"),
		errors.New("unexpected HTML response"),
	} {
		if retryable(err) {
			t.Errorf("%v should not be retryable", err)
		}
	}
	// Transport errors and 5xx: retry.
	if !retryable(&httpErr{status: 500}) || !retryable(&httpErr{status: 502}) {
		t.Error("5xx should be retryable")
	}
	if !retryable(&url.Error{Op: "Get", URL: apiURL, Err: errors.New("connection reset")}) {
		t.Error("url.Error should be retryable")
	}
}

func TestClip(t *testing.T) {
	if got := clip("短文本", 4000); got != "短文本" {
		t.Errorf("short text changed: %q", got)
	}
	long := strings.Repeat("长", 4500)
	got := clip(long, 4000)
	if n := len([]rune(got)); n != 4001 { // 4000 runes + ellipsis
		t.Errorf("clip = %d runes, want 4001", n)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("missing ellipsis: %q", got[len(got)-3:])
	}
}
