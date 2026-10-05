package main

import (
	"regexp"
	"strconv"
	"strings"
)

const minTTLSeconds = 5

// CmdRule is one command deletion rule, ported from TeleBox autodelcmd.
type CmdRule struct {
	ID             string   `json:"id"`
	Command        string   `json:"command"`
	Delay          int      `json:"delay"`
	Parameters     []string `json:"parameters,omitempty"`
	DeleteResponse bool     `json:"deleteResponse,omitempty"`
	ExactMatch     bool     `json:"exactMatch,omitempty"`
}

// defaultRules maps the TeleBox default table onto PaperValet's commands.
// Design-fixed fast commands get 10s; the rest keep their source delays.
// TeleBox-only commands with no PaperValet equivalent are dropped (lang,
// eat, tpm, sysinfo, service, bf, s, spt, v, whois).
func defaultRules() []CmdRule {
	rs := []CmdRule{
		{Command: "ping", Delay: 10},
		{Command: "info", Delay: 10},
		{Command: "status", Delay: 10},
		{Command: "dc", Delay: 10},
		{Command: "sendlog", Delay: 10},
		{Command: "help", Delay: 10},
		{Command: "alias", Delay: 10},
		{Command: "reload", Delay: 10},
		{Command: "pingdc", Delay: 120},
		{Command: "ip", Delay: 120},
		{Command: "trace", Delay: 120},
		{Command: "update", Delay: 120},
		{Command: "h", Delay: 120, DeleteResponse: true},
		{Command: "speedtest", Delay: 120, DeleteResponse: true},
	}
	for i := range rs {
		rs[i].ID = strconv.Itoa(i + 1)
	}
	return rs
}

// matchRule finds the rule for command + first parameter, ported from
// autodelcmd: parameter rules first, then exactMatch (bare calls only),
// then plain rules.
func matchRule(rules []CmdRule, command string, params []string) *CmdRule {
	first := ""
	if len(params) > 0 {
		first = params[0]
	}
	for i := range rules {
		r := &rules[i]
		if len(r.Parameters) > 0 && r.Command == command && first != "" {
			for _, p := range r.Parameters {
				if p == first {
					return r
				}
			}
		}
	}
	var exact, normal *CmdRule
	for i := range rules {
		r := &rules[i]
		if r.Command != command || len(r.Parameters) > 0 {
			continue
		}
		if r.ExactMatch {
			if len(params) == 0 && exact == nil {
				exact = r
			}
		} else if normal == nil {
			normal = r
		}
	}
	if exact != nil {
		return exact
	}
	return normal
}

// ruleKey identifies a rule by command+params+mode (like TeleBox getRuleKey).
func ruleKey(r CmdRule) string {
	return r.Command + ":" + strings.Join(r.Parameters, ",") + ":" + map[bool]string{true: "exact", false: "normal"}[r.ExactMatch]
}

// parseDuration parses TTL strings: 30s / 5m / 2h / 1d / "30 seconds" /
// "5 分钟" / 2小时 / 1天 … Returns seconds, or 0 when invalid.
// Ported from TeleBox autodel (English loose+compact and Chinese units).
var durationRe = regexp.MustCompile(`^(?i)(\d+)\s*(seconds?|secs?|s|minutes?|mins?|m|hours?|hrs?|h|days?|d)$`)

func parseDuration(s string) int {
	s = strings.Join(strings.Fields(s), "")
	if s == "" {
		return 0
	}
	if m := durationRe.FindStringSubmatch(s); m != nil {
		v, err := strconv.Atoi(m[1])
		if err != nil {
			return 0
		}
		return v * unitSeconds(strings.ToLower(m[2]))
	}
	// Chinese units, including 5分钟 style (分 must be tried after 分钟).
	for _, zh := range []string{"小时", "分钟", "秒", "天", "时", "分"} {
		if strings.HasSuffix(s, zh) {
			body := strings.TrimSuffix(s, zh)
			if !isAllDigits(body) {
				continue
			}
			v, err := strconv.Atoi(body)
			if err != nil {
				return 0
			}
			return v * zhUnitSeconds(zh)
		}
	}
	return 0
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func unitSeconds(u string) int {
	switch u {
	case "s", "sec", "secs", "second", "seconds":
		return 1
	case "m", "min", "mins", "minute", "minutes":
		return 60
	case "h", "hr", "hrs", "hour", "hours":
		return 3600
	case "d", "day", "days":
		return 86400
	}
	return 0
}

func zhUnitSeconds(u string) int {
	switch u {
	case "秒":
		return 1
	case "分", "分钟":
		return 60
	case "时", "小时":
		return 3600
	case "天":
		return 86400
	}
	return 0
}

// formatSeconds renders a TTL for lists (1d 2h 5m 30s).
func formatSeconds(n int) string {
	if n <= 0 {
		return "0s"
	}
	var parts []string
	for _, u := range []struct {
		d int
		s string
	}{{
		d: 86400, s: "d"}, {3600, "h"}, {60, "m"}, {1, "s"}} {
		if n >= u.d {
			parts = append(parts, strconv.Itoa(n/u.d)+u.s)
			n %= u.d
		}
	}
	return strings.Join(parts, " ")
}
