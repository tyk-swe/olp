package usage

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// consumerRow replays one stored health row into the scan targets
// ReadConsumerStatus uses, so the classification can be tested without a
// database.
type consumerRow struct {
	values []any
	err    error
}

func (r consumerRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != len(r.values) {
		return errors.New("unexpected scan arity")
	}
	for i, target := range dest {
		switch typed := target.(type) {
		case *int64:
			typed2, ok := r.values[i].(int64)
			if !ok {
				return errors.New("unexpected scan type")
			}
			*typed = typed2
		case **time.Time:
			at, ok := r.values[i].(*time.Time)
			if !ok && r.values[i] != nil {
				return errors.New("unexpected scan type")
			}
			*typed = at
		case *time.Time:
			at, ok := r.values[i].(time.Time)
			if !ok {
				return errors.New("unexpected scan type")
			}
			*typed = at
		default:
			return errors.New("unexpected scan target")
		}
	}
	return nil
}

type consumerQueryer struct{ row pgx.Row }

func (c consumerQueryer) QueryRow(context.Context, string, ...any) pgx.Row { return c.row }
func (c consumerQueryer) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("unused")
}

func TestConsumerStatusClassification(t *testing.T) {
	now := time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name          string
		pending, lag  int64
		age           time.Duration
		state         string
		heartbeatSecs int64
	}{
		{"idle is healthy", 0, 0, 2 * time.Second, ConsumerHealthy, 2},
		{"pending work is a backlog", 3, 0, time.Second, ConsumerBacklogged, 1},
		{"lag alone is a backlog", 0, 7, time.Second, ConsumerBacklogged, 1},
		{"the staleness boundary is inclusive", 0, 0, ConsumerStaleAfter, ConsumerHealthy, 20},
		{"a late checkpoint is stale", 0, 0, ConsumerStaleAfter + time.Second, ConsumerStale, 21},
		{"a backlog older than the window is stale", 9, 0, time.Minute, ConsumerStale, 60},
		{"a checkpoint from the future never ages backwards", 0, 0, -time.Hour, ConsumerHealthy, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			checked := now.Add(-c.age)
			oldest := checked.Add(-time.Second)
			q := consumerQueryer{row: consumerRow{values: []any{c.pending, c.lag, &oldest, checked}}}
			status, err := ReadConsumerStatus(context.Background(), q, now)
			if err != nil {
				t.Fatalf("read consumer status: %v", err)
			}
			if status.State != c.state {
				t.Fatalf("state = %q, want %q", status.State, c.state)
			}
			if status.PendingEvents != c.pending || status.LagEvents != c.lag {
				t.Fatalf("counters = %d/%d, want %d/%d", status.PendingEvents, status.LagEvents, c.pending, c.lag)
			}
			if status.HeartbeatAgeSeconds == nil || *status.HeartbeatAgeSeconds != c.heartbeatSecs {
				t.Fatalf("heartbeat age = %v, want %d", status.HeartbeatAgeSeconds, c.heartbeatSecs)
			}
			if status.CheckedAt == nil || !status.CheckedAt.Equal(checked) {
				t.Fatalf("checked at = %v, want %v", status.CheckedAt, checked)
			}
			if status.Complete() != (c.state == ConsumerHealthy) {
				t.Fatalf("complete = %v for state %q", status.Complete(), c.state)
			}
		})
	}
}

func TestConsumerStatusWithoutACheckpointIsUnknown(t *testing.T) {
	q := consumerQueryer{row: consumerRow{err: pgx.ErrNoRows}}
	status, err := ReadConsumerStatus(context.Background(), q, time.Now())
	if err != nil {
		t.Fatalf("read consumer status: %v", err)
	}
	if status.State != ConsumerUnknown || status.Complete() {
		t.Fatalf("status = %+v, want an incomplete unknown status", status)
	}
	if status.CheckedAt != nil || status.HeartbeatAgeSeconds != nil || status.OldestPendingAt != nil {
		t.Fatalf("status = %+v, want no timestamps", status)
	}
}

func TestConsumerStatusFailsClosedOnNegativeCounters(t *testing.T) {
	now := time.Now()
	q := consumerQueryer{row: consumerRow{values: []any{int64(-1), int64(0), (*time.Time)(nil), now}}}
	if _, err := ReadConsumerStatus(context.Background(), q, now); err == nil {
		t.Fatal("negative pending count was accepted")
	}
}

func TestGatewayEpochStateClassification(t *testing.T) {
	at := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name                           string
		closed, detected, acknowledged *time.Time
		want                           string
	}{
		{"a checkpointing epoch is open", nil, nil, nil, EpochOpen},
		{"a drained epoch is closed", &at, nil, nil, EpochGracefullyClosed},
		{"a closed epoch stays closed", &at, nil, &at, EpochGracefullyClosed},
		{"a detected epoch is unresolved", nil, &at, nil, EpochUnresolved},
		{"a seen epoch is acknowledged", nil, &at, &at, EpochAcknowledged},
		{"an acknowledgement without detection resolves nothing", nil, nil, &at, EpochOpen},
	} {
		t.Run(c.name, func(t *testing.T) {
			epoch := GatewayEpoch{GracefullyClosedAt: c.closed, StaleDetectedAt: c.detected,
				AcknowledgedAt: c.acknowledged}
			if state := epochState(epoch); state != c.want {
				t.Fatalf("state = %q, want %q", state, c.want)
			}
		})
	}
}
