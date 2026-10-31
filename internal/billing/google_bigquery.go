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
	completed, err := i.completedBillingPeriods(importCtx, row, earliest, today)
	if err != nil {
		return err
	}
	current := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC)
	first := time.Date(earliest.Year(), earliest.Month(), 1, 0, 0, 0, 0, time.UTC)
	for month := current; !month.Before(first); month = month.AddDate(0, -1, 0) {
		period := month.Format("20060102") + "-" + month.AddDate(0, 1, 0).Format("20060102")
		if _, ok := completed[period]; ok {
			continue
		}
		start, end := month, month.AddDate(0, 1, 0)
		if start.Before(earliest) {
			start = earliest
		}
		if end.After(today) {
			end = today
		}
		if !start.Before(end) {
			continue
		}
		days, revision, err := fetchGoogle(importCtx, client, auth, row.Scope, key, start, end, earliest)
		if err != nil {
			return err
		}
		if err := i.storeDays(importCtx, row, month.Format(dayFormat), revision, days); err != nil {
			return err
		}
	}
	return nil
}

func fetchGoogle(ctx context.Context, client *http.Client, auth *connectors.Auth, rawScope json.RawMessage, rawCredential []byte, start, end, watermarkStart time.Time) (map[string]map[lineKey]lineSum, *sourceRevision, error) {
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
	queryCtx, cancel := context.WithTimeout(ctx, googleImportTimeout)
	defer cancel()
	bq, err := bigquery.NewClient(queryCtx, scope.BigQueryProjectID, option.WithHTTPClient(&authenticated))
	if err != nil {
		return nil, nil, googleQueryError(queryCtx, err)
	}
	defer bq.Close()
	projectFilter, projectParameters := googleProjectFilter(scope.ProjectIDs)
	statement := fmt.Sprintf(`WITH snapshot AS (
		SELECT billing_account_id, service, sku, project, usage_start_time, currency, cost, cost_type, credits, export_time
		FROM %s WHERE _PARTITIONTIME >= TIMESTAMP(@start)
	), revision AS (
		SELECT COALESCE(FORMAT_TIMESTAMP('%%Y-%%m-%%dT%%H:%%M:%%E6SZ', MAX(export_time), 'UTC'), '') AS source_revision
		FROM %s WHERE _PARTITIONTIME >= TIMESTAMP(@watermark_start)
	), scoped AS (
		SELECT * FROM snapshot WHERE billing_account_id = @billing_account_id AND service.id = @service_id
		AND usage_start_time >= TIMESTAMP(@start) AND usage_start_time < TIMESTAMP(@end)
		AND cost_type IN ('regular', 'adjustment', 'rounding error')%s
	), amounts AS (
		SELECT DATE(usage_start_time, 'UTC') AS usage_day, service.id AS service_id, sku.id AS sku_id, currency,
		CASE WHEN cost_type = 'regular' THEN 'cost' ELSE cost_type END AS line_kind, CAST(cost AS BIGNUMERIC) AS amount FROM scoped
		UNION ALL
		SELECT DATE(usage_start_time, 'UTC'), service.id, sku.id, currency,
		CONCAT('credit/', COALESCE(credit.type, 'unknown')), CAST(credit.amount AS BIGNUMERIC)
		FROM scoped CROSS JOIN UNNEST(credits) AS credit
	), totals AS (
		SELECT CAST(usage_day AS STRING) AS usage_day, service_id, sku_id, currency, line_kind, CAST(SUM(amount) AS STRING) AS amount
		FROM amounts GROUP BY 1, 2, 3, 4, 5
	)
	SELECT COALESCE(totals.usage_day, '') AS usage_day, COALESCE(totals.service_id, '') AS service_id,
		COALESCE(totals.sku_id, '') AS sku_id, COALESCE(totals.currency, '') AS currency,
		COALESCE(totals.amount, '') AS amount, COALESCE(totals.line_kind, '') AS line_kind, revision.source_revision
	FROM revision LEFT JOIN totals ON TRUE`, "`"+scope.BigQueryProjectID+"."+scope.DatasetID+"."+scope.TableID+"`", "`"+scope.BigQueryProjectID+"."+scope.DatasetID+"."+scope.TableID+"`", projectFilter)
	query := bq.Query(statement)
	query.MaxBytesBilled = googleMaximumBytes
	query.JobTimeout = googleImportTimeout
	query.Parameters = append([]bigquery.QueryParameter{
		{Name: "billing_account_id", Value: scope.BillingAccountID},
		{Name: "service_id", Value: googleVertexServiceID},
		{Name: "start", Value: start.Format(dayFormat)},
		{Name: "watermark_start", Value: watermarkStart.Format(dayFormat)},
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
	for day := start; day.Before(end); day = day.AddDate(0, 0, 1) {
		result.ensureDay(day.Format(dayFormat))
	}
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
		if row.Kind == "" && row.Day == "" && row.Service == "" && row.SKU == "" && row.Currency == "" && row.Amount == "" {
			continue
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
