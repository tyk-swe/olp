//go:build integration

package integration_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/usage"
)

// TestM4SharedValkeyIsolatesInstallations proves that two installations may be
// given the identical Valkey and stay strangers: the namespace each derives from
// its own identity keeps their streams, their consumer groups and their
// admission counters apart, so one installation's traffic can neither be
// accounted for by the other nor spend the other's budget.
func TestM4SharedValkeyIsolatesInstallations(t *testing.T) {
	// Both installations read OLP_TEST_VALKEY_URL, so they share one Valkey by
	// construction; what they must not share is anything inside it.
	first := m4Provisioned(t)
	second := m4Installation(t)
	if first.prefix == second.prefix {
		t.Fatalf("two installations derived the same Valkey namespace %q", first.prefix)
	}
	if first.stream == second.stream {
		t.Fatalf("two installations derived the same metadata stream %q", first.stream)
	}
	_, secret := first.key("isolated", nil)

	// Both recovery planes run before any traffic: the second installation's
	// consumer is joined to its own group and reading, which is the only state
	// in which "it never took the first installation's deliveries" means
	// anything.
	// Every task checkpoints once as it starts and then on its own interval, so
	// the instant the planes are measured against is taken before they exist.
	started := time.Now().UTC()
	firstWorker := first.worker("m4-isolation-first-worker")
	secondWorker := second.worker("m4-isolation-second-worker")
	m4AwaitWorkerPlane(t, first, started)
	m4AwaitWorkerPlane(t, second, started)
	replica := first.replica("m4-isolation-first-gateway")

	const requests = 3
	for range requests {
		if status, code, _ := m4Chat(t, replica.PublicOrigin, secret); status != 200 {
			t.Fatalf("inference: %d %s", status, code)
		}
	}
	m4Eventually(t, "the first installation to account for its own traffic", 60*time.Second,
		func() bool {
			return first.count("SELECT count(*) FROM olp_go.attempt_usage_facts") == requests
		})

	// The second installation saw none of it: no rows, and no delivery its
	// consumer could have processed.
	for _, table := range []string{"requests", "attempts", "attempt_usage_facts",
		"request_metadata_event_receipts"} {
		if total := second.count("SELECT count(*) FROM olp_go." + table); total != 0 {
			t.Fatalf("installation B holds %d rows in %s that installation A produced", total, table)
		}
	}
	if processed, _, _ := m4Counters(t, second.h.Pool); processed != 0 {
		t.Fatalf("installation B's consumer processed %d events, all of them A's", processed)
	}
	if processed, _, _ := m4Counters(t, first.h.Pool); processed != requests {
		t.Fatalf("installation A's consumer processed %d events, want %d", processed, requests)
	}

	// Each group is served by exactly the consumer of its own installation, so
	// no pending entry of one was ever owned or acknowledged by the other.
	firstConsumers := m4Consumers(t, first.valkey, first.stream)
	secondConsumers := m4Consumers(t, second.valkey, second.stream)
	if len(firstConsumers) != 1 || len(secondConsumers) != 1 {
		t.Fatalf("consumers: A %v, B %v; each group must be served by its own worker",
			firstConsumers, secondConsumers)
	}
	if firstConsumers[0] == secondConsumers[0] {
		t.Fatalf("both installations are served by the same consumer %q", firstConsumers[0])
	}

	// Every key either installation created carries its own namespace, which is
	// what makes the identical lookup identifier below two separate counters.
	firstKeys := m4Keys(t, first.valkey, first.prefix)
	secondKeys := m4Keys(t, second.valkey, second.prefix)
	if len(firstKeys) == 0 || len(secondKeys) == 0 {
		t.Fatalf("namespaced keys: A %v, B %v", firstKeys, secondKeys)
	}
	for _, key := range firstKeys {
		if strings.HasPrefix(key, second.prefix) {
			t.Fatalf("installation A wrote %q into installation B's namespace", key)
		}
	}
	for _, key := range secondKeys {
		if strings.HasPrefix(key, first.prefix) {
			t.Fatalf("installation B wrote %q into installation A's namespace", key)
		}
	}
	if !slices.Contains(firstKeys, first.stream) {
		t.Fatalf("installation A's stream %q is not among its keys %v", first.stream, firstKeys)
	}
	if !slices.Contains(secondKeys, second.stream) {
		t.Fatalf("installation B's stream %q is not among its keys %v", second.stream, secondKeys)
	}

	// The same lookup identifier in two namespaces is two exhausted windows,
	// not one: each installation spends and refuses on its own state.
	firstLimiter := limLimiter(t, first.valkey, first.namespace)
	secondLimiter := limLimiter(t, second.valkey, second.namespace)
	lookup := "identical_lookup_01"
	request := limits.Request{CostOwnerID: limNilUUID, LookupID: lookup,
		RequestsPerMinute: limPointer(int64(1)), TokensPerMinute: limPointer(int64(10)),
		MaxConcurrency: limPointer(int64(1)), RequestedTokens: 10, LeaseTTL: 30 * time.Second}
	if _, err := firstLimiter.Reserve(t.Context(), request); err != nil {
		t.Fatalf("installation A's first reservation: %v", err)
	}
	if _, err := secondLimiter.Reserve(t.Context(), request); err != nil {
		t.Fatalf("installation A's reservation contaminated installation B: %v", err)
	}
	for name, limiter := range map[string]*limits.Limiter{"A": firstLimiter, "B": secondLimiter} {
		var exceeded *limits.ExceededError
		if _, err := limiter.Reserve(t.Context(), request); !errors.As(err, &exceeded) {
			t.Fatalf("installation %s did not enforce its own exhausted window: %v", name, err)
		}
	}

	// Stopping one installation's fleet leaves the other's durable state alone:
	// shutdown removes what that process owns, never the shared server's keys.
	if err := replica.Stop(15 * time.Second); err != nil {
		t.Fatalf("stop installation A's gateway: %v", err)
	}
	if err := firstWorker.Stop(15 * time.Second); err != nil {
		t.Fatalf("stop installation A's worker: %v", err)
	}
	for _, key := range secondKeys {
		if exists := do(t, second.valkey, "EXISTS", key); exists != int64(1) {
			t.Fatalf("installation A's shutdown removed installation B's key %q (EXISTS %v)",
				key, exists)
		}
	}
	if total := second.count("SELECT count(*) FROM olp_go.attempt_usage_facts"); total != 0 {
		t.Fatalf("installation B gained %d usage facts from A's shutdown", total)
	}
	if err := secondWorker.Stop(15 * time.Second); err != nil {
		t.Fatalf("stop installation B's worker: %v", err)
	}
}

// m4AwaitWorkerPlane waits until every worker task of one installation has
// checkpointed since the given instant, which is what proves the plane started
// by this scenario is the one running.
func m4AwaitWorkerPlane(t *testing.T, in *m4Install, since time.Time) {
	t.Helper()
	tasks := []usage.Task{usage.TaskRequestMetadataConsumer, usage.TaskMaintenance,
		usage.TaskCostReconciliation, usage.TaskEpochDetection}
	m4Eventually(t, "the worker plane to check in", 40*time.Second, func() bool {
		for _, task := range tasks {
			if in.count(`SELECT count(*) FROM olp_go.worker_task_health
                WHERE task = $1 AND checked_at >= $2`, string(task), since) == 0 {
				return false
			}
		}
		return true
	})
}
