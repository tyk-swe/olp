package health

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/tyk-swe/olp/internal/limits/fleet"
)

// fleetPoll is how often a replica reads the fleet's circuits. Local
// transitions are published as they happen, so one reaches the fleet within
// one poll and one round trip.
const fleetPoll = 2 * time.Second

// circuitChange is one local circuit transition: open until a moment, or
// closed when the moment is zero.
type circuitChange struct {
	provider string
	until    time.Time
}

// Fleet holds the circuits the fleet and the active probes have opened,
// each until the moment it closes. A provider in it moves to the end of the
// attempt order rather than leaving it, so a fleet-wide false positive costs
// latency, never availability.
type Fleet struct {
	open atomic.Pointer[map[string]time.Time]
	now  func() time.Time
}

func NewFleet(now func() time.Time) *Fleet {
	return &Fleet{now: now}
}

// Marked reports whether any provider is marked, which keeps the check out
// of selection while the fleet is healthy.
func (f *Fleet) Marked() bool {
	open := f.open.Load()
	return open != nil && len(*open) > 0
}

// Unhealthy reports whether the fleet has an open circuit for the provider.
func (f *Fleet) Unhealthy(providerID string) bool {
	open := f.open.Load()
	if open == nil {
		return false
	}
	until, ok := (*open)[providerID]
	return ok && f.now().Before(until)
}

// Follow shares the local circuit transitions with the fleet and follows the
// fleet's, until ctx ends. timeout bounds each round trip to the shared state.
func (f *Fleet) Follow(ctx context.Context, local *Tracker, shared *fleet.State, timeout time.Duration, log *slog.Logger) {
	ticker := time.NewTicker(fleetPoll)
	defer ticker.Stop()
	f.poll(ctx, shared, timeout)
	for {
		select {
		case <-ctx.Done():
			return
		case change := <-local.changes:
			call, cancel := context.WithTimeout(ctx, timeout)
			err := shared.PublishCircuit(call, change.provider, change.until)
			cancel()
			if err != nil {
				log.Warn("circuit transition not shared with the fleet", "provider_id", change.provider, "error", err.Error())
			}
		case <-ticker.C:
			f.poll(ctx, shared, timeout)
		}
	}
}

// poll replaces the fleet's circuits with the shared state. An unreadable
// state forgets them: a stale mark would outlive its bound.
func (f *Fleet) poll(ctx context.Context, shared *fleet.State, timeout time.Duration) {
	call, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	open, err := shared.Circuits(call, f.now())
	if err != nil {
		f.open.Store(nil)
		return
	}
	f.open.Store(&open)
}
