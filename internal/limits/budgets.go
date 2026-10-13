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
	"github.com/tyk-swe/olp/internal/budgetcalendar"
)

// budgetOwnership keeps raw and retained facts on the same subject boundary.
// These predicates are built only from reviewed SQL expressions, never input.
type budgetOwnership struct{ raw, hourly string }

func budgetTotalsSQL(owner budgetOwnership, basis string) string {
	return fmt.Sprintf(` FROM olp.budget_windows(now()) w,
 LATERAL (SELECT
 COALESCE(SUM(u.cost) FILTER (WHERE u.observed_at>=w.daily_start AND u.observed_at<w.daily_end),0) AS daily_accrued,
 COALESCE(SUM(u.cost) FILTER (WHERE u.observed_at>=w.weekly_start AND u.observed_at<w.weekly_end),0) AS weekly_accrued,
 COALESCE(SUM(u.cost) FILTER (WHERE u.observed_at>=w.monthly_start AND u.observed_at<w.monthly_end),0) AS monthly_accrued,
 COALESCE(SUM(u.charge) FILTER (WHERE u.observed_at>=w.daily_start AND u.observed_at<w.daily_end),0) AS daily_charged,
 COALESCE(SUM(u.charge) FILTER (WHERE u.observed_at>=w.weekly_start AND u.observed_at<w.weekly_end),0) AS weekly_charged,
 COALESCE(SUM(u.charge) FILTER (WHERE u.observed_at>=w.monthly_start AND u.observed_at<w.monthly_end),0) AS monthly_charged,
 COALESCE(SUM(u.unpriced_attempts) FILTER (WHERE u.observed_at>=w.monthly_start AND u.observed_at<w.monthly_end),0)::bigint AS unpriced_attempts,
 COALESCE(SUM(u.uncharged_attempts) FILTER (WHERE u.observed_at>=w.monthly_start AND u.observed_at<w.monthly_end),0)::bigint AS uncharged_attempts
 FROM (SELECT f.observed_at,COALESCE(f.estimated_cost,0)::numeric AS cost,COALESCE(f.charge,0)::numeric AS charge,
 CASE WHEN f.charge_status<>'not_billable' AND f.unpriced THEN 1 ELSE 0 END::bigint AS unpriced_attempts,
 CASE WHEN f.charge_status<>'not_billable' AND f.charge IS NULL THEN 1 ELSE 0 END::bigint AS uncharged_attempts
 FROM olp.attempt_usage_facts f WHERE NOT f.budget_exempt AND (%s)
 AND f.observed_at>=LEAST(w.daily_start,w.weekly_start,w.monthly_start) AND f.observed_at<GREATEST(w.daily_end,w.weekly_end,w.monthly_end)
 UNION ALL SELECT h.budget_bucket,COALESCE(h.estimated_cost,0)::numeric,COALESCE(h.charge,0)::numeric,h.unpriced_attempt_count,h.uncharged_attempt_count
 FROM olp.attempt_usage_hourly h WHERE NOT h.budget_exempt AND (%s)
 AND h.budget_bucket>=LEAST(w.daily_start,w.weekly_start,w.monthly_start) AND h.budget_bucket<GREATEST(w.daily_end,w.weekly_end,w.monthly_end)) u) t
 , LATERAL (SELECT %s='charge' AS charged) basis`, owner.raw, owner.hourly, basis)
}

func budgetReportSQL(owner budgetOwnership, basis string) string {
	return `(SELECT jsonb_build_object(
 'daily',jsonb_build_object('accrued',CASE WHEN basis.charged THEN t.daily_charged ELSE t.daily_accrued END::text,'window_ends_at',w.daily_end),
 'weekly',jsonb_build_object('accrued',CASE WHEN basis.charged THEN t.weekly_charged ELSE t.weekly_accrued END::text,'window_ends_at',w.weekly_end),
 'monthly',jsonb_build_object('accrued',CASE WHEN basis.charged THEN t.monthly_charged ELSE t.monthly_accrued END::text,'window_ends_at',w.monthly_end),
 'cost_basis',CASE WHEN basis.charged THEN 'charge' ELSE 'cost' END,
 'unpriced_attempts',t.unpriced_attempts,'uncharged_attempts',t.uncharged_attempts)` + budgetTotalsSQL(owner, basis) + `)`
}

// BudgetSQL reports one API key's raw and retained current-window spend as
// exact decimal strings. The enclosing query supplies the key row as k.
var BudgetSQL = budgetReportSQL(budgetOwnership{"f.api_key_id=k.id", "h.api_key_id=k.id"}, `COALESCE(olp.effective_limits(k.policy,(SELECT p.limit_templates->(k.policy->>'limit_template') FROM olp.projects p WHERE p.id=k.project_id))->>'cost_basis','cost')`)

// GroupBudgetSQL shares the same charge and calendar rules, adding the
// effective limits of the enclosing budget-group row g.
var GroupBudgetSQL = `(SELECT jsonb_build_object(
 'daily',jsonb_build_object('accrued',CASE WHEN basis.charged THEN t.daily_charged ELSE t.daily_accrued END::text,'limit',(g.effective_limits->>'daily_cost_limit')::numeric::text,
 'remaining',CASE WHEN g.effective_limits->>'daily_cost_limit' IS NULL THEN NULL ELSE GREATEST((g.effective_limits->>'daily_cost_limit')::numeric-(CASE WHEN basis.charged THEN t.daily_charged ELSE t.daily_accrued END),0)::text END,'reset_at',w.daily_end),
 'weekly',jsonb_build_object('accrued',CASE WHEN basis.charged THEN t.weekly_charged ELSE t.weekly_accrued END::text,'limit',(g.effective_limits->>'weekly_cost_limit')::numeric::text,
 'remaining',CASE WHEN g.effective_limits->>'weekly_cost_limit' IS NULL THEN NULL ELSE GREATEST((g.effective_limits->>'weekly_cost_limit')::numeric-(CASE WHEN basis.charged THEN t.weekly_charged ELSE t.weekly_accrued END),0)::text END,'reset_at',w.weekly_end),
 'monthly',jsonb_build_object('accrued',CASE WHEN basis.charged THEN t.monthly_charged ELSE t.monthly_accrued END::text,'limit',(g.effective_limits->>'monthly_cost_limit')::numeric::text,
 'remaining',CASE WHEN g.effective_limits->>'monthly_cost_limit' IS NULL THEN NULL ELSE GREATEST((g.effective_limits->>'monthly_cost_limit')::numeric-(CASE WHEN basis.charged THEN t.monthly_charged ELSE t.monthly_accrued END),0)::text END,'reset_at',w.monthly_end),
 'cost_basis',COALESCE(g.effective_limits->>'cost_basis','cost'),
 'unpriced_attempts',t.unpriced_attempts,'uncharged_attempts',t.uncharged_attempts)` + budgetTotalsSQL(budgetOwnership{"f.budget_group_id=g.id", "h.budget_group_id=g.id"}, "COALESCE(g.effective_limits->>'cost_basis','cost')") + `)`

// Windows are the fixed UTC day and month a cost budget is measured over.
type Windows struct {
	WeeklyStart  time.Time
	WeeklyEnd    time.Time
	WeeklyID     int64
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

// BudgetWindows describes the original UTC calendar. Production accounting
// uses CurrentBudgetWindows; this helper also supports legacy snapshot callers.
func BudgetWindows(now time.Time) Windows {
	utc := now.UTC()
	year, month, day := utc.Date()
	dailyStart := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	monthlyStart := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
	civil := (budgetcalendar.Calendar{}).Windows(now)
	return Windows{
		WeeklyStart: civil.Week.Start, WeeklyEnd: civil.Week.End, WeeklyID: (civil.Week.Start.Unix()/86400 + 3) / 7,
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
	// Basis names the amount the snapshot carries: cost accrued with unpriced
	// attempts, or charge accrued with uncharged attempts. Accounting installs
	// both, and the empty basis is cost.
	Basis                    Basis
	DailyStart, DailyEnd     time.Time
	MonthlyStart, MonthlyEnd time.Time
	WeeklyStart, WeeklyEnd   time.Time
	WeeklyWindowID           int64
	WeeklyAccrued            string
	CostOwnerID              string
	DailyWindowID            int64
	DailyAccrued             string
	MonthlyWindowID          int64
	MonthlyAccrued           string
	UnpricedAttempts         int64
	// The charge-basis mirrors scanned beside their cost counterparts.
	DailyCharged      string
	MonthlyCharged    string
	WeeklyCharged     string
	UnchargedAttempts int64
	// RequestID names the request whose spend this snapshot installed, when one
	// did: the identifier its cost reservation was made under. Installing the
	// spend removes the reservation in the same step, so the budget never counts
	// the request twice. A snapshot rebuilt from the facts as a whole carries none.
	RequestID string
}

// validate refuses a snapshot Valkey could not store exactly, which would
// otherwise install a wrong balance that admits or denies spending silently.
func (s CostSnapshot) validate() error {
	if !s.Basis.valid() {
		return fmt.Errorf("cost snapshot basis %q is not valid", s.Basis)
	}
	if _, err := uuid.Parse(s.CostOwnerID); err != nil {
		return fmt.Errorf("cost snapshot owner ID %q is not a UUID", s.CostOwnerID)
	}
	if s.DailyWindowID < 0 || s.DailyWindowID > maxLuaInteger ||
		s.MonthlyWindowID < 0 || s.MonthlyWindowID > maxLuaInteger {
		return errors.New("cost snapshot window IDs exceed the Valkey Lua integer range")
	}
	if s.WeeklyAccrued != "" && (s.WeeklyWindowID < 0 || s.WeeklyWindowID > maxLuaInteger || !validDecimal(s.WeeklyAccrued)) {
		return errors.New("invalid weekly cost snapshot")
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
	if !s.DailyStart.IsZero() || !s.MonthlyStart.IsZero() || !s.WeeklyStart.IsZero() {
		for _, period := range [][2]time.Time{{s.DailyStart, s.DailyEnd}, {s.MonthlyStart, s.MonthlyEnd}, {s.WeeklyStart, s.WeeklyEnd}} {
			if period[0].IsZero() || !period[1].After(period[0]) || period[0].UnixMilli() < 0 || period[1].UnixMilli() > maxLuaInteger {
				return errors.New("invalid budget calendar bounds")
			}
		}
	}
	if s.RequestID != "" {
		if _, err := uuid.Parse(s.RequestID); err != nil {
			return fmt.Errorf("cost snapshot request ID %q is not a UUID", s.RequestID)
		}
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

// costPrefix is what the cost keys of one owner share, which is their cluster
// hash tag: every lookup that spends from the owner meets the same state.
// costPrefixFor is the same tag for the basis the owner enforces: each owner
// keeps an independent "{owner}:cost:*" and "{owner}:charge:*" state, so a
// budget that changes basis finds fresh counters with nothing to migrate.
func (l *Limiter) costPrefix(costOwnerID string) string {
	return l.costPrefixFor(costOwnerID, BasisCost)
}
func (l *Limiter) costPrefixFor(costOwnerID string, basis Basis) string {
	return l.namespace + ":{" + simpleUUID(costOwnerID) + "}:" + string(basis.normalized())
}

// costKeys addresses the balances of one API key. Both live under the key's own
// cluster hash tag so every lookup that spends from it meets the same state.
func (l *Limiter) costKeys(costOwnerID string) (string, string) {
	prefix := l.costPrefix(costOwnerID)
	return prefix + ":day", prefix + ":month"
}
func (l *Limiter) costKeysFor(costOwnerID string, basis Basis) (string, string) {
	prefix := l.costPrefixFor(costOwnerID, basis)
	return prefix + ":day", prefix + ":month"
}

// pendingKeys addresses the reservations of requests in flight against one cost
// owner: a hash of what each holds, and the set that says when each lapses. They
// share the balances' hash tag so one script sees accrued and pending together.
func (l *Limiter) pendingKeys(costOwnerID string) (string, string) {
	prefix := l.costPrefix(costOwnerID)
	return prefix + ":pending", prefix + ":expiry"
}
func (l *Limiter) pendingKeysFor(costOwnerID string, basis Basis) (string, string) {
	prefix := l.costPrefixFor(costOwnerID, basis)
	return prefix + ":pending", prefix + ":expiry"
}

// Reserved reports the cost that requests in flight hold against one owner, as a
// canonical decimal string, without retiring any that have lapsed. It is for
// operators and tests: admission decides from the scripts, never from this.
func (l *Limiter) Reserved(ctx context.Context, costOwnerID string) (string, error) {
	if _, err := uuid.Parse(costOwnerID); err != nil {
		return "", &InvalidRequestError{Reason: "cost owner ID must be a UUID"}
	}
	pending, _ := l.pendingKeys(costOwnerID)
	value, err := l.client.Do(ctx, "HGET", pending, "sum")
	if err != nil {
		return "", &ServiceError{Err: err}
	}
	switch total := value.(type) {
	case nil:
		return "0", nil
	case string:
		if validDecimal(total) {
			return total, nil
		}
	}
	return "", ErrMalformedState
}

// ApplyCostSnapshot installs PostgreSQL's durable spend in Valkey and reports
// which windows it reconciled. The script only initialises a window whose
// identifier matches the current server window and never lowers a counter that
// is already valid, so a stale or future snapshot cannot widen a budget. A
// snapshot that names a request also removes that request's cost reservation in
// the same step, replayed or not.
func (l *Limiter) ApplyCostSnapshot(ctx context.Context, s CostSnapshot) (bool, bool, error) {
	if err := s.validate(); err != nil {
		return false, false, err
	}
	daily, monthly := l.costKeysFor(s.CostOwnerID, s.Basis)
	pending, expiry := l.pendingKeysFor(s.CostOwnerID, s.Basis)
	requestID := ""
	if s.RequestID != "" {
		requestID = canonicalUUID(s.RequestID)
	}
	args := []string{strconv.FormatInt(s.DailyWindowID, 10), s.DailyAccrued, strconv.FormatInt(s.MonthlyWindowID, 10), s.MonthlyAccrued, strconv.FormatInt(s.UnpricedAttempts, 10), "0", requestID, strconv.FormatInt(s.WeeklyWindowID, 10), s.WeeklyAccrued}
	if !s.DailyStart.IsZero() && (s.DailyWindowID == s.DailyStart.Unix() || s.MonthlyWindowID == s.MonthlyStart.Unix() || s.WeeklyWindowID == s.WeeklyStart.Unix()) {
		for _, instant := range []time.Time{s.DailyStart, s.DailyEnd, s.MonthlyStart, s.MonthlyEnd, s.WeeklyStart, s.WeeklyEnd} {
			args = append(args, strconv.FormatInt(instant.UnixMilli(), 10))
		}
	}
	value, err := l.eval(ctx, reconcileCostScript, []string{daily, monthly, pending, expiry, l.costPrefixFor(s.CostOwnerID, s.Basis) + ":week"}, args...)
	if err != nil {
		return false, false, err
	}
	return parseReconciliation(value)
}

// Every monetary owner uses one delta statement, with the same exact
// arithmetic, calendar and weekly completeness rules. Only these four fixed
// storage bindings are supported; no query text is rewritten after assembly.
type costOwner uint8

const (
	costOwnerKey costOwner = iota
	costOwnerGroup
	costOwnerEndUser
	costOwnerAggregate
)

func costOwnerStorage(owner costOwner) (table, column string) {
	switch owner {
	case costOwnerKey:
		return "olp.api_key_cost_windows", "api_key_id"
	case costOwnerGroup:
		return "olp.budget_group_cost_windows", "budget_group_id"
	case costOwnerEndUser:
		return "olp.end_user_cost_windows", "account_id"
	case costOwnerAggregate:
		return "olp.aggregate_cost_windows", "account_id"
	default:
		panic("unsupported monetary cost owner")
	}
}

func costDeltaSQL(owner costOwner) string {
	table, column := costOwnerStorage(owner)
	return fmt.Sprintf(`WITH budget_calendar AS MATERIALIZED (SELECT * FROM olp.budget_windows($6::timestamptz)),
 deltas (window_kind,window_id,accrued,unpriced_attempts,charged,uncharged_attempts) AS (
 VALUES ('day'::text,(SELECT daily_id FROM budget_calendar),$2::text::numeric,0::bigint,$4::text::numeric,0::bigint),
 ('month'::text,(SELECT monthly_id FROM budget_calendar),$2::text::numeric,$3::bigint,$4::text::numeric,$5::bigint),
 ('week'::text,(SELECT weekly_id FROM budget_calendar),$2::text::numeric,0::bigint,$4::text::numeric,0::bigint)
 ), applied AS (
 INSERT INTO %[1]s (%[2]s,window_kind,window_id,accrued,unpriced_attempts,charged,uncharged_attempts)
 SELECT $1::uuid,window_kind,window_id,accrued,unpriced_attempts,charged,uncharged_attempts FROM deltas
 ON CONFLICT (%[2]s,window_kind,window_id) DO UPDATE SET
 accrued=%[1]s.accrued+EXCLUDED.accrued,
 unpriced_attempts=%[1]s.unpriced_attempts+EXCLUDED.unpriced_attempts,
 charged=%[1]s.charged+EXCLUDED.charged,
 uncharged_attempts=%[1]s.uncharged_attempts+EXCLUDED.uncharged_attempts
 RETURNING %[2]s,window_kind,window_id,accrued,unpriced_attempts,charged,uncharged_attempts,weekly_complete
 ) SELECT %[2]s::text,
 MAX(window_id) FILTER(WHERE window_kind='day')::bigint,
 MAX(accrued) FILTER(WHERE window_kind='day')::text,
 MAX(charged) FILTER(WHERE window_kind='day')::text,
 MAX(window_id) FILTER(WHERE window_kind='month')::bigint,
 MAX(accrued) FILTER(WHERE window_kind='month')::text,
 MAX(charged) FILTER(WHERE window_kind='month')::text,
 MAX(unpriced_attempts) FILTER(WHERE window_kind='month')::bigint,
 MAX(uncharged_attempts) FILTER(WHERE window_kind='month')::bigint,
 MAX(window_id) FILTER(WHERE window_kind='week')::bigint,
 CASE WHEN bool_or(weekly_complete) FILTER(WHERE window_kind='week') THEN MAX(accrued) FILTER(WHERE window_kind='week')::text ELSE '' END,
 CASE WHEN bool_or(weekly_complete) FILTER(WHERE window_kind='week') THEN MAX(charged) FILTER(WHERE window_kind='week')::text ELSE '' END,
 (SELECT daily_start FROM budget_calendar),(SELECT daily_end FROM budget_calendar),
 (SELECT monthly_start FROM budget_calendar),(SELECT monthly_end FROM budget_calendar),
 (SELECT weekly_start FROM budget_calendar),(SELECT weekly_end FROM budget_calendar)
 FROM applied GROUP BY %[2]s`, table, column)
}

func reconcileCostWindowsSQL(owner costOwner, scoped bool, head string) string {
	table, column := costOwnerStorage(owner)
	pruneScope := ""
	if scoped {
		pruneScope = "($2::uuid IS NULL OR " + column + "=$2::uuid) AND "
	}
	return fmt.Sprintf(`WITH budget_calendar AS MATERIALIZED (SELECT * FROM olp.budget_windows($1::timestamptz)),
%[4]s, totals AS (
 SELECT a.id,
  COALESCE(SUM(u.cost) FILTER (WHERE u.observed_at >= (SELECT daily_start FROM budget_calendar) AND u.observed_at < (SELECT daily_end FROM budget_calendar)),0) AS daily_accrued,
  COALESCE(SUM(u.cost) FILTER (WHERE u.observed_at >= (SELECT monthly_start FROM budget_calendar) AND u.observed_at < (SELECT monthly_end FROM budget_calendar)),0) AS monthly_accrued,
  COALESCE(SUM(u.cost) FILTER (WHERE u.observed_at >= (SELECT weekly_start FROM budget_calendar) AND u.observed_at < (SELECT weekly_end FROM budget_calendar)),0) AS weekly_accrued,
  COALESCE(SUM(u.unpriced_attempts) FILTER (WHERE u.observed_at >= (SELECT monthly_start FROM budget_calendar) AND u.observed_at < (SELECT monthly_end FROM budget_calendar)),0)::bigint AS unpriced_attempts,
  COALESCE(SUM(u.charge) FILTER (WHERE u.observed_at >= (SELECT daily_start FROM budget_calendar) AND u.observed_at < (SELECT daily_end FROM budget_calendar)),0) AS daily_charged,
  COALESCE(SUM(u.charge) FILTER (WHERE u.observed_at >= (SELECT monthly_start FROM budget_calendar) AND u.observed_at < (SELECT monthly_end FROM budget_calendar)),0) AS monthly_charged,
  COALESCE(SUM(u.charge) FILTER (WHERE u.observed_at >= (SELECT weekly_start FROM budget_calendar) AND u.observed_at < (SELECT weekly_end FROM budget_calendar)),0) AS weekly_charged,
  COALESCE(SUM(u.uncharged_attempts) FILTER (WHERE u.observed_at >= (SELECT monthly_start FROM budget_calendar) AND u.observed_at < (SELECT monthly_end FROM budget_calendar)),0)::bigint AS uncharged_attempts
 FROM accounts a LEFT JOIN usage u ON u.id=a.id GROUP BY a.id
), pruned AS (
 DELETE FROM %[1]s
 WHERE %[3]s((window_kind='day' AND window_id<(SELECT daily_id FROM budget_calendar)) OR (window_kind='month' AND window_id<(SELECT monthly_id FROM budget_calendar)) OR (window_kind='week' AND window_id<(SELECT weekly_id FROM budget_calendar))) RETURNING 1
), desired AS (
 SELECT id,'day'::text AS window_kind,(SELECT daily_id FROM budget_calendar) AS window_id,daily_accrued AS accrued,0::bigint AS unpriced_attempts,daily_charged AS charged,0::bigint AS uncharged_attempts FROM totals
 UNION ALL
 SELECT id,'month',(SELECT monthly_id FROM budget_calendar),monthly_accrued,unpriced_attempts,monthly_charged,uncharged_attempts FROM totals
 UNION ALL SELECT id,'week',(SELECT weekly_id FROM budget_calendar),weekly_accrued,0,weekly_charged,0 FROM totals
), reconciled AS (
 INSERT INTO %[1]s (%[2]s,window_kind,window_id,accrued,unpriced_attempts,charged,uncharged_attempts,weekly_complete)
 SELECT id,window_kind,window_id,accrued,unpriced_attempts,charged,uncharged_attempts,true FROM desired
 ON CONFLICT (%[2]s,window_kind,window_id) DO UPDATE SET weekly_complete=true,
  accrued=GREATEST(%[1]s.accrued,EXCLUDED.accrued),
  unpriced_attempts=GREATEST(%[1]s.unpriced_attempts,EXCLUDED.unpriced_attempts),
  charged=GREATEST(%[1]s.charged,EXCLUDED.charged),
  uncharged_attempts=GREATEST(%[1]s.uncharged_attempts,EXCLUDED.uncharged_attempts)
 RETURNING %[2]s,window_kind,window_id,accrued,unpriced_attempts,charged,uncharged_attempts
) SELECT %[2]s::text,(SELECT daily_id FROM budget_calendar),
 MAX(accrued) FILTER (WHERE window_kind='day')::text,MAX(charged) FILTER (WHERE window_kind='day')::text,(SELECT monthly_id FROM budget_calendar),
 MAX(accrued) FILTER (WHERE window_kind='month')::text,MAX(charged) FILTER (WHERE window_kind='month')::text,
 MAX(unpriced_attempts) FILTER (WHERE window_kind='month')::bigint,
 MAX(uncharged_attempts) FILTER (WHERE window_kind='month')::bigint,
 (SELECT weekly_id FROM budget_calendar),MAX(accrued) FILTER (WHERE window_kind='week')::text,MAX(charged) FILTER (WHERE window_kind='week')::text,
 (SELECT daily_start FROM budget_calendar),(SELECT daily_end FROM budget_calendar),
 (SELECT monthly_start FROM budget_calendar),(SELECT monthly_end FROM budget_calendar),
 (SELECT weekly_start FROM budget_calendar),(SELECT weekly_end FROM budget_calendar)
FROM reconciled GROUP BY %[2]s ORDER BY %[2]s`, table, column, pruneScope, head)
}

var addCostDeltaSQL = costDeltaSQL(costOwnerKey)
var addGroupCostDeltaSQL = costDeltaSQL(costOwnerGroup)

// AddCostDelta accumulates one attempt's cost and charge inside the caller's
// transaction and returns the API key's resulting balances under both bases,
// ready to hand to Valkey. cost and charge are non-negative decimal strings.
func AddCostDelta(ctx context.Context, tx pgx.Tx, apiKeyID string, observedAt time.Time, cost string, unpriced int64, charge string, uncharged int64) ([]CostSnapshot, error) {
	if _, err := uuid.Parse(apiKeyID); err != nil {
		return nil, fmt.Errorf("API key ID %q is not a UUID", apiKeyID)
	}
	return addOwnerDelta(ctx, tx, addCostDeltaSQL, apiKeyID, observedAt, cost, unpriced, charge, uncharged)
}

func AddGroupCostDelta(ctx context.Context, tx pgx.Tx, budgetGroupID string, observedAt time.Time, cost string, unpriced int64, charge string, uncharged int64) ([]CostSnapshot, error) {
	if _, err := uuid.Parse(budgetGroupID); err != nil {
		return nil, fmt.Errorf("budget group ID %q is not a UUID", budgetGroupID)
	}
	return addOwnerDelta(ctx, tx, addGroupCostDeltaSQL, budgetGroupID, observedAt, cost, unpriced, charge, uncharged)
}

func addOwnerDelta(ctx context.Context, tx pgx.Tx, query string, ownerID string, observedAt time.Time, cost string, unpriced int64, charge string, uncharged int64) ([]CostSnapshot, error) {
	if !validDecimal(cost) {
		return nil, fmt.Errorf("cost delta %q is not a non-negative decimal with at most 12 fractional digits", cost)
	}
	if !validDecimal(charge) {
		return nil, fmt.Errorf("charge delta %q is not a non-negative decimal with at most 12 fractional digits", charge)
	}
	if unpriced < 0 || unpriced > maxLuaInteger {
		return nil, fmt.Errorf("unpriced attempt count %d is out of range", unpriced)
	}
	if uncharged < 0 || uncharged > maxLuaInteger {
		return nil, fmt.Errorf("uncharged attempt count %d is out of range", uncharged)
	}
	row := tx.QueryRow(ctx, query, ownerID, cost, unpriced, charge, uncharged, observedAt)
	return scanSnapshots(row)
}

// reconciliationSnapshotsSQL folds every active key's attempts into the current
// day and month windows, prunes windows that have passed, and returns the
// durable balances. GREATEST keeps a window that already counted more than the
// facts can still see, which is what makes reconciliation safe to repeat after
// retention has rolled attempts up.
var reconciliationSnapshotsSQL = reconcileCostWindowsSQL(costOwnerKey, false, `accounts AS (
  SELECT id FROM olp.api_keys
  WHERE revoked_at IS NULL AND (expires_at IS NULL OR expires_at > $1::timestamptz)
), usage AS (
  SELECT fact.api_key_id AS id, fact.observed_at,
         COALESCE(fact.estimated_cost, 0)::numeric AS cost,
         COALESCE(fact.charge, 0)::numeric AS charge,
         CASE WHEN fact.charge_status <> 'not_billable' AND fact.unpriced
              THEN 1 ELSE 0 END::bigint AS unpriced_attempts,
         CASE WHEN fact.charge_status <> 'not_billable' AND fact.charge IS NULL
              THEN 1 ELSE 0 END::bigint AS uncharged_attempts
  FROM (SELECT * FROM olp.attempt_usage_facts WHERE NOT budget_exempt) fact
  JOIN accounts a ON a.id = fact.api_key_id
  WHERE fact.observed_at >= LEAST((SELECT daily_start FROM budget_calendar),(SELECT monthly_start FROM budget_calendar),(SELECT weekly_start FROM budget_calendar)) AND fact.observed_at < GREATEST((SELECT daily_end FROM budget_calendar),(SELECT monthly_end FROM budget_calendar),(SELECT weekly_end FROM budget_calendar))
  UNION ALL
  SELECT hourly.api_key_id AS id, hourly.budget_bucket,
         COALESCE(hourly.estimated_cost, 0)::numeric,
         COALESCE(hourly.charge, 0)::numeric,
         hourly.unpriced_attempt_count, hourly.uncharged_attempt_count
  FROM (SELECT * FROM olp.attempt_usage_hourly WHERE NOT budget_exempt) hourly
  JOIN accounts a ON a.id = hourly.api_key_id
  WHERE hourly.budget_bucket >= LEAST((SELECT daily_start FROM budget_calendar),(SELECT monthly_start FROM budget_calendar),(SELECT weekly_start FROM budget_calendar)) AND hourly.budget_bucket < GREATEST((SELECT daily_end FROM budget_calendar),(SELECT monthly_end FROM budget_calendar),(SELECT weekly_end FROM budget_calendar))
)`)

var reconciliationGroupSnapshotsSQL = reconcileCostWindowsSQL(costOwnerGroup, false, `accounts AS (
 SELECT id FROM olp.budget_groups_with_limits
 WHERE $1::timestamptz IS NOT NULL
   AND (effective_limits->>'daily_cost_limit' IS NOT NULL OR effective_limits->>'monthly_cost_limit' IS NOT NULL OR effective_limits->>'weekly_cost_limit' IS NOT NULL)
), usage AS (
 SELECT fact.budget_group_id AS id,fact.observed_at,COALESCE(fact.estimated_cost,0)::numeric AS cost,
   COALESCE(fact.charge,0)::numeric AS charge,
   CASE WHEN fact.charge_status<>'not_billable' AND fact.unpriced THEN 1 ELSE 0 END::bigint AS unpriced_attempts,
   CASE WHEN fact.charge_status<>'not_billable' AND fact.charge IS NULL THEN 1 ELSE 0 END::bigint AS uncharged_attempts
 FROM (SELECT * FROM olp.attempt_usage_facts WHERE NOT budget_exempt) fact JOIN accounts a ON a.id = fact.budget_group_id
 WHERE fact.observed_at >= LEAST((SELECT daily_start FROM budget_calendar),(SELECT monthly_start FROM budget_calendar),(SELECT weekly_start FROM budget_calendar)) AND fact.observed_at < GREATEST((SELECT daily_end FROM budget_calendar),(SELECT monthly_end FROM budget_calendar),(SELECT weekly_end FROM budget_calendar))
 UNION ALL
 SELECT hourly.budget_group_id AS id,hourly.budget_bucket,COALESCE(hourly.estimated_cost,0)::numeric,COALESCE(hourly.charge,0)::numeric,hourly.unpriced_attempt_count,hourly.uncharged_attempt_count
 FROM (SELECT * FROM olp.attempt_usage_hourly WHERE NOT budget_exempt) hourly JOIN accounts a ON a.id = hourly.budget_group_id
 WHERE hourly.budget_bucket >= LEAST((SELECT daily_start FROM budget_calendar),(SELECT monthly_start FROM budget_calendar),(SELECT weekly_start FROM budget_calendar)) AND hourly.budget_bucket < GREATEST((SELECT daily_end FROM budget_calendar),(SELECT monthly_end FROM budget_calendar),(SELECT weekly_end FROM budget_calendar))
)`)

// ReconciliationSnapshots recomputes every active key's and budget group's
// durable spend for the windows containing now, adds the balances of every
// capped supply owner, and returns the snapshots to install in Valkey. It
// writes, so it must run on the connection that holds the reconciliation lock.
func ReconciliationSnapshots(ctx context.Context, conn *pgx.Conn, now time.Time) ([]CostSnapshot, error) {
	snapshots, err := querySnapshots(ctx, conn, reconciliationSnapshotsSQL, now.UTC())
	if err != nil {
		return nil, err
	}
	groups, err := querySnapshots(ctx, conn, reconciliationGroupSnapshotsSQL, now.UTC())
	if err != nil {
		return nil, err
	}
	snapshots = append(snapshots, groups...)
	supply, err := supplySnapshots(ctx, conn, now)
	if err != nil {
		return nil, err
	}
	snapshots = append(snapshots, supply...)
	endUsers, err := endUserSnapshots(ctx, conn, now)
	if err != nil {
		return nil, err
	}
	snapshots = append(snapshots, endUsers...)
	aggregates, err := aggregateSnapshots(ctx, conn, now)
	if err != nil {
		return nil, err
	}
	return append(snapshots, aggregates...), nil
}

func querySnapshots(ctx context.Context, conn *pgx.Conn, query string, args ...any) ([]CostSnapshot, error) {
	rows, err := conn.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var snapshots []CostSnapshot
	for rows.Next() {
		snapshot, err := scanSnapshots(rows)
		if err != nil {
			return nil, err
		}
		snapshots = append(snapshots, snapshot...)
	}
	return snapshots, rows.Err()
}

// scanSnapshots reads one delta/snapshot row's eighteen columns as the pair
// Valkey installs: the cost basis followed by the charge basis. The charge
// snapshot carries the uncharged-attempt count in the slot the cost snapshot
// keeps its unpriced-attempt count in — the stored shape is identical.
func scanSnapshots(row pgx.Row) ([]CostSnapshot, error) {
	var snapshot CostSnapshot
	if err := row.Scan(&snapshot.CostOwnerID, &snapshot.DailyWindowID, &snapshot.DailyAccrued, &snapshot.DailyCharged,
		&snapshot.MonthlyWindowID, &snapshot.MonthlyAccrued, &snapshot.MonthlyCharged,
		&snapshot.UnpricedAttempts, &snapshot.UnchargedAttempts, &snapshot.WeeklyWindowID, &snapshot.WeeklyAccrued, &snapshot.WeeklyCharged,
		&snapshot.DailyStart, &snapshot.DailyEnd, &snapshot.MonthlyStart, &snapshot.MonthlyEnd, &snapshot.WeeklyStart, &snapshot.WeeklyEnd); err != nil {
		return nil, err
	}
	if err := snapshot.validate(); err != nil {
		return nil, err
	}
	snapshot.Basis = BasisCost
	return []CostSnapshot{snapshot, {
		Basis:            BasisCharge,
		CostOwnerID:      snapshot.CostOwnerID,
		DailyWindowID:    snapshot.DailyWindowID,
		DailyAccrued:     snapshot.DailyCharged,
		MonthlyWindowID:  snapshot.MonthlyWindowID,
		MonthlyAccrued:   snapshot.MonthlyCharged,
		UnpricedAttempts: snapshot.UnchargedAttempts,
		WeeklyWindowID:   snapshot.WeeklyWindowID,
		WeeklyAccrued:    snapshot.WeeklyCharged,
		DailyStart:       snapshot.DailyStart,
		DailyEnd:         snapshot.DailyEnd,
		MonthlyStart:     snapshot.MonthlyStart,
		MonthlyEnd:       snapshot.MonthlyEnd,
		WeeklyStart:      snapshot.WeeklyStart,
		WeeklyEnd:        snapshot.WeeklyEnd,
		RequestID:        snapshot.RequestID,
	}}, nil
}

// scanSnapshot reads the fourteen columns supply budget statements project. A
// NULL in any of them means the statement did not produce both windows, which
// must fail rather than install a partial balance.
func scanSnapshot(row pgx.Row) (CostSnapshot, error) {
	var snapshot CostSnapshot
	if err := row.Scan(&snapshot.CostOwnerID, &snapshot.DailyWindowID, &snapshot.DailyAccrued,
		&snapshot.MonthlyWindowID, &snapshot.MonthlyAccrued, &snapshot.UnpricedAttempts, &snapshot.WeeklyWindowID, &snapshot.WeeklyAccrued, &snapshot.DailyStart, &snapshot.DailyEnd, &snapshot.MonthlyStart, &snapshot.MonthlyEnd, &snapshot.WeeklyStart, &snapshot.WeeklyEnd); err != nil {
		return CostSnapshot{}, err
	}
	if err := snapshot.validate(); err != nil {
		return CostSnapshot{}, err
	}
	return snapshot, nil
}
