// Package whois queries domain WHOIS registration data through
// namebeta.com's check API, ported faithfully from TeleBox's whois plugin.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	// checkURL is namebeta's SSE-streaming domain check endpoint.
	checkURL = "https://namebeta.com/api/search/check"
	// userAgent mirrors the source's client header.
	userAgent = "TeleBox/1.0"
	// rawMaxRunes caps the raw whois block shown in the result card.
	rawMaxRunes = 3000
)

var (
	// domainURLRe finds domain-looking tokens, with optional scheme/www —
	// the source's reply-text extraction regex.
	domainURLRe = regexp.MustCompile(`(?i)(?:https?://)?(?:www\.)?([a-zA-Z0-9][a-zA-Z0-9-]{0,61}[a-zA-Z0-9]?(?:\.[a-zA-Z]{2,})+)`)
	// domainValidRe is the source's domain format check.
	domainValidRe  = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9-]{0,61}[a-zA-Z0-9]?(?:\.[a-zA-Z]{2,})+$`)
	schemePrefixRe = regexp.MustCompile(`(?i)^https?://`)
	wwwPrefixRe    = regexp.MustCompile(`(?i)^www\.`)
	reRegistrar    = regexp.MustCompile(`(?i)Registrar:\s*(.+)`)
	reCreated      = regexp.MustCompile(`(?i)Creation Date:\s*(.+)`)
	reExpiry       = regexp.MustCompile(`(?i)Registry Expiry Date:\s*(.+)`)
	reUpdated      = regexp.MustCompile(`(?i)Updated Date:\s*(.+)`)
	reStatus       = regexp.MustCompile(`(?i)Domain Status:\s*(.+)`)
	reNameServer   = regexp.MustCompile(`(?i)(?:Name Server|nserver|NS):\s*(.+)`)
	reNSPrefix     = regexp.MustCompile(`(?i)(?:Name Server|nserver|NS):\s*`)
)

// cleanDomain strips an optional scheme, www. prefix and any path — the
// source's cleanup before validation.
func cleanDomain(s string) string {
	s = schemePrefixRe.ReplaceAllString(s, "")
	s = wwwPrefixRe.ReplaceAllString(s, "")
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	return s
}

// extractDomain returns the first domain-looking token in text (used for
// replied-to messages).
func extractDomain(text string) string {
	m := domainURLRe.FindString(text)
	if m == "" {
		return ""
	}
	return cleanDomain(m)
}

// validDomain reports whether s passes the source's domain regex.
func validDomain(s string) bool { return domainValidRe.MatchString(s) }

// sseEvent is one parsed `data: ` line of the SSE stream; Data stays raw
// because non-check events have unrelated shapes.
type sseEvent struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// parseSSEResponse parses the SSE stream, skipping lines that are not
// `data: `-prefixed JSON (the source logs and skips bad lines).
func parseSSEResponse(raw string) []sseEvent {
	var events []sseEvent
	for _, line := range strings.Split(raw, "\n") {
		t := strings.TrimSpace(line)
		if !strings.HasPrefix(t, "data: ") {
			continue
		}
		var e sseEvent
		if err := json.Unmarshal([]byte(t[len("data: "):]), &e); err != nil {
			continue
		}
		events = append(events, e)
	}
	return events
}

// extractWhoisFromSSE returns the raw whois text from the first check
// event carrying one (empty whois counts as absent, as in JS truthiness).
func extractWhoisFromSSE(raw string) string {
	for _, e := range parseSSEResponse(raw) {
		if e.Type != "check" {
			continue
		}
		var d struct {
			Whois struct {
				Whois string `json:"whois"`
			} `json:"whois"`
		}
		if err := json.Unmarshal(e.Data, &d); err == nil && d.Whois.Whois != "" {
			return d.Whois.Whois
		}
	}
	return ""
}

// cutRawData drops everything from "For more information" on, cutting the
// registry boilerplate tail like the source.
func cutRawData(s string) string {
	if i := strings.Index(s, "For more information"); i >= 0 {
		return s[:i]
	}
	return s
}

// extractInfo returns the first capture of re, trimmed; "" when absent
// (the source's "N/A" becomes an omitted field).
func extractInfo(data string, re *regexp.Regexp) string {
	if m := re.FindStringSubmatch(data); m != nil {
		return strings.TrimSpace(m[1])
	}
	return ""
}

// extractNameServers collects Name Server / nserver / NS entries in all
// three formats the source handles.
func extractNameServers(data string) []string {
	var out []string
	for _, m := range reNameServer.FindAllString(data, -1) {
		ns := strings.TrimSpace(reNSPrefix.ReplaceAllString(m, ""))
		if ns != "" {
			out = append(out, ns)
		}
	}
	return out
}

// buildRecord turns cleaned whois text into a WhoisRecord.
func buildRecord(domain, cleaned string) WhoisRecord {
	return WhoisRecord{
		Domain:      domain,
		Registrar:   extractInfo(cleaned, reRegistrar),
		CreatedDate: extractInfo(cleaned, reCreated),
		ExpiryDate:  extractInfo(cleaned, reExpiry),
		UpdatedDate: extractInfo(cleaned, reUpdated),
		Status:      extractInfo(cleaned, reStatus),
		NameServers: extractNameServers(cleaned),
		RawData:     strings.TrimSpace(cleaned),
		QueryTime:   nowFunc().UTC().Format(time.RFC3339),
	}
}

// whoisDateLayouts are the date formats registries actually emit.
var whoisDateLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05Z07:00",
	"2006-01-02 15:04:05",
	"2006-01-02",
	"02-Jan-2006 15:04:05 MST",
	"02-Jan-2006",
	"January 2, 2006",
}

// parseWhoisDate parses a whois date; ok is false when none match (the
// source swallows Date parse errors and skips the expiry note).
func parseWhoisDate(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	for _, l := range whoisDateLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// checkClient has no global timeout; each request carries its own context
// deadline from the timeout setting.
var checkClient = &http.Client{}

// httpStatusError marks a non-200 answer from the check API.
type httpStatusError struct{ code int }

func (e *httpStatusError) Error() string { return fmt.Sprintf("HTTP %d", e.code) }

// fetchWhoisCheck runs the live query and returns the extracted whois
// text; a package var so tests can stub it (no network in tests).
var fetchWhoisCheck = func(ctx context.Context, timeout time.Duration, domain string) (string, error) {
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	qctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	u := checkURL + "?" + url.Values{"query": {domain}}.Encode()
	req, err := http.NewRequestWithContext(qctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := checkClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", &httpStatusError{code: resp.StatusCode}
	}
	return extractWhoisFromSSE(string(body)), nil
}

// isTimeoutErr reports whether err is a deadline/timeout failure, for the
// source's ECONNABORTED/ETIMEDOUT branch.
func isTimeoutErr(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne interface{ Timeout() bool }
	if errors.As(err, &ne) {
		return ne.Timeout()
	}
	return false
}
