package main

// store.go — persistent config in data/textmode/config.json, mirroring the
// TeleBox mode.ts shape: per-chat modes, whitelist, blacklist. The global
// default mode lives in the bot-panel settings, not here.

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
)

// mode is a formatting mode. The zero value is invalid; modeOff means "no
// formatting" (fall through to the global default).
type mode string

const (
	modeOff       mode = "off"
	modeDel       mode = "del"
	modeBold      mode = "bold"
	modeItalic    mode = "italic"
	modeUnderline mode = "underline"
	modeMask      mode = "mask"
	modeAll       mode = "all"
)

// modes in display/help order.
var allModes = []mode{modeOff, modeDel, modeBold, modeItalic, modeUnderline, modeMask, modeAll}

func parseMode(s string) (mode, bool) {
	if s == "" {
		return "", false
	}
	m := mode(s)
	for _, v := range allModes {
		if v == m {
			return m, true
		}
	}
	return "", false
}

// modeLabel renders a mode with a human hint of what it does.
func modeLabel(tl func(string, string) string, m mode) string {
	switch m {
	case modeDel:
		return string(m) + " " + tl("（删除线）", "(strike)")
	case modeBold:
		return string(m) + " " + tl("（粗体）", "(bold)")
	case modeItalic:
		return string(m) + " " + tl("（斜体）", "(italic)")
	case modeUnderline:
		return string(m) + " " + tl("（下划线）", "(underline)")
	case modeMask:
		return string(m) + " " + tl("（遮罩）", "(spoiler)")
	case modeAll:
		return string(m) + " " + tl("（全格式）", "(all)")
	default:
		return string(m)
	}
}

const (
	listWhite = iota
	listBlack
)

type chatModeEntry struct {
	ID   string
	Mode mode
}

type textmodeConfig struct {
	Chats     map[string]mode `json:"chats"`
	Whitelist []string        `json:"whitelist"`
	Blacklist []string        `json:"blacklist"`
}

type store struct {
	mu   sync.Mutex
	path string
	cfg  textmodeConfig
}

func newStore(dir string) (*store, error) {
	s := &store{path: filepath.Join(dir, "config.json"), cfg: textmodeConfig{Chats: map[string]mode{}}}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *store) load() error {
	b, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if len(b) == 0 {
		return nil
	}
	var cfg textmodeConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		return err
	}
	if cfg.Chats == nil {
		cfg.Chats = map[string]mode{}
	}
	if cfg.Whitelist == nil {
		cfg.Whitelist = []string{}
	}
	if cfg.Blacklist == nil {
		cfg.Blacklist = []string{}
	}
	s.cfg = cfg
	return nil
}

// save writes atomically (temp file + rename, 0600). Call with mu held.
func (s *store) save() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s.cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".tmp-*")
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
	return os.Rename(name, s.path)
}

func chatKey(chatID int64) string { return strconv.FormatInt(chatID, 10) }

// ---- chat modes ----

// chatMode returns (value, set).
func (s *store) chatMode(chatID int64) (mode, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.cfg.Chats[chatKey(chatID)]
	return m, ok
}

func (s *store) setChatMode(chatID int64, m mode) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg.Chats[chatKey(chatID)] = m
	return s.save()
}

func (s *store) resetChat(chatID int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := chatKey(chatID)
	if _, ok := s.cfg.Chats[k]; !ok {
		return false, nil
	}
	delete(s.cfg.Chats, k)
	return true, s.save()
}

// chatModes lists the per-chat settings sorted by chat id.
func (s *store) chatModes() []chatModeEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]chatModeEntry, 0, len(s.cfg.Chats))
	for k, v := range s.cfg.Chats {
		out = append(out, chatModeEntry{ID: k, Mode: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ---- lists ----

func (s *store) list(kind int) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	src := s.cfg.Whitelist
	if kind == listBlack {
		src = s.cfg.Blacklist
	}
	return append([]string(nil), src...)
}

func (s *store) inList(kind int, chatID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	src := s.cfg.Whitelist
	if kind == listBlack {
		src = s.cfg.Blacklist
	}
	k := chatKey(chatID)
	for _, v := range src {
		if v == k {
			return true
		}
	}
	return false
}

func (s *store) listAdd(kind int, chatID int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dst := &s.cfg.Whitelist
	if kind == listBlack {
		dst = &s.cfg.Blacklist
	}
	k := chatKey(chatID)
	for _, v := range *dst {
		if v == k {
			return false, nil
		}
	}
	*dst = append(*dst, k)
	sort.Strings(*dst)
	return true, s.save()
}

func (s *store) listRemove(kind int, chatID int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dst := &s.cfg.Whitelist
	if kind == listBlack {
		dst = &s.cfg.Blacklist
	}
	k := chatKey(chatID)
	out := (*dst)[:0]
	found := false
	for _, v := range *dst {
		if v == k {
			found = true
			continue
		}
		out = append(out, v)
	}
	if !found {
		return false, nil
	}
	*dst = out
	return true, s.save()
}

// ---- effective mode (source priority: whitelist > blacklist > chat > global) ----

// effective returns the mode that applies to chatID. As in mode.ts: a
// non-empty whitelist restricts processing to its chats; a blacklisted chat
// is skipped entirely; a chat set to off (or unset) falls back to the global
// default; off global means "do nothing". A "skip" outcome is reported as
// modeOff with whyBlack / empty-whitelist semantics handled by the caller
// (shouldProcess), so effective stays a pure lookup for status display.
func (s *store) effective(chatID int64, global mode) mode {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.cfg.Chats[chatKey(chatID)]; ok && m != modeOff {
		return m
	}
	if global == modeOff {
		return modeOff
	}
	return global
}

// shouldProcess applies the source listener's gate to chatID and returns the
// mode to format with (modeOff = leave the message alone).
// Order: empty-text/command checks happen in the listener; here it is
// whitelist non-empty & not in it → skip; blacklisted → skip; then chat mode,
// falling back to the global default when off/unset.
func (s *store) shouldProcess(chatID int64, global mode) mode {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := chatKey(chatID)
	if len(s.cfg.Whitelist) > 0 && !contains(s.cfg.Whitelist, k) {
		return modeOff
	}
	if contains(s.cfg.Blacklist, k) {
		return modeOff
	}
	if m, ok := s.cfg.Chats[k]; ok && m != modeOff {
		return m
	}
	return global
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
