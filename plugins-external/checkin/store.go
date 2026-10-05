package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/gotd/td/tg"
)

// Target is one sign-in target.
type Target struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Target       string `json:"target"` // @username or numeric chat id
	Command      string `json:"command"`
	CallbackData string `json:"callbackData,omitempty"`
	ButtonText   string `json:"buttonText,omitempty"`
	Enabled      bool   `json:"enabled"`
}

func hasMatcher(t *Target) bool { return t.CallbackData != "" || t.ButtonText != "" }

// config is the persisted plugin state (data/checkin/config.json).
type config struct {
	RunTime     string    `json:"runTime"`
	RunTimeEnd  string    `json:"runTimeEnd"`
	RandomDelay int       `json:"randomDelay"`
	LastRunDate string    `json:"lastRunDate"`
	NextRunAt   int64     `json:"nextRunAt"`
	NextRunDate string    `json:"nextRunDate"`
	NotifyChat  int64     `json:"notifyChat,omitempty"`
	Targets     []*Target `json:"targets"`
}

func defaultConfig() config {
	return config{
		RunTime:     "10:00",
		RunTimeEnd:  "11:30",
		RandomDelay: 0,
	}
}

func configPath(dir string) string { return filepath.Join(dir, "config.json") }

// normalize drops legacy fields and fixes date formats on load.
func (c *config) normalize() {
	c.LastRunDate = normalizeDate(c.LastRunDate)
	c.NextRunDate = normalizeDate(c.NextRunDate)
	if c.Targets == nil {
		c.Targets = []*Target{}
	}
	kept := c.Targets[:0]
	seen := map[string]bool{}
	for _, t := range c.Targets {
		if t == nil || t.ID == "" || t.Name == "" || t.Target == "" || t.Command == "" || seen[t.ID] {
			continue
		}
		seen[t.ID] = true
		kept = append(kept, t)
	}
	c.Targets = kept
}

func (c config) enabled() bool {
	for _, t := range c.Targets {
		if t != nil && t.Enabled {
			return true
		}
	}
	return false
}

// normalizeDate accepts legacy "2026/10/1" and normalizes to "2026-10-01".
func normalizeDate(v string) string {
	m := dateRe.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return ""
	}
	return m[1] + "-" + pad2(mustAtoi(m[2])) + "-" + pad2(mustAtoi(m[3]))
}

var dateRe = regexp.MustCompile(`^(\d{4})[-/](\d{1,2})[-/](\d{1,2})$`)

var chatIDRe = regexp.MustCompile(`^-?\d{1,20}$`)

func mustAtoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if len(b) == 0 {
		return nil
	}
	return json.Unmarshal(b, v)
}

// writeJSON writes atomically (temp file + rename, 0600).
func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Chmod(name, 0o600); err != nil {
		os.Remove(name)
		return err
	}
	return os.Rename(name, path)
}

// parseMatcher turns the trailing args of `add` into a button matcher:
// "data:<cb>" (or a bare string) matches callback data, "text:<label>"
// matches the button caption.
func parseMatcher(args []string) (data, text string) {
	raw := strings.TrimSpace(strings.Join(args, " "))
	if raw == "" {
		return "", ""
	}
	if strings.HasPrefix(raw, "text:") {
		return "", strings.TrimSpace(strings.TrimPrefix(raw, "text:"))
	}
	return strings.TrimPrefix(raw, "data:"), ""
}

// refFromPeer persists an InputPeer so restored targets need no resolver.
type peerRef struct {
	Type       string `json:"type"` // user|chat|channel|self
	ID         int64  `json:"id"`
	AccessHash int64  `json:"access_hash,omitempty"`
}

func refFromPeer(p tg.InputPeerClass) *peerRef {
	switch v := p.(type) {
	case *tg.InputPeerUser:
		return &peerRef{Type: "user", ID: v.UserID, AccessHash: v.AccessHash}
	case *tg.InputPeerChat:
		return &peerRef{Type: "chat", ID: v.ChatID}
	case *tg.InputPeerChannel:
		return &peerRef{Type: "channel", ID: v.ChannelID, AccessHash: v.AccessHash}
	case *tg.InputPeerSelf:
		return &peerRef{Type: "self"}
	}
	return nil
}

func (r *peerRef) input() tg.InputPeerClass {
	if r == nil {
		return nil
	}
	switch r.Type {
	case "user":
		return &tg.InputPeerUser{UserID: r.ID, AccessHash: r.AccessHash}
	case "chat":
		return &tg.InputPeerChat{ChatID: r.ID}
	case "channel":
		return &tg.InputPeerChannel{ChannelID: r.ID, AccessHash: r.AccessHash}
	case "self":
		return &tg.InputPeerSelf{}
	}
	return nil
}
