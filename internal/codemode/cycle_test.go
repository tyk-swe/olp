package codemode_test

import (
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/codemode"
)

func TestNextMonthlyReset(t *testing.T) {
	cases := []struct {
		name   string
		anchor string
		now    time.Time
		want   time.Time
		ok     bool
	}{
		{"later this month", "2026-01-15", time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC), time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC), true},
		{"exactly at reset moves to next month", "2026-01-15", time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC), time.Date(2026, 12, 15, 0, 0, 0, 0, time.UTC), true},
		{"past this month", "2026-01-15", time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC), time.Date(2026, 12, 15, 0, 0, 0, 0, time.UTC), true},
		{"year rollover", "2026-01-15", time.Date(2026, 12, 16, 0, 0, 0, 0, time.UTC), time.Date(2027, 1, 15, 0, 0, 0, 0, time.UTC), true},
		{"day 31 clamps to february end", "2026-01-31", time.Date(2026, 2, 10, 0, 0, 0, 0, time.UTC), time.Date(2026, 2, 28, 0, 0, 0, 0, time.UTC), true},
		{"day 31 after clamped february", "2026-01-31", time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC), true},
		{"day 31 clamps to april end", "2026-01-31", time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC), true},
		{"day 29 clamps in non leap february", "2024-02-29", time.Date(2026, 2, 10, 0, 0, 0, 0, time.UTC), time.Date(2026, 2, 28, 0, 0, 0, 0, time.UTC), true},
		{"anchor in future month still monthly", "2027-06-15", time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC), true},
		{"anchor 15th from january 31st lands february 15th not march", "2026-01-15", time.Date(2026, 1, 31, 10, 0, 0, 0, time.UTC), time.Date(2026, 2, 15, 0, 0, 0, 0, time.UTC), true},
		{"anchor 31st from january 31st clamps to february 28th", "2026-01-31", time.Date(2026, 1, 31, 10, 0, 0, 0, time.UTC), time.Date(2026, 2, 28, 0, 0, 0, 0, time.UTC), true},
		{"anchor 31st from january 31st clamps to february 29th in leap year", "2028-01-31", time.Date(2028, 1, 31, 10, 0, 0, 0, time.UTC), time.Date(2028, 2, 29, 0, 0, 0, 0, time.UTC), true},
		{"anchor 15th from march 31st lands april 15th not may", "2026-01-15", time.Date(2026, 3, 31, 10, 0, 0, 0, time.UTC), time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC), true},
		{"anchor 31st from march 31st clamps to april 30th", "2026-01-31", time.Date(2026, 3, 31, 10, 0, 0, 0, time.UTC), time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC), true},
		{"invalid anchor", "2026-13-15", time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC), time.Time{}, false},
		{"empty anchor", "", time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC), time.Time{}, false},
		{"not a date", "15", time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC), time.Time{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := codemode.NextMonthlyReset(tc.anchor, tc.now)
			if ok != tc.ok || !got.Equal(tc.want) {
				t.Fatalf("NextMonthlyReset(%q, %s) = %s, %v want %s, %v", tc.anchor, tc.now, got, ok, tc.want, tc.ok)
			}
		})
	}
}
