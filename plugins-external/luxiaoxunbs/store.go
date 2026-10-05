package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

const stateFile = "subscriptions.json"

// state is the persisted subscription store (data/luxiaoxunbs/subscriptions.json).
type state struct {
	Subs map[string]int64 `json:"subs"` // chat id (marked) -> created unix
	// chat id -> last sent report message id, deleted on the next run
	Last map[string]int `json:"last"`
}

func newState() *state {
	return &state{Subs: map[string]int64{}, Last: map[string]int{}}
}

// load reads the JSON state, migrating the TeleBox subscriptions.json shape
// ({"subscriptions": ["…"], "lastMessages": {"…": id}}) on the fly.
func load(path string) (*state, error) {
	st := newState()
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return st, nil
		}
		return nil, err
	}
	if len(b) == 0 {
		return st, nil
	}
	var raw struct {
		// PaperValet shape (what save writes)
		Subs map[string]int64 `json:"subs"`
		Last map[string]int   `json:"last"`
		// TeleBox lowdb shape (migrated on the fly)
		Subscriptions []string         `json:"subscriptions"`
		LastMessages  map[string]int64 `json:"lastMessages"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		// tolerate hand-edited files: keep defaults
		return st, nil
	}
	for k, v := range raw.Subs {
		if k != "" {
			st.Subs[k] = v
		}
	}
	for k, v := range raw.Last {
		if v > 0 && v <= 1<<30 {
			st.Last[k] = v
		}
	}
	for _, s := range raw.Subscriptions {
		if s != "" {
			if _, ok := st.Subs[s]; !ok {
				st.Subs[s] = 0
			}
		}
	}
	for k, v := range raw.LastMessages {
		if v > 0 && v <= 1<<30 {
			st.Last[k] = int(v)
		}
	}
	return st, nil
}

// save writes the state atomically (temp file + rename, 0600).
func save(path string, st *state) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(st, "", "  ")
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
