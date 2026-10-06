package main

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var timeRe = regexp.MustCompile(`^([01]?\d|2[0-3]):([0-5]\d)$`)

var errNoWindow = errors.New("cannot compute the next run time")

// parseTime parses "HH:MM" into minutes since midnight.
func parseTime(v string) (int, bool) {
	m := timeRe.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return 0, false
	}
	h, _ := strconv.Atoi(m[1])
	mi, _ := strconv.Atoi(m[2])
	return h*60 + mi, true
}

// formatTime renders minutes since midnight as "HH:MM".
func formatTime(min int) string {
	return strconv.Itoa(min/60) + ":" + pad2(min%60)
}

func pad2(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}

// localDate returns YYYY-MM-DD of t in loc.
func localDate(t time.Time, loc *time.Location) string {
	return t.In(loc).Format("2006-01-02")
}

// midnight returns local midnight of date YYYY-MM-DD in loc.
func midnight(date string, loc *time.Location) time.Time {
	day, err := time.ParseInLocation("2006-01-02", date, loc)
	if err != nil {
		return time.Time{}
	}
	return day
}

// windowOf returns the [start, end] execution window of date; end before
// start (in wall-clock terms) means the window crosses midnight.
func windowOf(date string, runTime, runTimeEnd string, loc *time.Location) (time.Time, time.Time) {
	base := midnight(date, loc)
	start, _ := parseTime(runTime)
	if runTime == "" {
		start = 600 // 10:00 default
	}
	end := start
	if e, ok := parseTime(runTimeEnd); ok {
		end = e
	}
	if end < start {
		end += 24 * 60
	}
	s := base.Add(time.Duration(start) * time.Minute)
	e := base.Add(time.Duration(end) * time.Minute)
	return s, e
}

// windowText renders the execution window for display.
func windowText(runTime, runTimeEnd string) string {
	if runTimeEnd == "" {
		return "每天 " + runTime
	}
	cross := false
	if e, ok := parseTime(runTimeEnd); ok {
		if s, ok2 := parseTime(runTime); ok2 {
			cross = e < s
		}
	}
	day := ""
	if cross {
		day = "次日 "
	}
	return "每天 " + runTime + " ~ " + day + runTimeEnd + " 随机"
}

// planNextRun picks a random whole minute in the nearest not-yet-run window
// after now, plus a random extra delay. rand is injectable for tests.
func planNextRun(now time.Time, conf config, loc *time.Location, rnd func(n int) int) (plan, error) {
	today := midnight(localDate(now, loc), loc)
	for offset := -1; offset <= 2; offset++ {
		date := localDate(today.AddDate(0, 0, offset), loc)
		if conf.LastRunDate != "" && date <= conf.LastRunDate {
			continue
		}
		start, end := windowOf(date, conf.RunTime, conf.RunTimeEnd, loc)
		lower := start
		if ceil := now.Truncate(time.Minute).Add(time.Minute); ceil.After(lower) {
			lower = ceil
		}
		if lower.After(end) {
			continue
		}
		mins := int(end.Sub(lower).Minutes())
		if mins < 0 {
			mins = 0
		}
		slot := lower.Add(time.Duration(rnd(mins+1)) * time.Minute)
		var delay time.Duration
		if conf.RandomDelay > 0 {
			delay = time.Duration(rnd(conf.RandomDelay)) * time.Minute
		}
		return plan{at: slot.Add(delay), date: date}, nil
	}
	return plan{}, errNoWindow
}

type plan struct {
	at   time.Time
	date string
}

// dueState reports whether the planned time is still in the future, due
// today, or already missed (past the day). A run planned just before
// midnight and first seen a few minutes after gets a small grace window so
// clock/order jitter around the day boundary does not silently skip the
// day's check-in.
func dueState(now, at time.Time, loc *time.Location) dueKind {
	if now.Before(at) {
		return stateWait
	}
	if localDate(now, loc) == localDate(at, loc) {
		return stateRun
	}
	if now.Sub(at) <= dayBoundaryGrace {
		return stateRun
	}
	return stateMissed
}

// dayBoundaryGrace tolerates the 23:5x → 00:0x date flip for runs that were
// planned before midnight.
const dayBoundaryGrace = 10 * time.Minute

type dueKind int

const (
	stateWait dueKind = iota
	stateRun
	stateMissed
)
