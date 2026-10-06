package main

// store.go — persistent config in data/pangu/config.json, mirroring the
// TeleBox PanguConfig shape (chats, whitelist, blacklist, globalMode, stats).

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"
)

type panguStats struct {
	FormattedMessages int   `json:"formattedMessages"`
	LastFormatted     int64 `json:"lastFormatted"` // unix ms, 0 = never
	EnabledChats      int   `json:"enabledChats"`
}

type panguConfig struct {
	Chats      map[string]bool `json:"chats"`
	Whitelist  []string        `json:"whitelist"`
	Blacklist  []string        `json:"blacklist"`
	GlobalMode bool            `json:"globalMode"`
	Stats      panguStats      `json:"stats"`
}

type store struct {
	mu   sync.Mutex
	path string
	cfg  panguConfig

	// statsDirty marks unwritten stat counters; statsLastFlush backs the
	// debounced write in recordFormatted.
	statsDirty     bool
	statsLastFlush time.Time
}

func newStore(dir string) (*store, error) {
	s := &store{path: filepath.Join(dir, "config.json"), cfg: panguConfig{Chats: map[string]bool{}}}
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
	var cfg panguConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		return err
	}
	if cfg.Chats == nil {
		cfg.Chats = map[string]bool{}
	}
	if cfg.Whitelist == nil {
		cfg.Whitelist = []string{}
	}
	if cfg.Blacklist == nil {
		cfg.Blacklist = []string{}
	}
	s.cfg = cfg
	s.refreshEnabledChatsLocked()
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

func (s *store) refreshEnabledChatsLocked() {
	n := 0
	for _, v := range s.cfg.Chats {
		if v {
			n++
		}
	}
	s.cfg.Stats.EnabledChats = n
}

// ---- chat modes ----

// chatMode returns (value, set).
func (s *store) chatMode(chatID int64) (bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.cfg.Chats[strconv.FormatInt(chatID, 10)]
	return v, ok
}

func (s *store) setChatMode(chatID int64, on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg.Chats[strconv.FormatInt(chatID, 10)] = on
	s.refreshEnabledChatsLocked()
	return s.save()
}

func (s *store) resetChat(chatID int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := strconv.FormatInt(chatID, 10)
	if _, ok := s.cfg.Chats[k]; !ok {
		return false, nil
	}
	delete(s.cfg.Chats, k)
	s.refreshEnabledChatsLocked()
	return true, s.save()
}

// ---- lists ----

func (s *store) inList(list []string, chatID int64) bool {
	k := strconv.FormatInt(chatID, 10)
	for _, v := range list {
		if v == k {
			return true
		}
	}
	return false
}

func (s *store) list(kind string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	src := s.cfg.Whitelist
	if kind == "black" {
		src = s.cfg.Blacklist
	}
	return append([]string(nil), src...)
}

func (s *store) listAdd(kind string, chatID int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dst := &s.cfg.Whitelist
	if kind == "black" {
		dst = &s.cfg.Blacklist
	}
	k := strconv.FormatInt(chatID, 10)
	for _, v := range *dst {
		if v == k {
			return false, nil
		}
	}
	*dst = append(*dst, k)
	sort.Strings(*dst)
	return true, s.save()
}

func (s *store) listRemove(kind string, chatID int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dst := &s.cfg.Whitelist
	if kind == "black" {
		dst = &s.cfg.Blacklist
	}
	k := strconv.FormatInt(chatID, 10)
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

// ---- global mode ----

func (s *store) globalMode() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg.GlobalMode
}

func (s *store) setGlobalMode(on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg.GlobalMode = on
	return s.save()
}

// ---- effective state (priority: whitelist > blacklist > chat > global) ----

func (s *store) effective(chatID int64) (on bool, why string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.inList(s.cfg.Whitelist, chatID):
		return true, "white"
	case s.inList(s.cfg.Blacklist, chatID):
		return false, "black"
	default:
		if v, ok := s.cfg.Chats[strconv.FormatInt(chatID, 10)]; ok {
			return v, "chat"
		}
		return s.cfg.GlobalMode, "global"
	}
}

// statsFlushEvery bounds how long accumulated stats stay unwritten; the
// config is otherwise written on every list/mode mutation (rare).
const statsFlushEvery = time.Minute

// recordFormatted counts one formatted message. The write is debounced:
// the counters live in memory and are flushed at most once per minute (and
// on Stop), so a busy chat does not rewrite config.json on every message.
func (s *store) recordFormatted() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg.Stats.FormattedMessages++
	s.cfg.Stats.LastFormatted = time.Now().UnixMilli()
	if time.Since(s.statsLastFlush) < statsFlushEvery {
		s.statsDirty = true // accumulate; flushed by the next window/Stop
		return
	}
	s.statsLastFlush = time.Now()
	_ = s.save()
}

// flushStats persists pending stats counters if any.
func (s *store) flushStats() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.statsDirty {
		return
	}
	s.statsDirty = false
	_ = s.save()
}

func (s *store) statsSnapshot() (st panguStats, white, black, custom int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st = s.cfg.Stats
	white = len(s.cfg.Whitelist)
	black = len(s.cfg.Blacklist)
	custom = len(s.cfg.Chats)
	return
}
