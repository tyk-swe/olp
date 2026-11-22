package destination

import (
	"testing"
	"time"
)

func TestPendingOutcome(t *testing.T) {
	t.Parallel()
	now := time.Now()
	cases := []struct {
		name      string
		attempts  int
		delivered *time.Time
		rejected  *time.Time
		sendMay   bool
		want      MeteringDeliveryOutcome
	}{
		{"never claimed", 0, nil, nil, false, MeteringPending},
		{"claimed and queued", 1, nil, nil, true, MeteringRetrying},
		{"local failure still queued", 2, nil, nil, false, MeteringRetrying},
		{"recorded acknowledgement", 2, &now, nil, true, MeteringAccepted},
		{"rejected after possible send", 2, nil, &now, true, MeteringDeliveryUnconfirmed},
		{"rejected before any send", 0, nil, &now, false, MeteringExpiredBeforeDeliver},
		{"acknowledgement wins over rejection markers", 3, &now, &now, true, MeteringAccepted},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := PendingOutcome(c.attempts, c.delivered, c.rejected, c.sendMay); got != c.want {
				t.Fatalf("outcome = %q, want %q", got, c.want)
			}
		})
	}
}
