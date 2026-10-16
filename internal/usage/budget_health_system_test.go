//go:build integration

package usage

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Production registers this zero-counter epoch before binding listeners.
// Simulate a crash whose immediate loss write and next heartbeat never landed.

// Do not call DetectStaleEpochs: admission must protect budgets even without
// a running recovery worker and even though no durable event counter advanced.

// An epoch whose stream writes are still retrying keeps checkpointing
// while terminal events sit in the producer's buffer: the reservation
// grace cannot outlast that backlog, so admission stays closed.

// A live consumer reporting entries pending past the stale window means
// durable usage is not reaching accounting; a version this build cannot
// decode stays pending until a compatible consumer processes it.

// A fresh pending entry is ordinary in-flight work, not lost usage.

// Stranded undelivered entries trail durable accounting the same way:
// reported usage stays behind the stream while producers keep writing.

// Fresh undelivered entries are ordinary in-flight work.

// A stale heartbeat cannot speak for the group: a dead consumer leaves
// deliveries pending while producers keep writing, so admission stays
// closed until liveness is proven again.

// A missing heartbeat cannot either: the consumer may never have run.

// A fresh healthy heartbeat reopens admission.

// liveConsumerHeartbeat seeds the single consumer health row the way a live
// consumer's five-second checkpoint does, since a missing or stale heartbeat
// closes cost-budget admission.
func liveConsumerHeartbeat(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.request_metadata_consumer_health
		(singleton, pending_events, lag_events, oldest_pending_at, checked_at) VALUES(true,0,0,NULL,now())
		ON CONFLICT (singleton) DO UPDATE SET pending_events=0, lag_events=0,
			oldest_pending_at=NULL, oldest_lagged_at=NULL, checked_at=now()`); err != nil {
		t.Fatal(err)
	}
}
