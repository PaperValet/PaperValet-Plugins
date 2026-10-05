package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Option keywords accepted by `shift set` (bilingual where the source had both).
var typeOptions = map[string]string{
	"text": "text", "photo": "photo", "document": "document", "video": "video",
	"sticker": "sticker", "animation": "animation", "voice": "voice", "audio": "audio",
}

// Rule maps one source chat to a target chat with filtering options.
// The key in the rules map is the source chat id.
type Rule struct {
	Target        int64     `json:"target"`
	Options       []string  `json:"options,omitempty"`
	Filters       []string  `json:"filters,omitempty"`
	WhitelistMode bool      `json:"whitelist_mode,omitempty"`
	Whitelist     []string  `json:"whitelist,omitempty"`
	Paused        bool      `json:"paused,omitempty"`
	TopicID       int       `json:"topic_id,omitempty"`
	SourceTitle   string    `json:"source_title,omitempty"`
	TargetTitle   string    `json:"target_title,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

// has reports whether opt is set (exact keyword, not the replyTo: form).
func (r *Rule) has(opt string) bool {
	for _, o := range r.Options {
		if o == opt {
			return true
		}
	}
	return false
}

// types returns the message-type filter keywords of the rule.
func (r *Rule) types() []string {
	var out []string
	for _, o := range r.Options {
		if _, ok := typeOptions[o]; ok {
			out = append(out, o)
		}
	}
	return out
}

// parseSetArgs validates the arguments of `shift set`. src may be "here"/"me"
// meaning the current chat. The target may carry |topicID. Unknown words are
// reported as invalid options.
func parseSetArgs(args []string) (src, dst string, opts []string, topicID int, err error) {
	n := len(args)
	if n == 0 {
		return "", "", nil, 0, fmt.Errorf("missing target")
	}
	if n == 1 {
		return "here", args[0], nil, 0, nil
	}
	src, dst = args[0], args[1]
	for _, a := range args[2:] {
		switch {
		case a == "all", a == "silent", a == "handle_edited":
			opts = append(opts, a)
		case strings.HasPrefix(a, "replyTo:"):
			v := strings.TrimPrefix(a, "replyTo:")
			id, e := parseInt(v)
			if e != nil || id <= 0 {
				return "", "", nil, 0, fmt.Errorf("invalid replyTo %q", v)
			}
			topicID = id
		case typeOptions[a] != "":
			opts = append(opts, a)
		default:
			return "", "", nil, 0, fmt.Errorf("unknown option %q", a)
		}
	}
	return src, dst, opts, topicID, nil
}

// parseTarget splits "peer|topic" into the peer input and optional topic id.
func parseTarget(t string) (string, int, bool) {
	parts := strings.FieldsFunc(t, func(r rune) bool { return r == '|' || r == '｜' })
	if len(parts) == 0 {
		return t, 0, false
	}
	if len(parts) == 1 {
		return strings.TrimSpace(parts[0]), 0, false
	}
	id, err := parseInt(strings.TrimSpace(parts[1]))
	if err != nil || id <= 0 {
		return strings.TrimSpace(parts[0]), 0, false
	}
	return strings.TrimSpace(parts[0]), id, true
}

func parseInt(s string) (int, error) {
	var v int
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d", &v); err != nil {
		return 0, err
	}
	return v, nil
}

// parseIndices reads "1,3-5" rule numbers into zero-based indexes, bounded by
// total. Invalid entries are returned for the error message.
func parseIndices(s string, total int) (idx []int, invalid []string) {
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		lo, hi, isRange := strings.Cut(part, "-")
		a, errA := parseInt(lo)
		if !isRange {
			if errA != nil || a < 1 || a > total {
				invalid = append(invalid, part)
				continue
			}
			idx = append(idx, a-1)
			continue
		}
		b, errB := parseInt(hi)
		if errA != nil || errB != nil || a < 1 || b < a || a > total {
			invalid = append(invalid, part)
			continue
		}
		if b > total {
			b = total
		}
		for i := a; i <= b; i++ {
			idx = append(idx, i-1)
		}
	}
	return idx, invalid
}

// matchWhitelist compiles and caches rule whitelist regexes.
var wlCache = map[string]*regexp.Regexp{}

func compileWhitelist(patterns []string) []*regexp.Regexp {
	var out []*regexp.Regexp
	for _, p := range patterns {
		re, ok := wlCache[p]
		if !ok {
			var err error
			re, err = regexp.Compile("(?i)" + p)
			if err != nil {
				continue
			}
			wlCache[p] = re
		}
		out = append(out, re)
	}
	return out
}

// isFiltered reports whether the message must not be forwarded: a whitelist
// hit means allow; otherwise a blacklist keyword hit means block.
func (r *Rule) isFiltered(text string) bool {
	if r.WhitelistMode {
		if len(r.Whitelist) == 0 {
			return true
		}
		for _, re := range compileWhitelist(r.Whitelist) {
			if re.MatchString(text) {
				return false
			}
		}
		return true
	}
	lt := strings.ToLower(text)
	for _, kw := range r.Filters {
		if strings.Contains(lt, strings.ToLower(kw)) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- storage

// storeFile is persisted in data/shift/rules.json.
type storeFile struct {
	Rules map[string]*Rule `json:"rules"`
	Order []int64          `json:"order"`
}

func (p *ShiftPlugin) load() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rules = map[int64]*Rule{}
	p.order = nil
	var sf storeFile
	if err := readJSON(filepath.Join(p.dir, rulesFile), &sf); err == nil {
		for _, k := range sf.Order {
			r, ok := sf.Rules[fmt.Sprint(k)]
			if !ok || r == nil {
				continue
			}
			p.rules[k] = r
			p.order = append(p.order, k)
		}
		// Tolerate a missing or short order list.
		seen := map[int64]bool{}
		for _, id := range p.order {
			seen[id] = true
		}
		var ids []int64
		for id := range p.rules {
			if !seen[id] {
				ids = append(ids, id)
			}
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		for _, id := range ids {
			p.order = append(p.order, id)
			seen[id] = true
		}
	}
	p.stats = &Stats{}
	_ = readJSON(filepath.Join(p.dir, statsFile), p.stats)
	p.pruneStatsLocked()
}

func (p *ShiftPlugin) saveLocked() error {
	if p.dir == "" {
		return nil
	}
	sf := storeFile{Rules: map[string]*Rule{}}
	for _, id := range p.order {
		if r, ok := p.rules[id]; ok {
			sf.Rules[fmt.Sprint(id)] = r
		}
	}
	sf.Order = p.order
	return writeJSON(filepath.Join(p.dir, rulesFile), sf)
}

func (p *ShiftPlugin) saveStats() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.dir == "" {
		return
	}
	p.pruneStatsLocked()
	_ = writeJSON(filepath.Join(p.dir, statsFile), p.stats)
}

func (p *ShiftPlugin) pruneStatsLocked() {
	if p.stats == nil || len(p.stats.Days) <= maxHistory {
		return
	}
	var days []string
	for d := range p.stats.Days {
		days = append(days, d)
	}
	sort.Strings(days)
	for _, d := range days[:len(days)-maxHistory] {
		delete(p.stats.Days, d)
	}
}

// ordered returns a copy of the rules in insertion order.
func (p *ShiftPlugin) ordered() []*Rule {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*Rule, 0, len(p.order))
	for _, id := range p.order {
		if r, ok := p.rules[id]; ok {
			out = append(out, r)
		}
	}
	return out
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if len(b) == 0 {
		return nil
	}
	return json.Unmarshal(b, v)
}

func writeJSON(path string, v any) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
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
	return os.Rename(name, path)
}
