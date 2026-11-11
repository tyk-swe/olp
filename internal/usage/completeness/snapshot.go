package completeness

import (
	"time"
)

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

type SnapshotSource interface {
	Snapshot() Snapshot
}
