//go:build integration

package integration_test

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/coordination"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/usage"
)

// acctKeyspace opens a Valkey client over a keyspace of its own so concurrent
// suites cannot see one another's deliveries.
func acctKeyspace(t *testing.T) (*coordination.Client, string, string) {
	t.Helper()
	valkey := acctValkey(t)
	prefix := "olp-test:" + rand.Text() + ":"
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		reply, err := valkey.Do(ctx, "KEYS", prefix+"*")
		if err != nil {
			return
		}
		keys, ok := reply.([]any)
		if !ok {
			return
		}
		for _, key := range keys {
			name, ok := key.(string)
			if !ok {
				continue
			}
			if _, err := valkey.Do(ctx, "DEL", name); err != nil {
				t.Logf("removing test key %s: %v", name, err)
			}
		}
	})
	return valkey, prefix, usage.StreamName(prefix)
}

// acctValkey opens one Valkey client. Each consumer is given its own, because
// a blocking group read occupies the connection it is issued on for as long as
// it waits and replicas do not share one.
func acctValkey(t *testing.T) *coordination.Client {
	t.Helper()
	return client(t, required(t, "OLP_TEST_VALKEY_URL"), 3*time.Second)
}

// acctConsumer runs one consumer until the returned stop is called, which is
// also registered as cleanup so a failing assertion never leaks the goroutine.
func acctConsumer(t *testing.T, pool *pgxpool.Pool, valkey *coordination.Client,
	stream, name string, limiter *limits.Limiter, reclaimIdle time.Duration) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	go func() {
		done <- usage.RunConsumerWithReclaimIdle(ctx, pool, valkey, stream, name,
			limiter, log, reclaimIdle)
	}()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("consumer %s: %v", name, err)
			}
		case <-time.After(15 * time.Second):
			t.Errorf("consumer %s did not stop", name)
		}
	}
	t.Cleanup(stop)
	return stop
}

// Acknowledged deliveries are deleted, so a drained group empties the stream.

// Reconstructed spend is pushed back into the distributed counters.

// A worker takes the delivery and dies before acknowledging it.

// A delivery younger than the idle threshold belongs to whoever holds it,
// even when that owner is slow, so an impatient replica must leave it alone.

// The reclaim counter is written before the batch is processed and the
// remaining counters only once it has been, so an acknowledged delivery
// does not yet prove its work was accounted for.

// PostgreSQL always commits before the acknowledgement, so a crash in
// between redelivers an event that is already accounted for.

// Every delivery is in the stream before the consumer starts, so they are
// read as one page. The stream is at least once, so the page can carry the
// same event twice, and the second copy must see the first although
// neither has committed yet.

// Valkey came back without the stream, which took the consumer group with
// it: a flush, an eviction, or a restart with no persistence. Every group
// command now answers NOGROUP, and the writer's next event recreates the
// key without recreating the group, so a consumer that only retries would
// stop accounting for this installation until an operator restarted it.

// The consumer keeps its own identity across the loss, so its health is
// published again rather than left stale at the moment of the outage.
