package main

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/gotd/td/tg"
)

// relay is the runtime state of the bot conversation, persisted so a restart
// never forwards the bot's history or welcome message.
type relay struct {
	started     bool              // bot /started once per process
	ignoredUpTo int               // newest bot message id treated as seen
	botPeer     tg.InputPeerClass // cached resolved @ParseHubot peer
}

type relayReason string

const (
	reasonTimeout       relayReason = "timeout"
	reasonFetchFailed   relayReason = "fetch_failed"
	reasonSendFailed    relayReason = "send_failed"
	reasonForwardFailed relayReason = "forward_failed"
)

// relayOutcome summarises one link submission.
type relayOutcome struct {
	lastID    int
	forwarded bool
	reason    relayReason
	err       error
}

// stateFile is the JSON shape persisted in data/parsehub/state.json.
type stateFile struct {
	Initialized   bool `json:"initialized"`
	IgnoredUpToID int  `json:"ignoredUpToId"`
}

// load restores the persisted ignore baseline into the plugin state.
func (p *ParseHubPlugin) load() error {
	var sf stateFile
	if err := readJSON(filepath.Join(p.dir, "state.json"), &sf); err != nil {
		return err
	}
	p.mu.Lock()
	if sf.Initialized {
		if p.rel == nil {
			p.rel = &relay{}
		}
		p.rel.ignoredUpTo = sf.IgnoredUpToID
	} else {
		p.rel = &relay{}
	}
	p.mu.Unlock()
	return nil
}

// saveStateLocked persists the state; p.mu must be held.
func (p *ParseHubPlugin) saveStateLocked() error {
	sf := stateFile{Initialized: p.rel != nil && p.rel.ignoredUpTo > 0, IgnoredUpToID: p.ignoredUpToLocked()}
	return writeJSON(filepath.Join(p.dir, "state.json"), sf)
}

func (p *ParseHubPlugin) ignoredUpToLocked() int {
	if p.rel == nil {
		return 0
	}
	return p.rel.ignoredUpTo
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

// writeJSON writes atomically (temp file + rename), 0600.
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

// randomID returns a fresh client-side message id; crypto/rand avoids the
// UnixNano collision that makes Telegram silently drop the send.
func randomID() int64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err == nil {
		return int64(binary.LittleEndian.Uint64(b[:]))
	}
	return timeNow().UnixNano()
}

// randomIDs returns n fresh client-side message ids.
func randomIDs(n int) []int64 {
	out := make([]int64, n)
	for i := range out {
		out[i] = randomID()
	}
	return out
}
