package export

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/usage"
)

// FocusWorker generates and delivers the daily FOCUS 1.4 datasets a csv_gzip
// sink declares. It claims each due (sink, day) row in focus_datasets, builds
// the parts in one repeatable-read snapshot, uploads parts then the manifest,
// and records the new revision only after the manifest lands.
type FocusWorker struct {
	Pool         *pgxpool.Pool
	Keys         *secrets.KeyRing
	Installation string
	Sender       *Sender
	Log          *slog.Logger
	// Now supplies the pass's clock; tests inject it to advance days.
	Now        func() time.Time
	PassBudget time.Duration

	deliveries sync.Map // map[sinkID] -> *struct{delivered, failed atomic.Int64}
}

// FocusWorkerInterval is the pass cadence.
const FocusWorkerInterval = 60 * time.Second

// FocusPassBudget bounds one pass when no explicit budget is set.
const FocusPassBudget = 5 * time.Minute

const focusDaysPerSink = 4
const focusClaimLease = 5 * time.Minute

func (w *FocusWorker) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *FocusWorker) log() *slog.Logger {
	if w.Log != nil {
		return w.Log
	}
	return slog.New(slog.DiscardHandler)
}

// FocusMetrics is the process-local deliveries counter the metrics endpoint
// renders, keyed by sink and outcome.
func (w *FocusWorker) FocusMetrics() map[[2]string]int64 {
	out := map[[2]string]int64{}
	w.deliveries.Range(func(k, v any) bool {
		counter := v.(*focusDeliveryCounter)
		counter.mu.Lock()
		defer counter.mu.Unlock()
		if counter.delivered > 0 {
			out[[2]string{k.(string), "delivered"}] = counter.delivered
		}
		if counter.failed > 0 {
			out[[2]string{k.(string), "failed"}] = counter.failed
		}
		return true
	})
	return out
}

// Run passes every minute until the context ends, checkpointing task health
// the same way the record-delivery worker does.
func (w *FocusWorker) Run(ctx context.Context) {
	for {
		progress, err := w.Pass(ctx)
		outcome := usage.OutcomeSuccess
		if err != nil {
			outcome = usage.OutcomeFailure
			w.log().Warn("focus export pass failed", "code", "database")
		}
		checkpoint, done := context.WithTimeout(context.WithoutCancel(ctx), boundedWindow)
		if err := usage.CheckpointTask(checkpoint, w.Pool, usage.TaskFocusExport, outcome, progress); err != nil {
			w.log().Warn("focus export checkpoint failed", "code", "database")
		}
		done()
		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(FocusWorkerInterval):
		}
	}
}

// focusDeliveryCounter is the per-sink outcome counter the metric reports.
type focusDeliveryCounter struct {
	mu        sync.Mutex
	delivered int64
	failed    int64
}

func (w *FocusWorker) countDelivery(sinkID, outcome string) {
	holder, _ := w.deliveries.LoadOrStore(sinkID, &focusDeliveryCounter{})
	counter := holder.(*focusDeliveryCounter)
	counter.mu.Lock()
	defer counter.mu.Unlock()
	if outcome == "delivered" {
		counter.delivered++
	} else {
		counter.failed++
	}
}

// Pass runs one due-day scan. It purges dirty rows older than the safe first
// day, then for each enabled csv_gzip sink delivers up to four due days.
func (w *FocusWorker) Pass(ctx context.Context) (bool, error) {
	budget := w.PassBudget
	if budget <= 0 {
		budget = FocusPassBudget
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	now := w.now()
	usageDays, err := sinkUsageDays(ctx, w.Pool)
	if err != nil {
		return false, err
	}
	retentionSafe := now.UTC().AddDate(0, 0, -usageDays).Truncate(24*time.Hour).AddDate(0, 0, 1)
	if _, err := w.Pool.Exec(ctx, `DELETE FROM olp.focus_dirty_days WHERE day < $1`, retentionSafe); err != nil {
		return false, err
	}
	if _, err := w.Pool.Exec(ctx, `SELECT olp.materialize_commitments($1,$2,512)`, now, retentionSafe); err != nil {
		return false, err
	}
	// Backfill retained evidence in bounded batches; drafts cannot create schedules.
	if _, err := w.Pool.Exec(ctx, `WITH batch AS (
  SELECT f.ctid FROM olp.attempt_usage_facts f WHERE f.commitment_schedule_id IS NULL
   AND EXISTS(SELECT 1 FROM olp.commitment_schedules s
    WHERE s.provider_id=f.provider_id AND f.observed_at>=olp.commitment_bucket(s.period,$1::timestamptz) AND s.effective_at<=f.observed_at AND (s.ends_at IS NULL OR s.ends_at>f.observed_at))
  ORDER BY f.observed_at LIMIT 512 FOR UPDATE SKIP LOCKED)
  UPDATE olp.attempt_usage_facts f SET commitment_weight=f.commitment_weight FROM batch WHERE f.ctid=batch.ctid`, retentionSafe); err != nil {
		return false, err
	}
	if _, err := w.Pool.Exec(ctx, `SELECT olp.backfill_commitment_hours(512,$1)`, retentionSafe); err != nil {
		return false, err
	}
	sinks, err := w.focusSinks(ctx)
	if err != nil {
		return false, err
	}
	progress := false
	for _, sink := range sinks {
		if ctx.Err() != nil {
			return progress, nil
		}
		worked, err := w.deliverSink(ctx, sink, now, retentionSafe)
		progress = progress || worked
		if err != nil {
			w.log().Warn("focus sink pass failed", "sink", sink.ID, "code", "database")
		}
	}
	return progress, nil
}

// focusSink is a sink row plus the filter and settings a dataset needs.
type focusSink struct {
	Destination
	Name          string
	ProjectID     *string
	Filter        json.RawMessage
	CreatedAt     time.Time
	costBasis     string
	startDate     *time.Time
	filterProject *string
	filterRoute   *string
}

// focusSinks lists the enabled csv_gzip sinks in a fixed order.
func (w *FocusWorker) focusSinks(ctx context.Context) ([]focusSink, error) {
	rows, err := w.Pool.Query(ctx, `SELECT id::text,name,type,destination,credential_id::text,project_id::text,
		filter,settings,created_at::timestamptz::text
		FROM olp.export_sinks WHERE enabled AND format='csv_gzip' AND 'focus'=ANY(streams) ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []focusSink
	for rows.Next() {
		var s focusSink
		var created string
		if err := rows.Scan(&s.ID, &s.Name, &s.Type, &s.URL, &s.CredentialID, &s.ProjectID, &s.Filter, &s.Settings, &created); err != nil {
			return nil, err
		}
		s.CreatedAt, _ = time.Parse(time.RFC3339, created)
		settings, err := decodeSettings(s.Settings)
		if err != nil {
			return nil, err
		}
		s.costBasis = "cost"
		if settings.CostBasis != nil {
			s.costBasis = *settings.CostBasis
		}
		if settings.StartDate != nil {
			day, err := time.Parse("2006-01-02", *settings.StartDate)
			if err != nil {
				return nil, err
			}
			s.startDate = &day
		}
		var filter struct {
			Project *string `json:"project"`
			Route   *string `json:"route"`
		}
		if len(s.Filter) > 0 {
			if err := json.Unmarshal(s.Filter, &filter); err != nil {
				return nil, err
			}
		}
		s.filterProject, s.filterRoute = filter.Project, filter.Route
		out = append(out, s)
	}
	return out, rows.Err()
}

// dueDays picks a sink's next charge days: eligible days never delivered
// (oldest first), then delivered days whose dirty version changed that are at
// least an hour past delivery.
func (w *FocusWorker) dueDays(ctx context.Context, sink focusSink, now, retentionSafe time.Time) ([]time.Time, error) {
	first := sink.CreatedAt.UTC().Truncate(24 * time.Hour)
	if sink.startDate != nil && sink.startDate.After(first) {
		first = *sink.startDate
	}
	if first.Before(retentionSafe) {
		first = retentionSafe
	}
	last := now.UTC().Add(-2 * time.Hour).Add(-24 * time.Hour).Truncate(24 * time.Hour)
	if last.Before(first) {
		return nil, nil
	}
	rows, err := w.Pool.Query(ctx, `
		SELECT d.day::date::text FROM generate_series($1::date,$2::date,'1 day') d(day)
		LEFT JOIN olp.focus_datasets fd ON fd.sink_id=$3::uuid AND fd.charge_date=d.day
		WHERE fd.delivered_at IS NULL AND (fd.next_attempt_at IS NULL OR fd.next_attempt_at <= now())
		UNION
		SELECT fd.charge_date::text FROM olp.focus_datasets fd
		LEFT JOIN olp.focus_dirty_days dd ON dd.day=fd.charge_date
		WHERE fd.sink_id=$3::uuid AND fd.delivered_at IS NOT NULL
		  AND fd.delivered_at <= now() - interval '1 hour'
		  AND fd.next_attempt_at <= now()
		  AND (COALESCE(dd.version,0) IS DISTINCT FROM fd.dirty_version OR ($5::boolean AND fd.commitment_totals IS NULL) OR
   (fd.commitment_totals->>'provisional'='true' AND NOT EXISTS(
    SELECT 1 FROM olp.commitment_periods p WHERE p.bucket_start<fd.charge_date+1 AND p.bucket_end>fd.charge_date
     AND p.bucket_end>now()-interval '2 hours')))
		  AND fd.charge_date >= $1::date AND fd.charge_date <= $2::date
		ORDER BY 1 LIMIT $4`, first.Format("2006-01-02"), last.Format("2006-01-02"), sink.ID, focusDaysPerSink, sink.costBasis == "cost")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var days []time.Time
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			return nil, err
		}
		day, err := time.Parse("2006-01-02", text)
		if err != nil {
			return nil, err
		}
		days = append(days, day)
	}
	return days, rows.Err()
}

// claimDay takes a day's delivery lease: insert or bump attempts with a fresh
// next_attempt_at. attempts -1 means another worker holds the lease.
func (w *FocusWorker) claimDay(ctx context.Context, sinkID string, day time.Time) (int, error) {
	tx, err := w.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	var attempts int
	err = tx.QueryRow(ctx, `INSERT INTO olp.focus_datasets (sink_id,charge_date,attempts,next_attempt_at)
		VALUES($1::uuid,$2::date,1,now()+$3::interval)
		ON CONFLICT (sink_id,charge_date) DO UPDATE
		SET attempts=focus_datasets.attempts+1, next_attempt_at=now()+$3::interval
		WHERE focus_datasets.next_attempt_at <= now()
		RETURNING attempts`,
		sinkID, day.Format("2006-01-02"), focusClaimLease.String()).Scan(&attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return -1, nil
	}
	if err != nil {
		return 0, err
	}
	return attempts, tx.Commit(ctx)
}

// deliverSink delivers a sink's due days, oldest first.
func (w *FocusWorker) deliverSink(ctx context.Context, sink focusSink, now, retentionSafe time.Time) (bool, error) {
	days, err := w.dueDays(ctx, sink, now, retentionSafe)
	if err != nil {
		return false, err
	}
	progress := false
	for _, day := range days {
		if ctx.Err() != nil {
			return progress, nil
		}
		worked, err := w.deliverDay(ctx, sink, day)
		if err != nil {
			w.log().Warn("focus day delivery failed", "sink", sink.ID, "day", day.Format("2006-01-02"), "code", "database")
			continue
		}
		progress = progress || worked
	}
	return progress, nil
}

// deliverDay claims, generates and delivers one charge day.
func (w *FocusWorker) deliverDay(ctx context.Context, sink focusSink, day time.Time) (bool, error) {
	attempts, err := w.claimDay(ctx, sink.ID, day)
	if err != nil {
		return false, err
	}
	if attempts < 0 {
		return false, nil
	}
	result, sendErr := w.generateAndSend(ctx, sink, day)
	if sendErr == nil {
		w.countDelivery(sink.ID, "delivered")
	} else {
		w.countDelivery(sink.ID, "failed")
	}
	return true, w.finishDay(ctx, sink, day, attempts, result, sendErr)
}

// finishDay records the outcome, guarded by the claimed attempts value so a
// lease that expired mid-send cannot overwrite a newer claim.
func (w *FocusWorker) finishDay(ctx context.Context, sink focusSink, day time.Time, attempts int, result *dayResult, sendErr error) error {
	tx, err := w.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if sendErr == nil && result != nil {
		// attempts is a monotonic claim counter and is never reset — a
		// finisher whose lease lapsed matches no row and writes nothing.
		_, err = tx.Exec(ctx, `UPDATE olp.focus_datasets SET revision=revision+1, dirty_version=$3,
			generated_at=$4, delivered_at=now(), row_count=$5, part_count=$6,
			billed_cost=$7::numeric, effective_cost=$8::numeric, commitment_totals=$10::jsonb,
			last_error_code=NULL, failures=0, next_attempt_at=now()
			WHERE sink_id=$1::uuid AND charge_date=$2::date AND attempts=$9`,
			sink.ID, day.Format("2006-01-02"), result.dirtyVersion, result.generatedAt,
			result.rowCount, result.partCount, result.billed.String(), result.effective.String(), attempts, commitmentTotalsJSON(result.commitments))
	} else {
		// Backoff keys off the failure streak: failures+1 is the new value.
		_, err = tx.Exec(ctx, `UPDATE olp.focus_datasets SET last_error_code=$3, failures=failures+1,
			next_attempt_at=now() + (LEAST(60 * power(2, failures)::float8, 3600)::text || ' seconds')::interval
			WHERE sink_id=$1::uuid AND charge_date=$2::date AND attempts=$4`,
			sink.ID, day.Format("2006-01-02"), focusErrorCode(sendErr), attempts)
	}
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// focusErrorCode maps a delivery failure to its ledger code: transport and
// destination failures carry the sender's code; SQL and encoding failures
// read as `generation`.
func focusErrorCode(err error) string {
	var send *SendError
	if errors.As(err, &send) {
		return send.Code
	}
	return "generation"
}

// generationError wraps SQL or encoding failures so their ledger code reads
// `generation` rather than a transport code.
type generationError struct{ err error }

func (e generationError) Error() string { return e.err.Error() }
func (e generationError) Unwrap() error { return e.err }

// focusObject is one uploadable object: a CSV part or the manifest.
type focusObject struct {
	key         string
	contentType string
	body        []byte
}

// dayResult carries a generated day's objects and totals.
type dayResult struct {
	commitments  *CommitmentTotals
	dirtyVersion int64
	generatedAt  time.Time
	rowCount     int64
	partCount    int
	billed       decimal.Decimal
	effective    decimal.Decimal
	objects      []focusObject
}

// generateAndSend snapshots the day, builds and uploads the parts and the
// manifest, and returns the day's totals. The manifest PUT is last: it is the
// dataset's commit point.
func (w *FocusWorker) generateAndSend(ctx context.Context, sink focusSink, day time.Time) (*dayResult, error) {
	result, err := w.generateDay(ctx, sink, day)
	if err != nil {
		return nil, err
	}
	endpoint, err := w.Sender.policy.ValidateEndpoint(sink.URL)
	if err != nil {
		return nil, sendError("endpoint", err)
	}
	var credential []byte
	if sink.CredentialID != nil {
		credTx, err := w.Pool.Begin(ctx)
		if err != nil {
			return nil, generationError{err}
		}
		credential, err = access.SecretValueFor(ctx, credTx, w.Keys, secrets.SinkCredential, w.Installation, *sink.CredentialID)
		_ = credTx.Rollback(ctx)
		if err != nil {
			return nil, sendError("credential", err)
		}
	}
	for _, part := range result.objects {
		if err := w.Sender.putObject(ctx, endpoint, sink.Destination, credential, part.key, part.contentType, part.body); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func commitmentTotalsJSON(t *CommitmentTotals) any {
	if t == nil {
		return nil
	}
	b, _ := json.Marshal(t)
	return b
}
