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

// Pipeliner is a Commander that can also send several commands in one round
// trip. coordination.Client is one; a Commander that is not is asked one
// command at a time, which only tests do.
type Pipeliner interface {
	Pipeline(ctx context.Context, commands ...[]string) ([]any, error)
}

// QuotaProbe names a rate and concurrency quota whose headroom Supply reads.
type QuotaProbe struct {
	LookupID          string
	RequestsPerMinute *int64
	TokensPerMinute   *int64
	MaxConcurrency    *int64
}

// CapProbe names a cost cap whose spend Supply reads.
type CapProbe struct {
	OwnerID          string
	DailyCostLimit   *string
	MonthlyCostLimit *string
}

// CapState is what Supply learned about one cost cap.
type CapState int

const (
	// CapUnknown is a cap whose state is uninitialised, malformed or could not
	// be read, which selection treats as a cap it cannot honor.
	CapUnknown CapState = iota
	CapAvailable
	CapExhausted
)

// Headroom is what remains of a quota in the current minute, from 0 to 1: the
// smallest of its remaining request, token and concurrency fractions. Known is
// false when the quota could not be read.
type Headroom struct {
	Fraction float64
	Known    bool
}

// Supply reads the headroom of quotas and the state of cost caps in one round
// trip, from the same windows admission reserves against. It never reserves
// anything, and a value it cannot read is reported unknown rather than failing
// the read as a whole: selection ranks unknown headroom last and passes over a
// cap it cannot read.
func (l *Limiter) Supply(ctx context.Context, quotas []QuotaProbe, caps []CapProbe) ([]Headroom, []CapState) {
	calls := make([]scriptCall, 0, len(quotas)+len(caps))
	for _, quota := range quotas {
		rate, concurrency := l.rateKeys(quota.LookupID)
		calls = append(calls, scriptCall{providerUsageScript, []string{rate, concurrency}, nil})
	}
	for _, cap := range caps {
		prefix := l.costPrefix(cap.OwnerID)
		// Availability includes pending spend without changing balances or leases.
		calls = append(calls, scriptCall{reserveCostScript,
			[]string{prefix + ":day", prefix + ":month", prefix + ":pending", prefix + ":expiry"},
			[]string{optionalCost(cap.DailyCostLimit), optionalCost(cap.MonthlyCostLimit), "0", "check", "", "0"}})
	}
	replies := l.evalAll(ctx, calls)
	headroom := make([]Headroom, len(quotas))
	for index, quota := range quotas {
		if usage, err := parseUsage(replies[index]); err == nil {
			headroom[index] = Headroom{Fraction: quota.headroom(usage), Known: true}
		}
	}
	states := make([]CapState, len(caps))
	for index := range caps {
		result, err := parseCostReservation(replies[len(quotas)+index])
		switch {
		case err != nil:
		case result.kind == resultGranted:
			states[index] = CapAvailable
		case result.kind == resultRejected:
			states[index] = CapExhausted
		}
	}
	return headroom, states
}

// headroom is the smallest remaining fraction across the quota's dimensions.
func (q QuotaProbe) headroom(usage Usage) float64 {
	fraction := 1.0
	for _, dimension := range [...]struct {
		limit *int64
		used  int64
	}{
		{q.RequestsPerMinute, usage.RequestsThisMinute},
		{q.TokensPerMinute, usage.TokensThisMinute},
		{q.MaxConcurrency, usage.ConcurrentRequests},
	} {
		if dimension.limit != nil && *dimension.limit > 0 {
			fraction = min(fraction, max(0, float64(*dimension.limit-dimension.used)/float64(*dimension.limit)))
		}
	}
	return fraction
}

func optionalCost(limit *string) string {
	if limit == nil {
		return ""
	}
	return *limit
}

// scriptCall is one script invocation of a pipelined read.
type scriptCall struct {
	script script
	keys   []string
	args   []string
}

func (c scriptCall) command(cached bool) []string {
	command := make([]string, 0, 3+len(c.keys)+len(c.args))
	if cached {
		command = append(command, "EVALSHA", c.script.digest)
	} else {
		command = append(command, "EVAL", c.script.source)
	}
	command = append(command, strconv.Itoa(len(c.keys)))
	command = append(command, c.keys...)
	return append(command, c.args...)
}

// evalAll runs read-only script calls in one round trip and returns each reply
// or the error that took its place. Scripts the server has not cached are sent
// again in full, in a second round trip that only a server which has just
// started or failed over needs.
func (l *Limiter) evalAll(ctx context.Context, calls []scriptCall) []any {
	replies := make([]any, len(calls))
	if len(calls) == 0 {
		return replies
	}
	pipeline, ok := l.client.(Pipeliner)
	if !ok {
		for index, call := range calls {
			value, err := l.eval(ctx, call.script, call.keys, call.args...)
			if err != nil {
				replies[index] = err
				continue
			}
			replies[index] = value
		}
		return replies
	}
	commands := make([][]string, len(calls))
	for index, call := range calls {
		commands[index] = call.command(true)
	}
	values, err := pipeline.Pipeline(ctx, commands...)
	if err != nil || len(values) != len(calls) {
		for index := range replies {
			replies[index] = &ServiceError{Err: errors.Join(err, ErrUnexpectedResponse)}
		}
		return replies
	}
	copy(replies, values)
	var missing []int
	for index, value := range values {
		if failure, ok := value.(error); ok && (strings.Contains(failure.Error(), "NOSCRIPT") || strings.Contains(failure.Error(), "NoScriptError")) {
			missing = append(missing, index)
		}
	}
	if len(missing) == 0 {
		return replies
	}
	commands = commands[:0]
	for _, index := range missing {
		commands = append(commands, calls[index].command(false))
	}
	values, err = pipeline.Pipeline(ctx, commands...)
	for position, index := range missing {
		if err != nil || len(values) != len(missing) {
			replies[index] = &ServiceError{Err: errors.Join(err, ErrUnexpectedResponse)}
			continue
		}
		replies[index] = values[position]
	}
	return replies
}

// parseUsage decodes a provider_usage reply.
func parseUsage(value any) (Usage, error) {
	if failure, ok := value.(error); ok {
		return Usage{}, failure
	}
	items, ok := replyItems(value, 3)
	if !ok {
		return Usage{}, ErrUnexpectedResponse
	}
	counters := [3]int64{}
	for index := range counters {
		counter, ok := replyInt(items[index])
		if !ok || !luaSafe(counter) {
			return Usage{}, ErrUnexpectedResponse
		}
		counters[index] = counter
	}
	return Usage{RequestsThisMinute: counters[0], TokensThisMinute: counters[1], ConcurrentRequests: counters[2]}, nil
}

// SupplyBudget is one capped owner as publication records it.
type SupplyBudget struct {
	OwnerID          string
	Kind             string
	DailyCostLimit   *string
	MonthlyCostLimit *string
}

// ReplaceSupplyBudgets records the caps a release declares, inside the
// publishing transaction, so reconciliation keeps their windows current.
func ReplaceSupplyBudgets(ctx context.Context, tx pgx.Tx, budgets []SupplyBudget) error {
	if _, err := tx.Exec(ctx, "DELETE FROM olp.supply_budgets"); err != nil {
		return fmt.Errorf("clear supply budgets: %w", err)
	}
	for _, budget := range budgets {
		if _, err := tx.Exec(ctx, `INSERT INTO olp.supply_budgets(owner_id,owner_kind,daily_cost_limit,monthly_cost_limit)
VALUES($1,$2,$3::text::numeric,$4::text::numeric) ON CONFLICT (owner_id) DO NOTHING`,
			budget.OwnerID, budget.Kind, budget.DailyCostLimit, budget.MonthlyCostLimit); err != nil {
			return fmt.Errorf("record supply budget: %w", err)
		}
	}
	return nil
}

const addSupplyCostDeltaSQL = `WITH budget_calendar AS MATERIALIZED (SELECT * FROM olp.budget_windows($4::timestamptz)),
deltas (window_kind,window_id,accrued,unpriced_attempts) AS (
 VALUES ('day'::text,(SELECT daily_id FROM budget_calendar),$2::text::numeric,0::bigint),
        ('month'::text,(SELECT monthly_id FROM budget_calendar),$2::text::numeric,$3::bigint),
 ('week'::text,(SELECT weekly_id FROM budget_calendar),$2::text::numeric,0::bigint)
), applied AS (
 INSERT INTO olp.supply_cost_windows (owner_id,window_kind,window_id,accrued,unpriced_attempts)
 SELECT $1::uuid,window_kind,window_id,accrued,unpriced_attempts FROM deltas
 ON CONFLICT (owner_id,window_kind,window_id) DO UPDATE SET
   accrued=olp.supply_cost_windows.accrued+EXCLUDED.accrued,
   unpriced_attempts=olp.supply_cost_windows.unpriced_attempts+EXCLUDED.unpriced_attempts
 RETURNING owner_id,window_kind,window_id,accrued,unpriced_attempts,weekly_complete
) SELECT owner_id::text,
 MAX(window_id) FILTER (WHERE window_kind='day')::bigint,
 MAX(accrued) FILTER (WHERE window_kind='day')::text,
 MAX(window_id) FILTER (WHERE window_kind='month')::bigint,
 MAX(accrued) FILTER (WHERE window_kind='month')::text,
 MAX(unpriced_attempts) FILTER (WHERE window_kind='month')::bigint,
 MAX(window_id) FILTER(WHERE window_kind='week')::bigint,
 CASE WHEN bool_or(weekly_complete) FILTER(WHERE window_kind='week') THEN MAX(accrued) FILTER(WHERE window_kind='week')::text ELSE '' END
 , (SELECT daily_start FROM budget_calendar), (SELECT daily_end FROM budget_calendar), (SELECT monthly_start FROM budget_calendar), (SELECT monthly_end FROM budget_calendar), (SELECT weekly_start FROM budget_calendar), (SELECT weekly_end FROM budget_calendar)
FROM applied GROUP BY owner_id`

// AddSupplyCostDelta accumulates one attempt's cost against a supply cap
// owner, a provider connection, credential slot or route, inside the caller's
// transaction and returns the owner's resulting balances.
func AddSupplyCostDelta(ctx context.Context, tx pgx.Tx, ownerID string, observedAt time.Time, cost string, unpriced int64) (CostSnapshot, error) {
	if _, err := uuid.Parse(ownerID); err != nil {
		return CostSnapshot{}, fmt.Errorf("supply owner ID %q is not a UUID", ownerID)
	}
	return addOwnerDelta(ctx, tx, addSupplyCostDeltaSQL, ownerID, observedAt, cost, unpriced)
}

// supplySnapshotsSQL keeps the current windows of every capped owner present,
// prunes windows that have passed and returns the balances. Supply windows
// accrue in the transaction that records each attempt, so PostgreSQL already
// holds the authoritative totals: reconciliation only has to install them.
const supplySnapshotsSQL = `WITH budget_calendar AS MATERIALIZED (SELECT * FROM olp.budget_windows($1::timestamptz)),
pruned AS (
 DELETE FROM olp.supply_cost_windows
 WHERE (window_kind='day' AND window_id<(SELECT daily_id FROM budget_calendar)) OR (window_kind='month' AND window_id<(SELECT monthly_id FROM budget_calendar)) OR (window_kind='week' AND window_id<(SELECT weekly_id FROM budget_calendar)) RETURNING 1
), current AS (
 INSERT INTO olp.supply_cost_windows (owner_id,window_kind,window_id,accrued,unpriced_attempts)
 SELECT b.owner_id,w.kind,w.id,0,0 FROM olp.supply_budgets b
 CROSS JOIN (VALUES ('day'::text,(SELECT daily_id FROM budget_calendar)),('month'::text,(SELECT monthly_id FROM budget_calendar))) AS w(kind,id)
 ON CONFLICT (owner_id,window_kind,window_id) DO UPDATE SET accrued=olp.supply_cost_windows.accrued
 RETURNING owner_id,window_kind,window_id,accrued,unpriced_attempts
) SELECT owner_id::text,(SELECT daily_id FROM budget_calendar),
 MAX(accrued) FILTER (WHERE window_kind='day')::text,(SELECT monthly_id FROM budget_calendar),
 MAX(accrued) FILTER (WHERE window_kind='month')::text,
 MAX(unpriced_attempts) FILTER (WHERE window_kind='month')::bigint,(SELECT weekly_id FROM budget_calendar),''::text
 , (SELECT daily_start FROM budget_calendar), (SELECT daily_end FROM budget_calendar), (SELECT monthly_start FROM budget_calendar), (SELECT monthly_end FROM budget_calendar), (SELECT weekly_start FROM budget_calendar), (SELECT weekly_end FROM budget_calendar)
FROM current GROUP BY owner_id ORDER BY owner_id`

// supplySnapshots returns the durable spend of every capped supply owner.
func supplySnapshots(ctx context.Context, conn *pgx.Conn, now time.Time) ([]CostSnapshot, error) {
	rows, err := conn.Query(ctx, supplySnapshotsSQL, now)
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
	return snapshots, rows.Err()
}
