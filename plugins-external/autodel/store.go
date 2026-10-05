package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const (
	settingsFile = "settings.json"
	rulesFile    = "rules.json"
	pendingFile  = "pending.json"
)

// PendingDel is a scheduled deletion persisted across restarts:
// delete message MID in chat CID, scheduled for unix time At (Resp rules
// also take the responses found around the command).
type PendingDel struct {
	CID  int64 `json:"cid"`
	MID  int   `json:"mid"`
	At   int64 `json:"at"`
	Resp bool  `json:"resp,omitempty"`
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

// prunePending drops duplicates, invalid records and entries older than
// pendingMaxAge (they will never fire usefully after a restart).
func prunePending(list []PendingDel, now time.Time) []PendingDel {
	type key struct {
		cid int64
		mid int
	}
	seen := map[key]bool{}
	out := make([]PendingDel, 0, len(list))
	for _, pd := range list {
		k := key{pd.CID, pd.MID}
		if pd.CID == 0 || pd.MID == 0 || seen[k] {
			continue
		}
		if now.Sub(time.Unix(pd.At, 0)) > pendingMaxAge {
			continue
		}
		seen[k] = true
		out = append(out, pd)
	}
	return out
}

// loadTTL reads settings.json: chat id → seconds, "0" is the global TTL.
func loadTTL(path string) (map[string]int, error) {
	m := map[string]int{}
	if err := readJSON(path, &m); err != nil {
		return nil, err
	}
	for k, v := range m {
		if v < minTTLSeconds {
			delete(m, k)
		}
	}
	return m, nil
}

func (p *AutoDelPlugin) saveTTLLocked() error {
	return writeJSON(filepath.Join(p.dir, settingsFile), p.ttl)
}

type rulesDoc struct {
	Rules []CmdRule `json:"rules"`
}

// loadRulesLocked loads rules.json, seeding the defaults on first run and
// repairing missing IDs. Saves back whenever anything changed.
func (p *AutoDelPlugin) loadRulesLocked() error {
	var doc rulesDoc
	if err := readJSON(filepath.Join(p.dir, rulesFile), &doc); err != nil {
		return err
	}
	changed := doc.Rules == nil
	if changed {
		doc.Rules = defaultRules()
	}
	next := 1
	for _, r := range doc.Rules {
		if id, err := strconv.Atoi(r.ID); err == nil && id >= next {
			next = id + 1
		}
	}
	rules := make([]CmdRule, 0, len(doc.Rules))
	for _, r := range doc.Rules {
		if r.Command == "" || r.Delay < 1 {
			changed = true
			continue
		}
		if r.ID == "" {
			r.ID = strconv.Itoa(next)
			next++
			changed = true
		}
		rules = append(rules, r)
	}
	p.rules = rules
	if changed {
		return p.saveRulesLocked()
	}
	return nil
}

func (p *AutoDelPlugin) saveRulesLocked() error {
	return writeJSON(filepath.Join(p.dir, rulesFile), rulesDoc{Rules: p.rules})
}

func loadPending(path string) ([]PendingDel, error) {
	var doc struct {
		Pending []PendingDel `json:"pending"`
	}
	if err := readJSON(path, &doc); err != nil {
		return nil, err
	}
	return doc.Pending, nil
}

func (p *AutoDelPlugin) savePendingLocked() error {
	return writeJSON(filepath.Join(p.dir, pendingFile), struct {
		Pending []PendingDel `json:"pending"`
	}{p.pending})
}

// markPendingDirtyLocked flags pending.json as changed and schedules a
// debounced write, so bursts of schedules/removals coalesce into one disk
// write. Callers must hold p.mu.
func (p *AutoDelPlugin) markPendingDirtyLocked() {
	p.pendingDirty = true
	if p.pendingSaveTimer != nil {
		return // a write is already scheduled
	}
	p.pendingSaveTimer = time.AfterFunc(pendingSaveDelay, p.flushPending)
}

// flushPending writes pending.json if dirty. Safe to call without p.mu.
func (p *AutoDelPlugin) flushPending() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pendingSaveTimer = nil
	if !p.pendingDirty {
		return
	}
	if err := p.savePendingLocked(); err != nil && p.log != nil {
		p.log.Warn("autodel: save pending failed", "error", err)
	}
	p.pendingDirty = false
}

// stopPendingTimerLocked cancels a pending debounce timer (no-op if none).
// Callers must hold p.mu.
func (p *AutoDelPlugin) stopPendingTimerLocked() {
	if p.pendingSaveTimer != nil {
		p.pendingSaveTimer.Stop()
		p.pendingSaveTimer = nil
	}
}
