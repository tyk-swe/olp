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
	probing      bool
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
	p := h.provider(providerID)
	return p.probing || h.now().Before(p.openUntil)
}

// openCircuits counts providers whose circuit is open or half-open right now.
// It is the source of the olp_open_target_circuits metric.
func (h *healthTracker) openCircuits() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	var count int64
	for _, p := range h.providers {
		if p.probing || h.now().Before(p.openUntil) {
			count++
		}
	}
	return count
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
	wasProbe := p.probing
	p.probing = false
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
	// Fault attribution decides whether this attempt is evidence about the
	// provider at all. Facts that carry none fall back to the class default
	// so dispatchers that never attributed a fault keep their behavior.
	origin, scope := fact.FaultOrigin, fact.FaultScope
	if fact.Class != classSuccess && origin == "" && scope == "" {
		origin, scope = defaultFault(fact.Class)
	}
	// Proxy-local and request-local faults — capacity bounds, persistence
	// failures, contract defects, the client's own connection — say nothing
	// about the provider endpoint and must not charge its shared circuit.
	switch origin {
	case faultProxyCapacity, faultProxyPersistence, faultProxyPolicy, faultClientDelivery, faultContract:
		return
	}
	if scope == scopeRequest || scope == scopeContract {
		return
	}
	switch fact.Class {
	case "success":
		b.successes++
		p.failures = 0
		p.openUntil = time.Time{}
		return
	case "rate_limit":
		b.rateLimits++
		return
	case "upstream_server":
		b.serverErrors++
	case "connect", "timeout", "protocol":
		b.transportErrors++
	case "ambiguous":
		if fact.Interaction == nil {
			return
		}
		// Strict ambiguity changes retry permission, not the existing evidence
		// that the connection or provider failed to produce a usable response.
		if fact.Status >= 500 {
			b.serverErrors++
		} else {
			b.transportErrors++
		}
	default:
		return
	}
	if wasProbe {
		p.openUntil = now.Add(circuitOpenFor)
		p.failures = 0
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

// claim reserves the one half-open endpoint probe. The first return value
// reports whether the probe was acquired: record releases it once the attempt
// lands, and releaseProbe returns it when a local step fails before dispatch.
// Credential-only outcomes release it in record without charging an endpoint
// failure.
func (h *healthTracker) claim(id string) (probed, granted bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	p := h.provider(id)
	if p.probing || h.now().Before(p.openUntil) {
		return false, false
	}
	probed = !p.openUntil.IsZero()
	if probed {
		p.probing = true
	}
	return probed, true
}

// releaseProbe returns a claimed half-open probe whose attempt was abandoned
// before dispatch, leaving the endpoint free for the next sibling.
func (h *healthTracker) releaseProbe(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.provider(id).probing = false
}
