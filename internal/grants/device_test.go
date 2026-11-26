package grants

import (
	"testing"
	"time"
)

func TestDevicePollInterval(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		interval time.Duration
		slowed   bool
		want     time.Duration
	}{
		{name: "zero defaults to five seconds", interval: 0, slowed: false, want: defaultInterval},
		{name: "negative defaults to five seconds", interval: -3 * time.Second, slowed: true, want: defaultInterval},
		{name: "normal interval preserved", interval: 10 * time.Second, slowed: false, want: 10 * time.Second},
		{name: "normal interval capped at max", interval: maxInterval + time.Minute, slowed: false, want: maxInterval},
		{name: "slow_down adds five seconds", interval: 10 * time.Second, slowed: true, want: 15 * time.Second},
		{name: "slow_down capped at max", interval: maxInterval, slowed: true, want: maxInterval},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := DevicePollInterval(tc.interval, tc.slowed); got != tc.want {
				t.Fatalf("DevicePollInterval(%v, %t) = %v, want %v", tc.interval, tc.slowed, got, tc.want)
			}
		})
	}
}
