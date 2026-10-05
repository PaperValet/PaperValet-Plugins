package main

import (
	"strconv"
	"strings"
	"time"
)

// period is the reporting window the user asked for.
type period struct {
	From  time.Time
	To    time.Time
	Label string // cache/display key, e.g. "2025" or "2025-06" or "all"
}

// parsePeriod interprets one argument (joined words) as a period:
// "", "year", "lastyear", "annual", "去年度" → a year;
// "YYYY", "YYYY-MM", "YYYY-MM-DD", "YYYYMM", "YYYYMMDD";
// "all" → everything (bounded by the scan limit);
// "month" / "本月" → current month (title says "时段报告").
func parsePeriod(arg string, now time.Time) (period, bool) {
	s := strings.TrimSpace(strings.ToLower(arg))
	if s == "" || s == "year" || s == "annual" || s == "annualreport" || s == "年度" || s == "年报" || s == "去年" || s == "lastyear" {
		year := now.Year()
		if s == "去年" || s == "lastyear" || now.Month() == time.January {
			// Like the source: in January the "year in review" is last year.
			year--
		}
		return yearPeriod(year), true
	}
	if s == "all" || s == "全部" || s == "总" {
		return period{From: time.Time{}, To: time.Time{}, Label: "all"}, true
	}
	if s == "month" || s == "本月" || s == "当月" {
		return monthPeriod(now.Year(), int(now.Month())), true
	}
	switch len(s) {
	case 4:
		if y, err := strconv.Atoi(s); err == nil && y >= 2000 && y <= 2100 {
			return yearPeriod(y), true
		}
	case 6:
		if y, err := strconv.Atoi(s[:4]); err == nil && y >= 2000 && y <= 2100 {
			if m, err := strconv.Atoi(s[4:]); err == nil && m >= 1 && m <= 12 {
				return monthPeriod(y, m), true
			}
		}
	case 8:
		if y, err := strconv.Atoi(s[:4]); err == nil && y >= 2000 && y <= 2100 {
			if m, err := strconv.Atoi(s[4:6]); err == nil && m >= 1 && m <= 12 {
				if d, err := strconv.Atoi(s[6:]); err == nil && d >= 1 && d <= 31 {
					return dayPeriod(y, m, d), true
				}
			}
		}
	}
	if strings.Contains(s, "-") {
		parts := strings.Split(s, "-")
		if len(parts) == 2 {
			if y, err := strconv.Atoi(parts[0]); err == nil && y >= 2000 && y <= 2100 {
				if m, err := strconv.Atoi(parts[1]); err == nil && m >= 1 && m <= 12 {
					return monthPeriod(y, m), true
				}
			}
		}
		if len(parts) == 3 {
			if y, err := strconv.Atoi(parts[0]); err == nil && y >= 2000 && y <= 2100 {
				if m, err := strconv.Atoi(parts[1]); err == nil && m >= 1 && m <= 12 {
					if d, err := strconv.Atoi(parts[2]); err == nil && d >= 1 && d <= 31 {
						return dayPeriod(y, m, d), true
					}
				}
			}
		}
	}
	return period{}, false
}

func yearPeriod(y int) period {
	return period{
		From:  time.Date(y, time.January, 1, 0, 0, 0, 0, time.Local),
		To:    time.Date(y+1, time.January, 1, 0, 0, 0, 0, time.Local),
		Label: strconv.Itoa(y),
	}
}

func monthPeriod(y, m int) period {
	return period{
		From:  time.Date(y, time.Month(m), 1, 0, 0, 0, 0, time.Local),
		To:    time.Date(y, time.Month(m)+1, 1, 0, 0, 0, 0, time.Local),
		Label: time.Date(y, time.Month(m), 1, 0, 0, 0, 0, time.Local).Format("2006-01"),
	}
}

func dayPeriod(y, m, d int) period {
	from := time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.Local)
	return period{From: from, To: from.AddDate(0, 0, 1), Label: from.Format("2006-01-02")}
}
