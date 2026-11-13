package source

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"cloud.google.com/go/bigquery"
	"github.com/shopspring/decimal"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"

	"github.com/tyk-swe/olp/internal/connectors"
)

const (
	googleBigQueryEndpoint = "https://bigquery.googleapis.com"
	googleVertexServiceID  = "C7E2-9256-1C43"
	googleMaximumBytes     = 1 << 30
	googleMaximumRows      = 200000
	googleImportTimeout    = 45 * time.Second
	googleQueriesPerPass   = 3
	googleQueryTimeout     = 20 * time.Second
)

var googleBigQuery = vendor[googleScope, googleCredential, Cloud]{
	family:       "google",
	providerKind: connectors.KindVertexAI,
	scope:        parseGoogleScope,
	input:        googleCredentialInput,
	credential:   parseGoogleCredential,
	request:      Inputs.Cloud,
	read:         readGoogle,
}

var errGoogleAuthentication = errors.New("Google billing authentication failed")

const googleStatement = `WITH snapshot AS (
	SELECT billing_account_id, service, sku, project, usage_start_time, currency, cost, cost_type, credits, adjustment_info, export_time
	FROM %s WHERE _PARTITIONTIME >= TIMESTAMP(@start)
), billing_scope AS (
	SELECT * FROM snapshot WHERE billing_account_id = @billing_account_id%s
), revision AS (
	SELECT MAX(export_time) AS export_time,
	COALESCE(FORMAT_TIMESTAMP('%%Y-%%m-%%dT%%H:%%M:%%E6SZ', MAX(export_time), 'UTC'), '') AS source_revision FROM billing_scope
), scoped AS (
	SELECT * FROM billing_scope WHERE usage_start_time >= TIMESTAMP(@start) AND usage_start_time < TIMESTAMP(@end)
), evidence AS (
	SELECT DISTINCT DATE(usage_start_time, 'UTC') AS usage_day FROM scoped
	WHERE DATE(usage_start_time, 'UTC') < DATE((SELECT export_time FROM revision), 'UTC')
), amounts AS (
	SELECT DATE(usage_start_time, 'UTC') AS usage_day, service.id AS service_id, sku.id AS sku_id, currency,
	CASE WHEN adjustment_info.id IS NOT NULL THEN 'adjustment' WHEN cost_type = 'regular' THEN 'cost' ELSE cost_type END AS line_kind,
	CAST(CAST(cost AS STRING) AS BIGNUMERIC) AS amount FROM scoped
	WHERE service.id = @service_id AND cost_type IN ('regular', 'adjustment', 'rounding error')
	UNION ALL
	SELECT DATE(usage_start_time, 'UTC'), service.id, sku.id, currency,
	CONCAT('credit/', COALESCE(credit.type, 'unknown')), CAST(CAST(credit.amount AS STRING) AS BIGNUMERIC)
	FROM scoped CROSS JOIN UNNEST(credits) AS credit
	WHERE service.id = @service_id AND cost_type IN ('regular', 'adjustment', 'rounding error')
), totals AS (
	SELECT CAST(usage_day AS STRING) AS usage_day, service_id, sku_id, currency, line_kind, CAST(SUM(amount) AS STRING) AS amount
	FROM amounts GROUP BY 1, 2, 3, 4, 5
)
SELECT COALESCE(CAST(evidence.usage_day AS STRING), '') AS usage_day, COALESCE(totals.service_id, '') AS service_id,
	COALESCE(totals.sku_id, '') AS sku_id, COALESCE(totals.currency, '') AS currency,
	COALESCE(totals.amount, '') AS amount, COALESCE(totals.line_kind, '') AS line_kind, revision.source_revision
FROM revision LEFT JOIN evidence ON TRUE LEFT JOIN totals ON totals.usage_day = CAST(evidence.usage_day AS STRING)`

type googleRow struct {
	Day      string `bigquery:"usage_day"`
	Service  string `bigquery:"service_id"`
	SKU      string `bigquery:"sku_id"`
	Currency string `bigquery:"currency"`
	Amount   string `bigquery:"amount"`
	Kind     string `bigquery:"line_kind"`
	Revision string `bigquery:"source_revision"`
}

func (r googleRow) evidenceOnly() bool {
	return r.Kind == "" && r.Service == "" && r.SKU == "" && r.Currency == "" && r.Amount == ""
}

type googleTransport struct {
	base       http.RoundTripper
	auth       *connectors.Auth
	credential googleCredential
}

func (t googleTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	request := req.Clone(req.Context())
	mode, secret := t.credential.googleAuth()
	if _, err := t.auth.ApplyGoogleBigQuery(request.Context(), request, mode, secret); err != nil {
		return nil, errGoogleAuthentication
	}
	response, err := t.base.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	response.Body = http.MaxBytesReader(nil, response.Body, maxBodyBytes)
	return response, nil
}

type googleExport struct {
	client *http.Client
	scope  googleScope
}

func readGoogle(ctx context.Context, req Cloud, scope googleScope, credential googleCredential, emit func(Period) error) (*Resume, error) {
	client, err := req.Client(googleBigQueryEndpoint)
	if err != nil {
		return nil, fail(CodeEndpoint, err)
	}
	ctx, cancel := context.WithTimeout(ctx, googleImportTimeout)
	defer cancel()
	authenticated := *client
	authenticated.Timeout = RequestTimeout
	authenticated.Transport = googleTransport{base: cmp.Or[http.RoundTripper](client.Transport, http.DefaultTransport), auth: req.Auth, credential: credential}
	export := googleExport{client: &authenticated, scope: scope}
	return walkMonths(req, pendingMonths(req), googleQueriesPerPass, func(month time.Time) error {
		return export.month(ctx, req, month, emit)
	})
}

func (e googleExport) month(ctx context.Context, req Cloud, month time.Time, emit func(Period) error) error {
	start := slices.MaxFunc([]time.Time{month, req.Earliest}, time.Time.Compare)
	end := slices.MinFunc([]time.Time{month.AddDate(0, 1, 0), req.Today}, time.Time.Compare)
	if !start.Before(end) {
		return nil
	}
	rows, err := e.query(ctx, start, end)
	if err != nil {
		return err
	}
	watermark, err := googleWatermark(rows)
	if err != nil {
		return err
	}
	days, err := googleDays(rows, start, end, day(watermark))
	if err != nil {
		return err
	}
	return emit(Period{Days: days, Revision: &Revision{
		PeriodStart: month,
		ID:          watermark.UTC().Format(time.RFC3339Nano),
		OrderingAt:  watermark,
		Replaces:    &Window{From: start, To: slices.MinFunc([]time.Time{end, day(watermark)}, time.Time.Compare)},
	}})
}

func (e googleExport) query(ctx context.Context, start, end time.Time) ([]googleRow, error) {
	ctx, cancel := context.WithTimeout(ctx, googleQueryTimeout)
	defer cancel()
	bq, err := bigquery.NewClient(ctx, e.scope.BigQueryProjectID, option.WithHTTPClient(e.client))
	if err != nil {
		return nil, googleFailure(ctx, err)
	}
	defer bq.Close()
	table := "`" + e.scope.BigQueryProjectID + "." + e.scope.DatasetID + "." + e.scope.TableID + "`"
	query := bq.Query(fmt.Sprintf(googleStatement, table, googleProjectFilter(e.scope.ProjectIDs)))
	query.MaxBytesBilled = googleMaximumBytes
	query.JobTimeout = googleQueryTimeout
	query.Parameters = googleParameters(e.scope, start, end)
	job, err := query.Run(ctx)
	if err != nil {
		return nil, googleFailure(ctx, err)
	}
	defer func() {
		if ctx.Err() == nil {
			return
		}
		cancelCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer stop()
		_ = job.Cancel(cancelCtx)
	}()
	rows, err := job.Read(ctx)
	if err != nil {
		return nil, googleFailure(ctx, err)
	}
	rows.PageInfo().MaxSize = 100
	return googleCollect(ctx, rows)
}

func googleProjectFilter(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	return " AND project.id IN UNNEST(@project_ids)"
}

func googleParameters(scope googleScope, start, end time.Time) []bigquery.QueryParameter {
	parameters := []bigquery.QueryParameter{
		{Name: "billing_account_id", Value: scope.BillingAccountID},
		{Name: "service_id", Value: googleVertexServiceID},
		{Name: "start", Value: start.Format(time.DateOnly)},
		{Name: "end", Value: end.Format(time.DateOnly)},
	}
	if len(scope.ProjectIDs) == 0 {
		return parameters
	}
	return append(parameters, bigquery.QueryParameter{Name: "project_ids", Value: scope.ProjectIDs})
}

func googleCollect(ctx context.Context, rows *bigquery.RowIterator) ([]googleRow, error) {
	var collected []googleRow
	for {
		var row googleRow
		err := rows.Next(&row)
		if errors.Is(err, iterator.Done) {
			return collected, nil
		}
		if err != nil {
			return nil, googleFailure(ctx, err)
		}
		if len(collected) >= googleMaximumRows {
			return nil, fail(CodeResponse, errors.New("Google query returned too many rows"))
		}
		collected = append(collected, row)
	}
}

func googleWatermark(rows []googleRow) (time.Time, error) {
	if len(rows) == 0 {
		return time.Time{}, fail(CodeResponse, errors.New("Google query omitted its revision"))
	}
	first, err := time.Parse(time.RFC3339Nano, rows[0].Revision)
	if err != nil {
		return time.Time{}, fail(CodeStatus, errors.New("Google export has no valid watermark"))
	}
	for _, row := range rows {
		observed, err := time.Parse(time.RFC3339Nano, row.Revision)
		if err != nil {
			return time.Time{}, fail(CodeStatus, errors.New("Google export has no valid watermark"))
		}
		if !observed.Equal(first) {
			return time.Time{}, fail(CodeResponse, errors.New("Google query returned inconsistent watermarks"))
		}
	}
	return first, nil
}

func googleDays(rows []googleRow, start, end, watermark time.Time) (Days, error) {
	days := newDays()
	for _, row := range rows {
		if row.evidenceOnly() && row.Day == "" {
			continue
		}
		usage, err := time.Parse(time.DateOnly, row.Day)
		if err != nil || !usage.Before(watermark) || row.evidenceOnly() && (usage.Before(start) || !usage.Before(end)) {
			return Days{}, fail(CodeResponse, errors.New("invalid Google coverage day"))
		}
		if row.evidenceOnly() {
			days.include(usage)
			continue
		}
		if err := googleLine(days, row, usage, start, end); err != nil {
			return Days{}, err
		}
	}
	return days, nil
}

func googleLine(days Days, row googleRow, usage, start, end time.Time) error {
	if usage.Before(start) || !usage.Before(end) || row.Service != googleVertexServiceID || row.SKU == "" {
		return fail(CodeResponse, errors.New("invalid Google query dimensions"))
	}
	amount, err := decimal.NewFromString(row.Amount)
	if err != nil {
		return fail(CodeResponse, errors.New("invalid Google query amount"))
	}
	if row.Kind != "cost" && row.Kind != "adjustment" && row.Kind != "rounding error" && !strings.HasPrefix(row.Kind, "credit/") {
		return fail(CodeResponse, errors.New("invalid Google query line kind"))
	}
	return days.add(usage, googleLabel(row), row.Currency, amount)
}

func googleLabel(row googleRow) string {
	if row.Kind == "cost" {
		return row.Service + "/" + row.SKU
	}
	return row.Service + "/" + row.SKU + "/" + row.Kind
}

func googleFailure(ctx context.Context, err error) error {
	if errors.Is(err, errGoogleAuthentication) {
		return fail(CodeCredential, errGoogleAuthentication)
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fail(CodeTimeout, errors.New("Google query timed out"))
	}
	if apiError, ok := errors.AsType[*googleapi.Error](err); ok {
		if apiError.Code == http.StatusUnauthorized || apiError.Code == http.StatusForbidden {
			return fail(CodeCredential, errors.New("Google query was unauthorized"))
		}
		return fail(CodeStatus, errors.New("Google query failed"))
	}
	if networkError, ok := errors.AsType[net.Error](err); ok {
		if networkError.Timeout() {
			return fail(CodeTimeout, errors.New("Google query timed out"))
		}
		return fail(CodeNetwork, errors.New("Google query could not be reached"))
	}
	return fail(CodeResponse, errors.New("invalid Google query response"))
}
