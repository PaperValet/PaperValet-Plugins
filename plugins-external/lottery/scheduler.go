package main

import (
	"context"
	"errors"
	"time"
)

// maxSleep bounds the auto-loop's nap so clock jumps and resumed schedules
// are picked up quickly (like sendat).
const maxSleep = time.Minute

// autoLoop wakes at most every maxSleep, runs due auto-draws, and expires
// stale claims. Exits when the plugin stops.
func (p *LotteryPlugin) autoLoop(ctx context.Context) {
	defer p.wg.Done()
	ticker := time.NewTicker(maxSleep)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		now := time.Now()
		p.mu.Lock()
		var due []int64
		for _, l := range p.store.data.Lotteries {
			if l.Status == "active" && l.AutoDrawAt != 0 && l.AutoDrawAt <= now.Unix() {
				due = append(due, l.ID)
				l.AutoDrawAt = 0 // one-shot
			}
		}
		if n := p.expireClaimsLocked(now); n > 0 {
			p.saveLocked()
		}
		p.mu.Unlock()
		for _, id := range due {
			p.drawAsync(id, "auto")
		}
	}
}

// parseAt parses the "at <time>" argument of lottery create: 15:04 today
// (or tomorrow if past), 15:04:05, or a full 2006-01-02 15:04[:05].
func parseAt(s string, now time.Time) (time.Time, error) {
	layouts := []string{"2006-01-02 15:04:05", "2006-01-02 15:04", "15:04:05", "15:04"}
	for _, layout := range layouts {
		if t, err := time.ParseInLocation(layout, s, now.Location()); err == nil {
			if layout == "15:04" || layout == "15:04:05" {
				// Time-of-day: today, or tomorrow when already past.
				t = time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), t.Second(), 0, now.Location())
				if !t.After(now) {
					t = t.Add(24 * time.Hour)
				}
			}
			return t, nil
		}
	}
	return time.Time{}, errors.New("bad time: use HH:MM, HH:MM:SS or YYYY-MM-DD HH:MM")
}

// scheduleAutoLocked arms the auto-draw timer for a lottery.
func (p *LotteryPlugin) scheduleAutoLocked(id int64, at time.Time) {
	if l := p.findLocked(id); l != nil {
		l.AutoDrawAt = at.Unix()
	}
}
