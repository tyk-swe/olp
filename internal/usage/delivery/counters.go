package delivery

import (
	"errors"
	"sync/atomic"
	"time"

	"github.com/tyk-swe/olp/internal/usage"
	"github.com/tyk-swe/olp/internal/usage/completeness"
)

// ErrFull says the bounded buffer had no room for the event. The request that
// produced it has already been served; the loss is counted and reported rather
// than slowing the response path down to wait for Valkey.
var ErrFull = errors.New("the bounded request metadata buffer is full")

// ErrClosed says the stream writer has stopped, so nothing more can be
// accepted. Like ErrFull the event is counted as lost.
var ErrClosed = errors.New("the request metadata stream writer is not running")

// enqueue hands one event to the writer without blocking. The caller is on the
// inference path: it learns the event was lost, and continues.
func (d *Delivery) enqueue(ev usage.Event) error {
	d.intake.RLock()
	defer d.intake.RUnlock()
	if d.closed {
		d.countLoss(&d.dropped, 1)
		return ErrClosed
	}
	select {
	case d.events <- ev:
		// Counted after the handoff: the writer may already have persisted this
		// event, which Snapshot reconciles with its lower bound rather than
		// reporting an impossible negative backlog.
		d.accepted.Add(1)
		return nil
	default:
		d.countLoss(&d.dropped, 1)
		return ErrFull
	}
}

// MarkUnclean keeps the epoch open when HTTP handlers exceeded their drain
// budget: some may still emit after the writer stops accepting events.
func (d *Delivery) MarkUnclean() { d.unclean.Store(true) }

// Snapshot reads the counters. Persisted and abandoned are lower bounds on
// accepted (enqueue counts an event after handing it over), so accepted is raised
// to that bound rather than letting a checkpoint record a negative backlog.
func (d *Delivery) Snapshot() completeness.Snapshot {
	persisted := d.persisted.Load()
	abandoned := d.abandoned.Load()
	accepted := max(d.accepted.Load(), persisted+abandoned)
	d.intake.RLock()
	closed := d.closed
	d.intake.RUnlock()
	return completeness.Snapshot{
		ProcessEpoch: d.processEpoch,
		StartedAt:    d.startedAt,
		Accepted:     accepted,
		Persisted:    persisted,
		Dropped:      d.dropped.Load(),
		Abandoned:    abandoned,
		Retrying:     d.retrying.Load(),
		Unclean:      d.unclean.Load(),
		Closed:       closed,
		FirstLossAt:  lossTime(d.firstLossMS.Load()),
		LastLossAt:   lossTime(d.lastLossMS.Load()),
	}
}

func (d *Delivery) countLoss(counter *atomic.Int64, count int64) {
	counter.Add(count)
	if count <= 0 {
		return
	}
	now := time.Now().UnixMilli()
	d.firstLossMS.CompareAndSwap(0, now)
	d.lastLossMS.Store(now)
}

// closeIntake stops new events. It is idempotent so a writer that stops twice
// (shutdown racing a fatal error) cannot close the channel twice.
func (d *Delivery) closeIntake() {
	d.intake.Lock()
	defer d.intake.Unlock()
	if !d.closed {
		d.closed = true
		close(d.events)
	}
}

// abandonAndDrain closes the intake and accounts for every event still in the
// buffer, plus `inHand` the writer had already taken but could not deliver.
func (d *Delivery) abandonAndDrain(inHand int64) {
	d.closeIntake()
	abandoned := inHand
	for range d.events {
		abandoned++
	}
	d.countLoss(&d.abandoned, abandoned)
}

func lossTime(milliseconds int64) *time.Time {
	if milliseconds <= 0 {
		return nil
	}
	at := time.UnixMilli(milliseconds).UTC()
	return &at
}
