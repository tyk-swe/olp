package gateway

import (
	"sync"
	"time"

	"github.com/tyk-swe/olp/internal/providers"
)

const (
	circuitFailures    = 5
	circuitWindow      = 30 * time.Second
	circuitOpenFor     = 30 * time.Second
	credentialCooldown = time.Minute
	rateLimitCooldown  = 10 * time.Second
	maxCooldown        = time.Minute
	healthRetention    = 24 * time.Hour
)

// healthTracker keeps in-memory provider circuit state, credential slot
// cooldowns, and windowed attempt statistics for the health endpoint.
type healthTracker struct {
	mu        sync.Mutex
	providers map[string]*providerHealth
	now       func() time.Time
}

type providerHealth struct {
	buckets      map[int64]*bucket
	last         time.Time
	failures     int
	firstFailure time.Time
	openUntil    time.Time
	cooldowns    map[string]time.Time
}

type bucket struct {
	attempts, successes, rateLimits, serverErrors, transportErrors int64
	latency                                                        time.Duration
}

func newHealthTracker(now func() time.Time) *healthTracker {
	return &healthTracker{providers: map[string]*providerHealth{}, now: now}
}

func (h *healthTracker) provider(id string) *providerHealth {
	p := h.providers[id]
	if p == nil {
		p = &providerHealth{buckets: map[int64]*bucket{}, cooldowns: map[string]time.Time{}}
		h.providers[id] = p
	}
	return p
}

// open reports whether the provider circuit currently rejects attempts.
func (h *healthTracker) open(providerID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.now().Before(h.provider(providerID).openUntil)
}

// coolingDown reports whether a credential slot is paused after a failure.
func (h *healthTracker) coolingDown(providerID, slotID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.now().Before(h.provider(providerID).cooldowns[slotID])
}

func (h *healthTracker) cooldown(providerID, slotID string, d time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.provider(providerID).cooldowns[slotID] = h.now().Add(cooldownDuration(d))
}

// cooldownDuration bounds how long one rejection sidelines a credential slot.
// An upstream that named no delay gets the default wait, and one that named an
// implausible delay is not believed past the cap: a slot that never comes back
// is a slot that silently shrinks the pool. The shared cooldown every replica
// reads is written for the same span as the local one.
func cooldownDuration(d time.Duration) time.Duration {
	if d <= 0 {
		return rateLimitCooldown
	}
	return min(d, maxCooldown)
}

// record folds one attempt into the circuit and the health window.
func (h *healthTracker) record(providerID string, fact AttemptFact) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	p := h.provider(providerID)
	p.last = now
	minute := now.Unix() / 60
	b := p.buckets[minute]
	if b == nil {
		b = &bucket{}
		p.buckets[minute] = b
		for k := range p.buckets {
			if k < minute-int64(healthRetention/time.Minute) {
				delete(p.buckets, k)
			}
		}
	}
	b.attempts++
	b.latency += fact.Duration
	switch fact.Class {
	case "success":
		b.successes++
		p.failures = 0
		return
	case "rate_limit":
		b.rateLimits++
		return
	case "upstream_server":
		b.serverErrors++
	case "connect", "timeout", "protocol":
		b.transportErrors++
	default:
		return
	}
	if p.failures == 0 || now.Sub(p.firstFailure) > circuitWindow {
		p.failures = 0
		p.firstFailure = now
	}
	p.failures++
	if p.failures >= circuitFailures {
		p.openUntil = now.Add(circuitOpenFor)
		p.failures = 0
	}
}

// ProviderHealth implements providers.HealthSource.
func (h *healthTracker) ProviderHealth(window time.Duration) map[string]providers.HealthStats {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	since := (now.Add(-window).Unix()) / 60
	out := map[string]providers.HealthStats{}
	for id, p := range h.providers {
		var stats providers.HealthStats
		for minute, b := range p.buckets {
			if minute < since {
				continue
			}
			stats.Attempts += b.attempts
			stats.Successes += b.successes
			stats.RateLimits += b.rateLimits
			stats.ServerErrors += b.serverErrors
			stats.TransportErrors += b.transportErrors
			stats.TotalLatency += b.latency
		}
		stats.LastAttemptAt = p.last
		if stats.Attempts > 0 {
			out[id] = stats
		}
	}
	return out
}
