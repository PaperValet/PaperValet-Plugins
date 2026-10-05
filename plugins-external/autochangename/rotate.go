package main

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/gotd/td/tg"
	"github.com/robfig/cron/v3"
)

// Rotation targets.
const (
	targetName     = "name"
	targetBio      = "bio"
	targetUsername = "username"
)

const (
	maxItems   = 100
	maxNameLen = 64 // Telegram first/last name limit
	maxBioLen  = 70 // Telegram about limit
)

var usernameRe = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]{4,31}$`) // 5-32 chars

func validTarget(t string) bool {
	return t == targetName || t == targetBio || t == targetUsername
}

// splitNameItem splits a name entry "First|Last". Without "|" only the first
// name is updated; a trailing "|" ("First|") clears the last name.
func splitNameItem(item string) (first, last string, setLast bool) {
	if i := strings.Index(item, "|"); i >= 0 {
		return item[:i], item[i+1:], true
	}
	return item, "", false
}

// validateItem checks one list entry for the target.
func validateItem(item, target string) error {
	switch target {
	case targetBio:
		if n := len([]rune(item)); n > maxBioLen {
			return fmt.Errorf("%d/%d chars", n, maxBioLen)
		}
	case targetUsername:
		if !usernameRe.MatchString(item) {
			return fmt.Errorf("invalid username")
		}
	default: // name
		first, last, _ := splitNameItem(item)
		if strings.TrimSpace(first) == "" {
			return fmt.Errorf("empty first name")
		}
		if len([]rune(first)) > maxNameLen || len([]rune(last)) > maxNameLen {
			return fmt.Errorf("name longer than %d", maxNameLen)
		}
	}
	return nil
}

// parseItems turns pasted lines (one item per line) into clean items.
// Duplicates (already in existing or repeated in the input) and invalid
// lines are returned separately so the caller can report them.
func parseItems(text, target string, existing []string) (added, dup, invalid []string) {
	seen := make(map[string]bool, len(existing))
	for _, it := range existing {
		seen[strings.ToLower(it)] = true
	}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" {
			continue
		}
		if len(existing)+len(added) >= maxItems {
			break
		}
		key := strings.ToLower(line)
		switch {
		case seen[key]:
			dup = append(dup, line)
		case validateItem(line, target) != nil:
			invalid = append(invalid, line)
		default:
			seen[key] = true
			added = append(added, line)
		}
	}
	return added, dup, invalid
}

// nextIndex picks the next item position: sequential order wraps around,
// random order never repeats the current item when there is a choice.
func nextIndex(n, cur int, random bool, rng func(int) int) int {
	if n <= 1 {
		return 0
	}
	if !random {
		return (cur + 1) % n
	}
	if rng == nil {
		return (cur + 1) % n
	}
	j := rng(n - 1)
	if j >= cur {
		j++
	}
	return j
}

// nextDue computes when the next rotation is due after lastFire. A 5-field
// cron spec wins over the minute interval; a zero lastFire means "due now"
// (first rotation right after enabling).
func nextDue(lastFire time.Time, intervalMin int, cronSpec string, now time.Time) (time.Time, error) {
	if s := strings.TrimSpace(cronSpec); s != "" {
		sched, err := cron.ParseStandard(s)
		if err != nil {
			return time.Time{}, err
		}
		base := lastFire
		if base.IsZero() {
			base = now.Add(-time.Second)
		}
		return sched.Next(base), nil
	}
	d := time.Duration(intervalMin) * time.Minute
	if d < time.Minute {
		d = time.Minute
	}
	if lastFire.IsZero() {
		return now, nil
	}
	return lastFire.Add(d), nil
}

// nameRequest builds account.updateProfile for a name entry.
func nameRequest(first, last string, setLast bool) *tg.AccountUpdateProfileRequest {
	req := &tg.AccountUpdateProfileRequest{}
	req.SetFirstName(first)
	if setLast {
		req.SetLastName(last)
	}
	return req
}

// bioRequest builds account.updateProfile for a bio entry.
func bioRequest(about string) *tg.AccountUpdateProfileRequest {
	req := &tg.AccountUpdateProfileRequest{}
	req.SetAbout(about)
	return req
}

// restoreNameRequest restores the original first/last name (an empty last
// name is cleared explicitly).
func restoreNameRequest(first, last string) *tg.AccountUpdateProfileRequest {
	req := &tg.AccountUpdateProfileRequest{}
	req.SetFirstName(first)
	req.SetLastName(last)
	return req
}
