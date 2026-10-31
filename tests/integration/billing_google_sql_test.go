//go:build integration && google_sql

package integration_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/goccy/googlesqlite"
	"github.com/google/uuid"
)

type googleExportFixture struct {
	BillingAccountID string `json:"billing_account_id"`
	Service          struct {
		ID string `json:"id"`
	} `json:"service"`
	SKU struct {
		ID string `json:"id"`
	} `json:"sku"`
	Project struct {
		ID string `json:"id"`
	} `json:"project"`
	UsageStart     time.Time   `json:"usage_start_time"`
	Currency       string      `json:"currency"`
	CostType       string      `json:"cost_type"`
	Cost           json.Number `json:"cost"`
	AdjustmentInfo *struct {
		ID string `json:"id"`
	} `json:"adjustment_info"`
	Credits []struct {
		Type   string      `json:"type"`
		Amount json.Number `json:"amount"`
	} `json:"credits"`
}

func googleFixtureResults(t *testing.T, db *sql.DB, statement string, day time.Time, filters map[string]string, invalidCurrency, restated, empty, emptyExport bool, revision time.Time) []any {
	t.Helper()
	data, err := os.ReadFile("../../internal/billing/testdata/google_detailed_export.json")
	if err != nil {
		t.Fatal(err)
	}
	var input []googleExportFixture
	if err := json.Unmarshal(data, &input); err != nil {
		t.Fatal(err)
	}
	if restated {
		data, err := os.ReadFile("../../internal/billing/testdata/google_detailed_export_restatement.json")
		if err != nil {
			t.Fatal(err)
		}
		var additions []googleExportFixture
		if err := json.Unmarshal(data, &additions); err != nil {
			t.Fatal(err)
		}
		input = append(input, additions...)
	}
	if _, err := db.Exec("DELETE FROM `fixture-project.billing_export.gcp_billing_export_resource_v1_fixture` WHERE TRUE"); err != nil {
		t.Fatal(err)
	}
	shift := day.Sub(time.Date(2025, 1, 20, 0, 0, 0, 0, time.UTC))
	for _, item := range input {
		if emptyExport {
			continue
		}
		usage := item.UsageStart.Add(shift)
		if item.SKU.ID == "PREVIOUS-MONTH" {
			today := day.AddDate(0, 0, 3)
			usage = time.Date(today.Year(), today.Month(), 10, 12, 0, 0, 0, time.UTC).AddDate(0, -1, 0)
		}
		if item.SKU.ID == "HISTORICAL" || item.SKU.ID == "HISTORICAL-CORRECTION" {
			today := day.AddDate(0, 0, 3)
			usage = time.Date(today.Year(), today.Month(), 6, 10, 0, 0, 0, time.UTC).AddDate(0, -2, 0)
		}
		if empty && item.Service.ID == filters["service_id"] {
			continue
		}
		currency := item.Currency
		if invalidCurrency {
			currency = "U1D"
		}
		adjustment := "NULL"
		if item.AdjustmentInfo != nil {
			adjustment = strconv.Quote(item.AdjustmentInfo.ID)
		}
		credits := make([]string, 0, len(item.Credits))
		for _, credit := range item.Credits {
			credits = append(credits, fmt.Sprintf("STRUCT<type STRING, amount FLOAT64>(%s, %s)", strconv.Quote(credit.Type), credit.Amount.String()))
		}
		insert := fmt.Sprintf("INSERT INTO `fixture-project.billing_export.gcp_billing_export_resource_v1_fixture` VALUES (TIMESTAMP %s, %s, STRUCT<id STRING>(%s), STRUCT<id STRING>(%s), STRUCT<id STRING>(%s), TIMESTAMP %s, %s, %s, %s, ARRAY<STRUCT<type STRING, amount FLOAT64>>[%s], STRUCT<id STRING>(%s), TIMESTAMP %s)",
			strconv.Quote(revision.Format(time.RFC3339Nano)), strconv.Quote(item.BillingAccountID), strconv.Quote(item.Service.ID), strconv.Quote(item.SKU.ID), strconv.Quote(item.Project.ID), strconv.Quote(usage.Format(time.RFC3339Nano)), strconv.Quote(currency), item.Cost.String(), strconv.Quote(item.CostType), strings.Join(credits, ","), adjustment, strconv.Quote(revision.Format(time.RFC3339Nano)))
		if _, err := db.Exec(insert); err != nil {
			t.Fatalf("insert Google export fixture: %v", err)
		}
	}
	statement = strings.ReplaceAll(statement, "_PARTITIONTIME", "fixture_partition_time")
	rows, err := db.Query(statement, sql.Named("start", filters["start"]), sql.Named("end", filters["end"]), sql.Named("billing_account_id", filters["billing_account_id"]), sql.Named("service_id", filters["service_id"]), sql.Named("project_ids", []string{filters["project_ids"]}))
	if err != nil {
		t.Fatalf("execute submitted GoogleSQL: %v", err)
	}
	defer rows.Close()
	result := []any{}
	for rows.Next() {
		var dayValue, service, sku, currency, amount, kind, sourceRevision string
		if err := rows.Scan(&dayValue, &service, &sku, &currency, &amount, &kind, &sourceRevision); err != nil {
			t.Fatal(err)
		}
		if sourceRevision != "" && !strings.HasSuffix(sourceRevision, "Z") {
			sourceRevision += "Z"
		}
		result = append(result, map[string]any{"f": []any{map[string]any{"v": dayValue}, map[string]any{"v": service}, map[string]any{"v": sku}, map[string]any{"v": currency}, map[string]any{"v": amount}, map[string]any{"v": kind}, map[string]any{"v": sourceRevision}}})
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(result, func(a, b any) int { return strings.Compare(fmt.Sprint(a), fmt.Sprint(b)) })
	return result
}

func TestGoogleBillingImportEndToEnd(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	day, _, _ := billingDays()
	dayText := day.Format("2006-01-02")
	setting := h.want(owner, "GET", "/api/v1/settings/retention.usage_days", nil, nil, 200)
	h.want(owner, "PUT", "/api/v1/settings/retention.usage_days", map[string]any{"value": "180"}, etagHeader(setting), 200)
	today := day.AddDate(0, 0, 3)
	historicalMonth := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -2, 0)
	historicalDay := historicalMonth.AddDate(0, 0, 5)
	previousDay := time.Date(today.Year(), today.Month(), 10, 0, 0, 0, 0, time.UTC).AddDate(0, -1, 0)
	var failHistorical atomic.Bool
	var failCurrent atomic.Bool
	var queryCount atomic.Int64
	failHistorical.Store(true)
	var invalidCurrency atomic.Bool
	var failBytes atomic.Bool
	var malformedSchema atomic.Bool
	var revokedCredentials atomic.Bool
	var emptyExport atomic.Bool
	var blockQuery atomic.Bool
	var restated atomic.Bool
	var empty atomic.Bool
	var revisionOffset atomic.Int64
	var tokenExchanges atomic.Int64
	var queryParameters atomic.Value
	var queryStatement atomic.Value
	db, err := sql.Open("googlesqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE `fixture-project.billing_export.gcp_billing_export_resource_v1_fixture` (fixture_partition_time TIMESTAMP, billing_account_id STRING, service STRUCT<id STRING>, sku STRUCT<id STRING>, project STRUCT<id STRING>, usage_start_time TIMESTAMP, currency STRING, cost FLOAT64, cost_type STRING, credits ARRAY<STRUCT<type STRING, amount FLOAT64>>, adjustment_info STRUCT<id STRING>, export_time TIMESTAMP)"); err != nil {
		t.Fatal(err)
	}
	// Google lists the Vertex billing service ID at https://docs.cloud.google.com/billing/docs/how-to/budgets-spend-caps, accessed 2026-10-31.
	const fixtureService = "C7E2-9256-1C43"
	queryWaiting := make(chan struct{}, 1)
	jobCancelled := make(chan struct{}, 1)
	peer := newFixedHostPeer(t, "oauth2.googleapis.com", "bigquery.googleapis.com")
	peer.respond = func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost && req.URL.Path == "/token" {
			if revokedCredentials.Load() {
				http.Error(w, `{"error":"revoked credentials with private detail"}`, http.StatusUnauthorized)
				return
			}
			tokenExchanges.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "test-token", "token_type": "Bearer", "expires_in": 3600})
			return
		}
		if req.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(w, "bad query", http.StatusBadRequest)
			return
		}
		if req.Method == http.MethodPost && strings.HasPrefix(req.URL.Path, "/bigquery/v2/projects/fixture-project/jobs/") && strings.HasSuffix(req.URL.Path, "/cancel") {
			jobCancelled <- struct{}{}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
			return
		}
		if req.Method == http.MethodPost && req.URL.Path == "/bigquery/v2/projects/fixture-project/jobs" {
			queryCount.Add(1)
			var submission struct {
				JobReference  map[string]any `json:"jobReference"`
				Configuration struct {
					JobTimeout string `json:"jobTimeoutMs"`
					Query      struct {
						Query              string `json:"query"`
						UseLegacySQL       bool   `json:"useLegacySql"`
						MaximumBytesBilled string `json:"maximumBytesBilled"`
						QueryParameters    []struct {
							Name           string `json:"name"`
							ParameterValue struct {
								Value       string `json:"value"`
								ArrayValues []struct {
									Value string `json:"value"`
								} `json:"arrayValues"`
							} `json:"parameterValue"`
						} `json:"queryParameters"`
					} `json:"query"`
				} `json:"configuration"`
			}
			if json.NewDecoder(req.Body).Decode(&submission) != nil {
				http.Error(w, "bad query", http.StatusBadRequest)
				return
			}
			query := submission.Configuration.Query
			values := map[string]string{}
			for _, p := range query.QueryParameters {
				values[p.Name] = p.ParameterValue.Value
				if len(p.ParameterValue.ArrayValues) > 0 {
					values[p.Name] = p.ParameterValue.ArrayValues[0].Value
				}
			}
			maximumBytes, maximumErr := strconv.ParseUint(query.MaximumBytesBilled, 10, 64)
			if query.UseLegacySQL || maximumErr != nil || maximumBytes == 0 || maximumBytes > 1<<30 || submission.Configuration.JobTimeout == "" || values["billing_account_id"] != "123456-ABCDEF-123456" || values["service_id"] != fixtureService || values["project_ids"] != "workload-project" {
				http.Error(w, "bad query parameters", http.StatusBadRequest)
				return
			}
			if failCurrent.Load() && values["end"] == today.Format("2006-01-02") {
				http.Error(w, `{"error":{"code":400,"message":"current period unavailable"}}`, http.StatusBadRequest)
				return
			}
			if failBytes.Load() {
				http.Error(w, `{"error":{"code":400,"message":"maximum bytes exceeded with private query details"}}`, http.StatusBadRequest)
				return
			}
			if values["start"] == historicalMonth.Format("2006-01-02") && failHistorical.CompareAndSwap(true, false) {
				http.Error(w, `{"error":{"code":400,"message":"sensitive vendor query detail"}}`, http.StatusBadRequest)
				return
			}
			queryParameters.Store(values)
			queryStatement.Store(query.Query)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(submission)
			return
		}
		if req.Method != http.MethodGet || !strings.HasPrefix(req.URL.Path, "/bigquery/v2/projects/fixture-project/queries/") {
			http.Error(w, "bad query results request", http.StatusBadRequest)
			return
		}
		if blockQuery.Load() {
			queryWaiting <- struct{}{}
			<-req.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if malformedSchema.Load() {
			_, _ = w.Write([]byte(`{"jobComplete":true,"totalRows":"1","schema":{"fields":[{"name":"private-schema-detail","type":"STRING"}]},"rows":[{"f":[{"v":"private-schema-detail"}]}]}`))
			return
		}
		revision := day.AddDate(0, 0, 1).Add(time.Duration(revisionOffset.Load()) * time.Second)
		allRows := googleFixtureResults(t, db, queryStatement.Load().(string), day, queryParameters.Load().(map[string]string), invalidCurrency.Load(), restated.Load(), empty.Load(), emptyExport.Load(), revision)
		rows := allRows[:1]
		pageToken := ""
		if len(allRows) > 1 {
			pageToken = "second-page"
		}
		if req.URL.Query().Get("pageToken") == "second-page" {
			if req.URL.Query().Get("maxResults") != "100" {
				t.Errorf("BigQuery result page was not bounded: %q", req.URL.Query().Get("maxResults"))
			}
			rows = allRows[1:]
			pageToken = ""
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jobComplete": true, "totalRows": strconv.Itoa(len(allRows)),
			"pageToken": pageToken,
			"schema": map[string]any{"fields": []any{
				map[string]any{"name": "usage_day", "type": "STRING"}, map[string]any{"name": "service_id", "type": "STRING"},
				map[string]any{"name": "sku_id", "type": "STRING"}, map[string]any{"name": "currency", "type": "STRING"}, map[string]any{"name": "amount", "type": "STRING"},
				map[string]any{"name": "line_kind", "type": "STRING"}, map[string]any{"name": "source_revision", "type": "STRING"},
			}},
			"rows": rows,
		})
	}
	provider := h.want(owner, "POST", "/api/v1/providers", map[string]any{
		"name":          "Google invoice",
		"configuration": map[string]any{"kind": "vertex_ai", "auth_mode": "adc", "cloud_project": "fixture-project", "cloud_region": "us-central1", "endpoint": peer.proxy.URL, "options": map[string]any{"network": map[string]any{"proxy_url": peer.proxy.URL, "trust_roots_pem": peer.roots}}},
		"model":         "gemini-test",
	}, idem("provider-google-invoice"), 201)
	id := billingProviderID(provider)
	scope := map[string]any{"billing_account_id": "123456-ABCDEF-123456", "bigquery_project_id": "fixture-project", "dataset_id": "billing_export", "table_id": "gcp_billing_export_resource_v1_fixture", "project_ids": []string{"workload-project"}}
	serviceAccount := googleTestServiceAccount(t)
	billingPut(h, owner, id, map[string]any{"scope": scope, "credential": map[string]any{"auth_mode": "service_account", "service_account": serviceAccount}}, provider["etag"].(string), 201)
	priced := "0.001"
	billingFact(t, h, id, uuid.NewString(), day, "billable", true, &priced, false, false, false)
	billingFact(t, h, id, uuid.NewString(), historicalDay, "billable", true, &priced, false, false, false)
	gapDay := historicalDay.AddDate(0, 0, 1)
	billingFact(t, h, id, uuid.NewString(), gapDay, "billable", true, &priced, false, false, false)
	billingFact(t, h, id, uuid.NewString(), previousDay, "billable", true, &priced, false, false, false)
	importer := billingImporter(h, t)
	if err := importer.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if tokenExchanges.Load() == 0 {
		t.Fatal("service account did not exchange a token through the configured proxy")
	}
	initialStatus := h.want(owner, "GET", "/api/v1/providers/"+id+"/billing-import", nil, nil, 200)["status"].(map[string]any)
	if initialStatus["last_error_code"] != "status" || strings.Contains(fmt.Sprint(initialStatus), "sensitive vendor query detail") || initialStatus["imported_from"].(string) <= historicalDay.Format("2006-01-02") {
		t.Fatalf("failed historical period was reported as covered: %v", initialStatus)
	}
	lastAttempt, lastErr := time.Parse(time.RFC3339Nano, initialStatus["last_import_at"].(string))
	nextAttempt, nextErr := time.Parse(time.RFC3339Nano, initialStatus["next_import_at"].(string))
	if lastErr != nil || nextErr != nil || !nextAttempt.After(lastAttempt) || nextAttempt.Sub(lastAttempt) > time.Hour {
		t.Fatalf("failed historical period did not back off: %v", initialStatus)
	}
	historicalURL := fmt.Sprintf("/api/v1/usage/reconciliation?start=%s&end=%s&provider_id=%s", historicalDay.Format("2006-01-02"), historicalDay.AddDate(0, 0, 1).Format("2006-01-02"), id)
	historical := h.want(owner, "GET", historicalURL, nil, nil, 200)["items"].([]any)[0].(map[string]any)
	if historical["capture_rate_unavailable_reason"] != "not_imported" || historical["invoiced_cost"] != nil {
		t.Fatalf("failed historical period appeared as zero: %v", historical)
	}
	for range 8 {
		currentImport := h.want(owner, "GET", "/api/v1/providers/"+id+"/billing-import", nil, nil, 200)
		runHeaders := etagHeader(currentImport)
		runHeaders["Idempotency-Key"] = uuid.NewString()
		h.want(owner, "POST", "/api/v1/providers/"+id+"/billing-import/run", nil, runHeaders, 202)
		if err := importer.Pass(t.Context()); err != nil {
			t.Fatal(err)
		}
		if item := h.want(owner, "GET", historicalURL, nil, nil, 200)["items"].([]any)[0].(map[string]any); item["capture_rate_unavailable_reason"] != "not_imported" {
			break
		}
	}
	recovered := h.want(owner, "GET", historicalURL, nil, nil, 200)["items"].([]any)[0].(map[string]any)
	if recovered["capture_rate_unavailable_reason"] != "zero_invoice" || recovered["invoiced_cost"] != "0" {
		t.Fatalf("historical period was not retried: %v", recovered)
	}
	gapURL := fmt.Sprintf("/api/v1/usage/reconciliation?start=%s&end=%s&provider_id=%s", gapDay.Format("2006-01-02"), gapDay.AddDate(0, 0, 1).Format("2006-01-02"), id)
	gap := h.want(owner, "GET", gapURL, nil, nil, 200)["items"].([]any)[0].(map[string]any)
	if gap["capture_rate_unavailable_reason"] != "not_imported" || gap["invoiced_cost"] != nil {
		t.Fatalf("day without scope evidence appeared covered: %v", gap)
	}
	initialStatus = h.want(owner, "GET", "/api/v1/providers/"+id+"/billing-import", nil, nil, 200)["status"].(map[string]any)
	if initialStatus["last_error_code"] != nil || initialStatus["imported_from"] != dayText || initialStatus["imported_through"] != dayText || initialStatus["source_revision"] != day.AddDate(0, 0, 1).Format(time.RFC3339Nano) {
		t.Fatalf("Google export revision was not applied: %v", initialStatus)
	}
	end := day.AddDate(0, 0, 1).Format("2006-01-02")
	costs := h.want(owner, "GET", fmt.Sprintf("/api/v1/providers/%s/invoiced-costs?start=%s&end=%s", id, dayText, end), nil, nil, 200)
	items := costs["items"].([]any)
	if len(items) != 3 {
		t.Fatalf("invoiced costs: %v", items)
	}
	amounts := map[string]string{}
	for _, item := range items {
		line := item.(map[string]any)
		if line["currency"] != "USD" || len([]rune(line["line_item"].(string))) > 256 {
			t.Fatalf("invalid Google invoice line: %v", line)
		}
		amounts[line["amount"].(string)] = line["line_item"].(string)
	}
	if amounts["0.300000000001"] != fixtureService+"/123456-ABCDEF-654321" || amounts["1.1"] == "" || amounts["2.2"] == "" || amounts["1.1"] == amounts["2.2"] {
		t.Fatalf("Google invoice amounts and line identities: %v", amounts)
	}
	report := h.want(owner, "GET", fmt.Sprintf("/api/v1/usage/reconciliation?start=%s&end=%s&provider_id=%s", dayText, end, id), nil, nil, 200)
	if len(report["items"].([]any)) != 1 || report["items"].([]any)[0].(map[string]any)["invoiced_cost"] != "3.600000000001" {
		t.Fatalf("Google reconciliation: %v", report)
	}
	runImport := func() {
		before := queryCount.Load()
		current := h.want(owner, "GET", "/api/v1/providers/"+id+"/billing-import", nil, nil, 200)
		headers := etagHeader(current)
		headers["Idempotency-Key"] = uuid.NewString()
		h.want(owner, "POST", "/api/v1/providers/"+id+"/billing-import/run", nil, headers, 202)
		if err := importer.Pass(t.Context()); err != nil {
			t.Fatal(err)
		}
		if queryCount.Load()-before > 3 {
			t.Fatal("Google import exceeded the per-pass query budget")
		}
	}
	restated.Store(true)
	revisionOffset.Store(1)
	failCurrent.Store(true)
	historicalCostsURL := fmt.Sprintf("/api/v1/providers/%s/invoiced-costs?start=%s&end=%s", id, historicalDay.Format("2006-01-02"), historicalDay.AddDate(0, 0, 1).Format("2006-01-02"))
	for range 8 {
		runImport()
		if len(h.want(owner, "GET", historicalCostsURL, nil, nil, 200)["items"].([]any)) > 0 {
			break
		}
	}
	historicalCosts := h.want(owner, "GET", historicalCostsURL, nil, nil, 200)["items"].([]any)
	if len(historicalCosts) != 1 || historicalCosts[0].(map[string]any)["amount"] != "-0.3" {
		t.Fatalf("historical correction was starved by the current period: %v", historicalCosts)
	}
	failCurrent.Store(false)
	runImport()
	restatedItems := h.want(owner, "GET", fmt.Sprintf("/api/v1/providers/%s/invoiced-costs?start=%s&end=%s", id, dayText, end), nil, nil, 200)["items"].([]any)
	if len(restatedItems) != 6 {
		t.Fatalf("restated Google invoice lines: %v", restatedItems)
	}
	restatedAmounts := map[string]string{}
	for _, value := range restatedItems {
		line := value.(map[string]any)
		restatedAmounts[line["line_item"].(string)] = line["amount"].(string)
	}
	base := fixtureService + "/123456-ABCDEF-654321"
	if restatedAmounts[base] != "0.300000000001" || restatedAmounts[base+"/adjustment"] != "-0.1" || restatedAmounts[base+"/credit/DISCOUNT"] != "-0.05" || restatedAmounts[base+"/credit/PROMOTION"] != "-0.025" {
		t.Fatalf("adjustments and credits were not stored separately: %v", restatedAmounts)
	}
	previousURL := fmt.Sprintf("/api/v1/providers/%s/invoiced-costs?start=%s&end=%s", id, previousDay.Format("2006-01-02"), previousDay.AddDate(0, 0, 1).Format("2006-01-02"))
	previousItems := h.want(owner, "GET", previousURL, nil, nil, 200)["items"].([]any)
	if len(previousItems) != 1 || previousItems[0].(map[string]any)["amount"] != "-0.2" {
		t.Fatalf("previous period was not refreshed: %v", previousItems)
	}
	restatedReport := h.want(owner, "GET", fmt.Sprintf("/api/v1/usage/reconciliation?start=%s&end=%s&provider_id=%s", dayText, end, id), nil, nil, 200)
	if restatedReport["items"].([]any)[0].(map[string]any)["invoiced_cost"] != "3.425000000001" {
		t.Fatalf("restated reconciliation: %v", restatedReport)
	}
	invoiceAmounts := func() map[string]string {
		current := h.want(owner, "GET", fmt.Sprintf("/api/v1/providers/%s/invoiced-costs?start=%s&end=%s", id, dayText, end), nil, nil, 200)["items"].([]any)
		amounts := map[string]string{}
		for _, value := range current {
			line := value.(map[string]any)
			amounts[line["line_item"].(string)] = line["amount"].(string)
		}
		return amounts
	}
	runImport()
	if equal := invoiceAmounts(); !reflect.DeepEqual(restatedAmounts, equal) {
		t.Fatalf("equal revision changed invoice amounts: %v", equal)
	}
	restated.Store(false)
	revisionOffset.Store(0)
	runImport()
	if older := invoiceAmounts(); !reflect.DeepEqual(restatedAmounts, older) {
		t.Fatalf("older revision replaced newer rows: %v", older)
	}
	if previous := h.want(owner, "GET", previousURL, nil, nil, 200)["items"].([]any); len(previous) != 1 || previous[0].(map[string]any)["amount"] != "-0.2" {
		t.Fatalf("older revision replaced previous period: %v", previous)
	}
	newerStatus := h.want(owner, "GET", "/api/v1/providers/"+id+"/billing-import", nil, nil, 200)["status"].(map[string]any)
	if newerStatus["source_revision"] != day.AddDate(0, 0, 1).Add(time.Second).Format(time.RFC3339Nano) {
		t.Fatalf("older revision regressed status: %v", newerStatus)
	}
	empty.Store(true)
	revisionOffset.Store(2)
	runImport()
	if zero := h.want(owner, "GET", fmt.Sprintf("/api/v1/providers/%s/invoiced-costs?start=%s&end=%s", id, dayText, end), nil, nil, 200)["items"].([]any); len(zero) != 0 {
		t.Fatalf("empty covered day retained rows: %v", zero)
	}
	emptyReport := h.want(owner, "GET", fmt.Sprintf("/api/v1/usage/reconciliation?start=%s&end=%s&provider_id=%s", dayText, end, id), nil, nil, 200)
	if emptyReport["items"].([]any)[0].(map[string]any)["invoiced_cost"] != "0" {
		t.Fatalf("empty covered day was not zero: %v", emptyReport)
	}
	empty.Store(false)
	revisionOffset.Store(3)
	runImport()
	beforeInvalid := h.want(owner, "GET", fmt.Sprintf("/api/v1/providers/%s/invoiced-costs?start=%s&end=%s", id, dayText, end), nil, nil, 200)["items"].([]any)
	emptyExport.Store(true)
	runImport()
	noWatermark := h.want(owner, "GET", "/api/v1/providers/"+id+"/billing-import", nil, nil, 200)["status"].(map[string]any)
	if noWatermark["last_error_code"] != "status" || !reflect.DeepEqual(beforeInvalid, h.want(owner, "GET", fmt.Sprintf("/api/v1/providers/%s/invoiced-costs?start=%s&end=%s", id, dayText, end), nil, nil, 200)["items"].([]any)) {
		t.Fatalf("empty export incorrectly advanced coverage: %v", noWatermark)
	}
	emptyExport.Store(false)
	assertFailureBackoff := func(status map[string]any) {
		t.Helper()
		last, lastErr := time.Parse(time.RFC3339Nano, status["last_import_at"].(string))
		next, nextErr := time.Parse(time.RFC3339Nano, status["next_import_at"].(string))
		if lastErr != nil || nextErr != nil || !next.After(last) || next.Sub(last) > time.Hour {
			t.Fatalf("failed Google import did not back off: %v", status)
		}
	}
	malformedSchema.Store(true)
	runImport()
	malformedStatus := h.want(owner, "GET", "/api/v1/providers/"+id+"/billing-import", nil, nil, 200)["status"].(map[string]any)
	if malformedStatus["last_error_code"] != "status" || strings.Contains(fmt.Sprint(malformedStatus), "private-schema-detail") || !reflect.DeepEqual(beforeInvalid, h.want(owner, "GET", fmt.Sprintf("/api/v1/providers/%s/invoiced-costs?start=%s&end=%s", id, dayText, end), nil, nil, 200)["items"].([]any)) {
		t.Fatalf("malformed schema changed data or exposed details: %v", malformedStatus)
	}
	assertFailureBackoff(malformedStatus)
	malformedSchema.Store(false)
	revokedCredentials.Store(true)
	runImport()
	revokedStatus := h.want(owner, "GET", "/api/v1/providers/"+id+"/billing-import", nil, nil, 200)["status"].(map[string]any)
	if revokedStatus["last_error_code"] != "credential" || strings.Contains(fmt.Sprint(revokedStatus), "private detail") || !reflect.DeepEqual(beforeInvalid, h.want(owner, "GET", fmt.Sprintf("/api/v1/providers/%s/invoiced-costs?start=%s&end=%s", id, dayText, end), nil, nil, 200)["items"].([]any)) {
		t.Fatalf("revoked credentials changed data or exposed details: %v", revokedStatus)
	}
	assertFailureBackoff(revokedStatus)
	revokedCredentials.Store(false)
	failBytes.Store(true)
	runImport()
	failedStatus := h.want(owner, "GET", "/api/v1/providers/"+id+"/billing-import", nil, nil, 200)["status"].(map[string]any)
	failedRows := h.want(owner, "GET", fmt.Sprintf("/api/v1/providers/%s/invoiced-costs?start=%s&end=%s", id, dayText, end), nil, nil, 200)["items"].([]any)
	if failedStatus["last_error_code"] != "status" || strings.Contains(fmt.Sprint(failedStatus), "private query details") || !reflect.DeepEqual(beforeInvalid, failedRows) {
		t.Fatalf("query limit failure changed stored data or exposed details: %v %v", failedStatus, failedRows)
	}
	if metrics := importer.Metrics(); metrics[[2]string{id, "status"}] == 0 || metrics[[2]string{id, "success"}] == 0 {
		t.Fatalf("missing import outcome metrics: %v", metrics)
	}
	failBytes.Store(false)
	invalidCurrency.Store(true)
	current := h.want(owner, "GET", "/api/v1/providers/"+id+"/billing-import", nil, nil, 200)
	headers := etagHeader(current)
	headers["Idempotency-Key"] = uuid.NewString()
	h.want(owner, "POST", "/api/v1/providers/"+id+"/billing-import/run", nil, headers, 202)
	if err := importer.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	after := h.want(owner, "GET", fmt.Sprintf("/api/v1/providers/%s/invoiced-costs?start=%s&end=%s", id, dayText, end), nil, nil, 200)["items"].([]any)
	status := h.want(owner, "GET", "/api/v1/providers/"+id+"/billing-import", nil, nil, 200)["status"].(map[string]any)
	if !reflect.DeepEqual(beforeInvalid, after) || status["last_error_code"] != "response" {
		t.Fatalf("malformed currency changed invoiced costs or was accepted: %v %v", after, status)
	}
	blockQuery.Store(true)
	current = h.want(owner, "GET", "/api/v1/providers/"+id+"/billing-import", nil, nil, 200)
	headers = etagHeader(current)
	headers["Idempotency-Key"] = uuid.NewString()
	h.want(owner, "POST", "/api/v1/providers/"+id+"/billing-import/run", nil, headers, 202)
	ctx, cancel := context.WithCancel(context.Background())
	passDone := make(chan error, 1)
	go func() { passDone <- importer.Pass(ctx) }()
	select {
	case <-queryWaiting:
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("Google query did not start")
	}
	cancel()
	select {
	case <-jobCancelled:
	case <-time.After(10 * time.Second):
		t.Fatal("Google query job was not cancelled")
	}
	<-passDone

	blockQuery.Store(false)
	invalidCurrency.Store(false)
	data, err := json.Marshal(serviceAccount)
	if err != nil {
		t.Fatal(err)
	}
	credentialFile := filepath.Join(t.TempDir(), "application-default-credentials.json")
	if err := os.WriteFile(credentialFile, data, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", credentialFile)
	current = h.want(owner, "GET", "/api/v1/providers/"+id+"/billing-import", nil, nil, 200)
	updated := billingPut(h, owner, id, map[string]any{"scope": scope, "credential": map[string]any{"auth_mode": "adc"}}, current["etag"].(string), 200)
	headers = etagHeader(updated)
	headers["Idempotency-Key"] = uuid.NewString()
	h.want(owner, "POST", "/api/v1/providers/"+id+"/billing-import/run", nil, headers, 202)
	previousExchanges := tokenExchanges.Load()
	if err := importer.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if tokenExchanges.Load() <= previousExchanges {
		t.Fatal("ADC did not exchange a token through the configured proxy")
	}
}

// The credential is required on create, must be a string, and null is
// rejected rather than clearing the import.

// Idempotent replay returns the stored reply.

// An update that omits the credential keeps it.

// If-Match gates mutations.

// A non-openai/anthropic kind is refused.

// Scoping: a manager of another project gets 404; a viewer of the
// provider's project reads it.

// TestBillingImportEndToEnd runs the importer against the fake vendors and
// checks the stored invoiced costs and the reconciliation report.
