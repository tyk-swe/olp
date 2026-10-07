//go:build integration

package integration_test

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

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

func TestLimitsCapacitySharesHoldUnderConcurrentLoadAcrossGateways(t *testing.T) {
	first, second := limClient(t), limClient(t)
	namespace := limNamespace(t, first, "shares")
	gateways := [2]*limits.Limiter{limLimiter(t, first, namespace), limLimiter(t, second, namespace)}

	request := limRequest(limits.SlotLookup(uuid.NewString()))
	request.RequestsPerMinute, request.TokensPerMinute, request.RequestedTokens = nil, nil, 0
	request.MaxConcurrency = limPointer(int64(10))

	// Idle capacity is free to any class until the slot is half full, and then
	// low work is held to its 10 percent share, which it already exceeds.
	if granted, _ := limRace(t, gateways, request, "low", 64); granted != 5 {
		t.Fatalf("low work held %d slots of an idle quota, want the 5 below saturation", granted)
	}
	// Critical work still has its half of the limit, however busy the slot is.
	critical, leases := limRace(t, gateways, request, "critical", 64)
	if critical != 5 {
		t.Fatalf("critical work held %d slots, want its share of 5", critical)
	}
	// The quota itself still binds every class.
	if granted, _ := limRace(t, gateways, request, "high", 16); granted != 0 {
		t.Fatalf("high work held %d slots of a full quota", granted)
	}

	// Ending a critical lease frees a critical slot, never a low one.
	limReleaseTwice(t, leases[0])
	if granted, _ := limRace(t, gateways, request, "low", 16); granted != 0 {
		t.Fatalf("low work took %d slots freed beyond saturation", granted)
	}
	if granted, _ := limRace(t, gateways, request, "critical", 16); granted != 1 {
		t.Fatalf("critical work took %d freed slots, want 1", granted)
	}
}

func TestLimitsCapacitySharesDivideRequestAndTokenWindows(t *testing.T) {
	first, second := limClient(t), limClient(t)
	namespace := limNamespace(t, first, "share-rate")
	gateways := [2]*limits.Limiter{limLimiter(t, first, namespace), limLimiter(t, second, namespace)}

	for range 3 {
		lookup := limits.ConnectionLookup(uuid.NewString())
		requests := limRequest(lookup)
		requests.MaxConcurrency, requests.TokensPerMinute, requests.RequestedTokens = nil, nil, 0
		requests.RequestsPerMinute = limPointer(int64(20))

		limSettleInMinute(t, first, 5*time.Second)
		before := limServerTimeMS(t, first) / 60_000
		low, _ := limRace(t, gateways, requests, "low", 64)
		normal, _ := limRace(t, gateways, requests, "normal", 64)
		critical, _ := limRace(t, gateways, requests, "critical", 64)

		tokens := limRequest(limits.ConnectionLookup(uuid.NewString()))
		tokens.MaxConcurrency, tokens.RequestsPerMinute = nil, nil
		tokens.TokensPerMinute, tokens.RequestedTokens = limPointer(int64(1000)), 100
		lowTokens, lowLeases := limRace(t, gateways, tokens, "low", 32)
		if after := limServerTimeMS(t, first) / 60_000; after != before {
			continue
		}

		// Low work fills the window to saturation and has no share left
		// beyond it; normal work then has its two, and critical work the
		// rest of the window.
		if low != 10 || normal != 2 || critical != 8 {
			t.Fatalf("requests granted low %d, normal %d, critical %d; want 10, 2, 8", low, normal, critical)
		}
		if lowTokens != 5 {
			t.Fatalf("low work was granted %d token reservations, want the 5 below saturation", lowTokens)
		}

		// A refund returns tokens to the class that reserved them, so low
		// work regains exactly what it gave back.
		if err := lowLeases[0].Refund(t.Context()); err != nil {
			t.Fatalf("Refund: %v", err)
		}
		if granted, _ := limRace(t, gateways, tokens, "low", 8); granted != 1 {
			t.Fatalf("low work regained %d token reservations after one refund, want 1", granted)
		}
		hash := limHash(t, first, limRateKeysOf(namespace, tokens.LookupID))
		if got := limField(t, hash, "tpm:low"); got != 500 {
			t.Fatalf("tpm:low = %d, want 500 (hash %v)", got, hash)
		}
		return
	}
	t.Fatal("every attempt crossed a minute boundary")
}

func limRateKeysOf(namespace, lookup string) string {
	rate, _ := limRateKeys(namespace, lookup)
	return rate
}
