// Package budgetcalendar derives civil budget boundaries independently of the
// machine timezone. Durations are never used to advance days, weeks or months.
package budgetcalendar

import (
	"fmt"
	"strings"
	"time"
	_ "time/tzdata"
)

type Calendar struct {
	name     string
	location *time.Location
}
type Window struct{ Start, End time.Time }
type Windows struct{ Day, Week, Month Window }

func New(name string) (Calendar, error) {
	if name == "" || name == "Local" || strings.HasPrefix(name, "/") || strings.Contains(name, "..") {
		return Calendar{}, fmt.Errorf("use an IANA time zone")
	}
	location, err := time.LoadLocation(name)
	if err != nil {
		return Calendar{}, fmt.Errorf("unknown IANA time zone %q", name)
	}
	return Calendar{name, location}, nil
}
func (c Calendar) Name() string {
	if c.name == "" {
		return "UTC"
	}
	return c.name
}
func (c Calendar) Windows(at time.Time) Windows {
	location := c.location
	if location == nil {
		location = time.UTC
	}
	local := at.In(location)
	year, month, day := local.Date()
	// UTC here is civil-date arithmetic only; the resulting dates are then
	// resolved in the configured zone, including skipped/repeated midnights.
	date := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	monday := date.AddDate(0, 0, -(int(date.Weekday())+6)%7)
	first := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
	makeWindow := func(start, end time.Time) Window { return Window{boundary(start, location), boundary(end, location)} }
	return Windows{makeWindow(date, date.AddDate(0, 0, 1)), makeWindow(monday, monday.AddDate(0, 0, 7)), makeWindow(first, first.AddDate(0, 1, 0))}
}

// boundary finds the first instant of a civil date. time.Date alone can choose
// the later occurrence of an ambiguous midnight or normalize a missing one to
// the preceding date. Civil dates remain ordered through modern IANA changes,
// including a skipped date at the international date line.
func boundary(date time.Time, location *time.Location) time.Time {
	if location == time.UTC {
		return date
	}
	target := dateNumber(date)
	low, high := date.Unix()-48*3600, date.Unix()+48*3600
	for low < high {
		mid := low + (high-low)/2
		if dateNumber(time.Unix(mid, 0).In(location)) < target {
			low = mid + 1
		} else {
			high = mid
		}
	}
	return time.Unix(low, 0).UTC()
}
func dateNumber(t time.Time) int { y, m, d := t.Date(); return y*10000 + int(m)*100 + d }
