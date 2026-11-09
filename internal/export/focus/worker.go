package focus

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tyk-swe/olp/internal/export/destination"
	"github.com/tyk-swe/olp/internal/observability/workerhealth"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/usage"
)

const (
	interval    = 60 * time.Second
	passBudget  = 5 * time.Minute
	daysPerSink = 4
	claimLease  = 5 * time.Minute
)

const (
	purgeDirtyDaysSQL = `DELETE FROM olp.focus_dirty_days WHERE day < $1`

	sinksSQL = `SELECT id::text,name,type,format,destination,credential_id::text,project_id::text,filter,settings,created_at
 FROM olp.export_sinks WHERE enabled AND format='csv_gzip' AND 'focus'=ANY(streams) ORDER BY id`

	dueDaysSQL = `SELECT d.day::date FROM generate_series($1::date,$2::date,'1 day') d(day)
 LEFT JOIN olp.focus_datasets fd ON fd.sink_id=$3::uuid AND fd.charge_date=d.day
 WHERE fd.delivered_at IS NULL AND (fd.next_attempt_at IS NULL OR fd.next_attempt_at <= now())
 UNION
 SELECT fd.charge_date FROM olp.focus_datasets fd
 LEFT JOIN olp.focus_dirty_days dd ON dd.day=fd.charge_date
 WHERE fd.sink_id=$3::uuid AND fd.delivered_at IS NOT NULL
  AND fd.delivered_at <= now() - interval '1 hour'
  AND fd.next_attempt_at <= now()
  AND (COALESCE(dd.version,0) IS DISTINCT FROM fd.dirty_version OR ($5::boolean AND fd.commitment_totals IS NULL) OR
   (fd.commitment_totals->>'provisional'='true' AND NOT EXISTS(
    SELECT 1 FROM olp.commitment_periods p WHERE p.bucket_start<fd.charge_date+1 AND p.bucket_end>fd.charge_date
     AND p.bucket_end>now()-interval '2 hours')))
  AND fd.charge_date >= $1::date AND fd.charge_date <= $2::date
 ORDER BY 1 LIMIT $4`

	claimDaySQL = `INSERT INTO olp.focus_datasets (sink_id,charge_date,attempts,next_attempt_at)
 VALUES($1::uuid,$2::date,1,now()+$3::interval)
 ON CONFLICT (sink_id,charge_date) DO UPDATE
 SET attempts=focus_datasets.attempts+1, next_attempt_at=now()+$3::interval
 WHERE focus_datasets.next_attempt_at <= now()
 RETURNING attempts`

	deliveredDaySQL = `UPDATE olp.focus_datasets SET revision=revision+1, dirty_version=$3,
 generated_at=$4, delivered_at=now(), row_count=$5, part_count=$6,
 billed_cost=$7::numeric, effective_cost=$8::numeric, commitment_totals=$10::jsonb,
 last_error_code=NULL, failures=0, next_attempt_at=now()
 WHERE sink_id=$1::uuid AND charge_date=$2::date AND attempts=$9`

	failedDaySQL = `UPDATE olp.focus_datasets SET last_error_code=$3, failures=failures+1,
 next_attempt_at=now() + (LEAST(60 * power(2, failures)::float8, 3600)::text || ' seconds')::interval
 WHERE sink_id=$1::uuid AND charge_date=$2::date AND attempts=$4`
)

type Worker struct {
	Pool         *pgxpool.Pool
	Keys         *secrets.KeyRing
	Installation string
	Sender       *destination.Sender
	Version      string
	Log          *slog.Logger
	Now          func() time.Time

	deliveries sync.Map
}

type outcome string

const (
	delivered outcome = "delivered"
	failed    outcome = "failed"
)

type deliveryKey struct {
	sink    string
	outcome outcome
}

type scope struct {
	project, route *string
}

func (s scope) scoped() bool {
	return s.project != nil || s.route != nil
}

type sink struct {
	id, name   string
	url        string
	credential *string
	spec       destination.FOCUS
	basis      basis
	scope      scope
	first      time.Time
}

func (w *Worker) Run(ctx context.Context) {
	workerhealth.Run(ctx, workerhealth.Loop{
		Task:     workerhealth.TaskFocusExport,
		Interval: interval,
		Budget:   passBudget,
		Health:   w.Pool,
		Log:      w.Log,
		Pass:     w.Pass,
	})
}

func (w *Worker) Pass(ctx context.Context) (bool, error) {
	now := w.Now()
	days, err := usage.RetentionDays(ctx, w.Pool)
	if err != nil {
		return false, err
	}
	retained := firstRetainedDay(now, days)
	if _, err := w.Pool.Exec(ctx, purgeDirtyDaysSQL, retained); err != nil {
		return false, err
	}
	if err := usage.MaintainCommitments(ctx, w.Pool, now, retained); err != nil {
		return false, err
	}
	sinks, err := w.sinks(ctx, retained)
	if err != nil {
		return false, err
	}
	progress := false
	var errs []error
	for _, s := range sinks {
		if ctx.Err() != nil {
			break
		}
		worked, err := w.deliverSink(ctx, s, now)
		progress = progress || worked
		errs = append(errs, err)
	}
	return progress, errors.Join(errs...)
}

func firstRetainedDay(now time.Time, usageDays int) time.Time {
	return now.UTC().AddDate(0, 0, -usageDays).Truncate(24*time.Hour).AddDate(0, 0, 1)
}

func (w *Worker) sinks(ctx context.Context, retained time.Time) ([]sink, error) {
	rows, err := w.Pool.Query(ctx, sinksSQL)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (sink, error) {
		var (
			s                sink
			typ              destination.Type
			format           destination.Format
			project          *string
			filter, settings json.RawMessage
			createdAt        time.Time
		)
		if err := row.Scan(&s.id, &s.name, &typ, &format, &s.url, &s.credential, &project, &filter, &settings, &createdAt); err != nil {
			return sink{}, err
		}
		spec, err := destination.ParseSpec(typ, format, settings)
		if err != nil {
			return sink{}, fmt.Errorf("sink %s: %w", s.id, err)
		}
		focus, ok := spec.(destination.FOCUS)
		if !ok {
			return sink{}, fmt.Errorf("sink %s is not a FOCUS sink", s.id)
		}
		start, err := focus.Start()
		if err != nil {
			return sink{}, fmt.Errorf("sink %s: %w", s.id, err)
		}
		var f destination.Filter
		if err := json.Unmarshal(filter, &f); err != nil {
			return sink{}, fmt.Errorf("sink %s filter: %w", s.id, err)
		}
		s.spec, s.basis = focus, bases[focus.CostBasis]
		s.scope = scope{project: cmp.Or(f.Project, project), route: f.Route}
		s.first = firstDay(createdAt, start, retained)
		return s, nil
	})
}

func (w *Worker) deliverSink(ctx context.Context, s sink, now time.Time) (bool, error) {
	last := now.UTC().Add(-26 * time.Hour).Truncate(24 * time.Hour)
	if last.Before(s.first) {
		return false, nil
	}
	rows, err := w.Pool.Query(ctx, dueDaysSQL, s.first, last, s.id, daysPerSink, !s.basis.billsCallers())
	if err != nil {
		return false, fmt.Errorf("sink %s due days: %w", s.id, err)
	}
	days, err := pgx.CollectRows(rows, pgx.RowTo[time.Time])
	if err != nil {
		return false, fmt.Errorf("sink %s due days: %w", s.id, err)
	}
	progress := false
	var errs []error
	for _, day := range days {
		if ctx.Err() != nil {
			break
		}
		worked, err := w.deliverDay(ctx, s, day.UTC())
		progress = progress || worked
		if err != nil {
			w.Log.Warn("focus day delivery failed", "sink", s.id, "day", day.Format(time.DateOnly), "code", "database")
			errs = append(errs, err)
		}
	}
	return progress, errors.Join(errs...)
}

func (w *Worker) deliverDay(ctx context.Context, s sink, day time.Time) (bool, error) {
	var attempts int
	err := w.Pool.QueryRow(ctx, claimDaySQL, s.id, day, claimLease.String()).Scan(&attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	g, err := w.generateAndSend(ctx, s, day)
	if err != nil {
		w.count(s.id, failed)
		_, err := w.Pool.Exec(ctx, failedDaySQL, s.id, day, errorCode(err), attempts)
		return true, err
	}
	w.count(s.id, delivered)
	_, err = w.Pool.Exec(ctx, deliveredDaySQL, s.id, day, g.dirtyVersion, g.generatedAt,
		g.rowCount, g.partCount, g.sum.billed.String(), g.sum.effective.String(), attempts, g.commitments)
	return true, err
}

func errorCode(err error) string {
	if send, ok := errors.AsType[*destination.SendError](err); ok {
		return send.Code
	}
	return "generation"
}

func (w *Worker) generateAndSend(ctx context.Context, s sink, day time.Time) (generated, error) {
	g, err := w.generate(ctx, s, day)
	if err != nil {
		return generated{}, err
	}
	credential, err := destination.ReadCredential(ctx, w.Pool, w.Keys, w.Installation, s.credential)
	if err != nil {
		return generated{}, &destination.SendError{Code: "credential", Err: err}
	}
	store, err := destination.LoadStore(s.spec, s.url, credential)
	if err != nil {
		return generated{}, err
	}
	return g, w.Sender.PutObjects(ctx, store, g.objects)
}

func (w *Worker) count(sinkID string, o outcome) {
	counter, _ := w.deliveries.LoadOrStore(deliveryKey{sinkID, o}, new(atomic.Int64))
	counter.(*atomic.Int64).Add(1)
}

func firstDay(created time.Time, start *time.Time, retained time.Time) time.Time {
	if start != nil {
		return latest(start.UTC(), retained)
	}
	return latest(created.UTC().Truncate(24*time.Hour), retained)
}

func latest(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
