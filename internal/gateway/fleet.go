package gateway

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/tyk-swe/olp/internal/limits"
)

// fleetStaleness bounds how long a circuit one replica opens can go unseen by
// the others. Local transitions are published as they happen and every
// replica reads the fleet's every fleetPoll, so a transition reaches the fleet
// within one poll and one round trip.
const (
	fleetStaleness = 5 * time.Second
	fleetPoll      = 2 * time.Second
)

// circuitChange is one local circuit transition: open until a moment, or
// closed when the moment is zero.
type circuitChange struct {
	provider string
	until    time.Time
}

// fleetHealth holds the circuits the fleet and the active probes have opened,
// each until the moment it closes. A provider in it moves to the end of the
// attempt order rather than leaving it, so a fleet-wide false positive costs
// latency, never availability.
type fleetHealth struct {
	open atomic.Pointer[map[string]time.Time]
	now  func() time.Time
}

// marked reports whether any provider is marked, which keeps the check out
// of selection while the fleet is healthy.
func (f *fleetHealth) marked() bool {
	open := f.open.Load()
	return open != nil && len(*open) > 0
}

// unhealthy reports whether the fleet has an open circuit for the provider.
func (f *fleetHealth) unhealthy(providerID string) bool {
	open := f.open.Load()
	if open == nil {
		return false
	}
	until, ok := (*open)[providerID]
	return ok && f.now().Before(until)
}

// RunFleetHealth shares this gateway's circuit transitions with the fleet
// and follows the fleet's, until ctx ends. Without shared state it returns at
// once, and local circuits alone decide.
func (s *Server) RunFleetHealth(ctx context.Context) {
	if !s.Admission.ready() {
		return
	}
	limiter := s.Admission.limiter
	ticker := time.NewTicker(fleetPoll)
	defer ticker.Stop()
	s.pollFleet(ctx, limiter)
	for {
		select {
		case <-ctx.Done():
			return
		case change := <-s.health.changes:
			call, cancel := context.WithTimeout(ctx, reserveTimeout)
			err := limiter.PublishCircuit(call, change.provider, change.until)
			cancel()
			if err != nil {
				s.log.Warn("circuit transition not shared with the fleet", "provider_id", change.provider, "error", err.Error())
			}
		case <-ticker.C:
			s.pollFleet(ctx, limiter)
		}
	}
}

// pollFleet replaces the fleet's circuits with the shared state. An
// unreadable state forgets them: a stale mark would outlive its bound.
func (s *Server) pollFleet(ctx context.Context, limiter *limits.Limiter) {
	call, cancel := context.WithTimeout(ctx, reserveTimeout)
	defer cancel()
	open, err := limiter.Circuits(call, s.now())
	if err != nil {
		s.fleet.open.Store(nil)
		return
	}
	s.fleet.open.Store(&open)
}
