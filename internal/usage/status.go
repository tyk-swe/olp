package usage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
)

// Consumer states reported to the console. They are the only values the
// request_metadata_consumer field of a usage report can carry.
const (
	// ConsumerUnknown means no consumer has ever checkpointed: ingestion may
	// never have run, so nothing can be concluded about completeness.
	ConsumerUnknown = "unknown"
	// ConsumerHealthy means a recent checkpoint found nothing outstanding.
	ConsumerHealthy = "healthy"
	// ConsumerBacklogged means the consumer is alive with work still pending.
	ConsumerBacklogged = "backlogged"
	// ConsumerStale means the last checkpoint is too old to be evidence that
	// ingestion is still running.
	ConsumerStale = "stale"
)

// ConsumerStatus is the request metadata consumer's last self-report, aged
// against the reader's clock. Reports carry it so a total is never presented as
// final while the pipeline that feeds it is behind or silent.
type ConsumerStatus struct {
	State               string     `json:"state"`
	PendingEvents       int64      `json:"pending_events"`
	LagEvents           int64      `json:"lag_events"`
	OldestPendingAt     *time.Time `json:"oldest_pending_at"`
	CheckedAt           *time.Time `json:"checked_at"`
	HeartbeatAgeSeconds *int64     `json:"heartbeat_age_seconds"`
}

// Complete reports whether the consumer is evidence that ingestion is current.
// Only a healthy consumer is: a backlogged one still owes events, and a stale
// or unknown one cannot vouch for anything.
func (c ConsumerStatus) Complete() bool { return c.State == ConsumerHealthy }

const consumerHealthSQL = `SELECT pending_events, lag_events, oldest_pending_at, checked_at
    FROM olp_go.request_metadata_consumer_health WHERE singleton`

// ReadConsumerStatus loads the consumer health row and classifies it against
// `now`. A missing row is "unknown" rather than an error: an installation that
// has never ingested is a legitimate state, and it is reported honestly instead
// of as a healthy pipeline.
func ReadConsumerStatus(ctx context.Context, q access.Queryer, now time.Time) (ConsumerStatus, error) {
	var pending, lag int64
	var oldest *time.Time
	var checked time.Time
	err := q.QueryRow(ctx, consumerHealthSQL).Scan(&pending, &lag, &oldest, &checked)
	if errors.Is(err, pgx.ErrNoRows) {
		return ConsumerStatus{State: ConsumerUnknown}, nil
	}
	if err != nil {
		return ConsumerStatus{}, fmt.Errorf("read request metadata consumer health: %w", err)
	}
	if pending < 0 || lag < 0 {
		return ConsumerStatus{}, errors.New("stored request metadata consumer health is invalid")
	}
	age := max(int64(now.Sub(checked)/time.Second), 0)
	// The comparison stays in seconds: converting a very old heartbeat back to a
	// Duration could overflow and make a silent consumer look healthy.
	state := ConsumerHealthy
	switch {
	case age > int64(ConsumerStaleAfter/time.Second):
		state = ConsumerStale
	case pending > 0 || lag > 0:
		state = ConsumerBacklogged
	}
	checkedAt := checked.UTC()
	if oldest != nil {
		utc := oldest.UTC()
		oldest = &utc
	}
	return ConsumerStatus{
		State:               state,
		PendingEvents:       pending,
		LagEvents:           lag,
		OldestPendingAt:     oldest,
		CheckedAt:           &checkedAt,
		HeartbeatAgeSeconds: &age,
	}, nil
}
