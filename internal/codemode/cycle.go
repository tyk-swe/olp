package codemode

import "time"

func NextMonthlyReset(anchor string, now time.Time) (time.Time, bool) {
	parsed, err := time.Parse(time.DateOnly, anchor)
	if err != nil || parsed.Format(time.DateOnly) != anchor {
		return time.Time{}, false
	}
	day := parsed.Day()
	now = now.UTC()
	for offset := -1; offset <= 2; offset++ {
		year, month, _ := now.AddDate(0, offset, 0).Date()
		candidate := time.Date(year, month, min(day, daysInMonth(year, month)), 0, 0, 0, 0, time.UTC)
		if candidate.After(now) {
			return candidate, true
		}
	}
	return time.Time{}, false
}

func daysInMonth(year int, month time.Month) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}
