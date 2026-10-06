package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// dataFileLayout is the on-disk layout, mirroring the source's lowdb file.
type dataFileLayout struct {
	History  []WhoisRecord          `json:"history"`
	Cache    map[string]WhoisRecord `json:"cache"`
	Settings whoisSettings          `json:"settings"`
}

type whoisSettings struct {
	MaxHistory int `json:"maxHistory"`
	CacheHours int `json:"cacheHours"`
}

// store is the plugin's persistent state: history, cache and stats,
// serialized to data/whois/whois_data.json.
type store struct {
	mu   sync.Mutex
	path string
	d    dataFileLayout
}

const (
	defaultMaxHistory = 100
	defaultCacheHours = 24
)

func newStore(path string) (*store, error) {
	s := &store{path: path}
	s.d = dataFileLayout{
		History:  []WhoisRecord{},
		Cache:    map[string]WhoisRecord{},
		Settings: whoisSettings{MaxHistory: defaultMaxHistory, CacheHours: defaultCacheHours},
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// load reads the JSON file; a missing or empty file keeps the defaults.
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
	var d dataFileLayout
	if err := json.Unmarshal(b, &d); err != nil {
		return err
	}
	if d.History != nil {
		s.d.History = d.History
	}
	if d.Cache != nil {
		s.d.Cache = d.Cache
	}
	if d.Settings.MaxHistory > 0 {
		s.d.Settings.MaxHistory = d.Settings.MaxHistory
	}
	if d.Settings.CacheHours > 0 {
		s.d.Settings.CacheHours = d.Settings.CacheHours
	}
	return nil
}

// write persists atomically (temp file + rename, 0600).
func (s *store) write() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s.d, "", "  ")
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

// cached returns the live cache entry for domain, dropping expired ones
// like the source's getCachedResult.
func (s *store) cached(domain string) (WhoisRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := lowerKey(domain)
	rec, ok := s.d.Cache[key]
	if !ok {
		return WhoisRecord{}, false
	}
	if s.expiredLocked(rec) {
		delete(s.d.Cache, key)
		_ = s.write()
		return WhoisRecord{}, false
	}
	return rec, true
}

// pruneCache drops expired entries. Called when the cache outgrew
// cachePruneSize: without it, domains never queried again would pile up in
// whois_data.json forever (each carrying up to 3k chars of raw data).
const cachePruneSize = 500

func (s *store) pruneCacheLocked() (removed int) {
	if len(s.d.Cache) < cachePruneSize {
		return 0
	}
	for k, rec := range s.d.Cache {
		if s.expiredLocked(rec) {
			delete(s.d.Cache, k)
			removed++
		}
	}
	return removed
}

// expiredLocked reports whether rec outlived cacheHours.
func (s *store) expiredLocked(rec WhoisRecord) bool {
	t, ok := parseQueryTime(rec.QueryTime)
	if !ok {
		return true
	}
	hours := s.d.Settings.CacheHours
	if hours <= 0 {
		hours = defaultCacheHours
	}
	return nowFunc().Sub(t) > time.Duration(hours)*time.Hour
}

// save stores the record in cache and history, trimming history to
// maxHistory entries (newest first, like the source's unshift) and pruning
// expired cache entries once the cache grows large.
func (s *store) save(rec WhoisRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.d.Cache[lowerKey(rec.Domain)] = rec
	s.pruneCacheLocked()
	s.d.History = append([]WhoisRecord{rec}, s.d.History...)
	max := s.d.Settings.MaxHistory
	if max <= 0 {
		max = defaultMaxHistory
	}
	if len(s.d.History) > max {
		s.d.History = s.d.History[:max]
	}
	return s.write()
}

// clear wipes history and cache; it reports whether anything was removed.
func (s *store) clear() (historyCount, cacheCount int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	historyCount = len(s.d.History)
	cacheCount = len(s.d.Cache)
	if historyCount == 0 && cacheCount == 0 {
		return 0, 0, nil
	}
	s.d.History = []WhoisRecord{}
	s.d.Cache = map[string]WhoisRecord{}
	err = s.write()
	return historyCount, cacheCount, err
}

// snapshot returns a copy of the current state for rendering.
func (s *store) snapshot() (history []WhoisRecord, cacheCount, cacheHours int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := make([]WhoisRecord, len(s.d.History))
	copy(h, s.d.History)
	return h, len(s.d.Cache), s.d.Settings.CacheHours
}

func lowerKey(domain string) string { return strings.ToLower(domain) }

// parseQueryTime parses a stored queryTime (RFC3339).
func parseQueryTime(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, s)
	return t, err == nil
}

// nowFunc is the clock, stubbed in tests.
var nowFunc = time.Now
