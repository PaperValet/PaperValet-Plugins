package main

import "time"

// Stats keeps per-source forwarding counts, persisted in data/shift/stats.json.
type Stats struct {
	Days map[string]map[int64]int `json:"days"` // date -> source id -> count
}

// add records one forwarded message for the source chat on today's date.
func (s *Stats) add(source int64) {
	if s == nil {
		return
	}
	today := time.Now().Format("2006-01-02")
	if s.Days == nil {
		s.Days = map[string]map[int64]int{}
	}
	day, ok := s.Days[today]
	if !ok {
		day = map[int64]int{}
		s.Days[today] = day
	}
	day[source]++
}

// totals sums per-source counts over all kept days.
func (s *Stats) totals() map[int64]int {
	out := map[int64]int{}
	if s == nil {
		return out
	}
	for _, day := range s.Days {
		for src, n := range day {
			out[src] += n
		}
	}
	return out
}

// recent returns the newest limit dates with their counts for the source.
func (s *Stats) recent(source int64, limit int) []dayCount {
	if s == nil {
		return nil
	}
	var out []dayCount
	var days []string
	for d := range s.Days {
		days = append(days, d)
	}
	for i := len(days) - 1; i >= 0 && len(out) < limit; i-- {
		d := days[i]
		if n, ok := s.Days[d][source]; ok {
			out = append(out, dayCount{Date: d, Count: n})
		}
	}
	return out
}

type dayCount struct {
	Date  string
	Count int
}
