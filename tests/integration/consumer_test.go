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
	prefix := "olp-go-test:" + rand.Text() + ":"
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

// acctEventually waits for a condition the consumer reaches asynchronously.
func acctEventually(t *testing.T, what string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
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
        FROM olp_go.async_worker_counters WHERE singleton`).
		Scan(&reclaimed, &recovered, &duplicates, &processed)
	if err != nil {
		t.Fatalf("load counters: %v", err)
	}
	return reclaimed, recovered, duplicates, processed
}

// acctGapCount counts the gaps recorded for one reason.
func acctGapCount(t *testing.T, fixture acctFixture, reason string) int64 {
	t.Helper()
	return acctCount(t, fixture, `SELECT count(*) FROM olp_go.request_metadata_ingestion_gaps
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

func TestRequestMetadataConsumerDrainsTheGroupAcrossWorkers(t *testing.T) {
	t.Parallel()
	fixture := acctSeed(t, acctPool(t))
	acctPriceGPT4o(t, fixture)
	valkey, prefix, stream := acctKeyspace(t)
	limiter, err := limits.New(valkey, prefix+"limits")
	if err != nil {
		t.Fatalf("open limiter: %v", err)
	}

	const deliveries = 6
	for range deliveries {
		event := acctBillableEvent(t, fixture)
		payload, err := usage.Encode(event)
		if err != nil {
			t.Fatalf("encode event: %v", err)
		}
		acctPublish(t, valkey, stream, payload)
	}

	for worker := range 3 {
		acctConsumer(t, fixture.Pool, acctValkey(t), stream,
			fmt.Sprintf("worker-%d", worker), limiter, usage.ReclaimIdle)
	}

	acctEventually(t, "every delivery to become durable usage", func() bool {
		return acctCount(t, fixture, `SELECT count(*) FROM olp_go.attempt_usage_facts`) == deliveries
	})
	// Acknowledged deliveries are deleted, so a drained group empties the stream.
	acctEventually(t, "the stream to drain", func() bool {
		return acctStreamLength(t, valkey, stream) == 0
	})
	acctEventually(t, "the consumers to publish their health", func() bool {
		return acctCount(t, fixture, `SELECT count(*) FROM olp_go.request_metadata_consumer_health
            WHERE singleton AND pending_events = 0`) == 1
	})

	acctEventually(t, "every delivery to be counted", func() bool {
		_, _, duplicates, processed := acctCounters(t, fixture.Pool)
		if duplicates != 0 {
			t.Fatalf("duplicates = %d, want none: each delivery was handled once", duplicates)
		}
		return processed == deliveries
	})
	// Reconstructed spend is pushed back into the distributed counters.
	keys, err := valkey.Do(t.Context(), "KEYS", prefix+"limits*")
	if err != nil {
		t.Fatalf("list limit keys: %v", err)
	}
	if !strings.Contains(fmt.Sprint(keys), "cost") {
		t.Fatalf("limit keys = %v, want the reconstructed spend", keys)
	}
	daily, _ := acctWindow(t, fixture, "day")
	acctSameMoney(t, fixture, &daily, "0.063")
}

func TestRequestMetadataConsumerReclaimsADeadOwnersDeliveries(t *testing.T) {
	t.Parallel()
	fixture := acctSeed(t, acctPool(t))
	acctPriceGPT4o(t, fixture)
	valkey, _, stream := acctKeyspace(t)

	event := acctBillableEvent(t, fixture)
	payload, err := usage.Encode(event)
	if err != nil {
		t.Fatalf("encode event: %v", err)
	}
	acctPublish(t, valkey, stream, payload)

	// A worker takes the delivery and dies before acknowledging it.
	do(t, valkey, "XGROUP", "CREATE", stream, usage.Group, "0", "MKSTREAM")
	do(t, valkey, "XREADGROUP", "GROUP", usage.Group, "dead-worker", "COUNT", "1",
		"STREAMS", stream, ">")
	if pending := acctStreamLength(t, valkey, stream); pending != 1 {
		t.Fatalf("stream length = %d, want the unacknowledged delivery", pending)
	}

	// A delivery younger than the idle threshold belongs to whoever holds it,
	// even when that owner is slow, so an impatient replica must leave it alone.
	patient := acctConsumer(t, fixture.Pool, acctValkey(t), stream, "patient", nil, usage.ReclaimIdle)
	time.Sleep(2 * time.Second)
	if count := acctCount(t, fixture, `SELECT count(*) FROM olp_go.attempt_usage_facts`); count != 0 {
		t.Fatalf("facts = %d, want the delivery left with its owner until it goes idle", count)
	}
	patient()

	acctConsumer(t, fixture.Pool, acctValkey(t), stream, "survivor", nil, 0)
	acctEventually(t, "the survivor to reclaim the delivery", func() bool {
		return acctCount(t, fixture, `SELECT count(*) FROM olp_go.attempt_usage_facts`) == 1
	})
	acctEventually(t, "the reclaimed delivery to be acknowledged", func() bool {
		return acctStreamLength(t, valkey, stream) == 0
	})
	// The reclaim counter is written before the batch is processed and the
	// remaining counters only once it has been, so an acknowledged delivery
	// does not yet prove its work was accounted for.
	acctEventually(t, "the reclaimed delivery to be counted", func() bool {
		_, _, _, processed := acctCounters(t, fixture.Pool)
		return processed > 0
	})
	if reclaimed, _, _, processed := acctCounters(t, fixture.Pool); reclaimed < 1 || processed != 1 {
		t.Fatalf("counters = %d reclaimed, %d processed; want a reclaimed delivery", reclaimed, processed)
	}
}

func TestRequestMetadataConsumerRecordsUnusableDeliveriesAsGaps(t *testing.T) {
	t.Parallel()
	fixture := acctSeed(t, acctPool(t))
	acctPriceGPT4o(t, fixture)
	valkey, _, stream := acctKeyspace(t)

	// A payload that is not an event at all.
	acctPublish(t, valkey, stream, []byte("{not json"))
	// Payloads from another envelope version, or without one, are not events
	// this build can account for.
	acctPublish(t, valkey, stream, []byte(`{"version":99,"event_id":"unknown"}`))
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
	acctEventually(t, "the good delivery to become usage", func() bool {
		return acctCount(t, fixture, `SELECT count(*) FROM olp_go.attempt_usage_facts`) == 1
	})
	acctEventually(t, "every unusable delivery to be resolved", func() bool {
		return acctStreamLength(t, valkey, stream) == 0
	})

	if count := acctGapCount(t, fixture, "malformed_stream_event"); count != 3 {
		t.Fatalf("malformed gaps = %d, want three", count)
	}
	// One for the entry without a payload, one for the destroyed entry the
	// reclaim marker named.
	if count := acctGapCount(t, fixture, "missing_stream_event"); count != 2 {
		t.Fatalf("missing gaps = %d, want two", count)
	}
	if count := acctGapCount(t, fixture, "invalid_request_metadata_event"); count != 1 {
		t.Fatalf("invalid event gaps = %d, want one", count)
	}
	if count := acctCount(t, fixture, `SELECT count(*) FROM olp_go.request_metadata_ingestion_gaps
        WHERE event_count <> 1 OR certainty <> 'exact'`); count != 0 {
		t.Fatalf("%d gaps did not count exactly one lost event", count)
	}
}

func TestRequestMetadataConsumerReplaysACommittedEventAsADuplicate(t *testing.T) {
	t.Parallel()
	fixture := acctSeed(t, acctPool(t))
	acctPriceGPT4o(t, fixture)
	valkey, _, stream := acctKeyspace(t)

	// PostgreSQL always commits before the acknowledgement, so a crash in
	// between redelivers an event that is already accounted for.
	event := acctBillableEvent(t, fixture)
	payload, err := usage.Encode(event)
	if err != nil {
		t.Fatalf("encode event: %v", err)
	}
	if result := acctPersist(t, fixture, event); result.Outcome != usage.PersistOutcomePersisted {
		t.Fatalf("outcome = %v, want persisted", result.Outcome)
	}
	acctPublish(t, valkey, stream, payload)

	acctConsumer(t, fixture.Pool, valkey, stream, "replay", nil, usage.ReclaimIdle)
	acctEventually(t, "the redelivery to be acknowledged", func() bool {
		return acctStreamLength(t, valkey, stream) == 0
	})
	if count := acctCount(t, fixture, `SELECT count(*) FROM olp_go.attempt_usage_facts`); count != 1 {
		t.Fatalf("facts = %d, want the redelivery to charge nothing new", count)
	}
	acctEventually(t, "the duplicate to be counted", func() bool {
		_, _, duplicates, _ := acctCounters(t, fixture.Pool)
		return duplicates == 1
	})
	daily, _ := acctWindow(t, fixture, "day")
	acctSameMoney(t, fixture, &daily, "0.0105")
}

func TestRequestMetadataConsumerRebuildsAGroupLostWithItsStream(t *testing.T) {
	t.Parallel()
	fixture := acctSeed(t, acctPool(t))
	acctPriceGPT4o(t, fixture)
	valkey, _, stream := acctKeyspace(t)

	first := acctBillableEvent(t, fixture)
	firstPayload, err := usage.Encode(first)
	if err != nil {
		t.Fatalf("encode event: %v", err)
	}
	acctPublish(t, valkey, stream, firstPayload)

	acctConsumer(t, fixture.Pool, acctValkey(t), stream, "survivor", nil, usage.ReclaimIdle)
	acctEventually(t, "the first delivery to become usage", func() bool {
		return acctCount(t, fixture, `SELECT count(*) FROM olp_go.attempt_usage_facts`) == 1
	})

	// Valkey came back without the stream, which took the consumer group with
	// it: a flush, an eviction, or a restart with no persistence. Every group
	// command now answers NOGROUP, and the writer's next event recreates the
	// key without recreating the group, so a consumer that only retries would
	// stop accounting for this installation until an operator restarted it.
	do(t, valkey, "DEL", stream)
	second := acctBillableEvent(t, fixture)
	secondPayload, err := usage.Encode(second)
	if err != nil {
		t.Fatalf("encode event: %v", err)
	}
	acctPublish(t, valkey, stream, secondPayload)

	acctEventually(t, "the delivery published after the loss to become usage", func() bool {
		return acctCount(t, fixture, `SELECT count(*) FROM olp_go.attempt_usage_facts`) == 2
	})
	acctEventually(t, "the rebuilt group to drain the stream", func() bool {
		return acctStreamLength(t, valkey, stream) == 0
	})
	// The consumer keeps its own identity across the loss, so its health is
	// published again rather than left stale at the moment of the outage.
	acctEventually(t, "the consumer to publish its health against the new group", func() bool {
		return acctCount(t, fixture, `SELECT count(*) FROM olp_go.request_metadata_consumer_health
            WHERE singleton AND pending_events = 0 AND checked_at > now() - interval '10 seconds'`) == 1
	})
}
