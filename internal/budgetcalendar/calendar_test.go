package budgetcalendar

import (
	"testing"
	"time"
)

func TestCivilWindowsIncludeDSTAndISOWeeks(t *testing.T) {
	for _, test := range []struct {
		zone, at, start, end string
		hours                float64
	}{
		{"America/New_York", "2026-03-08T12:00:00Z", "2026-03-08T05:00:00Z", "2026-03-09T04:00:00Z", 23},
		{"America/New_York", "2026-11-01T12:00:00Z", "2026-11-01T04:00:00Z", "2026-11-02T05:00:00Z", 25},
		{"Australia/Lord_Howe", "2026-10-04T03:00:00Z", "2026-10-03T13:30:00Z", "2026-10-04T13:00:00Z", 23.5},
		{"Asia/Kathmandu", "2026-10-08T01:00:00Z", "2026-10-07T18:15:00Z", "2026-10-08T18:15:00Z", 24},
		{"America/Havana", "2026-11-01T12:00:00Z", "2026-11-01T04:00:00Z", "2026-11-02T05:00:00Z", 25},
		{"Pacific/Apia", "2011-12-29T12:00:00Z", "2011-12-29T10:00:00Z", "2011-12-30T10:00:00Z", 24},
	} {
		t.Run(test.zone+test.at, func(t *testing.T) {
			c, err := New(test.zone)
			if err != nil {
				t.Fatal(err)
			}
			at, _ := time.Parse(time.RFC3339, test.at)
			w := c.Windows(at)
			if w.Day.Start.Format(time.RFC3339) != test.start || w.Day.End.Format(time.RFC3339) != test.end || w.Day.End.Sub(w.Day.Start).Hours() != test.hours {
				t.Fatalf("wrong day: %+v", w.Day)
			}
			if w.Week.Start.In(c.location).Weekday() != time.Monday || at.Before(w.Week.Start) || !at.Before(w.Week.End) {
				t.Fatalf("wrong ISO week: %+v", w.Week)
			}
		})
	}
	c, _ := New("UTC")
	w := c.Windows(time.Date(2021, 1, 1, 12, 0, 0, 0, time.UTC))
	if w.Week.Start.Format("2006-01-02") != "2020-12-28" || w.Week.End.Format("2006-01-02") != "2021-01-04" {
		t.Fatal(w.Week)
	}
}
func TestCalendarRejectsMachineLocalAndInvalidZones(t *testing.T) {
	for _, name := range []string{"", "Local", "../UTC", "/UTC", "not-a-zone"} {
		if _, err := New(name); err == nil {
			t.Fatal(name)
		}
	}
}
