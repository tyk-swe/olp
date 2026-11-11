package ingest

import (
	"context"
	"embed"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/coordination"
	"github.com/tyk-swe/olp/internal/limits/admission"
	"github.com/tyk-swe/olp/internal/usage/completeness"
)

//go:embed scripts/*.lua
var scripts embed.FS

var (
	// claimScript reclaims stale deliveries and, before the server forgets
	// them, republishes the identifiers it dropped so their loss is recorded.
	claimScript = mustScript("scripts/claim_request_metadata.lua")
	// ackDeleteScript retires one delivery atomically.
	ackDeleteScript = mustScript("scripts/ack_delete.lua")
)

func mustScript(name string) string {
	source, err := scripts.ReadFile(name)
	if err != nil {
		panic("usage: embedded script " + name + " is missing")
	}
	return string(source)
}

const (
	consumerBlockInterval       = time.Second
	consumerActiveRecoveryBlock = 10 * time.Millisecond
	consumerOwnPendingInterval  = time.Second
	consumerHealthInterval      = 5 * time.Second
	consumerRetryFloor          = 100 * time.Millisecond
	consumerRetryCeiling        = 5 * time.Second
	// consumerResponseMargin is how much longer than its server-side block a
	// read may take before the client stops waiting for the reply.
	consumerResponseMargin = time.Second
	// consumerCommandTimeout bounds the non-blocking commands.
	consumerCommandTimeout = 5 * time.Second
)

type consumerPolicy struct {
	batchSize           int
	blockInterval       time.Duration
	activeRecoveryBlock time.Duration
	ownPendingInterval  time.Duration
	reclaimIdle         time.Duration
	recoveryInterval    time.Duration
	healthInterval      time.Duration
}

func defaultConsumerPolicy() consumerPolicy {
	return consumerPolicy{
		batchSize:           batchSize,
		blockInterval:       consumerBlockInterval,
		activeRecoveryBlock: consumerActiveRecoveryBlock,
		ownPendingInterval:  consumerOwnPendingInterval,
		reclaimIdle:         reclaimIdle,
		recoveryInterval:    completeness.RecoveryInterval,
		healthInterval:      consumerHealthInterval,
	}
}

// Consumer drains the request metadata stream into durable accounting until
// ctx is done. Several processes may run one concurrently against one stream:
// the consumer group hands each delivery to one of them, and a delivery whose
// owner disappears is reclaimed by the survivors.
//
// Name must be stable for one process and unique across live
// ones; use ConsumerName. Limiter may be nil, in which case reconstructed
// spend is not pushed back into the distributed counters and the periodic
// reconciliation pass repairs them instead.
//
// PostgreSQL always commits before the delivery is acknowledged, so a crash in
// between replays the event and the receipt makes it a duplicate. The consumer
// is never deleted on exit: its unacknowledged deliveries must stay reclaimable.
type Consumer struct {
	Pool    *pgxpool.Pool
	Client  *coordination.Client
	Stream  string
	Name    string
	Region  string
	Limiter *admission.Limiter
	Log     *slog.Logger
}

type consumerRun struct {
	pool     *pgxpool.Pool
	client   *coordination.Client
	stream   string
	consumer string
	region   string
	limiter  *admission.Limiter
	log      *slog.Logger
	policy   consumerPolicy
	// groupLost records that a stream command found no consumer group, which
	// no amount of retrying repairs. Only the loop that owns it reads it.
	groupLost bool
}

func (c Consumer) Run(ctx context.Context) error {
	policy := defaultConsumerPolicy()
	if err := validateConsumerConfig(c.Stream, c.Name, policy); err != nil {
		return err
	}
	run := &consumerRun{pool: c.Pool, client: c.Client, stream: c.Stream, consumer: c.Name, region: c.Region,
		limiter: c.Limiter, log: c.Log, policy: policy}

	ownPendingStart, staleStart := "0-0", "0-0"
	now := time.Now()
	ownPendingDue, staleDue, healthDue := now, now, now
	retryDelay := consumerRetryFloor
	created := false
	for {
		if ctx.Err() != nil {
			return nil
		}
		// The group is created on the first pass, and again whenever a command
		// reports it gone: a restart without persistence, a flush, or an
		// evicted key destroys it, and the next write brings the stream back
		// without it, so nothing here recovers until it is created again.
		if run.groupLost {
			created, run.groupLost = false, false
		}
		if !created {
			if err := run.createGroup(ctx); err != nil {
				run.log.Warn("request metadata consumer group is unavailable", "error", err)
				if waitForRetry(ctx, retryDelay) {
					return nil
				}
				retryDelay = min(2*retryDelay, consumerRetryCeiling)
				continue
			}
			created = true
			retryDelay = consumerRetryFloor
		}

		// One bounded page of this consumer's own unfinished deliveries and one
		// bounded stale scan precede every blocking read for new work. A full
		// recovery page shortens, but never removes, that block, so neither
		// source can starve the other.
		recoveryActive, cycleRetry := false, false
		if !time.Now().Before(ownPendingDue) {
			result := run.drainOwnPending(ctx, ownPendingStart)
			ownPendingStart, ownPendingDue = result.cursor, result.due
			if ctx.Err() != nil {
				return nil
			}
			recoveryActive, cycleRetry = recoveryActive || result.more, cycleRetry || result.retry
		}
		if !time.Now().Before(staleDue) {
			result := run.reclaimStale(ctx, staleStart)
			staleStart, staleDue = result.cursor, result.due
			if ctx.Err() != nil {
				return nil
			}
			recoveryActive, cycleRetry = recoveryActive || result.more, cycleRetry || result.retry
		}
		if !time.Now().Before(healthDue) {
			if err := run.checkpointHealth(ctx); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				run.log.Warn("request metadata consumer health was not recorded", "error", err)
				cycleRetry = true
			}
			healthDue = time.Now().Add(run.policy.healthInterval)
		}
		if cycleRetry {
			if waitForRetry(ctx, retryDelay) {
				return nil
			}
			retryDelay = min(2*retryDelay, consumerRetryCeiling)
		} else {
			retryDelay = consumerRetryFloor
		}

		block := run.policy.blockInterval
		if recoveryActive {
			block = run.policy.activeRecoveryBlock
		}
		if stopped := run.settleNew(ctx, &ownPendingStart, &ownPendingDue, block, &retryDelay); stopped {
			return nil
		}
	}
}

func validateConsumerConfig(stream, consumer string, policy consumerPolicy) error {
	switch {
	case strings.TrimSpace(stream) == "":
		return errors.New("request metadata stream name is empty")
	case strings.TrimSpace(consumer) == "":
		return errors.New("request metadata consumer name is empty")
	case policy.batchSize < 1 || policy.batchSize > 1000:
		return errors.New("request metadata batch size is out of range")
	case policy.blockInterval <= 0 || policy.activeRecoveryBlock <= 0 ||
		policy.ownPendingInterval <= 0 || policy.recoveryInterval <= 0 ||
		policy.healthInterval <= 0 || policy.reclaimIdle < 0:
		return errors.New("request metadata consumer intervals must be positive")
	}
	return nil
}

type recoveryPass struct {
	cursor string
	due    time.Time
	more   bool
	retry  bool
}

func (r *consumerRun) drainOwnPending(ctx context.Context, start string) recoveryPass {
	entries, err := r.readGroup(ctx, start, 0)
	if err != nil {
		if ctx.Err() == nil {
			r.log.Warn("request metadata pending replay failed", "error", err)
		}
		return recoveryPass{cursor: start, due: time.Now().Add(r.policy.ownPendingInterval), retry: true}
	}
	summary := r.processEntries(ctx, entries)
	r.reportActivity(ctx, summary, true)
	if len(entries) == r.policy.batchSize {
		return recoveryPass{cursor: entries[len(entries)-1].ID, due: time.Now(), more: true, retry: summary.retry}
	}
	return recoveryPass{cursor: "0-0", due: time.Now().Add(r.policy.ownPendingInterval), retry: summary.retry}
}

func (r *consumerRun) reclaimStale(ctx context.Context, start string) recoveryPass {
	page, err := r.autoClaim(ctx, start)
	if err != nil {
		if ctx.Err() == nil {
			r.log.Warn("request metadata reclaim failed", "error", err)
		}
		return recoveryPass{cursor: start, due: time.Now().Add(r.policy.recoveryInterval), retry: true}
	}
	// Reclaim removes deleted entries from the pending list, so persist their only loss notice before processing survivors.
	if err := r.reportDeletedPending(ctx, page.DeletedIDs); err != nil {
		if ctx.Err() == nil {
			r.log.Warn("missing request metadata deliveries were not recorded", "error", err)
		}
		return recoveryPass{cursor: start, due: time.Now().Add(r.policy.recoveryInterval), retry: true}
	}
	if len(page.Entries) > 0 {
		if err := ReportConsumerActivity(ctx, r.pool, r.region, Activity{Reclaimed: int64(len(page.Entries))}); err != nil {
			r.log.Warn("reclaimed request metadata counter was not recorded", "error", err)
		}
	}
	summary := r.processEntries(ctx, page.Entries)
	r.reportActivity(ctx, summary, true)
	if page.NextStart != "0-0" {
		return recoveryPass{cursor: page.NextStart, due: time.Now(), more: true, retry: summary.retry}
	}
	return recoveryPass{cursor: page.NextStart, due: time.Now().Add(r.policy.recoveryInterval), retry: summary.retry}
}

func (r *consumerRun) settleNew(ctx context.Context, ownPendingStart *string,
	ownPendingDue *time.Time, block time.Duration, retryDelay *time.Duration) bool {
	entries, err := r.readGroup(ctx, ">", block)
	if err != nil {
		if ctx.Err() != nil {
			return true
		}
		r.log.Warn("request metadata stream read failed", "error", err)
		if waitForRetry(ctx, *retryDelay) {
			return true
		}
		*retryDelay = min(2**retryDelay, consumerRetryCeiling)
		return false
	}
	summary := r.processEntries(ctx, entries)
	r.reportActivity(ctx, summary, false)
	if summary.stopped {
		return true
	}
	if summary.retry {
		// Something this consumer already owns could not be settled. Replay its
		// own pending list from the start after the backoff, because the entry
		// that failed is now the oldest unfinished work it has.
		*ownPendingStart = "0-0"
		*ownPendingDue = time.Now().Add(*retryDelay)
		if waitForRetry(ctx, *retryDelay) {
			return true
		}
		*retryDelay = min(2**retryDelay, consumerRetryCeiling)
	}
	return false
}

const (
	entryRetry entryOutcome = iota
	entryCompleted
	entryDuplicate
)

func waitForRetry(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return true
	case <-timer.C:
		return false
	}
}
