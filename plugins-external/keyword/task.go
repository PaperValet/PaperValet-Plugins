package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

// compileRegexp builds the task key as a regexp; case-insensitive unless
// case-sensitive was requested (the source compiles with "gi"/"g").
func compileRegexp(key string, caseSensitive bool) (*regexp.Regexp, error) {
	if !caseSensitive {
		key = "(?i)" + key
	}
	return regexp.Compile(key)
}

func mentionText(name string, userID int64) string {
	return plugin.Mention(name, userID)
}

// task is one keyword auto-reply rule (port of TeleBox KeywordTask).
type task struct {
	ID     int    `json:"id"`
	ChatID int64  `json:"cid"`
	Key    string `json:"key"`
	Msg    string `json:"msg"`
	// Match options (third +++ segment, space separated).
	Include       bool `json:"include"`        // contains match (default)
	Regexp        bool `json:"regexp"`         // regex match
	Exact         bool `json:"exact"`          // exact equality
	CaseSensitive bool `json:"case"`           // case-sensitive
	IgnoreForward bool `json:"ignore_forward"` // skip forwarded messages
	// Actions (fourth +++ segment, space separated).
	Reply             bool `json:"reply"`               // reply to the trigger (default)
	Delete            bool `json:"delete"`              // delete the trigger message
	Ban               int  `json:"ban"`                 // ban the sender for N seconds (supergroups)
	Restrict          int  `json:"restrict"`            // restrict the sender for N seconds (supergroups)
	Cooldown          int  `json:"cooldown"`            // seconds between two fires of this task
	DelayDelete       int  `json:"delay_delete"`        // delete the reply after N seconds
	SourceDelayDelete int  `json:"source_delay_delete"` // delete the trigger after N seconds
}

func newTask(id int, chatID int64) *task {
	return &task{ID: id, ChatID: chatID, Include: true, Reply: true}
}

// errTaskFormat is the bilingual reason for a malformed task.
type errTaskFormat struct{ zh, en string }

func (e errTaskFormat) Error() string { return e.en }

// parseTask fills t from the raw `keyword <rest>` text, mirroring the
// TeleBox format:
//
//	key
//	+++
//	reply message
//	+++
//	match options
//	+++
//	actions
//	+++
//	delay delete seconds
//	+++
//	source delay delete seconds
//
// Empty segments are invalid; match options come before actions.
func (t *task) parseTask(text string) error {
	parts := strings.Split(text, "\n+++\n")
	if len(parts) < 2 {
		return errTaskFormat{"任务格式无效", "invalid task format"}
	}
	for _, part := range parts[:2] {
		if part == "" {
			return errTaskFormat{"任务格式无效", "invalid task format"}
		}
	}
	t.Key = parts[0]
	t.Msg = parts[1]

	if len(parts) > 2 {
		for _, opt := range strings.Fields(parts[2]) {
			switch {
			case strings.HasPrefix(opt, "include"):
				t.Include = true
			case strings.HasPrefix(opt, "exact"):
				t.Include = false
				t.Exact = true
			case strings.HasPrefix(opt, "regexp"):
				t.Regexp = true
			case strings.HasPrefix(opt, "case"):
				t.CaseSensitive = true
			case strings.HasPrefix(opt, "ignore_forward"):
				t.IgnoreForward = true
			default:
				return errTaskFormat{"任务格式无效", "invalid task format"}
			}
		}
		if t.Include && t.Exact {
			return errTaskFormat{"不能同时设置 include 和 exact 选项", "include and exact cannot both be set"}
		}
	}

	if len(parts) > 3 {
		for _, act := range strings.Fields(parts[3]) {
			switch {
			case strings.HasPrefix(act, "reply"):
				t.Reply = true
			case strings.HasPrefix(act, "delete"):
				t.Delete = true
			case strings.HasPrefix(act, "ban"):
				t.Ban = parseSeconds(act, "ban")
			case strings.HasPrefix(act, "restrict"):
				t.Restrict = parseSeconds(act, "restrict")
			case strings.HasPrefix(act, "cooldown"):
				t.Cooldown = parseSeconds(act, "cooldown")
			default:
				return errTaskFormat{"任务格式无效", "invalid task format"}
			}
		}
	}

	if len(parts) > 4 && strings.TrimSpace(parts[4]) != "" {
		t.DelayDelete = parseIntOrZero(strings.TrimSpace(parts[4]))
	}
	if len(parts) > 5 && strings.TrimSpace(parts[5]) != "" {
		t.SourceDelayDelete = parseIntOrZero(strings.TrimSpace(parts[5]))
	}
	if len(parts) > 6 {
		return errTaskFormat{"任务格式无效", "invalid task format"}
	}

	if t.Ban < 0 || t.Restrict < 0 || t.Cooldown < 0 || t.DelayDelete < 0 || t.SourceDelayDelete < 0 {
		return errTaskFormat{"时间参数不能为负数", "time values cannot be negative"}
	}
	return nil
}

// parseSeconds reads the trailing number of "ban300" style actions.
func parseSeconds(s, prefix string) int {
	return parseIntOrZero(strings.TrimPrefix(s, prefix))
}

func parseIntOrZero(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return n
}

// matches reports whether the task's key matches text (message body or
// caption). Mirrors checkNeedReply.
func (t *task) matches(text string) bool {
	if text == "" {
		return false
	}
	if t.Regexp {
		re, err := compileRegexp(t.Key, t.CaseSensitive)
		if err != nil {
			return false
		}
		return re.MatchString(text)
	}
	key, msg := t.Key, text
	if !t.CaseSensitive {
		key, msg = strings.ToLower(key), strings.ToLower(msg)
	}
	if t.Include && strings.Contains(msg, key) {
		return true
	}
	return t.Exact && msg == key
}

// render fills the $mention/$code_id/$code_name/$delay_delete variables.
// name is the sender's display name ("" → the id), userID 0 means "no
// user identity" (channel post, anonymous admin).
func (t *task) render(userID int64, name string) string {
	text := t.Msg
	if userID != 0 {
		if name == "" {
			name = fmt.Sprintf("%d", userID)
		}
		// One pass per variable: str.Replace(-1) like the source.
		text = strings.ReplaceAll(text, "$mention", mentionText(name, userID))
		text = strings.ReplaceAll(text, "$code_id", strconv.FormatInt(userID, 10))
		text = strings.ReplaceAll(text, "$code_name", name)
	} else {
		text = strings.ReplaceAll(text, "$mention", "")
		text = strings.ReplaceAll(text, "$code_id", "")
		text = strings.ReplaceAll(text, "$code_name", "")
	}
	if t.DelayDelete > 0 {
		text = strings.ReplaceAll(text, "$delay_delete", strconv.Itoa(t.DelayDelete))
	}
	return strings.ReplaceAll(text, "$delay_delete", "")
}

func (t *task) flags() string {
	var opts, acts []string
	if t.Exact {
		opts = append(opts, "exact")
	} else if t.Include {
		opts = append(opts, "include")
	}
	if t.Regexp {
		opts = append(opts, "regexp")
	}
	if t.CaseSensitive {
		opts = append(opts, "case")
	}
	if t.IgnoreForward {
		opts = append(opts, "ignore_forward")
	}
	acts = append(acts, "reply")
	if t.Delete {
		acts = append(acts, "delete")
	}
	if t.Ban > 0 {
		acts = append(acts, "ban"+strconv.Itoa(t.Ban))
	}
	if t.Restrict > 0 {
		acts = append(acts, "restrict"+strconv.Itoa(t.Restrict))
	}
	if t.Cooldown > 0 {
		acts = append(acts, "cooldown"+strconv.Itoa(t.Cooldown))
	}
	parts := []string{t.Key, t.Msg, strings.Join(opts, " "), strings.Join(acts, " ")}
	if t.DelayDelete > 0 {
		parts = append(parts, strconv.Itoa(t.DelayDelete))
	}
	if t.SourceDelayDelete > 0 {
		parts = append(parts, strconv.Itoa(t.SourceDelayDelete))
	}
	return strings.Join(parts, "\n+++\n")
}

// parseTaskIDs splits "1, 2 ,3" into ids.
func parseTaskIDs(s string) ([]int, error) {
	var ids []int
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil, fmt.Errorf("invalid id %q", part)
		}
		ids = append(ids, n)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("no ids")
	}
	return ids, nil
}
