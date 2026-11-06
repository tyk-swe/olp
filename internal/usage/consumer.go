package usage

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tyk-swe/olp/internal/coordination"
	"github.com/tyk-swe/olp/internal/limits"
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
		batchSize:           BatchSize,
		blockInterval:       consumerBlockInterval,
		activeRecoveryBlock: consumerActiveRecoveryBlock,
		ownPendingInterval:  consumerOwnPendingInterval,
		reclaimIdle:         ReclaimIdle,
		recoveryInterval:    RecoveryInterval,
		healthInterval:      consumerHealthInterval,
	}
}

// RunConsumer drains the request metadata stream into durable accounting until
// ctx is done. Several processes may run it concurrently against one stream:
// the consumer group hands each delivery to one of them, and a delivery whose
// owner disappears is reclaimed by the survivors.
//
// The consumer identity must be stable for one process and unique across live
// ones; use ConsumerName. The limiter may be nil, in which case reconstructed
// spend is not pushed back into the distributed counters and the periodic
// reconciliation pass repairs them instead.
//
// PostgreSQL always commits before the delivery is acknowledged, so a crash in
// between replays the event and the receipt makes it a duplicate. The consumer
// is never deleted on exit: its unacknowledged deliveries must stay reclaimable.
func RunConsumer(ctx context.Context, pool *pgxpool.Pool, client *coordination.Client,
	stream, consumer, region string, limiter *limits.Limiter, log *slog.Logger) error {
	return runConsumer(ctx, pool, client, stream, consumer, region, limiter, log, defaultConsumerPolicy())
}

func RunConsumerWithReclaimIdle(ctx context.Context, pool *pgxpool.Pool, client *coordination.Client,
	stream, consumer, region string, limiter *limits.Limiter, log *slog.Logger, reclaimIdle time.Duration) error {
	policy := defaultConsumerPolicy()
	policy.reclaimIdle = reclaimIdle
	return runConsumer(ctx, pool, client, stream, consumer, region, limiter, log, policy)
}

type consumerRun struct {
	pool     *pgxpool.Pool
	client   *coordination.Client
	stream   string
	consumer string
	region   string
	limiter  *limits.Limiter
	log      *slog.Logger
	policy   consumerPolicy
	// groupLost records that a stream command found no consumer group, which
	// no amount of retrying repairs. Only the loop that owns it reads it.
	groupLost bool
}

func runConsumer(ctx context.Context, pool *pgxpool.Pool, client *coordination.Client,
	stream, consumer, region string, limiter *limits.Limiter, log *slog.Logger, policy consumerPolicy) error {
	if err := validateConsumerConfig(stream, consumer, policy); err != nil {
		return err
	}
	run := &consumerRun{pool: pool, client: client, stream: stream, consumer: consumer, region: region,
		limiter: limiter, log: log, policy: policy}

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

func (r *consumerRun) createGroup(ctx context.Context) error {
	command, cancel := context.WithTimeout(ctx, consumerCommandTimeout)
	defer cancel()
	reply, err := r.client.Do(command, "XGROUP", "CREATE", r.stream, Group, "0", "MKSTREAM")
	if err != nil {
		if isBusyGroup(err) {
			return nil
		}
		return fmt.Errorf("create request metadata consumer group: %w", err)
	}
	if text, ok := reply.(string); !ok || text != "OK" {
		return protocolError("invalid consumer group creation reply")
	}
	return nil
}

// serverAnswer is the server's own text for a failed command. The transport
// keeps that text out of its message so logs cannot leak command arguments, so
// the cause is read instead.
func serverAnswer(err error) string {
	var command *coordination.CommandError
	if errors.As(err, &command) && command.Cause != nil {
		err = command.Cause
	}
	if err == nil {
		return ""
	}
	return err.Error()
}

func isBusyGroup(err error) bool { return strings.Contains(serverAnswer(err), "BUSYGROUP") }

// noteGroupLoss records a stream command that found no consumer group and
// returns the error unchanged, so every caller keeps reporting it as before.
// Valkey answers NOGROUP to every group command once the stream key is gone,
// including after a later write recreates the key without the group, so the
// loop has to create it again rather than retry the read forever.
func (r *consumerRun) noteGroupLoss(err error) error {
	if strings.Contains(serverAnswer(err), "NOGROUP") {
		r.groupLost = true
	}
	return err
}

type entrySummary struct {
	completed  int64
	duplicates int64
	retry      bool
	stopped    bool
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

func (r *consumerRun) processEntries(ctx context.Context, entries []StreamEntry) entrySummary {
	var summary entrySummary
	for _, entry := range entries {
		if ctx.Err() != nil {
			summary.stopped = true
			break
		}
		switch r.processEntry(ctx, entry) {
		case entryRetry:
			summary.retry = true
		case entryCompleted:
			summary.completed++
		case entryDuplicate:
			summary.completed++
			summary.duplicates++
		}
	}
	return summary
}

func (r *consumerRun) reportActivity(ctx context.Context, summary entrySummary, recovered bool) {
	if summary.completed == 0 && summary.duplicates == 0 {
		return
	}
	activity := Activity{Duplicates: summary.duplicates, Processed: summary.completed}
	if recovered {
		activity.Recovered = summary.completed
	}
	if err := ReportConsumerActivity(ctx, r.pool, r.region, activity); err != nil {
		if ctx.Err() == nil {
			r.log.Warn("request metadata consumer counters were not recorded", "error", err)
		}
	}
}

type entryOutcome uint8

const (
	entryRetry entryOutcome = iota
	entryCompleted
	entryDuplicate
)

func (r *consumerRun) processEntry(ctx context.Context, entry StreamEntry) entryOutcome {
	switch {
	case entry.DeletedPendingID != "":
		// The reclaim scan found a pending delivery the server had already
		// dropped. The event is gone; only its loss can be recorded.
		r.log.Error("request metadata delivery disappeared from the stream",
			"stream_id", entry.DeletedPendingID)
		return r.finishGap(ctx, entry.ID, "missing_stream_event",
			"request-metadata-stream:"+entry.DeletedPendingID+":missing")
	case entry.Payload == nil:
		r.log.Error("request metadata stream event payload is missing", "stream_id", entry.ID)
		return r.finishGap(ctx, entry.ID, "missing_stream_event",
			"request-metadata-stream:"+entry.ID+":missing")
	}

	accepted, err := DecodeAccountingEvent(entry.Payload)
	if errors.Is(err, ErrUnsupportedVersion) {
		r.log.Warn("request metadata awaits a compatible consumer", "stream_id", entry.ID, "error", err)
		return entryRetry
	}
	if invalid, ok := errors.AsType[*invalidAccountingEvent](err); ok {
		r.log.Error("discarding permanently invalid request metadata event", "stream_id", entry.ID, "error", err)
		return r.finishGap(ctx, entry.ID, "invalid_request_metadata_event", "request-metadata-event:"+invalid.eventID+":invalid")
	}
	if err != nil {
		r.log.Error("discarding malformed request metadata stream event",
			"stream_id", entry.ID, "error", err)
		return r.finishGap(ctx, entry.ID, "malformed_stream_event",
			"request-metadata-stream:"+entry.ID+":malformed")
	}

	result, err := PersistEvent(ctx, r.pool, accepted)
	switch {
	case errors.Is(err, ErrInvalidEvent):
		r.log.Error("discarding permanently invalid request metadata event",
			"stream_id", entry.ID, "error", err)
		return r.finishGap(ctx, entry.ID, "invalid_request_metadata_event",
			"request-metadata-event:"+accepted.event.EventID+":invalid")
	case err != nil:
		if ctx.Err() == nil {
			r.log.Warn("request metadata persistence will retry", "stream_id", entry.ID, "error", err)
		}
		return entryRetry
	}
	return r.settle(ctx, entry.ID, result)
}

func (r *consumerRun) settle(ctx context.Context, streamID string, result Persisted) entryOutcome {
	if result.Outcome == PersistOutcomeRejectedOutsideReplayWindow {
		r.log.Warn("request metadata event outside the replay window was recorded as a gap",
			"stream_id", streamID)
	}
	if result.Outcome == PersistOutcomePersisted && r.limiter != nil {
		// Spend is already durable; the counters are a cache of it. A failure
		// here is repaired by the reconciliation pass, so it never blocks the
		// delivery from being acknowledged.
		for _, snapshot := range result.CostSnapshots {
			if _, _, err := r.limiter.ApplyCostSnapshot(ctx, snapshot); err != nil {
				r.log.Warn("cost snapshot application failed; reconciliation will repair it",
					"stream_id", streamID, "error", err)
			}
		}
	}
	if err := r.acknowledge(ctx, streamID); err != nil {
		r.log.Warn("request metadata delivery was not acknowledged",
			"stream_id", streamID, "error", err)
		return entryRetry
	}
	if result.Outcome == PersistOutcomeDuplicate {
		return entryDuplicate
	}
	return entryCompleted
}

// finishGap records a loss once and retires the delivery that reported it. If
// the gap cannot be written the delivery stays pending: an unrecorded loss must
// never be acknowledged away.
func (r *consumerRun) finishGap(ctx context.Context, streamID, reason, dedupeKey string) entryOutcome {
	now := time.Now().UTC()
	gap := Gap{GatewayInstance: r.consumer, EventCount: 1, Reason: reason,
		FirstObservedAt: now, LastObservedAt: now}
	if _, err := ReportGapOnce(ctx, r.pool, gap, dedupeKey); err != nil {
		if ctx.Err() == nil {
			r.log.Warn("request metadata gap persistence will retry",
				"stream_id", streamID, "error", err)
		}
		return entryRetry
	}
	if err := r.acknowledge(ctx, streamID); err != nil {
		r.log.Warn("request metadata delivery was not acknowledged",
			"stream_id", streamID, "error", err)
		return entryRetry
	}
	return entryCompleted
}

func (r *consumerRun) reportDeletedPending(ctx context.Context, ids []string) error {
	for _, id := range ids {
		now := time.Now().UTC()
		gap := Gap{GatewayInstance: r.consumer, EventCount: 1, Reason: "missing_stream_event",
			FirstObservedAt: now, LastObservedAt: now}
		if _, err := ReportGapOnce(ctx, r.pool, gap, "request-metadata-stream:"+id+":missing"); err != nil {
			return err
		}
	}
	return nil
}

// readGroup reads one page of deliveries. `id` is ">" for new work or a pending
// cursor for replay; a positive block waits server side for new entries.
func (r *consumerRun) readGroup(ctx context.Context, id string, block time.Duration) ([]StreamEntry, error) {
	args := []string{"XREADGROUP", "GROUP", Group, r.consumer, "COUNT", strconv.Itoa(r.policy.batchSize)}
	timeout := consumerCommandTimeout
	if block > 0 {
		args = append(args, "BLOCK", strconv.FormatInt(block.Milliseconds(), 10))
		timeout = block + consumerResponseMargin
	}
	args = append(args, "STREAMS", r.stream, id)
	command, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	reply, err := r.client.Do(command, args...)
	if err != nil {
		return nil, r.noteGroupLoss(fmt.Errorf("read request metadata stream: %w", err))
	}
	return parseReadReply(reply, r.stream, r.policy.batchSize)
}

func (r *consumerRun) autoClaim(ctx context.Context, start string) (*autoClaimPage, error) {
	command, cancel := context.WithTimeout(ctx, consumerCommandTimeout)
	defer cancel()
	reply, err := r.client.Do(command, "EVAL", claimScript, "1", r.stream, Group, r.consumer,
		strconv.FormatInt(r.policy.reclaimIdle.Milliseconds(), 10), start,
		strconv.Itoa(r.policy.batchSize))
	if err != nil {
		return nil, r.noteGroupLoss(fmt.Errorf("reclaim request metadata deliveries: %w", err))
	}
	return parseAutoClaimReply(reply, r.policy.batchSize)
}

// acknowledge retires one delivery after its transaction committed. It runs on
// a context that outlives shutdown: the work is already durable, and leaving
// the entry pending would only earn a duplicate delivery later.
func (r *consumerRun) acknowledge(ctx context.Context, id string) error {
	command, cancel := context.WithTimeout(context.WithoutCancel(ctx), consumerCommandTimeout)
	defer cancel()
	reply, err := r.client.Do(command, "EVAL", ackDeleteScript, "1", r.stream, Group, id)
	if err != nil {
		return fmt.Errorf("acknowledge request metadata delivery: %w", err)
	}
	counts, ok := reply.([]any)
	if !ok || len(counts) != 2 {
		return protocolError("invalid stream acknowledgement reply")
	}
	for _, count := range counts {
		value, ok := count.(int64)
		if !ok || value < 0 || value > 1 {
			return protocolError("invalid stream acknowledgement reply")
		}
	}
	return nil
}

func (r *consumerRun) checkpointHealth(ctx context.Context) error {
	pending, oldest, err := r.pendingSummary(ctx)
	if err != nil {
		return err
	}
	lag, oldestLagged, err := r.groupLag(ctx, pending)
	if err != nil {
		return err
	}
	return ReportConsumerHealth(ctx, r.pool, r.region, pending, lag, oldest, oldestLagged, time.Now().UTC())
}

func (r *consumerRun) pendingSummary(ctx context.Context) (int64, *time.Time, error) {
	command, cancel := context.WithTimeout(ctx, consumerCommandTimeout)
	defer cancel()
	reply, err := r.client.Do(command, "XPENDING", r.stream, Group)
	if err != nil {
		return 0, nil, r.noteGroupLoss(fmt.Errorf("read request metadata pending summary: %w", err))
	}
	items, ok := reply.([]any)
	if !ok || len(items) != 4 {
		return 0, nil, protocolError("unrecognized pending stream reply")
	}
	count, ok := items[0].(int64)
	if !ok || count < 0 {
		return 0, nil, protocolError("unrecognized pending stream reply")
	}
	if count == 0 {
		return 0, nil, nil
	}
	start, ok := items[1].(string)
	if !ok {
		return 0, nil, protocolError("unrecognized pending stream reply")
	}
	milliseconds, _, err := ParseStreamID(start)
	if err != nil {
		return 0, nil, err
	}
	if milliseconds > 1<<62 {
		return 0, nil, protocolError("pending stream ID overflow")
	}
	oldest := time.UnixMilli(int64(milliseconds)).UTC()
	return count, &oldest, nil
}

// groupLag is how many entries the group has never been delivered, and when
// the oldest of them was written. Undelivered entries are always the tail of
// the stream, so the lag newest entries end at the oldest lagged one. Valkey
// may return an unknown lag while deliveries and deletions race; because
// every acknowledged entry is deleted in the same step, the stream's
// remaining length minus what is pending is a safe fallback.
func (r *consumerRun) groupLag(ctx context.Context, pending int64) (int64, *time.Time, error) {
	command, cancel := context.WithTimeout(ctx, consumerCommandTimeout)
	defer cancel()
	reply, err := r.client.Do(command, "XINFO", "GROUPS", r.stream)
	if err != nil {
		return 0, nil, fmt.Errorf("read request metadata group info: %w", err)
	}
	groups, ok := reply.([]any)
	if !ok {
		return 0, nil, protocolError("invalid XINFO GROUPS reply")
	}
	var lag int64
	for _, item := range groups {
		fields, err := replyFields(item)
		if err != nil {
			return 0, nil, err
		}
		if name, ok := fields["name"].(string); !ok || name != Group {
			continue
		}
		if reported, ok := fields["lag"].(int64); ok {
			lag = max(reported, 0)
		} else {
			groupPending, ok := fields["pending"].(int64)
			if !ok {
				groupPending = pending
			}
			length, err := r.streamLength(ctx)
			if err != nil {
				return 0, nil, err
			}
			lag = max(length-groupPending, 0)
		}
		if lag == 0 {
			return 0, nil, nil
		}
		oldest, err := r.oldestLaggedEntry(ctx, lag)
		if err != nil {
			return 0, nil, err
		}
		return lag, oldest, nil
	}
	return 0, nil, protocolError("consumer group disappeared")
}

// oldestLaggedEntry reads back the lag newest entries — exactly the ones the
// group has never been delivered — and returns when the earliest of them was
// written. A reported lag whose entries are already gone cannot age, so the
// sample fails rather than publish one.
func (r *consumerRun) oldestLaggedEntry(ctx context.Context, lag int64) (*time.Time, error) {
	command, cancel := context.WithTimeout(ctx, consumerCommandTimeout)
	defer cancel()
	reply, err := r.client.Do(command, "XREVRANGE", r.stream, "+", "-", "COUNT", strconv.FormatInt(lag, 10))
	if err != nil {
		return nil, fmt.Errorf("read request metadata lagged entries: %w", err)
	}
	entries, ok := reply.([]any)
	if !ok {
		return nil, protocolError("invalid lagged entries reply")
	}
	if len(entries) == 0 {
		return nil, protocolError("reported consumer lag has no entries")
	}
	oldest, ok := entries[len(entries)-1].([]any)
	if !ok || len(oldest) == 0 {
		return nil, protocolError("invalid lagged entry reply")
	}
	id, ok := oldest[0].(string)
	if !ok {
		return nil, protocolError("invalid lagged entry id")
	}
	milliseconds, _, err := ParseStreamID(id)
	if err != nil {
		return nil, err
	}
	if milliseconds > 1<<62 {
		return nil, protocolError("lagged stream ID overflow")
	}
	written := time.UnixMilli(int64(milliseconds)).UTC()
	return &written, nil
}

func (r *consumerRun) streamLength(ctx context.Context) (int64, error) {
	command, cancel := context.WithTimeout(ctx, consumerCommandTimeout)
	defer cancel()
	reply, err := r.client.Do(command, "XLEN", r.stream)
	if err != nil {
		return 0, fmt.Errorf("read request metadata stream length: %w", err)
	}
	length, ok := reply.(int64)
	if !ok || length < 0 {
		return 0, protocolError("invalid XLEN reply")
	}
	return length, nil
}

// replyFields reads a reply that is a map under RESP3 and a flat field list
// otherwise.
func replyFields(reply any) (map[string]any, error) {
	switch value := reply.(type) {
	case map[string]any:
		return value, nil
	case []any:
		if len(value)%2 != 0 {
			return nil, protocolError("field list has odd length")
		}
		fields := make(map[string]any, len(value)/2)
		for index := 0; index+1 < len(value); index += 2 {
			name, ok := value[index].(string)
			if !ok {
				return nil, protocolError("field list has a non-string name")
			}
			fields[name] = value[index+1]
		}
		return fields, nil
	default:
		return nil, protocolError("invalid field container")
	}
}

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
