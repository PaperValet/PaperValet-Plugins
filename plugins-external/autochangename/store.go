package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

const stateFile = "state.json"

// store is the on-disk document.
type store struct {
	Items        map[string][]string `json:"items"`
	Index        map[string]int      `json:"index"`
	Origin       originInfo          `json:"origin"`
	LastFireUnix int64               `json:"last_fire_unix"`
}

type originInfo struct {
	First    string `json:"first,omitempty"`
	Last     string `json:"last,omitempty"`
	SetLast  bool   `json:"set_last,omitempty"`
	Bio      string `json:"bio,omitempty"`
	Username string `json:"username,omitempty"`
	// Captured marks which fields were read from the account before the
	// first rotation (an empty bio is a real, restorable value).
	CapturedBio      bool `json:"captured_bio,omitempty"`
	CapturedUsername bool `json:"captured_username,omitempty"`
}

// stateStore guards the whole document with one mutex.
type stateStore struct {
	mu   sync.Mutex
	path string
	doc  store
}

func newStateStore(path string) *stateStore {
	return &stateStore{path: path}
}

func (s *stateStore) load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.doc = store{Items: map[string][]string{}, Index: map[string]int{}}
			return nil
		}
		return err
	}
	if len(b) == 0 {
		s.doc = store{Items: map[string][]string{}, Index: map[string]int{}}
		return nil
	}
	var d store
	if err := json.Unmarshal(b, &d); err != nil {
		return err
	}
	if d.Items == nil {
		d.Items = map[string][]string{}
	}
	if d.Index == nil {
		d.Index = map[string]int{}
	}
	s.doc = d
	return nil
}

// save writes the document atomically (temp file + rename, 0600).
func (s *stateStore) save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked()
}

func (s *stateStore) saveLocked() error {
	b, err := json.MarshalIndent(s.doc, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
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

// items returns a copy of the list for one target.
func (s *stateStore) items(target string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.doc.Items[target]))
	copy(out, s.doc.Items[target])
	return out
}

// setItems replaces the list for one target and clamps the index.
func (s *stateStore) setItems(target string, items []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.doc.Items[target] = items
	if s.doc.Index[target] >= len(items) {
		s.doc.Index[target] = 0
	}
	return s.saveLocked()
}

// addItems appends validated items and reports duplicates/invalid lines.
func (s *stateStore) addItems(target string, added, dup, invalid []string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.doc.Items[target] = append(s.doc.Items[target], added...)
	return len(s.doc.Items[target]), s.saveLocked()
}

// deleteItem removes the item at pos (1-based) and clamps the index.
func (s *stateStore) deleteItem(target string, pos int) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := s.doc.Items[target]
	if pos < 1 || pos > len(items) {
		return "", os.ErrInvalid
	}
	removed := items[pos-1]
	s.doc.Items[target] = append(items[:pos-1], items[pos:]...)
	if s.doc.Index[target] >= len(s.doc.Items[target]) {
		s.doc.Index[target] = 0
	}
	return removed, s.saveLocked()
}

// clear removes every item for the target.
func (s *stateStore) clear(target string) error {
	return s.setItems(target, nil)
}

// curIndex returns the current rotation position for a target.
func (s *stateStore) curIndex(target string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.doc.Index[target]
	if i < 0 {
		return 0
	}
	n := len(s.doc.Items[target])
	if i >= n {
		return 0
	}
	return i
}

// setIndex persists the rotation position.
func (s *stateStore) setIndex(target string, i int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.doc.Index[target] = i
	return s.saveLocked()
}

// origin returns the captured originals for restore.
func (s *stateStore) origin() originInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.doc.Origin
}

// setOrigin captures the originals (only fields not captured yet).
func (s *stateStore) setOrigin(o originInfo) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.doc.Origin
	if cur.First == "" && !cur.SetLast {
		cur.First, cur.Last, cur.SetLast = o.First, o.Last, o.SetLast
	}
	if !cur.CapturedBio {
		cur.Bio, cur.CapturedBio = o.Bio, o.CapturedBio || o.Bio != ""
	}
	if !cur.CapturedUsername {
		cur.Username, cur.CapturedUsername = o.Username, o.CapturedUsername || o.Username != ""
	}
	s.doc.Origin = cur
	return s.saveLocked()
}

// lastFire returns the last successful rotation time.
func (s *stateStore) lastFire() (unix int64, zero bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.doc.LastFireUnix, s.doc.LastFireUnix == 0
}

// setLastFire records a successful rotation.
func (s *stateStore) setLastFire(unix int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.doc.LastFireUnix = unix
	return s.saveLocked()
}
