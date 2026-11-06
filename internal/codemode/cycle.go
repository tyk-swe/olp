package codemode

import "time"

func ParseCycleAnchor(anchor string) (time.Time, bool) {
	parsed, err := time.Parse(time.DateOnly, anchor)
	if err != nil || parsed.Format(time.DateOnly) != anchor {
		return time.Time{}, false
	}
	return parsed, true
}

func NextMonthlyReset(anchor string, now time.Time) (time.Time, bool) {
	parsed, ok := ParseCycleAnchor(anchor)
	if !ok {
		return time.Time{}, false
	}
	anchorDay := parsed.Day()
	utcNow := now.UTC()
	monthStart := time.Date(utcNow.Year(), utcNow.Month(), 1, 0, 0, 0, 0, time.UTC)
	for offset := 0; offset <= 1; offset++ {
		target := monthStart.AddDate(0, offset, 0)
		candidate := time.Date(target.Year(), target.Month(), min(anchorDay, daysInMonth(target.Year(), target.Month())), 0, 0, 0, 0, time.UTC)
		if candidate.After(utcNow) {
			return candidate, true
		}
	}
	return time.Time{}, false
}

func daysInMonth(year int, month time.Month) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}
