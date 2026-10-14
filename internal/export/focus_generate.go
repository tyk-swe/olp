package export

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"fmt"
	"time"

	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/tyk-swe/olp/internal/usage"
)

// focusRowsSQL expands a day's facts into grouped price-line rows. The cost
// variant joins the shared commitment-shares CTE; the charge variant does
// not, since charge rows never allocate commitments.
//
// Binds: $1 day (date text), $2 sink project, $3 filter project, $4 route.
func focusRowsSQL(costBasis string) string {
	basis := `f.estimated_cost IS NOT NULL AND NOT f.caller_credential AND NOT f.budget_exempt`
	fallbackColumn := "cost_lines"
	lines := `COALESCE(f.cost_lines, jsonb_build_array(jsonb_build_object(
		'component','attempt','tier',null,'unit','Requests','quantity','1',
		'list_unit_price', trim_scale(COALESCE(f.list_cost, f.estimated_cost + COALESCE(f.commitment_weight,0)))::text,
		'contracted_unit_price', trim_scale(COALESCE(f.contracted_cost, f.estimated_cost + COALESCE(f.commitment_weight,0)))::text)))`
	shares := `, focus_buckets AS (
		SELECT DISTINCT provider_id, commitment_period, commitment_bucket FROM facts
		WHERE commitment_period IS NOT NULL),
	commitment_shares AS (
		SELECT p.provider_id, p.commitment_period, p.commitment_bucket, w.period_weight, w.latest_amount
		FROM focus_buckets p CROSS JOIN LATERAL (` + usage.CommitmentSharesLateral() + `) w)`
	if costBasis == "charge" {
		basis = `f.charge IS NOT NULL`
		fallbackColumn = "charge_lines"
		lines = `COALESCE(f.charge_lines, jsonb_build_array(jsonb_build_object(
			'component','attempt','tier',null,'unit','Requests','quantity','1',
			'list_unit_price', trim_scale(f.charge)::text,
			'contracted_unit_price', trim_scale(f.charge)::text)))`
		shares = `, commitment_shares AS (SELECT NULL::uuid AS provider_id, NULL::text AS commitment_period,
			NULL::timestamp AS commitment_bucket, NULL::numeric AS period_weight, NULL::numeric AS latest_amount
			WHERE false)`
	}
	return `WITH facts AS (
	SELECT f.attempt_id::text, f.provider_id, f.route_slug, f.api_key_id::text,
		f.upstream_model, f.operation, f.attribution::text,
		f.commitment_period,
		CASE WHEN f.commitment_period IS NULL THEN NULL
			ELSE date_trunc(f.commitment_period, f.observed_at AT TIME ZONE 'UTC') END AS commitment_bucket,
		COALESCE(k.project_id, pr.project_id, rt.project_id)::text AS consumer,
		pj.name AS consumer_name,
		pr.name AS connection_name, pr.kind,
		pr.configuration#>>'{options,vendor_id}' AS vendor_id,
		pr.project_id::text AS provider_project,
		f.` + fallbackColumn + ` IS NULL AS any_fallback,
		` + lines + ` AS lines
	FROM olp.attempt_usage_facts f
	LEFT JOIN olp.api_keys k ON k.id=f.api_key_id
	LEFT JOIN olp.providers pr ON pr.id=f.provider_id
	LEFT JOIN olp.routes rt ON rt.slug=f.route_slug
	LEFT JOIN olp.projects pj ON pj.id=COALESCE(k.project_id, pr.project_id, rt.project_id)
	WHERE f.observed_at >= $1::date AND f.observed_at < ($1::date + 1)::timestamptz
		AND f.charge_status='billable' AND f.usage_complete
		AND ` + basis + `
		AND ($2::text IS NULL OR COALESCE(k.project_id, pr.project_id, rt.project_id)::text = $2)
		AND ($3::text IS NULL OR COALESCE(k.project_id, pr.project_id, rt.project_id)::text = $3)
		AND ($4::text IS NULL OR f.route_slug = $4))` + shares + `
SELECT f.consumer, COALESCE(f.consumer_name,''), f.provider_id::text, COALESCE(f.connection_name,''),
	COALESCE(f.vendor_id,''), f.kind, COALESCE(f.provider_project,''),
	f.upstream_model, f.operation, COALESCE(f.route_slug,''), COALESCE(f.api_key_id,''),
	COALESCE(f.attribution,''),
	(f.commitment_period IS NOT NULL) AS committed,
	COALESCE(f.commitment_period,''), COALESCE(f.commitment_bucket::text,''),
	line->>'component' AS component, line->>'tier' AS tier, line->>'unit' AS unit,
	(line->>'list_unit_price')::numeric AS list_unit_price,
	(line->>'contracted_unit_price')::numeric AS contracted_unit_price,
	trim_scale(SUM((line->>'quantity')::numeric))::text AS quantity,
	trim_scale(SUM((line->>'quantity')::numeric * (line->>'list_unit_price')::numeric))::text AS list_cost,
	trim_scale(SUM((line->>'quantity')::numeric * (line->>'contracted_unit_price')::numeric))::text AS contracted_cost,
	s.period_weight::text, s.latest_amount::text,
	count(DISTINCT f.attempt_id) AS attempts,
	count(DISTINCT f.attempt_id) FILTER (WHERE f.any_fallback) AS fallback_attempts
FROM facts f
CROSS JOIN LATERAL jsonb_array_elements(f.lines) line
LEFT JOIN commitment_shares s ON s.provider_id=f.provider_id AND s.commitment_period=f.commitment_period
	AND s.commitment_bucket=f.commitment_bucket
GROUP BY f.consumer, f.consumer_name, f.provider_id, f.connection_name, f.vendor_id, f.kind,
	f.provider_project, f.upstream_model, f.operation, f.route_slug, f.api_key_id, f.attribution,
	f.commitment_period IS NOT NULL, f.commitment_period, f.commitment_bucket,
	line->>'component', line->>'tier', line->>'unit',
	line->>'list_unit_price', line->>'contracted_unit_price', s.period_weight, s.latest_amount
ORDER BY 1,3,8,9,10,11,12,13,14,15,16,17,18,19,20,21`
}

// focusPurchasesSQL lists the day's commitment purchases: every (provider,
// period, bucket) starting on the day that holds committed facts. The route
// filter suppresses purchases entirely; a project filter pins them to
// providers that project owns.
//
// Binds: $1 day, $2 sink project, $3 filter project.
func focusPurchasesSQL() string {
	return `WITH buckets AS (
	SELECT DISTINCT f.provider_id, f.commitment_period,
		date_trunc(f.commitment_period, f.observed_at AT TIME ZONE 'UTC') AS commitment_bucket
	FROM olp.attempt_usage_facts f
	WHERE f.commitment_period IS NOT NULL
		AND f.observed_at >= $1::date AND f.observed_at < ($1::date + interval '1 month')
		AND date_trunc(f.commitment_period, f.observed_at AT TIME ZONE 'UTC')::date = $1::date),
purchases AS (
	SELECT p.provider_id, p.commitment_period, p.commitment_bucket, w.period_weight, w.latest_amount
	FROM buckets p CROSS JOIN LATERAL (` + usage.CommitmentSharesLateral() + `) w
	WHERE COALESCE(w.period_weight,0) <> 0)
SELECT p.provider_id::text, p.commitment_period, p.commitment_bucket::text, p.latest_amount::text,
	pr.name, pr.configuration#>>'{options,vendor_id}', pr.kind, pr.project_id::text, COALESCE(pj.name,'')
FROM purchases p JOIN olp.providers pr ON pr.id=p.provider_id
LEFT JOIN olp.projects pj ON pj.id=pr.project_id
WHERE ($2::text IS NULL OR pr.project_id::text=$2) AND ($3::text IS NULL OR pr.project_id::text=$3)
ORDER BY 1,2,3`
}

// focusCompletenessSQL counts the day's billable facts in scope by category,
// plus whether any request-metadata ingestion gap overlaps it. The caller-paid
// count is exactly what the basis excludes for being externally billed: priced
// caller or budget-exempt attempts on the cost basis; the charge basis bills
// callers, so nothing is excluded and the count is 0.
//
// Binds: $1 day, $2 sink project, $3 filter project, $4 route.
func focusCompletenessSQL(costBasis, fallbackColumn string) string {
	includedPredicate := `f.estimated_cost IS NOT NULL AND NOT f.caller_credential AND NOT f.budget_exempt`
	callerPaid := `f.usage_complete AND f.estimated_cost IS NOT NULL AND (f.caller_credential OR f.budget_exempt)`
	if costBasis == "charge" {
		includedPredicate = `f.charge IS NOT NULL`
		callerPaid = `false`
	}
	return `SELECT
		count(*) FILTER (WHERE f.usage_complete AND (` + includedPredicate + `)) AS included_attempts,
		count(*) FILTER (WHERE f.usage_complete AND (` + includedPredicate + `) AND f.` + fallbackColumn + ` IS NULL) AS fallback_line_attempts,
		count(*) FILTER (WHERE f.unpriced) AS unpriced_attempts,
		count(*) FILTER (WHERE NOT f.usage_complete) AS incomplete_attempts,
		count(*) FILTER (WHERE f.charge IS NULL) AS uncharged_attempts,
		count(*) FILTER (WHERE ` + callerPaid + `) AS caller_paid_excluded_attempts,
		EXISTS(SELECT 1 FROM olp.request_metadata_ingestion_gaps g
			WHERE g.first_observed_at < ($1::date + 1)::timestamptz AND g.last_observed_at >= $1::date) AS ingestion_gap
	FROM olp.attempt_usage_facts f
	LEFT JOIN olp.api_keys k ON k.id=f.api_key_id
	LEFT JOIN olp.providers pr ON pr.id=f.provider_id
	LEFT JOIN olp.routes rt ON rt.slug=f.route_slug
	WHERE f.observed_at >= $1::date AND f.observed_at < ($1::date + 1)::timestamptz
		AND f.charge_status='billable'
		AND ($2::text IS NULL OR COALESCE(k.project_id, pr.project_id, rt.project_id)::text = $2)
		AND ($3::text IS NULL OR COALESCE(k.project_id, pr.project_id, rt.project_id)::text = $3)
		AND ($4::text IS NULL OR f.route_slug = $4)`
}

// focusAccountingSQL sums the basis's included fact totals for the manifest's
// accounting_totals.
//
// Binds: $1 day, $2 sink project, $3 filter project, $4 route.
func focusAccountingSQL(costBasis string) string {
	predicate := `f.estimated_cost IS NOT NULL AND NOT f.caller_credential AND NOT f.budget_exempt`
	sum := `trim_scale(SUM(f.estimated_cost))::text`
	if costBasis == "charge" {
		predicate = `f.charge IS NOT NULL`
		sum = `trim_scale(SUM(f.charge))::text`
	}
	return `SELECT ` + sum + `
	FROM olp.attempt_usage_facts f
	LEFT JOIN olp.api_keys k ON k.id=f.api_key_id
	LEFT JOIN olp.providers pr ON pr.id=f.provider_id
	LEFT JOIN olp.routes rt ON rt.slug=f.route_slug
	WHERE f.observed_at >= $1::date AND f.observed_at < ($1::date + 1)::timestamptz
		AND f.charge_status='billable' AND f.usage_complete
		AND ` + predicate + `
		AND ($2::text IS NULL OR COALESCE(k.project_id, pr.project_id, rt.project_id)::text = $2)
		AND ($3::text IS NULL OR COALESCE(k.project_id, pr.project_id, rt.project_id)::text = $3)
		AND ($4::text IS NULL OR f.route_slug = $4)`
}

// generateDay reads the day's dataset from one repeatable-read snapshot and
// returns the uploadable objects in upload order (parts first, manifest last)
// plus the ledger totals.
func (w *FocusWorker) generateDay(ctx context.Context, sink focusSink, day time.Time) (*dayResult, error) {
	tx, err := w.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, generationError{err}
	}
	defer tx.Rollback(ctx)

	var dirtyVersion int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT version FROM olp.focus_dirty_days WHERE day=$1::date),0)`,
		day.Format("2006-01-02")).Scan(&dirtyVersion); err != nil {
		return nil, generationError{err}
	}
	// The claimed row already exists; the delivery writes revision+1, so a
	// retry after a failed PUT rewrites the same keys.
	var revision int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT revision FROM olp.focus_datasets
		WHERE sink_id=$1::uuid AND charge_date=$2::date),0)`, sink.ID, day.Format("2006-01-02")).Scan(&revision); err != nil {
		return nil, generationError{err}
	}
	revision++

	var installationID, installationName string
	if err := tx.QueryRow(ctx, `SELECT id::text,name FROM olp.installation WHERE singleton`).Scan(&installationID, &installationName); err != nil {
		return nil, generationError{err}
	}
	var currency string
	if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT btrim(currency) FROM olp.pricing_currency WHERE singleton),'USD')`).Scan(&currency); err != nil {
		return nil, generationError{err}
	}
	fc := &focusContext{
		installationID:   installationID,
		installationName: installationName,
		currency:         currency,
		day:              day.UTC(),
		costBasis:        sink.costBasis,
	}
	fc.monthStart = time.Date(day.Year(), day.Month(), 1, 0, 0, 0, 0, time.UTC)
	fc.monthEnd = fc.monthStart.AddDate(0, 1, 0)

	// The usage rows.
	var csvRows [][]string
	totals := struct{ billed, effective, list, contracted decimal.Decimal }{}
	rows, err := tx.Query(ctx, focusRowsSQL(sink.costBasis), day.Format("2006-01-02"),
		textOrNil(sink.ProjectID), textOrNil(sink.filterProject), textOrNil(sink.filterRoute))
	if err != nil {
		return nil, generationError{err}
	}
	for rows.Next() {
		var r focusRow
		var consumer, consumerName, connectionName, vendorID, kind, providerProject *string
		var model, operation, route, apiKeyID, attribution *string
		var committed bool
		var commitmentPeriod, commitmentBucket, component, unit string
		var tier *string
		var listPrice, contractedPrice string
		var quantity, listCost, contractedCost string
		var periodWeight, latestAmount *string
		if err := rows.Scan(&consumer, &consumerName, &r.providerID, &connectionName,
			&vendorID, &kind, &providerProject,
			&model, &operation, &route, &apiKeyID, &attribution,
			&committed, &commitmentPeriod, &commitmentBucket,
			&component, &tier, &unit, &listPrice, &contractedPrice,
			&quantity, &listCost, &contractedCost, &periodWeight, &latestAmount,
			&r.attempts, &r.fallbackAttempts); err != nil {
			rows.Close()
			return nil, generationError{err}
		}
		r.consumer = deref(consumer)
		r.consumerName = deref(consumerName)
		r.connectionName = deref(connectionName)
		r.vendorID = deref(vendorID)
		r.providerKind = deref(kind)
		r.providerProject = deref(providerProject)
		r.model = deref(model)
		r.operation = deref(operation)
		r.route = deref(route)
		r.apiKeyID = deref(apiKeyID)
		r.attribution = deref(attribution)
		r.committed = committed && sink.costBasis == "cost"
		r.commitmentPeriod = commitmentPeriod
		r.commitmentBucket = commitmentBucket
		r.component = component
		r.tier = tier
		r.unit = unit
		r.listUnitPrice = decimal.RequireFromString(listPrice)
		r.contractedUnitPrice = decimal.RequireFromString(contractedPrice)
		r.quantity = decimal.RequireFromString(quantity)
		r.listCost = decimal.RequireFromString(listCost)
		r.contractedCost = decimal.RequireFromString(contractedCost)
		if periodWeight != nil {
			v := decimal.RequireFromString(*periodWeight)
			r.periodWeight = &v
		}
		if latestAmount != nil {
			v := decimal.RequireFromString(*latestAmount)
			r.latestAmount = &v
		}
		csvRows = append(csvRows, r.render(fc))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, generationError{err}
	}

	// Purchase rows ride the bucket-start day's dataset, cost basis only, and
	// only when the sink's scope admits the provider's project.
	if sink.costBasis == "cost" && sink.filterRoute == nil {
		scopeProject := sink.ProjectID
		if sink.filterProject != nil {
			scopeProject = sink.filterProject
		}
		purchases, err := tx.Query(ctx, focusPurchasesSQL(), day.Format("2006-01-02"),
			textOrNil(scopeProject), textOrNil(scopeProject))
		if err != nil {
			return nil, generationError{err}
		}
		for purchases.Next() {
			var providerID, period, bucket, amount, connectionName string
			var vendorID, kind, providerProject, providerProjectName *string
			if err := purchases.Scan(&providerID, &period, &bucket, &amount,
				&connectionName, &vendorID, &kind, &providerProject, &providerProjectName); err != nil {
				purchases.Close()
				return nil, generationError{err}
			}
			csvRows = append(csvRows, purchaseRow(fc, providerID, connectionName,
				deref(vendorID), deref(kind), deref(providerProject), deref(providerProjectName),
				period, bucket, decimal.RequireFromString(amount)))
		}
		purchases.Close()
		if err := purchases.Err(); err != nil {
			return nil, generationError{err}
		}
	}

	// Completeness counts run over the day's billable facts in scope.
	var included, fallbackLines, unpriced, incomplete, uncharged, callerExcluded int64
	var ingestionGap bool
	fallbackColumn := "cost_lines"
	if sink.costBasis == "charge" {
		fallbackColumn = "charge_lines"
	}
	if err := tx.QueryRow(ctx, focusCompletenessSQL(sink.costBasis, fallbackColumn),
		day.Format("2006-01-02"), textOrNil(sink.ProjectID), textOrNil(sink.filterProject), textOrNil(sink.filterRoute)).
		Scan(&included, &fallbackLines, &unpriced, &incomplete, &uncharged, &callerExcluded, &ingestionGap); err != nil {
		return nil, generationError{err}
	}
	var accounting *string
	if err := tx.QueryRow(ctx, focusAccountingSQL(sink.costBasis), day.Format("2006-01-02"),
		textOrNil(sink.ProjectID), textOrNil(sink.filterProject), textOrNil(sink.filterRoute)).Scan(&accounting); err != nil {
		return nil, generationError{err}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, generationError{err}
	}

	// Totals from the rendered rows keep the manifest aligned with the CSV.
	for _, row := range csvRows {
		billed := cell(row, "BilledCost")
		effective := cell(row, "EffectiveCost")
		list := cell(row, "ListCost")
		contracted := cell(row, "ContractedCost")
		if billed != "" {
			totals.billed = totals.billed.Add(decimal.RequireFromString(billed))
		}
		if effective != "" {
			totals.effective = totals.effective.Add(decimal.RequireFromString(effective))
		}
		if list != "" {
			totals.list = totals.list.Add(decimal.RequireFromString(list))
		}
		if contracted != "" {
			totals.contracted = totals.contracted.Add(decimal.RequireFromString(contracted))
		}
	}

	// Split into parts at the row cap; a header-only day still gets one part.
	objects, parts := buildFocusParts(sink, day, revision, csvRows)
	manifest, err := focusManifest(w, sink, fc, revision, result{
		parts:          parts,
		totals:         totals,
		rowCount:       int64(len(csvRows)),
		included:       included,
		fallbackLines:  fallbackLines,
		unpriced:       unpriced,
		incomplete:     incomplete,
		uncharged:      uncharged,
		callerExcluded: callerExcluded,
		ingestionGap:   ingestionGap,
		accounting:     accounting,
	})
	if err != nil {
		return nil, generationError{err}
	}
	objects = append(objects, focusObject{key: manifestKey(day), contentType: "application/json", body: manifest})
	return &dayResult{
		dirtyVersion: dirtyVersion,
		generatedAt:  w.now().UTC(),
		rowCount:     int64(len(csvRows)),
		partCount:    len(parts),
		billed:       totals.billed,
		effective:    totals.effective,
		objects:      objects,
	}, nil
}

// result is the manifest's assembly input.
type result struct {
	parts                                                                    []focusPart
	totals                                                                   struct{ billed, effective, list, contracted decimal.Decimal }
	rowCount                                                                 int64
	included, fallbackLines, unpriced, incomplete, uncharged, callerExcluded int64
	ingestionGap                                                             bool
	accounting                                                               *string
}

// focusPart is one compressed CSV object and its checksum.
type focusPart struct {
	key       string
	rows      int64
	bytes     int
	sha256hex string
	body      []byte
}

// buildFocusParts renders the dataset's CSV parts: one gzip object per
// FocusPartRowLimit rows, each carrying the full header.
func buildFocusParts(sink focusSink, day time.Time, revision int64, rows [][]string) ([]focusObject, []focusPart) {
	billingPeriod := day.Format("2006-01")
	base := fmt.Sprintf("focus/v1.4/billing_period=%s/charge_date=%s/revision=%04d/",
		billingPeriod, day.Format("2006-01-02"), revision)
	var objects []focusObject
	var parts []focusPart
	chunks := [][][]string{rows}
	if len(rows) > FocusPartRowLimit {
		chunks = nil
		for i := 0; i < len(rows); i += FocusPartRowLimit {
			end := min(i+FocusPartRowLimit, len(rows))
			chunks = append(chunks, rows[i:end])
		}
	}
	for i, chunk := range chunks {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		writer := csv.NewWriter(gz)
		writer.UseCRLF = false
		_ = writer.Write(focusColumns)
		for _, row := range chunk {
			_ = writer.Write(row)
		}
		writer.Flush()
		_ = gz.Close()
		sum := sha256.Sum256(buf.Bytes())
		key := fmt.Sprintf("%spart-%05d.csv.gz", base, i)
		body := buf.Bytes()
		parts = append(parts, focusPart{key: key, rows: int64(len(chunk)), bytes: len(body), sha256hex: hex.EncodeToString(sum[:]), body: body})
		objects = append(objects, focusObject{key: key, contentType: "application/gzip", body: body})
	}
	return objects, parts
}

func manifestKey(day time.Time) string {
	return fmt.Sprintf("focus/v1.4/billing_period=%s/charge_date=%s/manifest.json",
		day.Format("2006-01"), day.Format("2006-01-02"))
}

func textOrNil(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// cell reads one rendered row's field.
func cell(row []string, name string) string {
	i := slices.Index(focusColumns, name)
	if i < 0 {
		return ""
	}
	return row[i]
}
