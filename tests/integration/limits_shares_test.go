//go:build integration

package integration_test

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/tyk-swe/olp/internal/limits"
)

// limShare divides a quota as an operator would: half for critical work, a
// little for low, and free use of idle capacity up to half the limit.
func limShare(class string) limits.Share {
	percent := map[string]int64{"critical": 50, "high": 30, "normal": 10, "low": 10}[class]
	return limits.Share{Class: class, Percent: percent, SaturationPercent: 50}
}

// limRace has callers reserve request from two gateways at once, alternating
// between them, and counts what each class was granted.
func limRace(t *testing.T, gateways [2]*limits.Limiter, request limits.Request, class string, callers int) (granted int64, leases []*limits.Lease) {
	t.Helper()
	request.Share = limShare(class)
	var mu sync.Mutex
	var count atomic.Int64
	var group sync.WaitGroup
	for caller := range callers {
		group.Go(func() {
			lease, err := gateways[caller%2].Reserve(t.Context(), request)
			var exceeded *limits.ExceededError
			switch {
			case err == nil:
				count.Add(1)
				mu.Lock()
				leases = append(leases, lease)
				mu.Unlock()
			case errors.As(err, &exceeded):
			default:
				t.Errorf("Reserve: %v", err)
			}
		})
	}
	group.Wait()
	return count.Load(), leases
}

// Idle capacity is free to any class until the slot is half full, and then
// low work is held to its 10 percent share, which it already exceeds.

// Critical work still has its half of the limit, however busy the slot is.

// The quota itself still binds every class.

// Ending a critical lease frees a critical slot, never a low one.

// Low work fills the window to saturation and has no share left
// beyond it; normal work then has its two, and critical work the
// rest of the window.

// A refund returns tokens to the class that reserved them, so low
// work regains exactly what it gave back.

func limRateKeysOf(namespace, lookup string) string {
	rate, _ := limRateKeys(namespace, lookup)
	return rate
}
