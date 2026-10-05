package main

import (
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

// Scheduler helpers: settings validation, next-fire computation and the
// day+time dedup key. The loop itself lives in main.go (Start/Stop).

const (
	schedTick    = time.Minute // re-evaluate at least this often (clock jumps)
	maxTimes     = 12
	lastRunsKeep = 80
)

var reHHMM = regexp.MustCompile(`^([01]\d|2[0-3]):([0-5]\d)$`)

// validTimes validates and normalizes the schedule_times setting
// ("09:00,21:30", comma separated HH:MM, max 12, deduped).
func validTimes(s string) (string, error) {
	if strings.TrimSpace(s) == "" {
		return "", nil
	}
	var out []string
	seen := map[string]bool{}
	for _, part := range strings.Split(s, ",") {
		v := strings.TrimSpace(part)
		if v == "" {
			continue
		}
		if !reHHMM.MatchString(v) {
			return "", plugin.Invalid(
				"时间格式应为 HH:MM（如 09:00,21:30），最多 12 个",
				"times must be HH:MM (e.g. 09:00,21:30), up to 12")
		}
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
		if len(out) > maxTimes {
			return "", plugin.Invalid("最多 12 个时间", "at most 12 times")
		}
	}
	return strings.Join(out, ","), nil
}

// validTarget validates the schedule_target setting: @username or a numeric
// chat id (users positive, basic groups -id, supergroups/channels -100…).
func validTarget(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	if strings.HasPrefix(s, "@") {
		name := strings.TrimPrefix(s, "@")
		if name == "" || strings.ContainsAny(name, " \t@,") {
			return "", plugin.Invalid("目标应为 @用户名 或数字 chat id", "target must be @username or a numeric chat id")
		}
		return s, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n == 0 {
		return "", plugin.Invalid("目标应为 @用户名 或数字 chat id", "target must be @username or a numeric chat id")
	}
	return s, nil
}

// parseTimes splits a normalized times setting back into HH:MM values.
func parseTimes(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// nextFire returns the earliest time strictly after now among times, or
// ok=false when empty.
func nextFire(times []string, now time.Time) (time.Time, bool) {
	var best time.Time
	for _, t := range times {
		hh, mm := clockParts(t)
		at := time.Date(now.Year(), now.Month(), now.Day(), hh, mm, 0, 0, now.Location())
		if !at.After(now) {
			at = at.AddDate(0, 0, 1)
		}
		if best.IsZero() || at.Before(best) {
			best = at
		}
	}
	return best, !best.IsZero()
}

func clockParts(hhmm string) (int, int) {
	m := reHHMM.FindStringSubmatch(hhmm)
	if m == nil {
		return 0, 0
	}
	h, _ := strconv.Atoi(m[1])
	mm, _ := strconv.Atoi(m[2])
	return h, mm
}

// dayKey dedupes runs: one generation per day per time.
func dayKey(t time.Time) string {
	return t.Format("2006-01-02")
}

// clampLimit clamps a wordcloud message limit into [50,2000].
func clampLimit(n int) int {
	if n < minLimit {
		return minLimit
	}
	if n > maxLimit {
		return maxLimit
	}
	return n
}

// parseLimitArg parses "500"-style args; 0/false → default.
func parseLimitArg(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n <= 0 {
		return defaultLimit
	}
	return clampLimit(n)
}
