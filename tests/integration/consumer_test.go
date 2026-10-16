//go:build integration

package integration_test

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
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

// acctPublish appends one payload to the stream exactly as the writer does.
func acctPublish(t *testing.T, valkey *coordination.Client, stream string, payload []byte) string {
	t.Helper()
	return fmt.Sprint(do(t, valkey, "XADD", stream, "*", "event", string(payload)))
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

func acctStreamLength(t *testing.T, valkey *coordination.Client, stream string) int64 {
	t.Helper()
	length, ok := do(t, valkey, "XLEN", stream).(int64)
	if !ok {
		t.Fatal("XLEN did not return a count")
	}
	return length
}

// acctCounters reads the durable consumer counters.
func acctCounters(t *testing.T, pool *pgxpool.Pool) (reclaimed, recovered, duplicates, processed int64) {
	t.Helper()
	err := pool.QueryRow(t.Context(), `SELECT request_metadata_reclaimed_total,
            request_metadata_recovered_total, request_metadata_duplicates_total,
            request_metadata_processed_total
        FROM olp.async_worker_counters WHERE singleton`).
		Scan(&reclaimed, &recovered, &duplicates, &processed)
	if err != nil {
		t.Fatalf("load counters: %v", err)
	}
	return reclaimed, recovered, duplicates, processed
}

// acctGapCount counts the gaps recorded for one reason.
func acctGapCount(t *testing.T, fixture acctFixture, reason string) int64 {
	t.Helper()
	return acctCount(t, fixture, `SELECT count(*) FROM olp.request_metadata_ingestion_gaps
        WHERE reason = $1`, reason)
}

// acctBillableEvent is an event that prices to a known charge.
func acctBillableEvent(t *testing.T, fixture acctFixture) *usage.Event {
	t.Helper()
	return acctEvent(t, fixture, acctEventOptions{Attempts: []usage.Attempt{
		acctAttempt(t, fixture.Provider, 1, "gpt-4o", 200, acctObserved(1000, 500, nil, nil)),
	}})
}

func acctPriceGPT4o(t *testing.T, fixture acctFixture) {
	t.Helper()
	acctPricing(t, fixture, 1, time.Now().UTC().Add(-time.Hour),
		acctPrice{Kind: "openai", VendorID: acctPtr("openai"), Model: "gpt-4o",
			Operation: "generation", Input: acctPtr("3"), Output: acctPtr("15")})
}

// Acknowledged deliveries are deleted, so a drained group empties the stream.

// Reconstructed spend is pushed back into the distributed counters.

// A worker takes the delivery and dies before acknowledging it.

// A delivery younger than the idle threshold belongs to whoever holds it,
// even when that owner is slow, so an impatient replica must leave it alone.

// The reclaim counter is written before the batch is processed and the
// remaining counters only once it has been, so an acknowledged delivery
// does not yet prove its work was accounted for.

func TestRequestMetadataConsumerRecordsUnusableDeliveriesAsGaps(t *testing.T) {
	t.Parallel()
	fixture := acctSeed(t, acctPool(t))
	acctPriceGPT4o(t, fixture)
	valkey, _, stream := acctKeyspace(t)

	// A payload that is not an event at all.
	acctPublish(t, valkey, stream, []byte("{not json"))
	// A future envelope must remain pending for a compatible consumer; a
	// payload without a version is permanently malformed.
	const futurePayload = `{"version":99,"event_id":"unknown"}`
	futureID := acctPublish(t, valkey, stream, []byte(futurePayload))
	versioned, err := usage.Encode(acctBillableEvent(t, fixture))
	if err != nil {
		t.Fatalf("encode unversioned event: %v", err)
	}
	unversioned := strings.Replace(string(versioned), `"version":1,`, "", 1)
	if unversioned == string(versioned) {
		t.Fatal("encoded event carries no version to remove")
	}
	acctPublish(t, valkey, stream, []byte(unversioned))
	// An entry carrying no payload, and the marker the reclaim writes when
	// Valkey reports a pending delivery whose entry has been destroyed.
	do(t, valkey, "XADD", stream, "*", "junk", "x")
	do(t, valkey, "XADD", stream, "*", "deleted_pending_id", "5-0")
	// An event that decodes but contradicts the accounting contract.
	invalid := acctBillableEvent(t, fixture)
	invalid.Attempts[0].Ordinal = 2
	invalidPayload, err := usage.Encode(invalid)
	if err != nil {
		t.Fatalf("encode invalid event: %v", err)
	}
	acctPublish(t, valkey, stream, invalidPayload)
	// A delivery that must still be accounted for despite its neighbours.
	good := acctBillableEvent(t, fixture)
	goodPayload, err := usage.Encode(good)
	if err != nil {
		t.Fatalf("encode event: %v", err)
	}
	acctPublish(t, valkey, stream, goodPayload)

	acctConsumer(t, fixture.Pool, valkey, stream, "recorder", nil, usage.ReclaimIdle)
	eventually(t, "the good delivery to become usage", 30*time.Second, func() bool {
		return acctCount(t, fixture, `SELECT count(*) FROM olp.attempt_usage_facts`) == 1
	})
	eventually(t, "only the unsupported version to remain", 30*time.Second, func() bool {
		return acctStreamLength(t, valkey, stream) == 1
	})
	if payloads := fleetPayloads(t, valkey, stream); len(payloads) != 1 || payloads[0] != futurePayload {
		t.Fatalf("future accounting evidence changed: %v", payloads)
	}
	if pending := do(t, valkey, "XPENDING", stream, usage.Group); !strings.Contains(fmt.Sprint(pending), futureID) {
		t.Fatalf("unsupported version was acknowledged: %v", pending)
	}

	if count := acctGapCount(t, fixture, "malformed_stream_event"); count != 2 {
		t.Fatalf("malformed gaps = %d, want two (future versions are retained)", count)
	}
	// One for the entry without a payload, one for the destroyed entry the
	// reclaim marker named.
	if count := acctGapCount(t, fixture, "missing_stream_event"); count != 2 {
		t.Fatalf("missing gaps = %d, want two", count)
	}
	if count := acctGapCount(t, fixture, "invalid_request_metadata_event"); count != 1 {
		t.Fatalf("invalid event gaps = %d, want one", count)
	}
	if count := acctCount(t, fixture, `SELECT count(*) FROM olp.request_metadata_ingestion_gaps
        WHERE event_count <> 1 OR certainty <> 'exact'`); count != 0 {
		t.Fatalf("%d gaps did not count exactly one lost event", count)
	}
}

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
