package retention

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/usage"
)

type Querier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

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
	HistoryCutoff  time.Time
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
	// GrantEnrollmentRows counts grant enrollments purged an hour after they
	// expired; their session state expires with them, among the secrets.
	GrantEnrollmentRows int64
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
	historyCutoff := windows.request
	if windows.usage.Before(historyCutoff) {
		historyCutoff = windows.usage
	}
	report := MaintenanceReport{LockAcquired: true, HistoryCutoff: historyCutoff}
	if report.RequestRows, err = purgeExpiredRequests(ctx, conn, windows.request); err != nil {
		return MaintenanceReport{}, err
	}
	if _, err = drainInBatches(ctx, conn, purgeTraceSpansSQL, windows.request, int64(retentionBatch)); err != nil {
		return MaintenanceReport{}, err
	}
	if report.RollupRows, report.UsageRows, err = rollUpAttemptUsage(ctx, conn, windows.usage); err != nil {
		return MaintenanceReport{}, err
	}
	if err = purgeOrphanedAnchors(ctx, conn, historyCutoff.UTC().Truncate(time.Hour)); err != nil {
		return MaintenanceReport{}, err
	}
	if report.AuditRows, err = purgeExpiredAudit(ctx, conn, windows.audit); err != nil {
		return MaintenanceReport{}, err
	}
	if err = purgeExpiringRecords(ctx, conn, now, windows, &report); err != nil {
		return MaintenanceReport{}, err
	}
	return report, nil
}

// retentionCutoffs reads the configured windows. A stored value outside the
// accepted range is an error rather than a default: silently substituting 30
// days for an unreadable setting would delete data the operator kept on purpose.
func retentionCutoffs(ctx context.Context, conn *pgx.Conn, now time.Time) (cutoffs, error) {
	rows, err := conn.Query(ctx, `SELECT key, value FROM olp.settings WHERE key IN
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
      SELECT id, started_at FROM olp.requests WHERE started_at < $1
      LIMIT $2 FOR UPDATE SKIP LOCKED
    )
    DELETE FROM olp.requests request USING expired
     WHERE request.id = expired.id AND request.started_at = expired.started_at`

const purgeTraceSpansSQL = `WITH expired AS (
 SELECT id FROM olp.trace_spans WHERE received_at < $1 ORDER BY received_at LIMIT $2 FOR UPDATE SKIP LOCKED
) DELETE FROM olp.trace_spans span USING expired WHERE span.id = expired.id`

func purgeExpiredRequests(ctx context.Context, conn *pgx.Conn, cutoff time.Time) (int64, error) {
	return drainInBatches(ctx, conn, purgeRequestsSQL, cutoff, int64(retentionBatch))
}

// Anchors outlive their request rows so facts keep a foreign key. Candidates
// are locked before deletion: a concurrent fact insert holds KEY SHARE on its
// anchor, so SKIP LOCKED leaves that anchor for the next pass instead of
// cascading a child this snapshot cannot see.
const purgeAnchorsSQL = `WITH orphan AS (
      SELECT anchor.request_id, anchor.request_started_at
        FROM olp.usage_request_anchors anchor
       WHERE anchor.request_started_at < $1 AND NOT EXISTS (
         SELECT 1 FROM olp.attempt_usage_facts fact
          WHERE fact.request_id = anchor.request_id
            AND fact.request_started_at = anchor.request_started_at
       )
       LIMIT $2 FOR UPDATE OF anchor SKIP LOCKED
    )
    DELETE FROM olp.usage_request_anchors anchor USING orphan
     WHERE anchor.request_id = orphan.request_id
       AND anchor.request_started_at = orphan.request_started_at`

func purgeOrphanedAnchors(ctx context.Context, conn *pgx.Conn, cutoff time.Time) error {
	_, err := drainInBatches(ctx, conn, purgeAnchorsSQL, cutoff, int64(retentionBatch))
	return err
}

const purgeAuditSQL = `WITH expired AS (
      SELECT ctid FROM olp.audit WHERE occurred_at < $1 LIMIT $2 FOR UPDATE SKIP LOCKED
    )
    DELETE FROM olp.audit entry USING expired WHERE entry.ctid = expired.ctid`

func purgeExpiredAudit(ctx context.Context, conn *pgx.Conn, cutoff time.Time) (int64, error) {
	return drainInBatches(ctx, conn, purgeAuditSQL, cutoff, int64(retentionBatch))
}

// Gap rows roll into hourly evidence rather than disappearing: the totals a
// report calls incomplete must stay incomplete after retention. A deduplicated
// gap is held until its dedupe key can no longer be replayed, so rolling it up
// cannot let the same loss be recorded twice.
const gapRollupSQL = `WITH candidates AS (
      SELECT id FROM olp.request_metadata_ingestion_gaps
       WHERE reported_at < $1
         AND (deduplication_key IS NULL
              OR reported_at < now() - make_interval(days => $2::integer, mins => $3::integer))
       LIMIT $4 FOR UPDATE SKIP LOCKED
    ), expired AS (
      DELETE FROM olp.request_metadata_ingestion_gaps gap USING candidates
       WHERE gap.id = candidates.id
      RETURNING gateway_instance, reason, event_count, certainty,
                first_observed_at, last_observed_at
    ), rolled AS (
      INSERT INTO olp.request_metadata_gap_hourly
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

func rollUpRequestMetadataGaps(ctx context.Context, conn *pgx.Conn, cutoff time.Time, report *MaintenanceReport) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return fmt.Errorf("roll up request metadata gaps: %w", err)
		}
		var batchRollups, batchGaps int64
		if err = tx.QueryRow(ctx, gapRollupSQL, cutoff, int64(usage.ReplayHorizonDays), int64(usage.FutureSkewMinutes), int64(retentionBatch)).
			Scan(&batchRollups, &batchGaps); err != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
			return fmt.Errorf("roll up request metadata gaps: %w", err)
		}
		if err = tx.Commit(ctx); err != nil {
			return fmt.Errorf("roll up request metadata gaps: %w", err)
		}
		if batchRollups < 0 || batchGaps < 0 {
			return errors.New("maintenance returned an invalid gap rollup count")
		}
		report.GapRollupRows += batchRollups
		report.GapRows += batchGaps
		if batchGaps < retentionBatch {
			return nil
		}
	}
}

// A resolved epoch is evidence only until its usage window closes; the gap it
// opened is retained separately.
const purgeEpochsSQL = `WITH expired AS (
      SELECT ctid FROM olp.request_metadata_gateway_epochs
       WHERE (gracefully_closed_at IS NOT NULL AND gracefully_closed_at < $1)
          OR (acknowledged_at IS NOT NULL AND acknowledged_at < $1)
       LIMIT $2 FOR UPDATE SKIP LOCKED
    )
    DELETE FROM olp.request_metadata_gateway_epochs target USING expired
     WHERE target.ctid = expired.ctid`

// Receipts are the delivery idempotency record; they expire with the replay
// horizon they protect, not with the usage window.
const purgeReceiptsSQL = `WITH expired AS (
      SELECT ctid FROM olp.request_metadata_event_receipts
       WHERE recorded_at < now() - make_interval(days => $1::integer, mins => $2::integer)
       LIMIT $3 FOR UPDATE SKIP LOCKED
    )
    DELETE FROM olp.request_metadata_event_receipts receipt USING expired
     WHERE receipt.ctid = expired.ctid`

const purgeSessionsSQL = `WITH expired AS (
      SELECT ctid FROM olp.sessions WHERE expires_at <= $1 LIMIT $2 FOR UPDATE SKIP LOCKED
    )
    DELETE FROM olp.sessions target USING expired WHERE target.ctid = expired.ctid`

const purgeInvitationsSQL = `WITH expired AS (
      SELECT ctid FROM olp.invitations
       WHERE expires_at <= $1 AND accepted_at IS NULL AND revoked_at IS NULL
       LIMIT $2 FOR UPDATE SKIP LOCKED
    )
    DELETE FROM olp.invitations target USING expired WHERE target.ctid = expired.ctid`

const purgeReplaysSQL = `WITH expired AS (
      SELECT ctid FROM olp.replays WHERE expires_at <= $1 LIMIT $2 FOR UPDATE SKIP LOCKED
    )
    DELETE FROM olp.replays target USING expired WHERE target.ctid = expired.ctid`

const purgeSAMLFlowsSQL = `WITH expired AS (
      SELECT ctid FROM olp.saml_flows WHERE expires_at <= $1 LIMIT $2 FOR UPDATE SKIP LOCKED
    )
    DELETE FROM olp.saml_flows target USING expired WHERE target.ctid = expired.ctid`

const purgeSAMLAssertionReplaysSQL = `WITH expired AS (
      SELECT ctid FROM olp.saml_assertion_replays WHERE expires_at <= $1 LIMIT $2 FOR UPDATE SKIP LOCKED
    )
    DELETE FROM olp.saml_assertion_replays target USING expired WHERE target.ctid = expired.ctid`

const purgeOIDCFlowsSQL = `WITH expired AS (
      SELECT ctid FROM olp.oidc_flows WHERE expires_at <= $1 LIMIT $2 FOR UPDATE SKIP LOCKED
    )
    DELETE FROM olp.oidc_flows target USING expired WHERE target.ctid = expired.ctid`

const purgeGrantEnrollmentsSQL = `WITH expired AS (
      SELECT ctid FROM olp.grant_enrollments WHERE expires_at <= $1 LIMIT $2 FOR UPDATE SKIP LOCKED
    )
    DELETE FROM olp.grant_enrollments target USING expired WHERE target.ctid = expired.ctid`

const purgeMFAChallengesSQL = `WITH expired AS (
      SELECT ctid FROM olp.mfa_challenges WHERE expires_at <= $1 LIMIT $2 FOR UPDATE SKIP LOCKED
    )
    DELETE FROM olp.mfa_challenges target USING expired WHERE target.ctid = expired.ctid`

const purgeSecretsSQL = `WITH expired AS (
      SELECT ctid FROM olp.secrets WHERE expires_at <= $1 LIMIT $2 FOR UPDATE SKIP LOCKED
    )
    DELETE FROM olp.secrets target USING expired WHERE target.ctid = expired.ctid`

const purgeRecentAuthSQL = `WITH expired AS (
      SELECT ctid FROM olp.recent_auth WHERE expires_at <= $1 LIMIT $2 FOR UPDATE SKIP LOCKED
    )
    DELETE FROM olp.recent_auth target USING expired WHERE target.ctid = expired.ctid`

// purgeExpiringRecords clears the tables that carry their own expiry. Each
// table drains in its own committed batches, like the purges above, so one
// locked row or a cancelled pass cannot roll back a whole pass of reclaimed
// rows; a later table failing leaves the earlier ones purged.
func purgeExpiringRecords(ctx context.Context, conn *pgx.Conn, now time.Time, windows cutoffs, report *MaintenanceReport) error {
	if err := rollUpRequestMetadataGaps(ctx, conn, windows.usage, report); err != nil {
		return err
	}
	deletes := []struct {
		into *int64
		sql  string
		args []any
	}{
		{&report.EpochRows, purgeEpochsSQL, []any{windows.usage, int64(retentionBatch)}},
		{&report.ReceiptRows, purgeReceiptsSQL,
			[]any{int64(usage.ReplayHorizonDays), int64(usage.FutureSkewMinutes), int64(receiptBatch)}},
		{&report.SessionRows, purgeSessionsSQL, []any{now, int64(retentionBatch)}},
		{&report.InvitationRows, purgeInvitationsSQL, []any{now, int64(retentionBatch)}},
		// Replay records first, then the encrypted responses they point at:
		// deleting the secret would cascade the replay away uncounted.
		{&report.ReplayRows, purgeReplaysSQL, []any{now, int64(retentionBatch)}},
		{&report.OIDCFlowRows, purgeSAMLFlowsSQL, []any{now, int64(retentionBatch)}},
		{&report.OIDCFlowRows, purgeSAMLAssertionReplaysSQL, []any{now, int64(retentionBatch)}},
		{&report.OIDCFlowRows, purgeOIDCFlowsSQL, []any{now, int64(retentionBatch)}},
		// An expired grant enrollment's row outlives its session state for an
		// hour, so a late continuation learns that it expired.
		{&report.GrantEnrollmentRows, purgeGrantEnrollmentsSQL, []any{now.Add(-time.Hour), int64(retentionBatch)}},
	}
	for _, statement := range deletes {
		deleted, err := drainInBatches(ctx, conn, statement.sql, statement.args...)
		if err != nil {
			return fmt.Errorf("purge expiring records: %w", err)
		}
		*statement.into += deleted
	}
	// Stored replay responses and re-authentication grants expire with their
	// owners; both are counted under the records above rather than separately.
	for _, sql := range []string{purgeMFAChallengesSQL, purgeSecretsSQL, purgeRecentAuthSQL} {
		if _, err := drainInBatches(ctx, conn, sql, now, int64(retentionBatch)); err != nil {
			return fmt.Errorf("purge expiring records: %w", err)
		}
	}
	return nil
}

// MaintenanceInterval paces the maintenance worker. Frequent bounded passes
// keep receipt expiry from becoming one hourly delete spike.
const MaintenanceInterval = 60 * time.Second

func RetentionDays(ctx context.Context, q Querier) (int, error) {
	var value string
	err := q.QueryRow(ctx, `SELECT value FROM olp.settings WHERE key='retention.usage_days'`).Scan(&value)
	if errors.Is(err, pgx.ErrNoRows) {
		return defaultUsageDays, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read retention.usage_days: %w", err)
	}
	days, err := strconv.Atoi(value)
	if err != nil || days < 1 || days > 3650 {
		return 0, fmt.Errorf("retention setting retention.usage_days is invalid: %q", value)
	}
	return days, nil
}
