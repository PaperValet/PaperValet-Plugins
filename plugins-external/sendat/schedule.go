package main

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Task modes.
const (
	modeOnce     = "once"     // fire once at At
	modeInterval = "interval" // every PeriodSec seconds, anchored at Anchor
	modeDaily    = "daily"    // every day at Hour:Minute:Second (configured zone)
)

// Task is one persisted scheduled message (data/sendat/tasks.json).
type Task struct {
	ID        int      `json:"task_id"`
	ChatID    int64    `json:"cid"`
	Peer      *peerRef `json:"peer,omitempty"`
	Msg       string   `json:"msg"`
	Entities  []string `json:"entities,omitempty"` // base64 TL-encoded MessageEntity
	Mode      string   `json:"mode"`
	Pause     bool     `json:"pause"`
	TimeLimit int      `json:"time_limit"` // -1 = unlimited, >0 = remaining runs
	Hour      int      `json:"hour"`
	Minute    int      `json:"minute"`
	Second    int      `json:"second"`
	PeriodSec int64    `json:"period_sec,omitempty"`
	Anchor    int64    `json:"anchor,omitempty"` // unix seconds, interval phase
	At        int64    `json:"at,omitempty"`     // unix seconds, once
	Count     int      `json:"current_count"`
	Created   int64    `json:"created"`
	LastRun   int64    `json:"last_run,omitempty"`
	LastError string   `json:"last_error,omitempty"`
}

var (
	reDate     = regexp.MustCompile(`^\d{4}-\d{1,2}-\d{1,2}$`)
	reClock    = regexp.MustCompile(`^\d{1,2}:\d{1,2}(:\d{1,2})?$`)
	reDuration = regexp.MustCompile(`^\+?(\d+(\.\d+)?(d|h|m|s|ms))+$`)
	reNumber   = regexp.MustCompile(`^\d+$`)
)

type spec struct {
	every    bool
	times    int // 0 = not given
	rel      time.Duration
	hasRel   bool
	clock    [3]int
	hasClock bool
	abs      time.Time
	hasAbs   bool
}

func unitKind(u string) string {
	switch u {
	case "s", "sec", "secs", "second", "seconds", "秒":
		return "s"
	case "m", "min", "mins", "minute", "minutes", "分", "分钟":
		return "m"
	case "h", "hr", "hrs", "hour", "hours", "时", "小时":
		return "h"
	case "d", "day", "days", "天":
		return "d"
	case "time", "times", "次":
		return "times"
	case "date":
		return "date"
	}
	return ""
}

func parseClock(s string) ([3]int, error) {
	var c [3]int
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return c, fmt.Errorf("bad time %q (want HH:MM[:SS])", s)
	}
	max := [3]int{23, 59, 59}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > max[i] {
			return c, fmt.Errorf("bad time value %q", s)
		}
		c[i] = n
	}
	return c, nil
}

// parseDur accepts Go durations plus a "d" (day) unit, with an optional "+".
func parseDur(s string) (time.Duration, error) {
	s = strings.TrimPrefix(s, "+")
	var total time.Duration
	for s != "" {
		i := strings.Index(s, "d")
		if i < 0 {
			break
		}
		// "d" may only be a unit of a leading integer block.
		n, err := strconv.Atoi(s[:i])
		if err != nil {
			break
		}
		total += time.Duration(n) * 24 * time.Hour
		s = s[i+1:]
	}
	if s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			return 0, err
		}
		total += d
	}
	return total, nil
}

// parseSpec parses the time part of a task ("every 1 minutes",
// "3 times 10 seconds", "16:00:00 date", "every 23:59:59 date",
// "+5m", "18:00", "2026-01-01 12:00").
func parseSpec(text string, loc *time.Location) (spec, error) {
	var sp spec
	toks := strings.Fields(strings.ToLower(text))
	if len(toks) == 0 {
		return sp, errors.New("empty time spec")
	}
	addRel := func(d time.Duration) {
		sp.rel += d
		sp.hasRel = true
	}
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		next := ""
		if i+1 < len(toks) {
			next = toks[i+1]
		}
		switch {
		case t == "every" || t == "每":
			sp.every = true
		case reDate.MatchString(t):
			if !reClock.MatchString(next) {
				return sp, fmt.Errorf("date %q must be followed by HH:MM[:SS]", t)
			}
			c, err := parseClock(next)
			if err != nil {
				return sp, err
			}
			d, err := time.ParseInLocation("2006-1-2", t, loc)
			if err != nil {
				return sp, fmt.Errorf("bad date %q", t)
			}
			sp.abs = time.Date(d.Year(), d.Month(), d.Day(), c[0], c[1], c[2], 0, loc)
			sp.hasAbs = true
			i++
		case reClock.MatchString(t):
			c, err := parseClock(t)
			if err != nil {
				return sp, err
			}
			if sp.hasClock {
				return sp, errors.New("time of day given twice")
			}
			sp.clock, sp.hasClock = c, true
			if unitKind(next) == "date" {
				i++
			}
		case reNumber.MatchString(t) && unitKind(next) != "":
			n, err := strconv.Atoi(t)
			if err != nil {
				return sp, fmt.Errorf("bad number %q", t)
			}
			switch unitKind(next) {
			case "s":
				addRel(time.Duration(n) * time.Second)
			case "m":
				addRel(time.Duration(n) * time.Minute)
			case "h":
				addRel(time.Duration(n) * time.Hour)
			case "d":
				addRel(time.Duration(n) * 24 * time.Hour)
			case "times":
				if n < 1 {
					return sp, errors.New("times must be >= 1")
				}
				sp.times = n
			case "date":
				return sp, fmt.Errorf("date needs HH:MM:SS, got %q", t)
			}
			i++
		case reDuration.MatchString(t):
			d, err := parseDur(t)
			if err != nil {
				return sp, fmt.Errorf("bad duration %q", t)
			}
			addRel(d)
		default:
			return sp, fmt.Errorf("unknown time token %q", t)
		}
	}
	n := 0
	for _, b := range []bool{sp.hasRel, sp.hasClock, sp.hasAbs} {
		if b {
			n++
		}
	}
	switch {
	case n == 0:
		return sp, errors.New("missing time")
	case n > 1:
		return sp, errors.New("mix of relative time, time of day and date")
	case sp.hasAbs && (sp.every || sp.times > 0):
		return sp, errors.New("an absolute date cannot repeat")
	case sp.hasRel && sp.rel < time.Second:
		return sp, errors.New("interval must be at least 1 second")
	}
	return sp, nil
}

// buildTask turns a parsed spec into a Task relative to now.
func buildTask(sp spec, now time.Time, loc *time.Location) (Task, error) {
	t := Task{TimeLimit: -1, Created: now.Unix()}
	repeat := sp.every || sp.times > 0
	if sp.times > 0 {
		t.TimeLimit = sp.times
	}
	switch {
	case sp.hasAbs:
		if !sp.abs.After(now) {
			return t, errors.New("that time has already passed")
		}
		t.Mode, t.At, t.TimeLimit = modeOnce, sp.abs.Unix(), 1
		lt := sp.abs.In(loc)
		t.Hour, t.Minute, t.Second = lt.Hour(), lt.Minute(), lt.Second()
	case sp.hasClock:
		t.Hour, t.Minute, t.Second = sp.clock[0], sp.clock[1], sp.clock[2]
		if repeat {
			t.Mode = modeDaily
		} else {
			t.Mode, t.TimeLimit = modeOnce, 1
			t.At = nextDaily(now, loc, t.Hour, t.Minute, t.Second).Unix()
		}
	case sp.hasRel:
		if repeat {
			t.Mode = modeInterval
			t.PeriodSec = int64(sp.rel / time.Second)
			t.Anchor = now.Unix()
			h, m, s := splitHMS(t.PeriodSec)
			t.Hour, t.Minute, t.Second = h, m, s
		} else {
			at := now.Add(sp.rel)
			t.Mode, t.At, t.TimeLimit = modeOnce, at.Unix(), 1
			lt := at.In(loc)
			t.Hour, t.Minute, t.Second = lt.Hour(), lt.Minute(), lt.Second()
		}
	}
	return t, nil
}

func splitHMS(sec int64) (int, int, int) {
	return int(sec / 3600), int(sec % 3600 / 60), int(sec % 60)
}

// nextDaily returns the first h:m:s strictly after now in loc.
func nextDaily(now time.Time, loc *time.Location, h, m, s int) time.Time {
	ln := now.In(loc)
	t := time.Date(ln.Year(), ln.Month(), ln.Day(), h, m, s, 0, loc)
	for !t.After(now) {
		ln = ln.AddDate(0, 0, 1)
		t = time.Date(ln.Year(), ln.Month(), ln.Day(), h, m, s, 0, loc)
	}
	return t
}

// nextRun returns when the task fires next (strictly after now for
// repeating tasks). Once tasks return their fixed time even if past.
func nextRun(t Task, now time.Time, loc *time.Location) time.Time {
	switch t.Mode {
	case modeOnce:
		return time.Unix(t.At, 0)
	case modeDaily:
		return nextDaily(now, loc, t.Hour, t.Minute, t.Second)
	case modeInterval:
		if t.PeriodSec <= 0 {
			return time.Time{}
		}
		anchor := time.Unix(t.Anchor, 0)
		if now.Before(anchor) {
			return anchor
		}
		k := int64(now.Sub(anchor)/time.Second)/t.PeriodSec + 1
		return anchor.Add(time.Duration(k*t.PeriodSec) * time.Second)
	}
	return time.Time{}
}

type tlFunc func(zh, en string) string

func fmtPeriod(sec int64, tl tlFunc) string {
	d, rest := sec/86400, sec%86400
	h, m, s := splitHMS(rest)
	var b strings.Builder
	if d > 0 {
		b.WriteString(fmt.Sprintf(tl("%d天", "%dd "), d))
	}
	if h > 0 {
		b.WriteString(fmt.Sprintf(tl("%d小时", "%dh "), h))
	}
	if m > 0 {
		b.WriteString(fmt.Sprintf(tl("%d分钟", "%dm "), m))
	}
	if s > 0 {
		b.WriteString(fmt.Sprintf(tl("%d秒", "%ds "), s))
	}
	return strings.TrimSpace(b.String())
}

// scheduleText describes when the task runs (plain text, unescaped).
func scheduleText(t Task, loc *time.Location, tl tlFunc) string {
	var s string
	switch t.Mode {
	case modeInterval:
		s = tl("每 ", "every ") + fmtPeriod(t.PeriodSec, tl)
	case modeDaily:
		s = fmt.Sprintf(tl("每天 %02d:%02d:%02d", "daily at %02d:%02d:%02d"), t.Hour, t.Minute, t.Second)
	default:
		s = tl("指定时间 ", "once at ") + time.Unix(t.At, 0).In(loc).Format("2006-01-02 15:04:05")
	}
	if t.Mode != modeOnce {
		if t.TimeLimit > 0 {
			s += fmt.Sprintf(tl("，剩余 %d 次", ", %d runs left"), t.TimeLimit)
		} else {
			s += tl("，无限执行", ", unlimited")
		}
	}
	return s
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}

// splitCommand splits "spec | message" (message may contain '|' and
// newlines). Without '|' the legacy form "<when> <message>" is accepted,
// where <when> is one token or "YYYY-MM-DD HH:MM".
//
// It returns the spec and the byte range [start,end) of the message in raw,
// so formatting entities of the original message can be kept.
func splitCommand(raw string) (specStr string, start, end int, err error) {
	trimRange := func(s, e int) (int, int) {
		for s < e && isSpace(raw[s]) {
			s++
		}
		for e > s && isSpace(raw[e-1]) {
			e--
		}
		return s, e
	}
	if i := strings.Index(raw, "|"); i >= 0 {
		start, end = trimRange(i+1, len(raw))
		return strings.TrimSpace(raw[:i]), start, end, nil
	}
	// token returns the next whitespace-delimited token starting at pos.
	token := func(pos int) (string, int) {
		for pos < len(raw) && isSpace(raw[pos]) {
			pos++
		}
		e := pos
		for e < len(raw) && !isSpace(raw[e]) {
			e++
		}
		return raw[pos:e], e
	}
	first, pos := token(0)
	if first == "" {
		return "", 0, 0, errors.New("missing time")
	}
	specStr = first
	if reDate.MatchString(first) {
		if second, p2 := token(pos); reClock.MatchString(second) {
			specStr, pos = first+" "+second, p2
		}
	}
	start, end = trimRange(pos, len(raw))
	return specStr, start, end, nil
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }
