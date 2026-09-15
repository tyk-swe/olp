package usage

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// ErrFull says the bounded buffer had no room for the event. The request that
// produced it has already been served; the loss is counted and reported rather
// than slowing the response path down to wait for Valkey.
var ErrFull = errors.New("the bounded request metadata buffer is full")

// ErrClosed says the stream writer has stopped, so nothing more can be
// accepted. Like ErrFull the event is counted as lost.
var ErrClosed = errors.New("the request metadata stream writer is not running")

// Emitter is the bounded handoff between a finished request and durable
// delivery. Emitting never blocks and never fails a request: an overflow is
// counted, timestamped, and later reported as a gap, because accounting that
// silently forgets what it dropped is worse than accounting that admits it.
type Emitter struct {
	events chan Event

	// intake guards the transition from open to closed. A reader is one
	// in-flight Emit; the writer closes the channel, so the lock is what makes
	// "send on a closed channel" impossible without making Emit block on a
	// slow drain.
	intake sync.RWMutex
	closed bool

	processEpoch string
	startedAt    time.Time

	accepted    atomic.Int64
	persisted   atomic.Int64
	dropped     atomic.Int64
	abandoned   atomic.Int64
	retrying    atomic.Bool
	unclean     atomic.Bool
	firstLossMS atomic.Int64
	lastLossMS  atomic.Int64
}

// NewEmitter creates the buffer with room for `capacity` pending events. One
// process keeps one emitter: its process epoch identifies this run's counters
// so a restart cannot be mistaken for a reset.
func NewEmitter(capacity int) *Emitter {
	if capacity < 1 {
		capacity = 1
	}
	return &Emitter{
		events:       make(chan Event, capacity),
		processEpoch: uuid.Must(uuid.NewV7()).String(),
		startedAt:    time.Now().UTC(),
	}
}

// Emit hands one event to the writer without blocking. The caller is on the
// inference path: it learns the event was lost, and continues.
func (e *Emitter) Emit(ev Event) error {
	e.intake.RLock()
	defer e.intake.RUnlock()
	if e.closed {
		e.recordLoss(&e.dropped, 1)
		return ErrClosed
	}
	select {
	case e.events <- ev:
		// Counted after the handoff: the writer may already have persisted this
		// event, which Snapshot reconciles with its lower bound rather than
		// reporting an impossible negative backlog.
		e.accepted.Add(1)
		return nil
	default:
		e.recordLoss(&e.dropped, 1)
		return ErrFull
	}
}

// Close stops intake without discarding queued events. RunWriter drains them;
// cancelling its context is reserved for an expired shutdown budget.
func (e *Emitter) Close() { e.closeIntake() }

// MarkUnclean keeps the epoch open when HTTP handlers exceeded their drain
// budget: some may still emit after the writer stops accepting events.
func (e *Emitter) MarkUnclean() { e.unclean.Store(true) }

// Drop records an accountable event rejected before it could enter the buffer.
func (e *Emitter) Drop() { e.recordLoss(&e.dropped, 1) }

// Snapshot is the emitter's accounting at one instant: what was accepted, what
// reached the stream, and what was lost on the way.
type Snapshot struct {
	// ProcessEpoch distinguishes this process's counters from those of the
	// process that ran before it under the same gateway instance label.
	ProcessEpoch string
	StartedAt    time.Time
	Accepted     int64
	Persisted    int64
	// Dropped are events refused before entering the buffer.
	Dropped int64
	// Abandoned are events that entered the buffer but never reached Valkey.
	Abandoned int64
	Retrying  bool
	Unclean   bool
	// Closed says the writer has stopped accepting events.
	Closed      bool
	FirstLossAt *time.Time
	LastLossAt  *time.Time
}

// Pending is the backlog: accepted work that is neither delivered nor lost.
func (s Snapshot) Pending() int64 {
	return max(s.Accepted-s.Persisted-s.Abandoned, 0)
}

// Lost is everything this process could not deliver.
func (s Snapshot) Lost() int64 { return s.Dropped + s.Abandoned }

// Complete says every accepted event reached the stream and delivery is not
// currently degraded. A pending backlog is not loss, but a retry is a warning.
func (s Snapshot) Complete() bool {
	return s.Dropped == 0 && s.Abandoned == 0 && !s.Retrying && !s.Closed && !s.Unclean
}

// GracefullyDrained requires both a drained writer and a clean HTTP shutdown;
// otherwise late terminal events could be missing from a supposedly closed epoch.
func (s Snapshot) GracefullyDrained() bool { return s.Closed && s.Pending() == 0 && !s.Unclean }

// Snapshot reads the counters. Persisted and abandoned are lower bounds on
// accepted (Emit counts an event after handing it over), so accepted is raised
// to that bound rather than letting a checkpoint record a negative backlog.
func (e *Emitter) Snapshot() Snapshot {
	persisted := e.persisted.Load()
	abandoned := e.abandoned.Load()
	accepted := max(e.accepted.Load(), persisted+abandoned)
	e.intake.RLock()
	closed := e.closed
	e.intake.RUnlock()
	return Snapshot{
		ProcessEpoch: e.processEpoch,
		StartedAt:    e.startedAt,
		Accepted:     accepted,
		Persisted:    persisted,
		Dropped:      e.dropped.Load(),
		Abandoned:    abandoned,
		Retrying:     e.retrying.Load(),
		Unclean:      e.unclean.Load(),
		Closed:       closed,
		FirstLossAt:  lossTime(e.firstLossMS.Load()),
		LastLossAt:   lossTime(e.lastLossMS.Load()),
	}
}

func (e *Emitter) recordLoss(counter *atomic.Int64, count int64) {
	counter.Add(count)
	if count <= 0 {
		return
	}
	now := time.Now().UnixMilli()
	e.firstLossMS.CompareAndSwap(0, now)
	e.lastLossMS.Store(now)
}

// closeIntake stops new events. It is idempotent so a writer that stops twice
// (shutdown racing a fatal error) cannot close the channel twice.
func (e *Emitter) closeIntake() {
	e.intake.Lock()
	defer e.intake.Unlock()
	if !e.closed {
		e.closed = true
		close(e.events)
	}
}

// abandonAndDrain closes the intake and accounts for every event still in the
// buffer, plus `inHand` the writer had already taken but could not deliver.
func (e *Emitter) abandonAndDrain(inHand int64) {
	e.closeIntake()
	abandoned := inHand
	for range e.events {
		abandoned++
	}
	e.recordLoss(&e.abandoned, abandoned)
}

func lossTime(milliseconds int64) *time.Time {
	if milliseconds <= 0 {
		return nil
	}
	at := time.UnixMilli(milliseconds).UTC()
	return &at
}
