package health

import (
	"sync"
	"time"

	"github.com/tyk-swe/olp/internal/gateway/events"
)

const (
	circuitFailures    = 5
	circuitWindow      = 30 * time.Second
	CircuitOpenFor     = 30 * time.Second
	CredentialCooldown = time.Minute
	rateLimitCooldown  = 10 * time.Second
	maxCooldown        = time.Minute
	healthRetention    = 24 * time.Hour
)

// Tracker keeps in-memory provider circuit state, credential slot
// cooldowns, and windowed attempt statistics for the health endpoint.
type Tracker struct {
	mu        sync.Mutex
	providers map[string]*providerHealth
	now       func() time.Time
	// changes carries circuit transitions to the fleet.
	changes chan circuitChange
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

func NewTracker(now func() time.Time) *Tracker {
	return &Tracker{providers: map[string]*providerHealth{}, now: now, changes: make(chan circuitChange, 64)}
}

func (h *Tracker) provider(id string) *providerHealth {
	p := h.providers[id]
	if p == nil {
		p = &providerHealth{buckets: map[int64]*bucket{}, cooldowns: map[string]time.Time{}}
		h.providers[id] = p
	}
	return p
}

// Open reports whether the provider circuit currently rejects attempts.
func (h *Tracker) Open(providerID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	p := h.provider(providerID)
	return p.probing || h.now().Before(p.openUntil)
}

// OpenCircuits counts providers whose circuit is open or half-open right now.
// It is the source of the olp_open_target_circuits metric.
func (h *Tracker) OpenCircuits() int64 {
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

// CoolingDown reports whether a credential slot is paused after a failure.
func (h *Tracker) CoolingDown(providerID, slotID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.now().Before(h.provider(providerID).cooldowns[slotID])
}

func (h *Tracker) Cooldown(providerID, slotID string, d time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.provider(providerID).cooldowns[slotID] = h.now().Add(CooldownDuration(d))
}

// EndCooldown ends a credential slot's cooldown before it runs out.
func (h *Tracker) EndCooldown(providerID, slotID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.provider(providerID).cooldowns, slotID)
}

// CooldownDuration bounds how long one rejection sidelines a credential slot.
// An upstream that named no delay gets the default wait, and one that named an
// implausible delay is not believed past the cap: a slot that never comes back
// is a slot that silently shrinks the pool. The shared cooldown every replica
// reads is written for the same span as the local one.
func CooldownDuration(d time.Duration) time.Duration {
	if d <= 0 {
		return rateLimitCooldown
	}
	return min(d, maxCooldown)
}

// Record folds one attempt into the circuit and the health window. probe says
// the attempt holds the half-open probe, which only its own completion releases.
func (h *Tracker) Record(providerID string, fact events.AttemptFact, probe bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	p := h.provider(providerID)
	if probe {
		p.probing = false
	}
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
		// Grant generations have separate cooldowns. Retire expired ones
		// even when a gateway missed intermediate refresh notifications.
		for key, until := range p.cooldowns {
			if !now.Before(until) {
				delete(p.cooldowns, key)
			}
		}
	}
	b.attempts++
	b.latency += fact.Duration
	if fact.LocalRefusal && fact.Class != events.ClassSuccess {
		return
	}
	switch fact.Class {
	case "success":
		b.successes++
		p.failures = 0
		if !p.openUntil.IsZero() {
			p.openUntil = time.Time{}
			h.publish(providerID, p.openUntil)
		}
		return
	case "rate_limit":
		b.rateLimits++
		return
	case "upstream_server":
		b.serverErrors++
	case "connect", "timeout", "protocol":
		b.transportErrors++
	case "ambiguous":
		// Ambiguity changes retry permission, not the evidence that the
		// connection or provider failed to produce a usable response. That
		// evidence counts for a strict interaction, and for traffic a plugin
		// carries, whose every failure after sending is ambiguous; other
		// side-effecting operations leave their ambiguous failures out.
		if fact.Interaction == nil && !fact.Carried {
			return
		}
		if fact.Status >= 500 {
			b.serverErrors++
		} else {
			b.transportErrors++
		}
	default:
		return
	}
	if probe {
		p.openUntil = now.Add(CircuitOpenFor)
		p.failures = 0
		h.publish(providerID, p.openUntil)
		return
	}
	if p.failures == 0 || now.Sub(p.firstFailure) > circuitWindow {
		p.failures = 0
		p.firstFailure = now
	}
	p.failures++
	if p.failures >= circuitFailures {
		p.openUntil = now.Add(CircuitOpenFor)
		p.failures = 0
		h.publish(providerID, p.openUntil)
	}
}

// publish hands a circuit transition to the fleet without waiting: a full
// queue drops it, and the circuit still holds on this gateway.
func (h *Tracker) publish(providerID string, until time.Time) {
	select {
	case h.changes <- circuitChange{provider: providerID, until: until}:
	default:
	}
}

// Claim reserves the one half-open endpoint probe. The first return value
// reports whether the probe was acquired: Record releases it once the attempt
// lands, and ReleaseProbe returns it when a local step fails before dispatch.
// Credential-only outcomes release it in Record without charging an endpoint
// failure.
func (h *Tracker) Claim(id string) (probed, granted bool) {
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

// ReleaseProbe returns a claimed half-open probe whose attempt was abandoned
// before dispatch, leaving the endpoint free for the next sibling.
func (h *Tracker) ReleaseProbe(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.provider(id).probing = false
}
