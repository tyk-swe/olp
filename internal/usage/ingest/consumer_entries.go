package ingest

import (
	"context"
	"errors"
	"time"

	"github.com/tyk-swe/olp/internal/usage"
	"github.com/tyk-swe/olp/internal/usage/completeness"
)

type entrySummary struct {
	completed  int64
	duplicates int64
	retry      bool
	stopped    bool
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

	accepted, err := usage.DecodeAccountingEvent(entry.Payload)
	if errors.Is(err, usage.ErrUnsupportedVersion) {
		r.log.Warn("request metadata awaits a compatible consumer", "stream_id", entry.ID, "error", err)
		return entryRetry
	}
	if invalid, ok := errors.AsType[*usage.InvalidAccountingEvent](err); ok {
		r.log.Error("discarding permanently invalid request metadata event", "stream_id", entry.ID, "error", err)
		return r.finishGap(ctx, entry.ID, "invalid_request_metadata_event", "request-metadata-event:"+invalid.EventID+":invalid")
	}
	if err != nil {
		r.log.Error("discarding malformed request metadata stream event",
			"stream_id", entry.ID, "error", err)
		return r.finishGap(ctx, entry.ID, "malformed_stream_event",
			"request-metadata-stream:"+entry.ID+":malformed")
	}

	result, err := PersistEvent(ctx, r.pool, accepted)
	switch {
	case errors.Is(err, usage.ErrInvalidEvent):
		r.log.Error("discarding permanently invalid request metadata event",
			"stream_id", entry.ID, "error", err)
		return r.finishGap(ctx, entry.ID, "invalid_request_metadata_event",
			"request-metadata-event:"+accepted.Event().EventID+":invalid")
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
		if err := r.limiter.InstallSnapshots(ctx, result.CostSnapshots); err != nil {
			r.log.Warn("cost snapshot application failed; reconciliation will repair it",
				"stream_id", streamID, "error", err)
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
	gap := completeness.Gap{GatewayInstance: r.consumer, EventCount: 1, Reason: reason,
		FirstObservedAt: now, LastObservedAt: now}
	if _, err := completeness.ReportGapOnce(ctx, r.pool, gap, dedupeKey); err != nil {
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
		gap := completeness.Gap{GatewayInstance: r.consumer, EventCount: 1, Reason: "missing_stream_event",
			FirstObservedAt: now, LastObservedAt: now}
		if _, err := completeness.ReportGapOnce(ctx, r.pool, gap, "request-metadata-stream:"+id+":missing"); err != nil {
			return err
		}
	}
	return nil
}
