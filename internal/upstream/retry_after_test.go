package upstream

import (
	"net/http"
	"testing"
	"time"
)

func TestRetryAfterBounds(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	saturated := time.Duration(uint64((1<<63-1)/time.Second)) * time.Second
	for value, want := range map[string]time.Duration{
		"":                         0,
		"   ":                      0,
		"0":                        0,
		"2":                        2 * time.Second,
		"120":                      2 * time.Minute,
		" 120 ":                    2 * time.Minute,
		"-1":                       0,
		"1.5":                      0,
		"1e3":                      0,
		"NaN":                      0,
		"Inf":                      0,
		"nonsense":                 0,
		"12junk":                   0,
		"18446744073709551616x":    0,
		"18446744073709551616junk": 0,
		"9223372036":               saturated,
		"9223372037":               saturated,
		"9223372036854775807":      saturated,
		"18446744073709551616":     saturated,
		"18446744073709551617":     saturated,
		now.Add(5 * time.Second).Format(http.TimeFormat):  5 * time.Second,
		now.Add(-5 * time.Second).Format(http.TimeFormat): 0,
	} {
		if got := RetryAfter(value, now); got != want {
			t.Errorf("%q: %v, want %v", value, got, want)
		}
	}
}
