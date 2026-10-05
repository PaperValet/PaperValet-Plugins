package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// state mirrors the source's stats.json: first run and report counter.
type state struct {
	StartTime   int64 `json:"startTime"`
	ReportCount int   `json:"reportCount"`
}

func loadState(path string) state {
	s := state{}
	if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
		_ = json.Unmarshal(b, &s)
	}
	if s.StartTime == 0 {
		s.StartTime = time.Now().Unix()
	}
	return s
}

// cacheEntry is one stored report, keyed by chat+period+language.
type cacheEntry struct {
	GeneratedAt int64   `json:"generatedAt"`
	Report      *Report `json:"report"`
}

// cacheFileV2 is the on-disk shape: map key → entry, newest kept.
type cacheFileV2 struct {
	Entries map[string]*cacheEntry `json:"entries"`
	Order   []string               `json:"order"`
}

func loadCacheEntry(dir, key string) (*cacheEntry, bool) {
	var c cacheFileV2
	if err := readJSON(filepath.Join(dir, cacheFile), &c); err != nil || c.Entries == nil {
		return nil, false
	}
	e, ok := c.Entries[key]
	return e, ok && e.Report != nil
}

func saveCacheEntry(dir, key string, e *cacheEntry) {
	path := filepath.Join(dir, cacheFile)
	var c cacheFileV2
	_ = readJSON(path, &c)
	if c.Entries == nil {
		c.Entries = map[string]*cacheEntry{}
	}
	c.Entries[key] = e
	c.Order = append([]string{key}, c.Order...)
	if len(c.Order) > cacheLimit {
		for _, k := range c.Order[cacheLimit:] {
			delete(c.Entries, k)
		}
		c.Order = c.Order[:cacheLimit]
	}
	_ = writeJSON(path, c)
}

func cacheKey(chatID int64, label, lang string) string {
	return strconv.FormatInt(chatID, 10) + "|" + label + "|" + lang
}

// readJSON tolerates missing/empty files.
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
