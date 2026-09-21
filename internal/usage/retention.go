package usage

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// maintenanceLockID is the session advisory lock one maintenance pass holds
// ("OLP_MT"). Replicas run the same schedule; only one may purge at a time.
const maintenanceLockID = 0x4f4c_505f_4d54

// retentionBatch bounds one delete batch. Batches commit separately so row
// locks stay brief and a cancelled pass keeps whatever it already reclaimed.
const retentionBatch = 50000

// receiptBatch is larger because receipts are small, single-table rows with no
// dependants: one pass can clear a day of them without a long transaction.
const receiptBatch = 250000

// Default retention windows, used when a setting is absent.
const (
	defaultRequestDays = 30
	defaultUsageDays   = 90
	defaultAuditDays   = 365
)

// MaintenanceReport counts what one pass reclaimed. A pass that did not get the
// lock reports LockAcquired false and nothing else; it is a skip, not a failure.
type MaintenanceReport struct {
	LockAcquired   bool
	RollupRows     int64
	GapRollupRows  int64
	RequestRows    int64
	UsageRows      int64
	AuditRows      int64
	GapRows        int64
	EpochRows      int64
	ReceiptRows    int64
	SessionRows    int64
	InvitationRows int64
	ReplayRows     int64
	OIDCFlowRows   int64
	ResourceRows   int64
}

type cutoffs struct{ request, usage, audit time.Time }

// RunMaintenance rolls completed hours into aggregates and then enforces the
// installation's retention windows. It runs on a connection hijacked out of the
// pool because it holds a session advisory lock across many transactions: that
// connection must never be handed back to another caller while locked, and
// closing it is what releases the lock.
func RunMaintenance(ctx context.Context, pool *pgxpool.Pool, now time.Time) (MaintenanceReport, error) {
	resource, err := pool.Acquire(ctx)
	if err != nil {
		return MaintenanceReport{}, fmt.Errorf("acquire maintenance connection: %w", err)
	}
	conn := resource.Hijack()
	defer conn.Close(context.WithoutCancel(ctx))
	var locked bool
	if err = conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", int64(maintenanceLockID)).Scan(&locked); err != nil {
		return MaintenanceReport{}, fmt.Errorf("acquire maintenance lock: %w", err)
	}
	if !locked {
		return MaintenanceReport{}, nil
	}
	windows, err := retentionCutoffs(ctx, conn, now)
	if err != nil {
		return MaintenanceReport{}, err
	}
	report := MaintenanceReport{LockAcquired: true}
	if report.RequestRows, err = purgeExpiredRequests(ctx, conn, windows.request); err != nil {
		return MaintenanceReport{}, err
	}
	if report.RollupRows, report.UsageRows, err = rollUpAttemptUsage(ctx, conn, windows.usage); err != nil {
		return MaintenanceReport{}, err
	}
	if err = purgeOrphanedAnchors(ctx, conn, windows.request); err != nil {
		return MaintenanceReport{}, err
	}
	if report.AuditRows, err = purgeExpiredAudit(ctx, conn, windows.audit); err != nil {
		return MaintenanceReport{}, err
	}
	if err = purgeExpiringRecords(ctx, conn, now, windows, &report); err != nil {
		return MaintenanceReport{}, err
	}
	if report.ResourceRows, err = drainInBatches(ctx, conn, purgeExpiredResourcesSQL, now, int64(retentionBatch)); err != nil {
		return MaintenanceReport{}, err
	}
	return report, nil
}

// retentionCutoffs reads the configured windows. A stored value outside the
// accepted range is an error rather than a default: silently substituting 30
// days for an unreadable setting would delete data the operator kept on purpose.
func retentionCutoffs(ctx context.Context, conn *pgx.Conn, now time.Time) (cutoffs, error) {
	rows, err := conn.Query(ctx, `SELECT key, value FROM olp_go.settings WHERE key IN
        ('retention.requests_days', 'retention.usage_days', 'retention.audit_days')`)
	if err != nil {
		return cutoffs{}, fmt.Errorf("read retention settings: %w", err)
	}
	defer rows.Close()
	requests, usage, audit := defaultRequestDays, defaultUsageDays, defaultAuditDays
	for rows.Next() {
		var key, value string
		if err = rows.Scan(&key, &value); err != nil {
			return cutoffs{}, fmt.Errorf("read retention settings: %w", err)
		}
		days, parseErr := strconv.Atoi(value)
		if parseErr != nil || days < 1 || days > 3650 {
			return cutoffs{}, fmt.Errorf("retention setting %s is invalid", key)
		}
		switch key {
		case "retention.requests_days":
			requests = days
		case "retention.usage_days":
			usage = days
		case "retention.audit_days":
			audit = days
		}
	}
	if err = rows.Err(); err != nil {
		return cutoffs{}, fmt.Errorf("read retention settings: %w", err)
	}
	day := 24 * time.Hour
	return cutoffs{
		request: now.Add(-time.Duration(requests) * day),
		usage:   now.Add(-time.Duration(usage) * day),
		audit:   now.Add(-time.Duration(audit) * day),
	}, nil
}

// drainInBatches deletes in committed batches until a short batch says nothing
// is left that this pass can claim. Every caller narrows its candidate set with
// LIMIT and FOR UPDATE SKIP LOCKED, so a short batch is a real end, not a row
// another transaction happens to hold.
func drainInBatches(ctx context.Context, conn *pgx.Conn, sql string, args ...any) (int64, error) {
	var deleted int64
	for {
		if err := ctx.Err(); err != nil {
			return deleted, err
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return deleted, fmt.Errorf("maintenance batch: %w", err)
		}
		tag, err := tx.Exec(ctx, sql, args...)
		if err != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
			return deleted, fmt.Errorf("maintenance batch: %w", err)
		}
		if err = tx.Commit(ctx); err != nil {
			return deleted, fmt.Errorf("maintenance batch: %w", err)
		}
		rows := tag.RowsAffected()
		deleted += rows
		if rows < retentionBatch {
			return deleted, nil
		}
	}
}

// Request history is deleted before facts, matching ingestion's
// request → anchor → fact lock order. Facts reference the anchor, not the
// partitioned history table, so purging history does not touch usage.
const purgeRequestsSQL = `WITH expired AS (
      SELECT id, started_at FROM olp_go.requests WHERE started_at < $1
      LIMIT $2 FOR UPDATE SKIP LOCKED
    )
    DELETE FROM olp_go.requests request USING expired
     WHERE request.id = expired.id AND request.started_at = expired.started_at`

const purgeExpiredResourcesSQL = `WITH expired AS (
      SELECT id FROM olp_go.provider_resources WHERE expires_at IS NOT NULL AND expires_at < $1
      LIMIT $2 FOR UPDATE SKIP LOCKED
    )
    DELETE FROM olp_go.provider_resources WHERE id IN (SELECT id FROM expired)`

func purgeExpiredRequests(ctx context.Context, conn *pgx.Conn, cutoff time.Time) (int64, error) {
	return drainInBatches(ctx, conn, purgeRequestsSQL, cutoff, int64(retentionBatch))
}

// Deleting and aggregating the same rows in one statement keeps a late stream
// event out of the delete set until a later pass, and makes repeated rollups
// additive for an hour that already carries retained totals.
const rollupSQL = `WITH candidates AS (
      SELECT ctid FROM olp_go.attempt_usage_facts
       WHERE observed_at < date_trunc('hour', $1::timestamptz AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'
       LIMIT $2 FOR UPDATE SKIP LOCKED
    ), expired AS (
      DELETE FROM olp_go.attempt_usage_facts fact USING candidates
       WHERE fact.ctid = candidates.ctid
      RETURNING route_slug, provider_id, upstream_model, operation, surface, api_key_id,
                budget_group_id, observed_at, input_tokens, output_tokens, cached_input_tokens,
                cache_write_input_tokens, cache_write_5m_input_tokens, cache_write_1h_input_tokens,
                media_units,
                estimated_cost, currency, charge_status, unpriced,
                request_counted, provider_request_counted, model_request_counted,
                target_request_counted, request_unpriced_counted, provider_unpriced_counted,
                model_unpriced_counted, target_unpriced_counted, request_incomplete_counted,
                provider_incomplete_counted, model_incomplete_counted, target_incomplete_counted,
                attribution
    ), rolled AS (
      INSERT INTO olp_go.attempt_usage_hourly
        (bucket, route_slug, provider_id, upstream_model, operation, surface, api_key_id,
         budget_group_id, attribution,
         request_count, provider_request_count, model_request_count, target_request_count,
         input_tokens, output_tokens, cached_input_tokens,
         cache_write_input_tokens, cache_write_5m_input_tokens, cache_write_1h_input_tokens,
         media_units, estimated_cost,
         request_unpriced_count, provider_unpriced_count, model_unpriced_count,
         target_unpriced_count, unpriced_attempt_count, request_incomplete_count,
         provider_incomplete_count, model_incomplete_count, target_incomplete_count, currency)
      SELECT date_trunc('hour', observed_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC',
             route_slug, provider_id, upstream_model, operation, surface, api_key_id,
             budget_group_id, attribution,
             COUNT(*) FILTER (WHERE request_counted),
             COUNT(*) FILTER (WHERE provider_request_counted),
             COUNT(*) FILTER (WHERE model_request_counted),
             COUNT(*) FILTER (WHERE target_request_counted),
             COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0),
             COALESCE(SUM(cached_input_tokens), 0),
             COALESCE(SUM(cache_write_input_tokens), 0),
             COALESCE(SUM(cache_write_5m_input_tokens), 0),
             COALESCE(SUM(cache_write_1h_input_tokens), 0),
             COALESCE(SUM(media_units), 0),
             SUM(estimated_cost),
             COUNT(*) FILTER (WHERE request_unpriced_counted),
             COUNT(*) FILTER (WHERE provider_unpriced_counted),
             COUNT(*) FILTER (WHERE model_unpriced_counted),
             COUNT(*) FILTER (WHERE target_unpriced_counted),
             COUNT(*) FILTER (WHERE charge_status <> 'not_billable' AND unpriced),
             COUNT(*) FILTER (WHERE request_incomplete_counted),
             COUNT(*) FILTER (WHERE provider_incomplete_counted),
             COUNT(*) FILTER (WHERE model_incomplete_counted),
             COUNT(*) FILTER (WHERE target_incomplete_counted), MAX(currency)
        FROM expired
       GROUP BY date_trunc('hour', observed_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC',
                route_slug, provider_id, upstream_model, operation, surface, api_key_id,
                budget_group_id, attribution
      ON CONFLICT ON CONSTRAINT attempt_usage_hourly_dimensions_key DO UPDATE SET
        request_count = attempt_usage_hourly.request_count + EXCLUDED.request_count,
        provider_request_count = attempt_usage_hourly.provider_request_count + EXCLUDED.provider_request_count,
        model_request_count = attempt_usage_hourly.model_request_count + EXCLUDED.model_request_count,
        target_request_count = attempt_usage_hourly.target_request_count + EXCLUDED.target_request_count,
        input_tokens = attempt_usage_hourly.input_tokens + EXCLUDED.input_tokens,
        output_tokens = attempt_usage_hourly.output_tokens + EXCLUDED.output_tokens,
        cached_input_tokens = attempt_usage_hourly.cached_input_tokens + EXCLUDED.cached_input_tokens,
        cache_write_input_tokens = attempt_usage_hourly.cache_write_input_tokens + EXCLUDED.cache_write_input_tokens,
        cache_write_5m_input_tokens = attempt_usage_hourly.cache_write_5m_input_tokens + EXCLUDED.cache_write_5m_input_tokens,
        cache_write_1h_input_tokens = attempt_usage_hourly.cache_write_1h_input_tokens + EXCLUDED.cache_write_1h_input_tokens,
        media_units = attempt_usage_hourly.media_units + EXCLUDED.media_units,
        estimated_cost = CASE
          WHEN attempt_usage_hourly.estimated_cost IS NULL AND EXCLUDED.estimated_cost IS NULL THEN NULL
          ELSE COALESCE(attempt_usage_hourly.estimated_cost, 0) + COALESCE(EXCLUDED.estimated_cost, 0) END,
        request_unpriced_count = attempt_usage_hourly.request_unpriced_count + EXCLUDED.request_unpriced_count,
        provider_unpriced_count = attempt_usage_hourly.provider_unpriced_count + EXCLUDED.provider_unpriced_count,
        model_unpriced_count = attempt_usage_hourly.model_unpriced_count + EXCLUDED.model_unpriced_count,
        target_unpriced_count = attempt_usage_hourly.target_unpriced_count + EXCLUDED.target_unpriced_count,
        unpriced_attempt_count = attempt_usage_hourly.unpriced_attempt_count + EXCLUDED.unpriced_attempt_count,
        request_incomplete_count = attempt_usage_hourly.request_incomplete_count + EXCLUDED.request_incomplete_count,
        provider_incomplete_count = attempt_usage_hourly.provider_incomplete_count + EXCLUDED.provider_incomplete_count,
        model_incomplete_count = attempt_usage_hourly.model_incomplete_count + EXCLUDED.model_incomplete_count,
        target_incomplete_count = attempt_usage_hourly.target_incomplete_count + EXCLUDED.target_incomplete_count,
        currency = COALESCE(attempt_usage_hourly.currency, EXCLUDED.currency)
      RETURNING 1
    )
    SELECT (SELECT count(*) FROM rolled)::bigint, (SELECT count(*) FROM expired)::bigint`

// rollUpAttemptUsage folds whole expired hours of facts into the hourly table.
// Only hours that have completely passed the usage cutoff are rolled up, so a
// bucket is never half aggregated and half live.
func rollUpAttemptUsage(ctx context.Context, conn *pgx.Conn, cutoff time.Time) (int64, int64, error) {
	var rollups, facts int64
	for {
		if err := ctx.Err(); err != nil {
			return rollups, facts, err
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return rollups, facts, fmt.Errorf("roll up attempt usage: %w", err)
		}
		var batchRollups, batchFacts int64
		if err = tx.QueryRow(ctx, rollupSQL, cutoff, int64(retentionBatch)).
			Scan(&batchRollups, &batchFacts); err != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
			return rollups, facts, fmt.Errorf("roll up attempt usage: %w", err)
		}
		if err = tx.Commit(ctx); err != nil {
			return rollups, facts, fmt.Errorf("roll up attempt usage: %w", err)
		}
		if batchRollups < 0 || batchFacts < 0 {
			return rollups, facts, errors.New("maintenance returned an invalid rollup count")
		}
		rollups += batchRollups
		facts += batchFacts
		if batchFacts < retentionBatch {
			return rollups, facts, nil
		}
	}
}

// Anchors outlive their request rows so facts keep a foreign key. Candidates
// are locked before deletion: a concurrent fact insert holds KEY SHARE on its
// anchor, so SKIP LOCKED leaves that anchor for the next pass instead of
// cascading a child this snapshot cannot see.
const purgeAnchorsSQL = `WITH orphan AS (
      SELECT anchor.request_id, anchor.request_started_at
        FROM olp_go.usage_request_anchors anchor
       WHERE anchor.request_started_at < $1 AND NOT EXISTS (
         SELECT 1 FROM olp_go.attempt_usage_facts fact
          WHERE fact.request_id = anchor.request_id
            AND fact.request_started_at = anchor.request_started_at
       )
       LIMIT $2 FOR UPDATE OF anchor SKIP LOCKED
    )
    DELETE FROM olp_go.usage_request_anchors anchor USING orphan
     WHERE anchor.request_id = orphan.request_id
       AND anchor.request_started_at = orphan.request_started_at`

func purgeOrphanedAnchors(ctx context.Context, conn *pgx.Conn, cutoff time.Time) error {
	_, err := drainInBatches(ctx, conn, purgeAnchorsSQL, cutoff, int64(retentionBatch))
	return err
}

const purgeAuditSQL = `WITH expired AS (
      SELECT ctid FROM olp_go.audit WHERE occurred_at < $1 LIMIT $2 FOR UPDATE SKIP LOCKED
    )
    DELETE FROM olp_go.audit entry USING expired WHERE entry.ctid = expired.ctid`

func purgeExpiredAudit(ctx context.Context, conn *pgx.Conn, cutoff time.Time) (int64, error) {
	return drainInBatches(ctx, conn, purgeAuditSQL, cutoff, int64(retentionBatch))
}

// Gap rows roll into hourly evidence rather than disappearing: the totals a
// report calls incomplete must stay incomplete after retention. A deduplicated
// gap is held until its dedupe key can no longer be replayed, so rolling it up
// cannot let the same loss be recorded twice.
const gapRollupSQL = `WITH expired AS (
      DELETE FROM olp_go.request_metadata_ingestion_gaps
       WHERE reported_at < $1
         AND (deduplication_key IS NULL
              OR reported_at < now() - make_interval(days => $2::integer, mins => $3::integer))
      RETURNING gateway_instance, reason, event_count, certainty,
                first_observed_at, last_observed_at
    ), rolled AS (
      INSERT INTO olp_go.request_metadata_gap_hourly
        (bucket, gateway_instance, reason, event_count, uncertain_gap_count,
         first_observed_at, last_observed_at)
      SELECT date_trunc('hour', first_observed_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC',
             gateway_instance, reason, SUM(event_count),
             COUNT(*) FILTER (WHERE certainty = 'lower_bound'),
             MIN(first_observed_at), MAX(last_observed_at)
        FROM expired
       GROUP BY date_trunc('hour', first_observed_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC',
                gateway_instance, reason
      ON CONFLICT (bucket, gateway_instance, reason) DO UPDATE SET
        event_count = request_metadata_gap_hourly.event_count + EXCLUDED.event_count,
        uncertain_gap_count = request_metadata_gap_hourly.uncertain_gap_count + EXCLUDED.uncertain_gap_count,
        first_observed_at = LEAST(request_metadata_gap_hourly.first_observed_at, EXCLUDED.first_observed_at),
        last_observed_at = GREATEST(request_metadata_gap_hourly.last_observed_at, EXCLUDED.last_observed_at)
      RETURNING 1
    )
    SELECT (SELECT count(*) FROM rolled)::bigint, (SELECT count(*) FROM expired)::bigint`

// purgeExpiringRecords clears the tables that carry their own expiry, in one
// transaction: they are small, bounded sets whose deletion has no dependants.
func purgeExpiringRecords(ctx context.Context, conn *pgx.Conn, now time.Time, windows cutoffs, report *MaintenanceReport) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("purge expiring records: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err = tx.QueryRow(ctx, gapRollupSQL, windows.usage, int64(ReplayHorizonDays), int64(FutureSkewMinutes)).
		Scan(&report.GapRollupRows, &report.GapRows); err != nil {
		return fmt.Errorf("roll up request metadata gaps: %w", err)
	}
	if report.GapRollupRows < 0 || report.GapRows < 0 {
		return errors.New("maintenance returned an invalid gap rollup count")
	}
	deletes := []struct {
		into *int64
		sql  string
		args []any
	}{
		// A resolved epoch is evidence only until its usage window closes; the
		// gap it opened is retained separately.
		{&report.EpochRows, `DELETE FROM olp_go.request_metadata_gateway_epochs
            WHERE (gracefully_closed_at IS NOT NULL AND gracefully_closed_at < $1)
               OR (acknowledged_at IS NOT NULL AND acknowledged_at < $1)`, []any{windows.usage}},
		// Receipts are the delivery idempotency record; they expire with the
		// replay horizon they protect, not with the usage window.
		{&report.ReceiptRows, `WITH expired AS (
              SELECT ctid FROM olp_go.request_metadata_event_receipts
               WHERE recorded_at < now() - make_interval(days => $1::integer, mins => $2::integer)
               LIMIT $3 FOR UPDATE SKIP LOCKED
            )
            DELETE FROM olp_go.request_metadata_event_receipts receipt USING expired
             WHERE receipt.ctid = expired.ctid`,
			[]any{int64(ReplayHorizonDays), int64(FutureSkewMinutes), int64(receiptBatch)}},
		{&report.SessionRows, "DELETE FROM olp_go.sessions WHERE expires_at <= $1", []any{now}},
		{&report.InvitationRows, `DELETE FROM olp_go.invitations
            WHERE expires_at <= $1 AND accepted_at IS NULL AND revoked_at IS NULL`, []any{now}},
		// Replay records first, then the encrypted responses they point at:
		// deleting the secret would cascade the replay away uncounted.
		{&report.ReplayRows, "DELETE FROM olp_go.replays WHERE expires_at <= $1", []any{now}},
		{&report.OIDCFlowRows, "DELETE FROM olp_go.oidc_flows WHERE expires_at <= $1", []any{now}},
	}
	for _, statement := range deletes {
		tag, execErr := tx.Exec(ctx, statement.sql, statement.args...)
		if execErr != nil {
			return fmt.Errorf("purge expiring records: %w", execErr)
		}
		*statement.into = tag.RowsAffected()
	}
	// Stored replay responses and re-authentication grants expire with their
	// owners; both are counted under the records above rather than separately.
	for _, sql := range []string{
		"DELETE FROM olp_go.secrets WHERE expires_at <= $1",
		"DELETE FROM olp_go.recent_auth WHERE expires_at <= $1",
	} {
		if _, err = tx.Exec(ctx, sql, now); err != nil {
			return fmt.Errorf("purge expiring records: %w", err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("purge expiring records: %w", err)
	}
	return nil
}

// MaintenanceInterval paces the maintenance worker. Frequent bounded passes
// keep receipt expiry from becoming one hourly delete spike.
const MaintenanceInterval = 60 * time.Second

// RunMaintenanceLoop runs maintenance until the context is done, checkpointing
// each pass so readiness can tell a working worker from a stalled one. A pass
// that could not take the lock is a skip: another replica is doing the work.
func RunMaintenanceLoop(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) {
	ticker := time.NewTicker(MaintenanceInterval)
	defer ticker.Stop()
	for {
		// The first pass runs at startup rather than a minute later: a process
		// that has just restarted may be the only one left to reclaim a backlog.
		report, err := RunMaintenance(ctx, pool, time.Now())
		if ctx.Err() != nil {
			return
		}
		outcome, progress := OutcomeSuccess, report.LockAcquired
		if err != nil {
			outcome, progress = OutcomeFailure, false
			log.Error("maintenance pass failed", "error_type", fmt.Sprintf("%T", err))
		} else if !report.LockAcquired {
			outcome = OutcomeSkipped
		}
		if err = CheckpointTask(ctx, pool, TaskMaintenance, outcome, progress); err != nil {
			log.Warn("maintenance health checkpoint failed", "error_type", fmt.Sprintf("%T", err))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
