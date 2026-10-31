package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
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
	googleQueryTimeout     = 20 * time.Second
)

var errGoogleAuthentication = errors.New("Google billing authentication failed")

type googleQueryTransport struct {
	base       http.RoundTripper
	auth       *connectors.Auth
	credential googleCredential
}

func (transport googleQueryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	request := req.Clone(req.Context())
	if _, err := transport.auth.ApplyGoogleBigQuery(request.Context(), request, transport.credential.AuthMode, transport.credential.ServiceAccount); err != nil {
		return nil, errGoogleAuthentication
	}
	response, err := transport.base.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	response.Body = http.MaxBytesReader(nil, response.Body, maxBodyBytes)
	return response, nil
}

type googleInvoiceRow struct {
	Day      string `bigquery:"usage_day"`
	Service  string `bigquery:"service_id"`
	SKU      string `bigquery:"sku_id"`
	Currency string `bigquery:"currency"`
	Amount   string `bigquery:"amount"`
	Kind     string `bigquery:"line_kind"`
	Revision string `bigquery:"source_revision"`
}

func googleProjectFilter(ids []string) (string, []bigquery.QueryParameter) {
	if len(ids) == 0 {
		return "", nil
	}
	return " AND project.id IN UNNEST(@project_ids)", []bigquery.QueryParameter{{Name: "project_ids", Value: ids}}
}

func (i *Importer) importGoogle(ctx context.Context, row *importRow, client *http.Client, auth *connectors.Auth, key []byte) error {
	importCtx, cancel := context.WithTimeout(ctx, googleImportTimeout)
	defer cancel()
	today := i.now().UTC().Truncate(24 * time.Hour)
	var retention int
	if err := i.Pool.QueryRow(importCtx, `SELECT COALESCE((SELECT value::int FROM olp.settings WHERE key='retention.usage_days'),90)`).Scan(&retention); err != nil {
		return err
	}
	earliest := today.AddDate(0, 0, -retention)
	current := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC)
	historical, phase, err := i.nextGoogleHistoricalPeriod(importCtx, row, earliest, current)
	if err != nil {
		return err
	}
	periods := googlePeriodOrder(historical, current, phase)
	return i.importGooglePeriods(importCtx, row, client, auth, key, earliest, today, periods[:])
}

func (i *Importer) nextGoogleHistoricalPeriod(ctx context.Context, row *importRow, earliest, current time.Time) (time.Time, int, error) {
	first := time.Date(earliest.Year(), earliest.Month(), 1, 0, 0, 0, 0, time.UTC)
	last := current.AddDate(0, -2, 0)
	var cursor *time.Time
	var phase int
	if err := i.Pool.QueryRow(ctx, `SELECT google_next_period, google_period_phase FROM olp.provider_billing_imports
		WHERE provider_id=$1::uuid AND etag=$2::uuid`, row.ProviderID, row.ETag).Scan(&cursor, &phase); err != nil {
		return time.Time{}, 0, err
	}
	selected := googlePeriodCursor(cursor, first, last)
	prior := selected.AddDate(0, -1, 0)
	next := googlePeriodCursor(&prior, first, last)
	if _, err := i.Pool.Exec(ctx, `UPDATE olp.provider_billing_imports SET google_next_period=$3::date, google_period_phase=$4
		WHERE provider_id=$1::uuid AND etag=$2::uuid`, row.ProviderID, row.ETag, googlePeriodDate(next), (phase+1)%3); err != nil {
		return time.Time{}, 0, err
	}
	return selected, phase, nil
}

func googlePeriodDate(value time.Time) *string {
	if value.IsZero() {
		return nil
	}
	date := value.Format(dayFormat)
	return &date
}

func googlePeriodOrder(historical, current time.Time, phase int) [3]time.Time {
	previous := current.AddDate(0, -1, 0)
	switch phase {
	case 1:
		return [3]time.Time{current, historical, previous}
	case 2:
		return [3]time.Time{previous, historical, current}
	default:
		return [3]time.Time{historical, previous, current}
	}
}

func (i *Importer) importGooglePeriods(ctx context.Context, row *importRow, client *http.Client, auth *connectors.Auth, key []byte, earliest, today time.Time, periods []time.Time) error {
	if len(periods) == 0 {
		return nil
	}
	return errors.Join(
		i.importGooglePeriod(ctx, row, client, auth, key, earliest, today, periods[0]),
		i.importGooglePeriods(ctx, row, client, auth, key, earliest, today, periods[1:]),
	)
}

func googlePeriodCursor(cursor *time.Time, first, last time.Time) time.Time {
	if last.Before(first) {
		return time.Time{}
	}
	if cursor == nil || cursor.Before(first) || cursor.After(last) {
		return last
	}
	return cursor.UTC()
}

func googlePeriodBounds(month, earliest, today time.Time) (time.Time, time.Time) {
	end := month.AddDate(0, 1, 0)
	if month.Before(earliest) {
		if today.Before(end) {
			return earliest, today
		}
		return earliest, end
	}
	if today.Before(end) {
		return month, today
	}
	return month, end
}

func (i *Importer) importGooglePeriod(ctx context.Context, row *importRow, client *http.Client, auth *connectors.Auth, key []byte, earliest, today, month time.Time) error {
	if month.IsZero() {
		return nil
	}
	start, end := googlePeriodBounds(month, earliest, today)
	if !start.Before(end) {
		return nil
	}
	days, revision, err := fetchGoogle(ctx, client, auth, row.Scope, key, start, end)
	if err != nil {
		return err
	}
	return i.storeDays(ctx, row, month.Format(dayFormat), revision, days)
}

func fetchGoogle(ctx context.Context, client *http.Client, auth *connectors.Auth, rawScope json.RawMessage, rawCredential []byte, start, end time.Time) (map[string]map[lineKey]lineSum, *sourceRevision, error) {
	var scope googleScope
	if err := json.Unmarshal(rawScope, &scope); err != nil {
		return nil, nil, failFetch("response", errors.New("invalid Google billing scope"))
	}
	var credential googleCredential
	if err := json.Unmarshal(rawCredential, &credential); err != nil {
		return nil, nil, failFetch("credential", errors.New("invalid Google billing credential"))
	}
	base := client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	authenticated := *client
	authenticated.Transport = googleQueryTransport{base: base, auth: auth, credential: credential}
	queryCtx, cancel := context.WithTimeout(ctx, googleQueryTimeout)
	defer cancel()
	bq, err := bigquery.NewClient(queryCtx, scope.BigQueryProjectID, option.WithHTTPClient(&authenticated))
	if err != nil {
		return nil, nil, googleQueryError(queryCtx, err)
	}
	defer bq.Close()
	projectFilter, projectParameters := googleProjectFilter(scope.ProjectIDs)
	statement := fmt.Sprintf(`WITH snapshot AS (
		SELECT billing_account_id, service, sku, project, usage_start_time, currency, cost, cost_type, credits, adjustment_info, export_time
		FROM %s WHERE _PARTITIONTIME >= TIMESTAMP(@start)
	), revision AS (
		SELECT MAX(export_time) AS export_time,
		COALESCE(FORMAT_TIMESTAMP('%%Y-%%m-%%dT%%H:%%M:%%E6SZ', MAX(export_time), 'UTC'), '') AS source_revision FROM snapshot
	), scoped AS (
		SELECT * FROM snapshot WHERE billing_account_id = @billing_account_id
		AND usage_start_time >= TIMESTAMP(@start) AND usage_start_time < TIMESTAMP(@end)%s
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
	FROM revision LEFT JOIN evidence ON TRUE LEFT JOIN totals ON totals.usage_day = CAST(evidence.usage_day AS STRING)`, "`"+scope.BigQueryProjectID+"."+scope.DatasetID+"."+scope.TableID+"`", projectFilter)
	query := bq.Query(statement)
	query.MaxBytesBilled = googleMaximumBytes
	query.JobTimeout = googleQueryTimeout
	query.Parameters = append([]bigquery.QueryParameter{
		{Name: "billing_account_id", Value: scope.BillingAccountID},
		{Name: "service_id", Value: googleVertexServiceID},
		{Name: "start", Value: start.Format(dayFormat)},
		{Name: "end", Value: end.Format(dayFormat)},
	}, projectParameters...)
	job, err := query.Run(queryCtx)
	if err != nil {
		return nil, nil, googleQueryError(queryCtx, err)
	}
	defer func() {
		if queryCtx.Err() != nil {
			cancelCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer stop()
			_ = job.Cancel(cancelCtx)
		}
	}()
	rows, err := job.Read(queryCtx)
	if err != nil {
		return nil, nil, googleQueryError(queryCtx, err)
	}
	rows.PageInfo().MaxSize = 100
	result := reportAccumulator{}
	var revision *sourceRevision
	for count := 0; ; count++ {
		var row googleInvoiceRow
		err := rows.Next(&row)
		if errors.Is(err, iterator.Done) {
			if revision == nil {
				return nil, nil, failFetch("response", errors.New("Google query omitted its revision"))
			}
			return result, revision, nil
		}
		if err != nil {
			return nil, nil, googleQueryError(queryCtx, err)
		}
		if count >= googleMaximumRows {
			return nil, nil, failFetch("response", errors.New("Google query returned too many rows"))
		}
		observed, err := time.Parse(time.RFC3339Nano, row.Revision)
		if err != nil {
			return nil, nil, failFetch("status", errors.New("Google export has no valid watermark"))
		}
		id := observed.UTC().Format(time.RFC3339Nano)
		if revision != nil && revision.id != id {
			return nil, nil, failFetch("response", errors.New("Google query returned inconsistent watermarks"))
		}
		revision = &sourceRevision{id: id, orderingAt: observed}
		if row.Kind == "" && row.Service == "" && row.SKU == "" && row.Currency == "" && row.Amount == "" {
			if row.Day == "" {
				continue
			}
			day, err := time.Parse(dayFormat, row.Day)
			if err != nil || day.Before(start) || !day.Before(end) || !day.Before(observed.UTC().Truncate(24*time.Hour)) {
				return nil, nil, failFetch("response", errors.New("invalid Google coverage day"))
			}
			result.ensureDay(row.Day)
			continue
		}
		if day, err := time.Parse(dayFormat, row.Day); err != nil || !day.Before(observed.UTC().Truncate(24*time.Hour)) {
			return nil, nil, failFetch("response", errors.New("invalid Google coverage day"))
		}
		if err := addGoogleRow(result, row, start, end); err != nil {
			return nil, nil, err
		}
	}
}

func addGoogleRow(result reportAccumulator, row googleInvoiceRow, start, end time.Time) error {
	parsedDay, err := time.Parse(dayFormat, row.Day)
	if err != nil || parsedDay.Before(start) || !parsedDay.Before(end) || row.Service != googleVertexServiceID || row.SKU == "" || !validGoogleCurrency(row.Currency) {
		return failFetch("response", errors.New("invalid Google query dimensions"))
	}
	amount, err := decimal.NewFromString(row.Amount)
	if err != nil {
		return failFetch("response", errors.New("invalid Google query amount"))
	}
	if row.Kind != "cost" && row.Kind != "adjustment" && row.Kind != "rounding error" && !strings.HasPrefix(row.Kind, "credit/") {
		return failFetch("response", errors.New("invalid Google query line kind"))
	}
	item := row.Service + "/" + row.SKU
	if row.Kind != "cost" {
		item += "/" + row.Kind
	}
	item = googleLineItem(item)
	result.add(row.Day, amount, row.Currency, &item)
	return nil
}

func validGoogleCurrency(currency string) bool {
	if len(currency) != 3 {
		return false
	}
	for _, char := range currency {
		if (char < 'A' || char > 'Z') && (char < 'a' || char > 'z') {
			return false
		}
	}
	return true
}

func googleLineItem(value string) string {
	runes := []rune(value)
	if len(runes) <= 256 {
		return value
	}
	digest := sha256.Sum256([]byte(value))
	return string(runes[:191]) + "-" + hex.EncodeToString(digest[:])
}

func googleQueryError(ctx context.Context, err error) error {
	if errors.Is(err, errGoogleAuthentication) {
		return failFetch("credential", errGoogleAuthentication)
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return failFetch("timeout", errors.New("Google query timed out"))
	}
	var apiError *googleapi.Error
	if errors.As(err, &apiError) {
		if apiError.Code == http.StatusUnauthorized || apiError.Code == http.StatusForbidden {
			return failFetch("credential", errors.New("Google query was unauthorized"))
		}
		return failFetch("status", errors.New("Google query failed"))
	}
	var networkError net.Error
	if errors.As(err, &networkError) {
		if networkError.Timeout() {
			return failFetch("timeout", errors.New("Google query timed out"))
		}
		return failFetch("network", errors.New("Google query could not be reached"))
	}
	return failFetch("response", errors.New("invalid Google query response"))
}
