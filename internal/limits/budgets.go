package limits

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// BudgetSQL is a self-contained scalar expression that reports one API key's
// current spend as jsonb. It expects the surrounding query to expose the API
// key row as alias "k" and reads both the raw attempt facts and the hourly
// rollups they are folded into, so a key's total never dips when retention
// rolls attempts up. Decimals are rendered as strings: money never travels
// through a JSON number.
//
// A calendar day or month added to a timestamptz is added in the session's
// timezone, so the boundaries are derived in the timezone-free domain and only
// then converted back with AT TIME ZONE 'UTC': a client that connects with any
// other TimeZone must still see the same UTC windows enforcement charges
// against, and a shifted month end would silently zero the whole report.
const BudgetSQL = `(SELECT jsonb_build_object(` +
	`'daily',jsonb_build_object('accrued',t.daily_accrued::text,'window_ends_at',w.daily_end),` +
	`'monthly',jsonb_build_object('accrued',t.monthly_accrued::text,'window_ends_at',w.monthly_end),` +
	`'unpriced_attempts',t.unpriced_attempts)` +
	` FROM (SELECT b.day AT TIME ZONE 'UTC' AS daily_start,` +
	`(b.day+interval '1 day') AT TIME ZONE 'UTC' AS daily_end,` +
	`b.month AT TIME ZONE 'UTC' AS monthly_start,` +
	`(b.month+interval '1 month') AT TIME ZONE 'UTC' AS monthly_end` +
	` FROM (SELECT date_trunc('day',now() AT TIME ZONE 'UTC') AS day,` +
	`date_trunc('month',now() AT TIME ZONE 'UTC') AS month) b) w,` +
	` LATERAL (SELECT COALESCE(SUM(u.cost) FILTER (WHERE u.observed_at>=w.daily_start` +
	` AND u.observed_at<w.daily_end),0) AS daily_accrued,` +
	`COALESCE(SUM(u.cost),0) AS monthly_accrued,` +
	`COALESCE(SUM(u.unpriced_attempts),0)::bigint AS unpriced_attempts` +
	` FROM (SELECT f.observed_at,COALESCE(f.estimated_cost,0)::numeric AS cost,` +
	`CASE WHEN f.charge_status<>'not_billable' AND f.unpriced THEN 1 ELSE 0 END::bigint` +
	` AS unpriced_attempts FROM olp.attempt_usage_facts f` +
	` WHERE f.api_key_id=k.id AND f.observed_at>=w.monthly_start AND f.observed_at<w.monthly_end` +
	` UNION ALL SELECT h.bucket,COALESCE(h.estimated_cost,0)::numeric,h.unpriced_attempt_count` +
	` FROM olp.attempt_usage_hourly h` +
	` WHERE h.api_key_id=k.id AND h.bucket>=w.monthly_start AND h.bucket<w.monthly_end) u) t)`

const GroupBudgetSQL = `(SELECT jsonb_build_object(` +
	`'daily',jsonb_build_object('accrued',t.daily_accrued::text,'limit',g.daily_cost_limit::text,` +
	`'remaining',CASE WHEN g.daily_cost_limit IS NULL THEN NULL ELSE GREATEST(g.daily_cost_limit-t.daily_accrued,0)::text END,` +
	`'reset_at',w.daily_end),` +
	`'monthly',jsonb_build_object('accrued',t.monthly_accrued::text,'limit',g.monthly_cost_limit::text,` +
	`'remaining',CASE WHEN g.monthly_cost_limit IS NULL THEN NULL ELSE GREATEST(g.monthly_cost_limit-t.monthly_accrued,0)::text END,` +
	`'reset_at',w.monthly_end),` +
	`'unpriced_attempts',t.unpriced_attempts)` +
	` FROM (SELECT b.day AT TIME ZONE 'UTC' AS daily_start,` +
	`(b.day+interval '1 day') AT TIME ZONE 'UTC' AS daily_end,` +
	`b.month AT TIME ZONE 'UTC' AS monthly_start,` +
	`(b.month+interval '1 month') AT TIME ZONE 'UTC' AS monthly_end` +
	` FROM (SELECT date_trunc('day',now() AT TIME ZONE 'UTC') AS day,` +
	`date_trunc('month',now() AT TIME ZONE 'UTC') AS month) b) w,` +
	` LATERAL (SELECT COALESCE(SUM(u.cost) FILTER (WHERE u.observed_at>=w.daily_start` +
	` AND u.observed_at<w.daily_end),0) AS daily_accrued,` +
	`COALESCE(SUM(u.cost),0) AS monthly_accrued,` +
	`COALESCE(SUM(u.unpriced_attempts),0)::bigint AS unpriced_attempts` +
	` FROM (SELECT f.observed_at,COALESCE(f.estimated_cost,0)::numeric AS cost,` +
	`CASE WHEN f.charge_status<>'not_billable' AND f.unpriced THEN 1 ELSE 0 END::bigint` +
	` AS unpriced_attempts FROM olp.attempt_usage_facts f` +
	` WHERE f.budget_group_id=g.id AND f.observed_at>=w.monthly_start AND f.observed_at<w.monthly_end` +
	` UNION ALL SELECT h.bucket,COALESCE(h.estimated_cost,0)::numeric,h.unpriced_attempt_count` +
	` FROM olp.attempt_usage_hourly h` +
	` WHERE h.budget_group_id=g.id AND h.bucket>=w.monthly_start AND h.bucket<w.monthly_end) u) t)`

// Windows are the fixed UTC day and month a cost budget is measured over.
type Windows struct {
	DailyStart   time.Time
	DailyEnd     time.Time
	MonthlyStart time.Time
	MonthlyEnd   time.Time
	// DailyID is the count of whole UTC days since the epoch and MonthlyID the
	// count of months. Both identify a window in Valkey and PostgreSQL without
	// carrying a timestamp.
	DailyID   int64
	MonthlyID int64
}

// BudgetWindows derives the day and month that contain now. Windows end on
// fixed UTC boundaries, never on a rolling offset from now.
func BudgetWindows(now time.Time) Windows {
	utc := now.UTC()
	year, month, day := utc.Date()
	dailyStart := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	monthlyStart := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
	return Windows{
		DailyStart:   dailyStart,
		DailyEnd:     dailyStart.AddDate(0, 0, 1),
		MonthlyStart: monthlyStart,
		MonthlyEnd:   monthlyStart.AddDate(0, 1, 0),
		// Midnight UTC is always a whole number of days from the epoch, so the
		// division is exact and the identifier counts days, not seconds.
		DailyID:   dailyStart.Unix() / 86_400,
		MonthlyID: int64(year)*12 + int64(month) - 1,
	}
}

// CostSnapshot is the durable spend PostgreSQL holds for one API key in the
// windows it names. Accrued totals are canonical decimal strings.
type CostSnapshot struct {
	CostOwnerID      string
	DailyWindowID    int64
	DailyAccrued     string
	MonthlyWindowID  int64
	MonthlyAccrued   string
	UnpricedAttempts int64
}

// validate refuses a snapshot Valkey could not store exactly, which would
// otherwise install a wrong balance that admits or denies spending silently.
func (s CostSnapshot) validate() error {
	if _, err := uuid.Parse(s.CostOwnerID); err != nil {
		return fmt.Errorf("cost snapshot owner ID %q is not a UUID", s.CostOwnerID)
	}
	if s.DailyWindowID < 0 || s.DailyWindowID > maxLuaInteger ||
		s.MonthlyWindowID < 0 || s.MonthlyWindowID > maxLuaInteger {
		return errors.New("cost snapshot window IDs exceed the Valkey Lua integer range")
	}
	if !validDecimal(s.DailyAccrued) {
		return fmt.Errorf("daily accrued cost %q is not a non-negative decimal with at most 12 fractional digits", s.DailyAccrued)
	}
	if !validDecimal(s.MonthlyAccrued) {
		return fmt.Errorf("monthly accrued cost %q is not a non-negative decimal with at most 12 fractional digits", s.MonthlyAccrued)
	}
	if s.UnpricedAttempts < 0 || s.UnpricedAttempts > maxLuaInteger {
		return fmt.Errorf("unpriced attempt count %d exceeds the Valkey Lua integer range", s.UnpricedAttempts)
	}
	return nil
}

// validDecimal reports whether value is a non-negative decimal the scripts
// compare exactly: plain digits, at most one point, at most twelve fractional
// digits, and short enough for the script's own normaliser.
func validDecimal(value string) bool {
	if len(value) < 1 || len(value) > 96 {
		return false
	}
	integer, fraction := value, ""
	if before, after, ok := strings.Cut(value, "."); ok {
		integer, fraction = before, after
		if len(fraction) < 1 || len(fraction) > 12 {
			return false
		}
	}
	if len(integer) < 1 {
		return false
	}
	for _, part := range [...]string{integer, fraction} {
		for index := 0; index < len(part); index++ {
			if part[index] < '0' || part[index] > '9' {
				return false
			}
		}
	}
	return true
}

// costKeys addresses the balances of one API key. Both live under the key's own
// cluster hash tag so every lookup that spends from it meets the same state.
func (l *Limiter) costKeys(costOwnerID string) (string, string) {
	prefix := l.namespace + ":{" + simpleUUID(costOwnerID) + "}:cost"
	return prefix + ":day", prefix + ":month"
}

// ApplyCostSnapshot installs PostgreSQL's durable spend in Valkey and reports
// which windows it reconciled. The script only initialises a window whose
// identifier matches the current server window and never lowers a counter that
// is already valid, so a stale or future snapshot cannot widen a budget.
func (l *Limiter) ApplyCostSnapshot(ctx context.Context, s CostSnapshot) (bool, bool, error) {
	if err := s.validate(); err != nil {
		return false, false, err
	}
	daily, monthly := l.costKeys(s.CostOwnerID)
	value, err := l.eval(ctx, reconcileCostScript, []string{daily, monthly},
		strconv.FormatInt(s.DailyWindowID, 10), s.DailyAccrued,
		strconv.FormatInt(s.MonthlyWindowID, 10), s.MonthlyAccrued,
		strconv.FormatInt(s.UnpricedAttempts, 10), "0")
	if err != nil {
		return false, false, err
	}
	return parseReconciliation(value)
}

// addCostDeltaSQL accumulates one attempt's cost into the day and month windows
// that contain its observation, and reports the resulting balances. A delta for
// a window other than the current one is kept in its own row, so clock skew can
// never overwrite today's total.
const addCostDeltaSQL = `WITH deltas (window_kind, window_id, accrued, unpriced_attempts) AS (
  VALUES ('day'::text, $2::bigint, $3::text::numeric, 0::bigint),
         ('month'::text, $4::bigint, $3::text::numeric, $5::bigint)
), applied AS (
  INSERT INTO olp.api_key_cost_windows
    (api_key_id, window_kind, window_id, accrued, unpriced_attempts)
  SELECT $1::uuid, window_kind, window_id, accrued, unpriced_attempts FROM deltas
  ON CONFLICT (api_key_id, window_kind, window_id) DO UPDATE SET
    accrued = olp.api_key_cost_windows.accrued + EXCLUDED.accrued,
    unpriced_attempts = olp.api_key_cost_windows.unpriced_attempts
                        + EXCLUDED.unpriced_attempts
  RETURNING api_key_id, window_kind, window_id, accrued, unpriced_attempts
) SELECT api_key_id::text,
         MAX(window_id) FILTER (WHERE window_kind = 'day')::bigint,
         MAX(accrued) FILTER (WHERE window_kind = 'day')::text,
         MAX(window_id) FILTER (WHERE window_kind = 'month')::bigint,
         MAX(accrued) FILTER (WHERE window_kind = 'month')::text,
         MAX(unpriced_attempts) FILTER (WHERE window_kind = 'month')::bigint
  FROM applied GROUP BY api_key_id`

const addGroupCostDeltaSQL = `WITH deltas (window_kind,window_id,accrued,unpriced_attempts) AS (
 VALUES ('day'::text,$2::bigint,$3::text::numeric,0::bigint),
        ('month'::text,$4::bigint,$3::text::numeric,$5::bigint)
), applied AS (
 INSERT INTO olp.budget_group_cost_windows
   (budget_group_id,window_kind,window_id,accrued,unpriced_attempts)
 SELECT $1::uuid,window_kind,window_id,accrued,unpriced_attempts FROM deltas
 ON CONFLICT (budget_group_id,window_kind,window_id) DO UPDATE SET
   accrued=olp.budget_group_cost_windows.accrued+EXCLUDED.accrued,
   unpriced_attempts=olp.budget_group_cost_windows.unpriced_attempts+EXCLUDED.unpriced_attempts
 RETURNING budget_group_id,window_kind,window_id,accrued,unpriced_attempts
) SELECT budget_group_id::text,
 MAX(window_id) FILTER (WHERE window_kind='day')::bigint,
 MAX(accrued) FILTER (WHERE window_kind='day')::text,
 MAX(window_id) FILTER (WHERE window_kind='month')::bigint,
 MAX(accrued) FILTER (WHERE window_kind='month')::text,
 MAX(unpriced_attempts) FILTER (WHERE window_kind='month')::bigint
 FROM applied GROUP BY budget_group_id`

// AddCostDelta accumulates one attempt's cost inside the caller's transaction
// and returns the API key's resulting balances, ready to hand to Valkey. cost
// is a non-negative decimal string.
func AddCostDelta(ctx context.Context, tx pgx.Tx, apiKeyID string, observedAt time.Time, cost string, unpriced int64) (CostSnapshot, error) {
	if _, err := uuid.Parse(apiKeyID); err != nil {
		return CostSnapshot{}, fmt.Errorf("API key ID %q is not a UUID", apiKeyID)
	}
	return addOwnerDelta(ctx, tx, addCostDeltaSQL, apiKeyID, observedAt, cost, unpriced)
}

func AddGroupCostDelta(ctx context.Context, tx pgx.Tx, budgetGroupID string, observedAt time.Time, cost string, unpriced int64) (CostSnapshot, error) {
	if _, err := uuid.Parse(budgetGroupID); err != nil {
		return CostSnapshot{}, fmt.Errorf("budget group ID %q is not a UUID", budgetGroupID)
	}
	return addOwnerDelta(ctx, tx, addGroupCostDeltaSQL, budgetGroupID, observedAt, cost, unpriced)
}

func addOwnerDelta(ctx context.Context, tx pgx.Tx, query string, ownerID string, observedAt time.Time, cost string, unpriced int64) (CostSnapshot, error) {
	if !validDecimal(cost) {
		return CostSnapshot{}, fmt.Errorf("cost delta %q is not a non-negative decimal with at most 12 fractional digits", cost)
	}
	if unpriced < 0 || unpriced > maxLuaInteger {
		return CostSnapshot{}, fmt.Errorf("unpriced attempt count %d is out of range", unpriced)
	}
	windows := BudgetWindows(observedAt)
	row := tx.QueryRow(ctx, query, ownerID, windows.DailyID, cost, windows.MonthlyID, unpriced)
	return scanSnapshot(row)
}

// reconciliationSnapshotsSQL folds every active key's attempts into the current
// day and month windows, prunes windows that have passed, and returns the
// durable balances. GREATEST keeps a window that already counted more than the
// facts can still see, which is what makes reconciliation safe to repeat after
// retention has rolled attempts up.
const reconciliationSnapshotsSQL = `WITH active_keys AS (
  SELECT id AS api_key_id FROM olp.api_keys
  WHERE revoked_at IS NULL AND (expires_at IS NULL OR expires_at > $1::timestamptz)
), usage AS (
  SELECT fact.api_key_id, fact.observed_at,
         COALESCE(fact.estimated_cost, 0)::numeric AS cost,
         CASE WHEN fact.charge_status <> 'not_billable' AND fact.unpriced
              THEN 1 ELSE 0 END::bigint AS unpriced_attempts
  FROM olp.attempt_usage_facts fact
  JOIN active_keys key ON key.api_key_id = fact.api_key_id
  WHERE fact.observed_at >= $3::timestamptz AND fact.observed_at < $4::timestamptz
  UNION ALL
  SELECT hourly.api_key_id, hourly.bucket,
         COALESCE(hourly.estimated_cost, 0)::numeric,
         hourly.unpriced_attempt_count
  FROM olp.attempt_usage_hourly hourly
  JOIN active_keys key ON key.api_key_id = hourly.api_key_id
  WHERE hourly.bucket >= $3::timestamptz AND hourly.bucket < $4::timestamptz
), totals AS (
  SELECT key.api_key_id,
         COALESCE(SUM(usage.cost) FILTER (
           WHERE usage.observed_at >= $2::timestamptz
             AND usage.observed_at < $7::timestamptz), 0) AS daily_accrued,
         COALESCE(SUM(usage.cost), 0) AS monthly_accrued,
         COALESCE(SUM(usage.unpriced_attempts), 0)::bigint AS unpriced_attempts
  FROM active_keys key LEFT JOIN usage ON usage.api_key_id = key.api_key_id
  GROUP BY key.api_key_id
), pruned AS (
  DELETE FROM olp.api_key_cost_windows
  WHERE (window_kind = 'day' AND window_id < $5::bigint)
     OR (window_kind = 'month' AND window_id < $6::bigint) RETURNING 1
), desired AS (
  SELECT api_key_id, 'day'::text AS window_kind, $5::bigint AS window_id,
         daily_accrued AS accrued, 0::bigint AS unpriced_attempts FROM totals
  UNION ALL
  SELECT api_key_id, 'month', $6::bigint, monthly_accrued, unpriced_attempts FROM totals
), reconciled AS (
  INSERT INTO olp.api_key_cost_windows
    (api_key_id, window_kind, window_id, accrued, unpriced_attempts)
  SELECT api_key_id, window_kind, window_id, accrued, unpriced_attempts FROM desired
  ON CONFLICT (api_key_id, window_kind, window_id) DO UPDATE SET
    accrued = GREATEST(olp.api_key_cost_windows.accrued, EXCLUDED.accrued),
    unpriced_attempts = GREATEST(olp.api_key_cost_windows.unpriced_attempts,
                                 EXCLUDED.unpriced_attempts)
  RETURNING api_key_id, window_kind, window_id, accrued, unpriced_attempts
) SELECT api_key_id::text, $5::bigint,
         MAX(accrued) FILTER (WHERE window_kind = 'day')::text,
         $6::bigint,
         MAX(accrued) FILTER (WHERE window_kind = 'month')::text,
         MAX(unpriced_attempts) FILTER (WHERE window_kind = 'month')::bigint
  FROM reconciled GROUP BY api_key_id ORDER BY api_key_id`

const reconciliationGroupSnapshotsSQL = `WITH active_groups AS (
 SELECT id AS budget_group_id FROM olp.budget_groups
 WHERE $1::timestamptz IS NOT NULL
   AND (daily_cost_limit IS NOT NULL OR monthly_cost_limit IS NOT NULL)
), usage AS (
 SELECT fact.budget_group_id,fact.observed_at,COALESCE(fact.estimated_cost,0)::numeric AS cost,
   CASE WHEN fact.charge_status<>'not_billable' AND fact.unpriced THEN 1 ELSE 0 END::bigint AS unpriced_attempts
 FROM olp.attempt_usage_facts fact JOIN active_groups g USING (budget_group_id)
 WHERE fact.observed_at >= $3::timestamptz AND fact.observed_at < $4::timestamptz
 UNION ALL
 SELECT hourly.budget_group_id,hourly.bucket,COALESCE(hourly.estimated_cost,0)::numeric,hourly.unpriced_attempt_count
 FROM olp.attempt_usage_hourly hourly JOIN active_groups g USING (budget_group_id)
 WHERE hourly.bucket >= $3::timestamptz AND hourly.bucket < $4::timestamptz
), totals AS (
 SELECT g.budget_group_id,
  COALESCE(SUM(u.cost) FILTER (WHERE u.observed_at >= $2::timestamptz AND u.observed_at < $7::timestamptz),0) AS daily_accrued,
  COALESCE(SUM(u.cost),0) AS monthly_accrued,
  COALESCE(SUM(u.unpriced_attempts),0)::bigint AS unpriced_attempts
 FROM active_groups g LEFT JOIN usage u USING (budget_group_id) GROUP BY g.budget_group_id
), pruned AS (
 DELETE FROM olp.budget_group_cost_windows
 WHERE (window_kind='day' AND window_id<$5::bigint) OR (window_kind='month' AND window_id<$6::bigint) RETURNING 1
), desired AS (
 SELECT budget_group_id,'day'::text AS window_kind,$5::bigint AS window_id,daily_accrued AS accrued,0::bigint AS unpriced_attempts FROM totals
 UNION ALL
 SELECT budget_group_id,'month',$6::bigint,monthly_accrued,unpriced_attempts FROM totals
), reconciled AS (
 INSERT INTO olp.budget_group_cost_windows (budget_group_id,window_kind,window_id,accrued,unpriced_attempts)
 SELECT budget_group_id,window_kind,window_id,accrued,unpriced_attempts FROM desired
 ON CONFLICT (budget_group_id,window_kind,window_id) DO UPDATE SET
  accrued=GREATEST(olp.budget_group_cost_windows.accrued,EXCLUDED.accrued),
  unpriced_attempts=GREATEST(olp.budget_group_cost_windows.unpriced_attempts,EXCLUDED.unpriced_attempts)
 RETURNING budget_group_id,window_kind,window_id,accrued,unpriced_attempts
) SELECT budget_group_id::text,$5::bigint,
 MAX(accrued) FILTER (WHERE window_kind='day')::text,$6::bigint,
 MAX(accrued) FILTER (WHERE window_kind='month')::text,
 MAX(unpriced_attempts) FILTER (WHERE window_kind='month')::bigint
 FROM reconciled GROUP BY budget_group_id ORDER BY budget_group_id`

// ReconciliationSnapshots recomputes every active key's durable spend for the
// windows containing now and returns the snapshots to install in Valkey. It
// writes, so it must run on the connection that holds the reconciliation lock.
func ReconciliationSnapshots(ctx context.Context, conn *pgx.Conn, now time.Time) ([]CostSnapshot, error) {
	windows := BudgetWindows(now)
	rows, err := conn.Query(ctx, reconciliationSnapshotsSQL, now.UTC(), windows.DailyStart,
		windows.MonthlyStart, windows.MonthlyEnd, windows.DailyID, windows.MonthlyID,
		windows.DailyEnd)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var snapshots []CostSnapshot
	for rows.Next() {
		snapshot, err := scanSnapshot(rows)
		if err != nil {
			return nil, err
		}
		snapshots = append(snapshots, snapshot)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	groupRows, err := conn.Query(ctx, reconciliationGroupSnapshotsSQL, now.UTC(), windows.DailyStart,
		windows.MonthlyStart, windows.MonthlyEnd, windows.DailyID, windows.MonthlyID,
		windows.DailyEnd)
	if err != nil {
		return nil, err
	}
	defer groupRows.Close()
	for groupRows.Next() {
		snapshot, err := scanSnapshot(groupRows)
		if err != nil {
			return nil, err
		}
		snapshots = append(snapshots, snapshot)
	}
	if err := groupRows.Err(); err != nil {
		return nil, err
	}
	return snapshots, nil
}

// scanSnapshot reads the six columns both budget statements project. A NULL in
// any of them means the statement did not produce both windows, which must fail
// rather than install a partial balance.
func scanSnapshot(row pgx.Row) (CostSnapshot, error) {
	var snapshot CostSnapshot
	if err := row.Scan(&snapshot.CostOwnerID, &snapshot.DailyWindowID, &snapshot.DailyAccrued,
		&snapshot.MonthlyWindowID, &snapshot.MonthlyAccrued, &snapshot.UnpricedAttempts); err != nil {
		return CostSnapshot{}, err
	}
	if err := snapshot.validate(); err != nil {
		return CostSnapshot{}, err
	}
	return snapshot, nil
}
