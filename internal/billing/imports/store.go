package imports

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/billing/reconciliation"
	"github.com/tyk-swe/olp/internal/billing/source"
	"github.com/tyk-swe/olp/internal/providers"
)

const (
	claimLease   = 10 * time.Minute
	successDelay = time.Hour
	baseBackoff  = 5 * time.Minute
	maxBackoff   = time.Hour
	purgeBatch   = 5000
	purgeBatches = 20
)

type record struct {
	ProviderID          uuid.UUID
	Source              source.Kind
	CredentialID        *string
	Scope               json.RawMessage
	Enabled             bool
	ETag                string
	NextImportAt        *time.Time
	LastImportAt        *time.Time
	LastSuccessAt       *time.Time
	LastErrorCode       *string
	ConsecutiveFailures int
	ImportedFrom        *time.Time
	ImportedThrough     *time.Time
	SourceRevision      *string
	ResumePeriod        *time.Time
	ResumeRemaining     int
}

func (r record) resume() *source.Resume {
	if r.ResumePeriod == nil {
		return nil
	}
	return &source.Resume{Period: *r.ResumePeriod, Remaining: r.ResumeRemaining}
}

const (
	columns = `provider_id,source,credential_id::text,scope,enabled,etag::text,
		next_import_at,last_import_at,last_success_at,last_error_code,consecutive_failures,
		imported_from,imported_through,source_revision,resume_period,resume_remaining`
	selectRecord = `SELECT ` + columns + ` FROM olp.provider_billing_imports WHERE provider_id=$1::uuid`
	claimRecord  = `UPDATE olp.provider_billing_imports SET next_import_at=now()+$1::interval
		WHERE provider_id IN (
			SELECT provider_id FROM olp.provider_billing_imports
			WHERE enabled AND next_import_at <= now()
			ORDER BY next_import_at LIMIT 1 FOR UPDATE SKIP LOCKED
		)
		RETURNING ` + columns
)

func collectOne[T any](rows pgx.Rows, err error) (T, bool, error) {
	if err != nil {
		var zero T
		return zero, false, err
	}
	value, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByPos[T])
	if errors.Is(err, pgx.ErrNoRows) {
		return value, false, nil
	}
	return value, err == nil, err
}

func readRecord(ctx context.Context, q access.Queryer, providerID string) (record, bool, error) {
	return collectOne[record](q.Query(ctx, selectRecord, providerID))
}

func lockRecord(ctx context.Context, tx pgx.Tx, providerID string) (record, bool, error) {
	return collectOne[record](tx.Query(ctx, selectRecord+` FOR UPDATE`, providerID))
}

func claim(ctx context.Context, pool *pgxpool.Pool) (record, bool, error) {
	return collectOne[record](pool.Query(ctx, claimRecord, claimLease))
}

type provider struct {
	ETag          string
	ProjectID     *string
	Configuration providers.Configuration
}

const selectProvider = `SELECT etag::text,project_id::text,configuration FROM olp.providers WHERE id=$1::uuid`

func readProvider(ctx context.Context, q access.Queryer, providerID string) (provider, bool, error) {
	return collectOne[provider](q.Query(ctx, selectProvider, providerID))
}

func lockProvider(ctx context.Context, tx pgx.Tx, providerID string) (provider, bool, error) {
	return collectOne[provider](tx.Query(ctx, selectProvider+` FOR UPDATE`, providerID))
}

func completedMonths(ctx context.Context, q access.Queryer, r record, scope source.Scope, earliest, today time.Time) ([]time.Time, error) {
	digest := scope.Digest()
	rows, err := q.Query(ctx, `SELECT period_start FROM olp.provider_billing_revisions
		WHERE provider_id=$1 AND source=$2 AND scope_digest=$3
		AND period_start < date_trunc('month', $5::date) - interval '1 month'
		AND covered_from <= GREATEST(period_start, $4::date)
		AND covered_through >= (period_start + interval '1 month' - interval '1 day')::date`,
		r.ProviderID, r.Source, digest[:], earliest, today)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[time.Time])
}

func storePeriod(ctx context.Context, pool *pgxpool.Pool, r record, scope source.Scope, period source.Period) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	current, err := holdsImport(ctx, tx, r)
	if err != nil || !current {
		return err
	}
	revision, fresh, err := applyRevision(ctx, tx, r, scope, period)
	if err != nil {
		return err
	}
	if fresh {
		if err := writeDays(ctx, tx, r, scope, period, revision); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE olp.provider_billing_imports SET
		last_import_at=now(),last_success_at=now(),last_error_code=NULL,
		consecutive_failures=0,next_import_at=now()+$3::interval,updated_at=now(),
		source_revision=COALESCE($2,source_revision)
		WHERE provider_id=$1`, r.ProviderID, revision, successDelay); err != nil {
		return err
	}
	if err := refreshCoverage(ctx, tx, r.ProviderID.String(), r.Source, scope.JSON()); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func holdsImport(ctx context.Context, tx pgx.Tx, r record) (bool, error) {
	var etag string
	err := tx.QueryRow(ctx, `SELECT i.etag::text FROM olp.providers p
		JOIN olp.provider_billing_imports i ON i.provider_id=p.id
		WHERE p.id=$1 FOR KEY SHARE OF p FOR UPDATE OF i`, r.ProviderID).Scan(&etag)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return etag == r.ETag, err
}

func applyRevision(ctx context.Context, tx pgx.Tx, r record, scope source.Scope, period source.Period) (*string, bool, error) {
	revision := period.Revision
	if revision == nil {
		return nil, true, nil
	}
	digest := scope.Digest()
	from, through := period.Coverage()
	tag, err := tx.Exec(ctx, `INSERT INTO olp.provider_billing_revisions
		(provider_id,source,scope_digest,period_start,source_revision,ordering_at,covered_from,covered_through)
		VALUES($1,$2,$3,$4::date,$5,$6,$7::date,$8::date)
		ON CONFLICT (provider_id,source,scope_digest,period_start) DO UPDATE SET
			source_revision=EXCLUDED.source_revision,
			ordering_at=CASE WHEN olp.provider_billing_revisions.source_revision=EXCLUDED.source_revision
				THEN olp.provider_billing_revisions.ordering_at ELSE EXCLUDED.ordering_at END,
			covered_from=CASE WHEN olp.provider_billing_revisions.source_revision=EXCLUDED.source_revision AND NOT $9
				THEN LEAST(olp.provider_billing_revisions.covered_from,EXCLUDED.covered_from) ELSE EXCLUDED.covered_from END,
			covered_through=CASE WHEN olp.provider_billing_revisions.source_revision=EXCLUDED.source_revision AND NOT $9
				THEN GREATEST(olp.provider_billing_revisions.covered_through,EXCLUDED.covered_through) ELSE EXCLUDED.covered_through END
		WHERE olp.provider_billing_revisions.source_revision=EXCLUDED.source_revision
			OR olp.provider_billing_revisions.ordering_at < EXCLUDED.ordering_at`,
		r.ProviderID, r.Source, digest[:], revision.PeriodStart, revision.ID, revision.OrderingAt, from, through, revision.Replaces != nil)
	if err != nil || tag.RowsAffected() == 0 {
		return nil, false, err
	}
	return &revision.ID, true, nil
}

type invoicedLine struct {
	day  time.Time
	line source.Line
}

func writeDays(ctx context.Context, tx pgx.Tx, r record, scope source.Scope, period source.Period, revision *string) error {
	days := period.Days.Days()
	if replaces := period.Revision; replaces != nil && replaces.Replaces != nil {
		if _, err := tx.Exec(ctx, `WITH omitted AS (
				DELETE FROM olp.provider_billing_days WHERE provider_id=$1
				AND day>=$2::date AND day<$3::date AND day <> ALL(COALESCE($4::date[], '{}'))
				RETURNING day
			)
			DELETE FROM olp.provider_invoiced_costs WHERE provider_id=$1 AND day IN (SELECT day FROM omitted)`,
			r.ProviderID, replaces.Replaces.From, replaces.Replaces.To, days); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO olp.provider_billing_days(provider_id,day,source,scope,last_success_at)
		SELECT $1,day,$2,$3::jsonb,now() FROM unnest($4::date[]) AS day
		ON CONFLICT (provider_id,day) DO UPDATE SET source=EXCLUDED.source,scope=EXCLUDED.scope,last_success_at=EXCLUDED.last_success_at`,
		r.ProviderID, r.Source, scope.JSON(), days); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM olp.provider_invoiced_costs WHERE provider_id=$1 AND day=ANY($2::date[])`, r.ProviderID, days); err != nil {
		return err
	}
	lines := slices.Collect(func(yield func(invoicedLine) bool) {
		for _, day := range days {
			for _, line := range period.Days.Lines(day) {
				if !yield(invoicedLine{day: day, line: line}) {
					return
				}
			}
		}
	})
	_, err := tx.Exec(ctx, `INSERT INTO olp.provider_invoiced_costs(provider_id,day,line_item,currency,amount,source,source_revision)
		SELECT $1,day,item,currency,amount::numeric,$2,$3
		FROM unnest($4::date[],$5::text[],$6::text[],$7::text[]) AS line(day,item,currency,amount)`,
		r.ProviderID, r.Source, revision,
		pluck(lines, func(l invoicedLine) time.Time { return l.day }),
		pluck(lines, func(l invoicedLine) string { return l.line.Item }),
		pluck(lines, func(l invoicedLine) string { return l.line.Currency }),
		pluck(lines, func(l invoicedLine) string { return l.line.Amount.String() }))
	return err
}

func pluck[T, V any](items []T, field func(T) V) []V {
	return slices.Collect(func(yield func(V) bool) {
		for _, item := range items {
			if !yield(field(item)) {
				return
			}
		}
	})
}

func refreshCoverage(ctx context.Context, tx pgx.Tx, providerID string, kind source.Kind, scope json.RawMessage) error {
	_, err := tx.Exec(ctx, `WITH spans AS (
		SELECT unnest(range_agg(daterange(day,day,'[]'))) AS span
		FROM olp.provider_billing_days
		WHERE provider_id=$1::uuid AND source=$2 AND scope=$3::jsonb
	), latest AS (
		SELECT lower(span) AS first_day,upper(span)-1 AS last_day
		FROM spans ORDER BY upper(span) DESC LIMIT 1
	)
	UPDATE olp.provider_billing_imports SET
		imported_from=(SELECT first_day FROM latest),imported_through=(SELECT last_day FROM latest)
	WHERE provider_id=$1::uuid`, providerID, kind, scope)
	return err
}

func recordResume(ctx context.Context, pool *pgxpool.Pool, r record, resume source.Resume, interval time.Duration) error {
	_, err := pool.Exec(ctx, `UPDATE olp.provider_billing_imports SET
		resume_period=$3::date,resume_remaining=$4,next_import_at=now()+$5::interval,
		last_error_code=NULL,consecutive_failures=0,updated_at=now()
		WHERE provider_id=$1 AND etag=$2::uuid`, r.ProviderID, r.ETag, resume.Period, resume.Remaining, interval)
	return err
}

func clearResume(ctx context.Context, pool *pgxpool.Pool, r record) error {
	_, err := pool.Exec(ctx, `UPDATE olp.provider_billing_imports SET resume_period=NULL,resume_remaining=0
		WHERE provider_id=$1 AND etag=$2::uuid`, r.ProviderID, r.ETag)
	return err
}

func backoff(failures int) time.Duration {
	return min(baseBackoff<<min(failures-1, 5), maxBackoff)
}

func recordFailure(ctx context.Context, pool *pgxpool.Pool, r record, code source.Code) error {
	failures := r.ConsecutiveFailures + 1
	_, err := pool.Exec(ctx, `UPDATE olp.provider_billing_imports SET
		last_import_at=now(),last_error_code=$2,consecutive_failures=$3,resume_period=NULL,resume_remaining=0,
		next_import_at=now()+$4::interval,updated_at=now()
		WHERE provider_id=$1 AND etag=$5::uuid`, r.ProviderID, code, failures, backoff(failures), r.ETag)
	return err
}

func purge(ctx context.Context, pool *pgxpool.Pool, cutoff time.Time) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, query := range []string{
		`UPDATE olp.provider_billing_imports SET
			imported_from=CASE WHEN imported_through < $1::date THEN NULL ELSE GREATEST(imported_from,$1::date) END,
			imported_through=CASE WHEN imported_through < $1::date THEN NULL ELSE imported_through END
			WHERE imported_from < $1::date`,
		`UPDATE olp.provider_billing_contexts SET
			imported_from=CASE WHEN imported_through < $1::date THEN NULL ELSE GREATEST(imported_from,$1::date) END,
			imported_through=CASE WHEN imported_through < $1::date THEN NULL ELSE imported_through END
			WHERE imported_from < $1::date`,
		`DELETE FROM olp.provider_billing_days WHERE day < $1::date`,
		`UPDATE olp.provider_billing_revisions SET
			covered_from=CASE WHEN covered_through < $1::date THEN NULL ELSE GREATEST(covered_from,$1::date) END,
			covered_through=CASE WHEN covered_through < $1::date THEN NULL ELSE covered_through END
			WHERE covered_from < $1::date`,
	} {
		if _, err := tx.Exec(ctx, query, cutoff); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	for range purgeBatches {
		tag, err := pool.Exec(ctx, `DELETE FROM olp.provider_invoiced_costs WHERE ctid IN (
			SELECT ctid FROM olp.provider_invoiced_costs WHERE day < $1::date LIMIT $2)`, cutoff, purgeBatch)
		if err != nil || tag.RowsAffected() < purgeBatch {
			return err
		}
	}
	return nil
}

func scheduleNow(ctx context.Context, tx pgx.Tx, providerID, etag string) (bool, error) {
	tag, err := tx.Exec(ctx, `UPDATE olp.provider_billing_imports
		SET next_import_at=now(),etag=$2,updated_at=now() WHERE provider_id=$1::uuid`, providerID, etag)
	return tag.RowsAffected() > 0, err
}

func invoiced(ctx context.Context, q access.Queryer, providerID string, window reconciliation.Range) ([]invoicedCost, error) {
	rows, err := q.Query(ctx, `SELECT amount::text,btrim(currency),day,fetched_at,line_item,source
		FROM olp.provider_invoiced_costs
		WHERE provider_id=$1::uuid AND day>=$2::date AND day<$3::date
		ORDER BY day,line_item,currency`, providerID, window.Start, window.End)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (invoicedCost, error) {
		var cost invoicedCost
		err := row.Scan(&cost.Amount, &cost.Currency, &cost.Day.Time, &cost.FetchedAt, &cost.LineItem, &cost.Source)
		return cost, err
	})
}
