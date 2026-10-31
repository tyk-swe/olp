//go:build integration

package integration_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/tyk-swe/olp/internal/billing"
	"github.com/tyk-swe/olp/internal/database"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/secrets"
)

type billingFake struct {
	mu                    sync.Mutex
	path                  string
	pages                 map[string]string // page token -> fixture name; "" is page 1
	shift                 map[string]string // fixture text rewrites (dates near today)
	queries               []url.Values
	fails                 int
	expectedAuthorization string
	expectedAPIKey        string
	started               chan struct{}
	release               <-chan struct{}
	server                *httptest.Server
}

// billingDays are the three consecutive UTC days the shifted fixtures cover,
// most recent first so they survive the importer's retention purge.
func billingDays() (day1, day2, day3 time.Time) {
	today := time.Now().UTC().Truncate(24 * time.Hour)
	return today.AddDate(0, 0, -3), today.AddDate(0, 0, -2), today.AddDate(0, 0, -1)
}

func newBillingFake(t *testing.T, path string, pages map[string]string, shift map[string]string) *billingFake {
	t.Helper()
	f := &billingFake{path: path, pages: pages, shift: shift}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		started, release := f.started, f.release
		f.started, f.release = nil, nil
		f.mu.Unlock()
		if started != nil {
			close(started)
			<-release
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.queries = append(f.queries, r.URL.Query())
		if r.URL.Path != f.path || f.fails > 0 || (f.expectedAuthorization != "" && r.Header.Get("Authorization") != f.expectedAuthorization) || (f.expectedAPIKey != "" && r.Header.Get("x-api-key") != f.expectedAPIKey) {
			f.fails--
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		name := f.pages[r.URL.Query().Get("page")]
		data, err := os.ReadFile("../../internal/billing/testdata/" + name)
		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		text := string(data)
		for from, to := range f.shift {
			text = strings.ReplaceAll(text, from, to)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(text))
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *billingFake) failOnce() {
	f.mu.Lock()
	f.fails++
	f.mu.Unlock()
}

func (f *billingFake) call(i int) url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.queries[i]
}

// billingImporter is an Importer wired to the harness's pool, keys and
// egress policy.
func billingImporter(h *accessHarness, t *testing.T) *billing.Importer {
	policy := egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
	return &billing.Importer{Pool: h.Pool, Keys: h.Ring2(), Installation: h.Server.Installation, Egress: &policy, Log: slog.New(slog.DiscardHandler)}
}

// billingProvider creates a provider of kind kind pointing at the fake
// admin endpoint.
func billingProvider(h *accessHarness, owner *browser, name, kind, endpoint string, projectID any) map[string]any {
	body := map[string]any{
		"name": name,
		"configuration": map[string]any{
			"kind": kind, "auth_mode": "api_key", "endpoint": endpoint,
		},
		"credential": "sk-provider-" + strings.Repeat("x", 20),
		"model":      "gpt-test",
	}
	if projectID != nil {
		body["project_id"] = projectID
	}
	return h.want(owner, "POST", "/api/v1/providers", body, idem("provider-"+name), 201)
}

func billingPut(h *accessHarness, b *browser, providerID string, body map[string]any, etag string, want int) map[string]any {
	headers := map[string]string{"Idempotency-Key": uuid.NewString()}
	if etag != "" {
		headers["If-Match"] = `"` + etag + `"`
	}
	return h.want(b, "PUT", "/api/v1/providers/"+providerID+"/billing-import", body, headers, want)
}

// billingFact inserts one usage fact for the reconciliation report.
func billingFact(t *testing.T, h *accessHarness, providerID, requestID string, observedAt time.Time,
	chargeStatus string, complete bool, cost *string, unpriced, caller, exempt bool) {
	t.Helper()
	if _, err := h.Pool.Exec(t.Context(), `INSERT INTO olp.usage_request_anchors (request_id, request_started_at)
		VALUES ($1,$2) ON CONFLICT DO NOTHING`, requestID, observedAt.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	var userID string
	if err := h.Pool.QueryRow(t.Context(), "SELECT id::text FROM olp.users ORDER BY created_at LIMIT 1").Scan(&userID); err != nil {
		t.Fatal(err)
	}
	keyID := uuid.NewString()
	if _, err := h.Pool.Exec(t.Context(), `INSERT INTO olp.api_keys (id, lookup_id, digest, name, created_by, policy, etag)
		VALUES ($1,$2,$3,$4,$5,'{}'::jsonb,$6)`,
		keyID, uuid.NewString(), make([]byte, 32), "billing", userID, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Pool.Exec(t.Context(), `INSERT INTO olp.attempt_usage_facts
		(attempt_id, event_id, request_id, request_started_at, attempt_ordinal, api_key_id, provider_id,
		 route_slug, upstream_model, operation, surface, observed_at, charge_status, usage_observed,
		 usage_complete, input_tokens, estimated_cost, unpriced, currency,
		 request_counted, provider_request_counted, model_request_counted, target_request_counted,
		 request_unpriced_counted, provider_unpriced_counted, model_unpriced_counted, target_unpriced_counted,
		 request_incomplete_counted, provider_incomplete_counted, model_incomplete_counted, target_incomplete_counted,
		 caller_credential, budget_exempt)
		VALUES ($1,$2,$3,$4,1,$5,$6,'route','model','generation','api',$7,$8,true,$9,100,$10::numeric,$11,'USD',
			true,true,true,true,$11,$11,$11,$11,
			NOT $9,NOT $9,NOT $9,NOT $9,
			$12,$13)`,
		uuid.NewString(), uuid.NewString(), requestID, observedAt.Add(-time.Second), keyID, providerID,
		observedAt, chargeStatus, complete, cost, unpriced, caller, exempt); err != nil {
		t.Fatal(err)
	}
}

func billingProviderID(provider map[string]any) string { return provider["id"].(string) }

func TestAzureBillingScopeAndCredential(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	provider := h.want(owner, "POST", "/api/v1/providers", map[string]any{
		"name": "Azure billing",
		"configuration": map[string]any{
			"kind": "azure_openai", "auth_mode": "api_key", "endpoint": "https://example.openai.azure.com",
			"deployment": "model", "api_version": "2024-10-21",
		},
		"credential": "inference-key", "model": "model",
	}, idem("azure-billing-provider"), 201)
	providerID := billingProviderID(provider)
	credential := map[string]any{"auth_mode": "azure_default"}
	scope := map[string]any{
		"billing_scope": "/providers/Microsoft.Billing/billingAccounts/1234567",
		"export_name":   "costs", "storage_account": "billingaccount", "container": "exports", "prefix": "billing",
		"subscription_ids": []string{"11111111-1111-4111-8111-111111111111"},
	}
	billingPut(h, owner, providerID, map[string]any{"credential": credential}, provider["etag"].(string), 422)
	billingPut(h, owner, providerID, map[string]any{"credential": credential, "scope": map[string]any{
		"billing_scope": scope["billing_scope"], "export_name": "costs", "storage_account": "billingaccount", "container": "exports", "subscription_ids": []string{"bad"},
	}}, provider["etag"].(string), 422)
	billingPut(h, owner, providerID, map[string]any{"credential": credential, "scope": map[string]any{
		"billing_scope": scope["billing_scope"], "export_name": "costs", "storage_account": "billingaccount", "container": "exports", "project_ids": []string{"proj_123"},
	}}, provider["etag"].(string), 422)
	for _, invalid := range []string{
		"/providers/Microsoft.Billing/billingAccounts/1234567/garbage/anything",
		"/providers/Microsoft.Billing/billingAccounts/1234567/billingProfiles/profile/garbage/anything",
		"/subscriptions/11111111-1111-4111-8111-111111111111/resourceGroups/",
	} {
		billingPut(h, owner, providerID, map[string]any{"credential": credential, "scope": map[string]any{
			"billing_scope": invalid, "export_name": "costs", "storage_account": "billingaccount", "container": "exports",
		}}, provider["etag"].(string), 422)
	}
	created := billingPut(h, owner, providerID, map[string]any{"credential": credential, "scope": scope}, provider["etag"].(string), 201)
	if created["source"] != "azure_cost_management" || created["credential_configured"] != true || created["credential"] != nil {
		t.Fatalf("unexpected billing import %v", created)
	}
	groupScope := map[string]any{"billing_scope": "/subscriptions/11111111-1111-4111-8111-111111111111/resourceGroups/example-group",
		"export_name": "costs", "storage_account": "billingaccount", "container": "exports"}
	updated := billingPut(h, owner, providerID, map[string]any{"scope": groupScope}, created["etag"].(string), 200)
	if updated["scope"].(map[string]any)["billing_scope"] != groupScope["billing_scope"] {
		t.Fatalf("resource group scope was not preserved: %v", updated)
	}
	billingPut(h, owner, providerID, map[string]any{"scope": scope}, updated["etag"].(string), 200)
	read := h.want(owner, "GET", "/api/v1/providers/"+providerID+"/billing-import", nil, nil, 200)
	if read["credential"] != nil || read["credential_configured"] != true {
		t.Fatalf("credential returned in billing import %v", read)
	}
	export := h.want(owner, "GET", "/api/v1/configuration/export", nil, nil, 200)
	body := map[string]any{"document": export["document"], "secret_bindings": map[string]any{
		"Azure billing/billing": `{ "auth_mode" : "azure_default" }`, "Azure billing/default": "inference-key",
	}}
	h.want(owner, "POST", "/api/v1/configuration/plan", body, nil, 200)
	invalidBody := map[string]any{"document": export["document"], "secret_bindings": map[string]any{
		"Azure billing/billing": `{"auth_mode":"azure_default","auth_mode":"azure_default"}`, "Azure billing/default": "inference-key",
	}}
	h.want(owner, "POST", "/api/v1/configuration/plan", invalidBody, nil, 422)
	h.want(owner, "POST", "/api/v1/configuration/apply", body, idem("azure-billing-promotion"), 200)
	after := h.want(owner, "GET", "/api/v1/providers/"+providerID+"/billing-import", nil, nil, 200)
	if after["credential"] != nil || after["credential_configured"] != true {
		t.Fatalf("promotion did not preserve the credential: %v", after)
	}
}

func TestAzureBillingImport(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	provider := h.want(owner, "POST", "/api/v1/providers", map[string]any{
		"name": "Azure export",
		"configuration": map[string]any{
			"kind": "azure_openai", "auth_mode": "api_key", "endpoint": "https://example.openai.azure.com",
			"deployment": "model", "api_version": "2024-10-21",
		},
		"credential": "inference-key", "model": "model",
	}, idem("azure-export-provider"), 201)
	providerID := billingProviderID(provider)
	subscription := "11111111-1111-4111-8111-111111111111"
	billingPut(h, owner, providerID, map[string]any{
		"credential": map[string]any{"auth_mode": "azure_default"},
		"scope": map[string]any{"billing_scope": "/providers/Microsoft.Billing/billingAccounts/1234567",
			"export_name": "costs", "storage_account": "billingaccount", "container": "exports", "prefix": "billing", "subscription_ids": []string{subscription}},
	}, provider["etag"].(string), 201)
	today := time.Now().UTC().Truncate(24 * time.Hour)
	yesterday := today.AddDate(0, 0, -1)
	period := time.Date(yesterday.Year(), yesterday.Month(), 1, 0, 0, 0, 0, time.UTC)
	base := "billing/costs/" + period.Format("20060102") + "-" + period.AddDate(0, 1, 0).Format("20060102") + "/run/"
	header := "Date,SubscriptionId,MeterCategory,MeterSubCategory,MeterId,ChargeType,BillingCurrency,CostInBillingCurrency\n"
	row := func(day time.Time, sub, category, meter, amount string) string {
		return fmt.Sprintf("%s,%s,%s,%s,meter-123,Usage,USD,%s\n", day.Format("2006-01-02"), sub, category, meter, amount)
	}
	part1 := header + row(yesterday, subscription, "Cognitive Services", "Azure Open AI", "0.100000000000000001") +
		row(yesterday, subscription, "Cognitive Services", "Speech Services", "99") +
		row(yesterday, "22222222-2222-4222-8222-222222222222", "Cognitive Services", "Azure Open AI", "99")
	longMeter := strings.Repeat("m", 280)
	longRow := func(suffix string) string {
		return fmt.Sprintf("%s,%s,Cognitive Services,Azure Open AI,%s%s,Usage,USD,0\n", yesterday.Format("2006-01-02"), subscription, longMeter, suffix)
	}
	part2 := header + row(yesterday, subscription, "Cognitive Services", "Azure Open AI", "0.200000000000000002") +
		row(today, subscription, "Cognitive Services", "Azure Open AI", "99") +
		row(yesterday, subscription, "Storage", "Azure Open AI", "99") + longRow("a") + longRow("b")
	compress := func(contents string) string {
		t.Helper()
		var compressed bytes.Buffer
		writer := gzip.NewWriter(&compressed)
		if _, err := writer.Write([]byte(contents)); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		return compressed.String()
	}
	compressed1, compressed2 := compress(part1), compress(part2)
	// Manifest example: https://learn.microsoft.com/en-us/azure/cost-management-billing/costs/tutorial-improved-exports, accessed 2026-10-31.
	manifest, err := json.Marshal(map[string]any{
		"manifestVersion": "2024-04-01", "byteCount": len(compressed1) + len(compressed2), "blobCount": 2, "dataRowCount": 8,
		"exportConfig":   map[string]any{"exportName": "costs", "resourceId": "/providers/Microsoft.Billing/billingAccounts/1234567/providers/Microsoft.CostManagement/exports/costs", "type": "AmortizedCost", "dataVersion": "2023-05-01"},
		"deliveryConfig": map[string]any{"fileFormat": "Csv", "compressionMode": "Gzip"},
		"runInfo":        map[string]any{"submittedTime": today.Format(time.RFC3339), "runId": "run", "startDate": period.Format("2006-01-02T15:04:05"), "endDate": today.Format(time.RFC3339)},
		"blobs":          []map[string]any{{"blobName": base + "part0.csv", "byteCount": len(compressed1), "dataRowCount": 3}, {"blobName": base + "part1.csv", "byteCount": len(compressed2), "dataRowCount": 5}},
	})
	if err != nil {
		t.Fatal(err)
	}
	newerBase := strings.Replace(base, "/run/", "/newer/", 1)
	newerManifest := strings.NewReplacer(base, newerBase, `"runId":"run"`, `"runId":"newer"`, today.Format(time.RFC3339), today.Add(time.Hour).Format(time.RFC3339)).Replace(string(manifest))
	objects := map[string]string{base + "_manifest.json": string(manifest), newerBase + "_manifest.json": newerManifest, base + "part0.csv": compressed1, base + "part1.csv": compressed2}
	oldPeriod := period.AddDate(0, -2, 0)
	oldDay := oldPeriod.AddDate(0, 0, 14)
	missingDay := period.AddDate(0, -1, 14)
	oldBase := "billing/costs/" + oldPeriod.Format("20060102") + "-" + oldPeriod.AddDate(0, 1, 0).Format("20060102") + "/old/"
	oldPart := compress(header + row(oldDay, subscription, "Cognitive Services", "Azure Open AI", "0.5"))
	oldManifest, err := json.Marshal(map[string]any{
		"manifestVersion": "2024-04-01", "byteCount": len(oldPart), "blobCount": 1, "dataRowCount": 1,
		"exportConfig":   map[string]any{"exportName": "costs", "resourceId": "/providers/Microsoft.Billing/billingAccounts/1234567/providers/Microsoft.CostManagement/exports/costs", "type": "AmortizedCost", "dataVersion": "2023-05-01"},
		"deliveryConfig": map[string]any{"fileFormat": "Csv", "compressionMode": "Gzip"},
		"runInfo":        map[string]any{"submittedTime": oldPeriod.AddDate(0, 1, 1).Format(time.RFC3339), "runId": "old", "startDate": oldPeriod.Format("2006-01-02T15:04:05"), "endDate": oldPeriod.AddDate(0, 1, 0).Format(time.RFC3339)},
		"blobs":          []map[string]any{{"blobName": oldBase + "part0.csv", "byteCount": len(oldPart), "dataRowCount": 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	objects[oldBase+"_manifest.json"] = string(oldManifest)
	objects[oldBase+"part0.csv"] = oldPart
	requests := make(chan string, 100)
	var emptyListing atomic.Bool
	var stallPart atomic.Bool
	emptyListing.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.URL.Path
		if r.URL.Path == "/token" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"access_token":"test-azure-token","expires_on":"%d","token_type":"Bearer","resource":"https://storage.azure.com/"}`, time.Now().Add(time.Hour).Unix())
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-azure-token" {
			http.Error(w, "missing token", http.StatusUnauthorized)
			return
		}
		if r.URL.Query().Get("comp") == "list" {
			if emptyListing.Load() {
				_, _ = fmt.Fprint(w, "<EnumerationResults><Blobs></Blobs><NextMarker></NextMarker></EnumerationResults>")
				return
			}
			if r.URL.Query().Get("marker") == "" {
				_, _ = fmt.Fprintf(w, "<EnumerationResults><Blobs><Blob><Name>%s_manifest.json</Name></Blob></Blobs><NextMarker>next</NextMarker></EnumerationResults>", newerBase)
				return
			}
			_, _ = fmt.Fprintf(w, "<EnumerationResults><Blobs><Blob><Name>%s_manifest.json</Name></Blob><Blob><Name>%s_manifest.json</Name></Blob></Blobs><NextMarker></NextMarker></EnumerationResults>", base, oldBase)
			return
		}
		if object, ok := objects[strings.TrimPrefix(r.URL.Path, "/exports/")]; ok {
			_, _ = fmt.Fprint(w, object)
			if stallPart.Load() && r.URL.Path == "/exports/"+base+"part0.csv" {
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	t.Setenv("IDENTITY_ENDPOINT", server.URL+"/token")
	t.Setenv("IDENTITY_HEADER", "test-identity")
	t.Setenv("AZURE_TENANT_ID", "")
	t.Setenv("AZURE_CLIENT_ID", "")
	if err := os.Unsetenv("AZURE_CLIENT_ID"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AZURE_CLIENT_SECRET", "")
	importer := billingImporter(h, t)
	importer.AzureBlobEndpoint = func(string) string { return server.URL }
	if err := importer.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	importStatus := h.want(owner, "GET", "/api/v1/providers/"+providerID+"/billing-import", nil, nil, 200)
	status := importStatus["status"].(map[string]any)
	if status["last_error_code"] != "response" || status["last_success_at"] != nil {
		t.Fatalf("empty export was not reported as a failure: %v", status)
	}
	emptyListing.Store(false)
	headers := etagHeader(importStatus)
	headers["Idempotency-Key"] = uuid.NewString()
	h.want(owner, "POST", "/api/v1/providers/"+providerID+"/billing-import/run", nil, headers, 202)
	if err := importer.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	importStatus = h.want(owner, "GET", "/api/v1/providers/"+providerID+"/billing-import", nil, nil, 200)
	if errorCode := importStatus["status"].(map[string]any)["last_error_code"]; errorCode != nil {
		close(requests)
		var paths []string
		for request := range requests {
			paths = append(paths, request)
		}
		t.Fatalf("import failed: %v, requests: %v", errorCode, paths)
	}
	start, end := yesterday.Format("2006-01-02"), today.AddDate(0, 0, 1).Format("2006-01-02")
	result := h.want(owner, "GET", fmt.Sprintf("/api/v1/providers/%s/invoiced-costs?start=%s&end=%s", providerID, start, end), nil, nil, 200)
	items := result["items"].([]any)
	if len(items) != 3 {
		t.Fatalf("unexpected invoice items %v", items)
	}
	longLabels := map[string]bool{}
	var total decimal.Decimal
	for _, raw := range items {
		item := raw.(map[string]any)
		amount, err := decimal.NewFromString(item["amount"].(string))
		if err != nil || item["day"] != start || item["currency"] != "USD" {
			t.Fatalf("unexpected invoice item %v", item)
		}
		total = total.Add(amount)
		label := item["line_item"].(string)
		if strings.Contains(label, strings.Repeat("m", 100)) {
			if len([]rune(label)) > 256 {
				t.Fatalf("long label not bounded: %d", len([]rune(label)))
			}
			longLabels[label] = true
		}
	}
	if !total.Equal(decimal.RequireFromString("0.300000000000000003")) || len(longLabels) != 2 {
		t.Fatalf("incorrect decimal sum or collapsed labels: %s %v", total, longLabels)
	}
	cost := "0.15"
	for _, day := range []time.Time{oldDay, missingDay} {
		billingFact(t, h, providerID, uuid.NewString(), day, "billable", true, &cost, false, false, false)
		dayReport := h.want(owner, "GET", fmt.Sprintf("/api/v1/usage/reconciliation?start=%s&end=%s&provider_id=%s", day.Format("2006-01-02"), day.AddDate(0, 0, 1).Format("2006-01-02"), providerID), nil, nil, 200)
		item := dayReport["items"].([]any)[0].(map[string]any)
		if day.Equal(oldDay) && (item["invoiced_cost"] != "0.5" || item["capture_rate_unavailable_reason"] != nil) {
			t.Fatalf("previously imported period was not covered: %v", item)
		}
		if day.Equal(missingDay) && (item["invoiced_cost"] != nil || item["capture_rate_unavailable_reason"] != "not_imported") {
			t.Fatalf("missing period was treated as invoiced: %v", item)
		}
	}
	billingFact(t, h, providerID, uuid.NewString(), yesterday, "billable", true, &cost, false, false, false)
	billingFact(t, h, providerID, uuid.NewString(), yesterday, "billable", true, nil, true, false, false)
	billingFact(t, h, providerID, uuid.NewString(), yesterday, "billable", false, nil, false, false, false)
	billingFact(t, h, providerID, uuid.NewString(), yesterday, "billable", true, &cost, false, true, false)
	report := h.want(owner, "GET", fmt.Sprintf("/api/v1/usage/reconciliation?start=%s&end=%s&provider_id=%s", start, today.Format("2006-01-02"), providerID), nil, nil, 200)
	reconciled := report["items"].([]any)[0].(map[string]any)
	if reconciled["priced_attempts"] != float64(1) || reconciled["unpriced_attempts"] != float64(1) ||
		reconciled["incomplete_attempts"] != float64(1) || reconciled["excluded_caller_attempts"] != float64(1) || reconciled["capture_rate"] == nil {
		t.Fatalf("unexpected reconciliation %v", reconciled)
	}
	objects[base+"part0.csv"] = compress(strings.Replace(part1, "USD", "U$D", 1))
	var changed map[string]any
	if err := json.Unmarshal(manifest, &changed); err != nil {
		t.Fatal(err)
	}
	changed["byteCount"] = len(objects[base+"part0.csv"]) + len(compressed2)
	changed["blobs"].([]any)[0].(map[string]any)["byteCount"] = len(objects[base+"part0.csv"])
	changedManifest, err := json.Marshal(changed)
	if err != nil {
		t.Fatal(err)
	}
	objects[base+"_manifest.json"] = string(changedManifest)
	current := h.want(owner, "GET", "/api/v1/providers/"+providerID+"/billing-import", nil, nil, 200)
	headers = etagHeader(current)
	headers["Idempotency-Key"] = uuid.NewString()
	h.want(owner, "POST", "/api/v1/providers/"+providerID+"/billing-import/run", nil, headers, 202)
	if err := importer.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	failed := h.want(owner, "GET", "/api/v1/providers/"+providerID+"/billing-import", nil, nil, 200)
	if failed["status"].(map[string]any)["last_error_code"] != "response" {
		t.Fatalf("malformed currency was not rejected: %v", failed)
	}
	after := h.want(owner, "GET", fmt.Sprintf("/api/v1/providers/%s/invoiced-costs?start=%s&end=%s", providerID, start, end), nil, nil, 200)
	if !reflect.DeepEqual(after["items"], items) {
		t.Fatalf("failed import changed stored data: %v", after)
	}
	objects[base+"_manifest.json"] = strings.Replace(string(manifest), "billingAccounts/1234567", "billingAccounts/other", 1)
	current = h.want(owner, "GET", "/api/v1/providers/"+providerID+"/billing-import", nil, nil, 200)
	headers = etagHeader(current)
	headers["Idempotency-Key"] = uuid.NewString()
	h.want(owner, "POST", "/api/v1/providers/"+providerID+"/billing-import/run", nil, headers, 202)
	if err := importer.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	failed = h.want(owner, "GET", "/api/v1/providers/"+providerID+"/billing-import", nil, nil, 200)
	after = h.want(owner, "GET", fmt.Sprintf("/api/v1/providers/%s/invoiced-costs?start=%s&end=%s", providerID, start, end), nil, nil, 200)
	if failed["status"].(map[string]any)["last_error_code"] != "response" || !reflect.DeepEqual(after["items"], items) {
		t.Fatalf("different billing scope affected stored data: %v %v", failed, after)
	}
	var oversized strings.Builder
	oversized.WriteString(header)
	rowCount := 0
	for i := 0; i <= 100_000; i++ {
		fmt.Fprintf(&oversized, "%s,%s,Cognitive Services,Azure Open AI,meter-%d,Usage,USD,1\n", start, subscription, i)
		rowCount++
		if (i+1)%999 == 0 {
			oversized.WriteString(row(yesterday, subscription, "Storage", "Azure Open AI", "1"))
			rowCount++
		}
	}
	objects[base+"part0.csv"] = oversized.String()
	changed["byteCount"] = len(objects[base+"part0.csv"])
	changed["blobCount"] = 1
	changed["dataRowCount"] = rowCount
	changed["deliveryConfig"].(map[string]any)["compressionMode"] = "None"
	changed["blobs"] = []map[string]any{{"blobName": base + "part0.csv", "byteCount": len(objects[base+"part0.csv"]), "dataRowCount": rowCount}}
	changedManifest, err = json.Marshal(changed)
	if err != nil {
		t.Fatal(err)
	}
	objects[base+"_manifest.json"] = string(changedManifest)
	stallPart.Store(true)
	current = h.want(owner, "GET", "/api/v1/providers/"+providerID+"/billing-import", nil, nil, 200)
	headers = etagHeader(current)
	headers["Idempotency-Key"] = uuid.NewString()
	h.want(owner, "POST", "/api/v1/providers/"+providerID+"/billing-import/run", nil, headers, 202)
	bounded, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := importer.Pass(bounded); err != nil {
		t.Fatal(err)
	}
	failed = h.want(owner, "GET", "/api/v1/providers/"+providerID+"/billing-import", nil, nil, 200)
	after = h.want(owner, "GET", fmt.Sprintf("/api/v1/providers/%s/invoiced-costs?start=%s&end=%s", providerID, start, end), nil, nil, 200)
	if failed["status"].(map[string]any)["last_error_code"] != "response" || !reflect.DeepEqual(after["items"], items) {
		t.Fatalf("oversized export was not rejected without changing stored data: %v %v", failed, after)
	}
}

func googleTestServiceAccount(t *testing.T) map[string]any {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	privateKey, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{
		"type": "service_account", "project_id": "fixture-project", "private_key_id": "fixture",
		"private_key":  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateKey})),
		"client_email": "billing@fixture-project.iam.gserviceaccount.com", "token_uri": "https://oauth2.googleapis.com/token",
	}
}

func TestGoogleBillingImportScopeAndCredential(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	provider := h.want(owner, "POST", "/api/v1/providers", map[string]any{
		"name": "Google billing",
		"configuration": map[string]any{
			"kind": "vertex_ai", "auth_mode": "adc", "cloud_project": "fixture-project", "cloud_region": "us-central1",
			"endpoint": "https://us-central1-aiplatform.googleapis.com/v1/projects/fixture-project/locations/us-central1/publishers/google",
		},
		"model": "gemini-test",
	}, idem("provider-google-billing"), 201)
	id := billingProviderID(provider)
	scope := map[string]any{
		"billing_account_id": "123456-ABCDEF-123456", "bigquery_project_id": "example.com:fixture-project",
		"dataset_id": "1_billing_export", "table_id": "billing export-é",
		"project_ids": []string{"example.com:workload-project"},
	}
	credential := map[string]any{"auth_mode": "service_account", "service_account": googleTestServiceAccount(t)}
	for _, invalid := range []map[string]any{
		{"credential": credential},
		{"scope": map[string]any{"billing_account_id": "bad", "bigquery_project_id": "fixture-project", "dataset_id": "billing_export", "table_id": "export"}, "credential": credential},
		{"scope": map[string]any{"billing_account_id": "123456-ABCDEF-123456", "bigquery_project_id": "fixture-project", "dataset_id": "bad.dataset", "table_id": "export"}, "credential": credential},
		{"scope": map[string]any{"billing_account_id": "123456-ABCDEF-123456", "bigquery_project_id": "fixture-project", "dataset_id": "billing_export", "table_id": "export", "workspace_ids": []string{"default"}}, "credential": credential},
		{"scope": scope, "credential": map[string]any{"auth_mode": "service_account", "service_account": map[string]any{"type": "service_account", "private_key": "invalid", "client_email": "billing@fixture-project.iam.gserviceaccount.com", "token_uri": "https://oauth2.googleapis.com/token"}}},
	} {
		billingPut(h, owner, id, invalid, provider["etag"].(string), 422)
	}
	created := billingPut(h, owner, id, map[string]any{"scope": scope, "credential": credential}, provider["etag"].(string), 201)
	if created["source"] != "google_bigquery" || strings.Contains(fmt.Sprint(created), "PRIVATE KEY") {
		t.Fatalf("unexpected import response: %v", created)
	}
	export := h.want(owner, "GET", "/api/v1/configuration/export", nil, nil, 200)
	if strings.Contains(fmt.Sprint(export), "PRIVATE KEY") {
		t.Fatal("configuration export exposed the Google billing credential")
	}
	credentialJSON, err := json.Marshal(credential)
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"document": export["document"], "secret_bindings": map[string]any{"Google billing/billing": string(credentialJSON)}}
	h.want(owner, "POST", "/api/v1/configuration/plan", body, nil, 200)
	h.want(owner, "POST", "/api/v1/configuration/apply", body, idem("google-billing-configuration"), 200)
}

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
	UsageStart time.Time   `json:"usage_start_time"`
	Currency   string      `json:"currency"`
	CostType   string      `json:"cost_type"`
	Cost       json.Number `json:"cost"`
	Credits    []struct {
		Type   string      `json:"type"`
		Amount json.Number `json:"amount"`
	} `json:"credits"`
}

func googleFixtureResults(t *testing.T, day time.Time, filters map[string]string, invalidCurrency, restated, empty bool, revision time.Time) []any {
	t.Helper()
	// Google detailed export schema, https://docs.cloud.google.com/billing/docs/how-to/export-data-bigquery-tables/detailed-usage, accessed 2026-10-31.
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
	if empty {
		input = nil
	}
	start, err := time.Parse("2006-01-02", filters["start"])
	if err != nil {
		t.Fatal(err)
	}
	end, err := time.Parse("2006-01-02", filters["end"])
	if err != nil {
		t.Fatal(err)
	}
	type key struct{ day, service, sku, currency, kind string }
	groups := map[key]decimal.Decimal{}
	shift := day.Sub(time.Date(2025, 1, 20, 0, 0, 0, 0, time.UTC))
	for _, item := range input {
		usage := item.UsageStart.Add(shift)
		if item.SKU.ID == "PREVIOUS-MONTH" {
			today := day.AddDate(0, 0, 3)
			usage = time.Date(today.Year(), today.Month(), 10, 12, 0, 0, 0, time.UTC).AddDate(0, -1, 0)
		}
		if item.BillingAccountID != filters["billing_account_id"] || item.Service.ID != filters["service_id"] || item.Project.ID != filters["project_ids"] || (item.CostType != "regular" && item.CostType != "adjustment" && item.CostType != "rounding error") || usage.Before(start) || !usage.Before(end) {
			continue
		}
		amount, err := decimal.NewFromString(item.Cost.String())
		if err != nil {
			t.Fatal(err)
		}
		currency := item.Currency
		if invalidCurrency {
			currency = "U1D"
		}
		kind := item.CostType
		if kind == "regular" {
			kind = "cost"
		}
		k := key{usage.UTC().Format("2006-01-02"), item.Service.ID, item.SKU.ID, currency, kind}
		groups[k] = groups[k].Add(amount)
		for _, credit := range item.Credits {
			creditAmount, err := decimal.NewFromString(credit.Amount.String())
			if err != nil {
				t.Fatal(err)
			}
			creditKey := key{k.day, k.service, k.sku, k.currency, "credit/" + credit.Type}
			groups[creditKey] = groups[creditKey].Add(creditAmount)
		}
	}
	keys := make([]key, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b key) int { return strings.Compare(a.sku+"/"+a.kind, b.sku+"/"+b.kind) })
	result := make([]any, 0, len(keys))
	for _, k := range keys {
		result = append(result, map[string]any{"f": []any{map[string]any{"v": k.day}, map[string]any{"v": k.service}, map[string]any{"v": k.sku}, map[string]any{"v": k.currency}, map[string]any{"v": groups[k].String()}, map[string]any{"v": k.kind}, map[string]any{"v": revision.Format(time.RFC3339Nano)}}})
	}
	return result
}

func TestGoogleBillingAuthRejectsPrivateDestination(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer proxy.Close()
	policy := loopbackPolicy()
	client, err := policy.PublicDestinationClient(&egress.ConnectionOptions{ProxyURL: proxy.URL}, nil, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://127.0.0.1/token", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err == nil {
		response.Body.Close()
		t.Fatal("public-only authentication accepted a private token destination")
	}
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
	failHistorical.Store(true)
	var invalidCurrency atomic.Bool
	var failBytes atomic.Bool
	var emptyExport atomic.Bool
	var blockQuery atomic.Bool
	var restated atomic.Bool
	var empty atomic.Bool
	var revisionOffset atomic.Int64
	var tokenExchanges atomic.Int64
	var queryParameters atomic.Value
	// Google lists the Vertex billing service ID at https://docs.cloud.google.com/billing/docs/how-to/budgets-spend-caps, accessed 2026-10-31.
	const fixtureService = "C7E2-9256-1C43"
	queryWaiting := make(chan struct{}, 1)
	jobCancelled := make(chan struct{}, 1)
	peer := newFixedHostPeer(t, "oauth2.googleapis.com", "bigquery.googleapis.com")
	peer.respond = func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost && req.URL.Path == "/token" {
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
			if failBytes.Load() {
				http.Error(w, `{"error":{"code":400,"message":"maximum bytes exceeded with private query details"}}`, http.StatusBadRequest)
				return
			}
			if values["start"] == historicalMonth.Format("2006-01-02") && failHistorical.CompareAndSwap(true, false) {
				http.Error(w, `{"error":{"code":400,"message":"sensitive vendor query detail"}}`, http.StatusBadRequest)
				return
			}
			queryParameters.Store(values)
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
		revision := day.AddDate(0, 0, 1).Add(time.Duration(revisionOffset.Load()) * time.Second)
		allRows := googleFixtureResults(t, day, queryParameters.Load().(map[string]string), invalidCurrency.Load(), restated.Load(), empty.Load(), revision)
		if emptyExport.Load() {
			allRows = []any{map[string]any{"f": []any{map[string]any{"v": ""}, map[string]any{"v": ""}, map[string]any{"v": ""}, map[string]any{"v": ""}, map[string]any{"v": ""}, map[string]any{"v": ""}, map[string]any{"v": ""}}}}
		}
		if len(allRows) == 0 {
			allRows = []any{map[string]any{"f": []any{map[string]any{"v": ""}, map[string]any{"v": ""}, map[string]any{"v": ""}, map[string]any{"v": ""}, map[string]any{"v": ""}, map[string]any{"v": ""}, map[string]any{"v": revision.Format(time.RFC3339Nano)}}}}
		}
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
	currentImport := h.want(owner, "GET", "/api/v1/providers/"+id+"/billing-import", nil, nil, 200)
	runHeaders := etagHeader(currentImport)
	runHeaders["Idempotency-Key"] = uuid.NewString()
	h.want(owner, "POST", "/api/v1/providers/"+id+"/billing-import/run", nil, runHeaders, 202)
	if err := importer.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	recovered := h.want(owner, "GET", historicalURL, nil, nil, 200)["items"].([]any)[0].(map[string]any)
	if recovered["capture_rate_unavailable_reason"] != "zero_invoice" || recovered["invoiced_cost"] != "0" {
		t.Fatalf("historical period was not retried: %v", recovered)
	}
	initialStatus = h.want(owner, "GET", "/api/v1/providers/"+id+"/billing-import", nil, nil, 200)["status"].(map[string]any)
	if initialStatus["last_error_code"] != nil || initialStatus["imported_from"] != today.AddDate(0, 0, -180).Format("2006-01-02") || initialStatus["imported_through"] != today.AddDate(0, 0, -1).Format("2006-01-02") || initialStatus["source_revision"] != day.AddDate(0, 0, 1).Format(time.RFC3339Nano) {
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
		current := h.want(owner, "GET", "/api/v1/providers/"+id+"/billing-import", nil, nil, 200)
		headers := etagHeader(current)
		headers["Idempotency-Key"] = uuid.NewString()
		h.want(owner, "POST", "/api/v1/providers/"+id+"/billing-import/run", nil, headers, 202)
		if err := importer.Pass(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	restated.Store(true)
	revisionOffset.Store(1)
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
func TestBillingImportEndToEnd(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := createProject(h, owner, "Billing")

	day1, day2, day3 := billingDays()
	d1, d3 := day1.Format("2006-01-02"), day3.Format("2006-01-02")
	u1, u2, u3 := fmt.Sprint(day1.Unix()), fmt.Sprint(day2.Unix()), fmt.Sprint(day3.Unix())
	openai := newBillingFake(t, "/v1/organization/costs", map[string]string{
		"":             "openai_costs_page1.json",
		"page-2-token": "openai_costs_page2.json",
	}, map[string]string{"1759104000": u1, "1759190400": u2, "1759276800": u3})
	anthropic := newBillingFake(t, "/v1/organizations/cost_report", map[string]string{
		"":             "anthropic_cost_report_page1.json",
		"page-2-token": "anthropic_cost_report_page2.json",
	}, map[string]string{
		`"starting_at": "2025-10-01T`: `"starting_at": "` + day1.Format("2006-01-02") + "T",
		`"ending_at": "2025-10-02T`:   `"ending_at": "` + day2.Format("2006-01-02") + "T",
		`"starting_at": "2025-10-02T`: `"starting_at": "` + day2.Format("2006-01-02") + "T",
		`"ending_at": "2025-10-03T`:   `"ending_at": "` + day3.Format("2006-01-02") + "T",
		`"starting_at": "2025-10-03T`: `"starting_at": "` + day3.Format("2006-01-02") + "T",
		`"ending_at": "2025-10-04T`:   `"ending_at": "` + day3.AddDate(0, 0, 1).Format("2006-01-02") + "T",
	})
	openaiProvider := billingProvider(h, owner, "OpenAI", "openai", openai.server.URL+"/v1", project)
	anthropicProvider := billingProvider(h, owner, "Anthropic", "anthropic", anthropic.server.URL+"/v1", project)
	openaiID := billingProviderID(openaiProvider)
	anthropicID := billingProviderID(anthropicProvider)

	credential := "sk-admin-" + strings.Repeat("k", 30)
	billingPut(h, owner, openaiID, map[string]any{"credential": credential}, openaiProvider["etag"].(string), 201)
	billingPut(h, owner, anthropicID, map[string]any{"credential": "sk-ant-" + strings.Repeat("k", 30)}, anthropicProvider["etag"].(string), 201)

	// The credential is sealed under the billing purpose.
	var purpose string
	if err := h.Pool.QueryRow(t.Context(), `SELECT purpose FROM olp.secrets s
		JOIN olp.provider_billing_imports b ON b.credential_id=s.id
		WHERE b.provider_id=$1`, openaiID).Scan(&purpose); err != nil || purpose != "billing_credential" {
		t.Fatalf("sealed purpose %q err %v", purpose, err)
	}

	// Usage facts across the OpenAI fixture days.
	cost := "0.15"
	costs := "0.25"
	billingFact(t, h, openaiID, uuid.NewString(), day1, "billable", true, &cost, false, false, false)
	billingFact(t, h, openaiID, uuid.NewString(), day1, "billable", true, &costs, false, false, false)
	billingFact(t, h, openaiID, uuid.NewString(), day1, "billable", true, nil, true, false, false)    // unpriced
	billingFact(t, h, openaiID, uuid.NewString(), day1, "billable", false, nil, false, false, false)  // incomplete
	billingFact(t, h, openaiID, uuid.NewString(), day1, "billable", true, &cost, false, true, false)  // caller-paid: excluded
	billingFact(t, h, openaiID, uuid.NewString(), day1, "billable", true, &costs, false, false, true) // budget-exempt: excluded

	importer := billingImporter(h, t)
	if err := importer.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	var errCode *string
	var from, through *string
	if err := h.Pool.QueryRow(t.Context(), `SELECT last_error_code,imported_from::text,imported_through::text FROM olp.provider_billing_imports WHERE provider_id=$1`, openaiID).Scan(&errCode, &from, &through); err != nil {
		t.Fatal(err)
	}
	if errCode != nil || from == nil || through == nil || *from != d1 || *through != d3 {
		t.Fatalf("import state err=%v range=%v..%v", errCode, from, through)
	}
	if got := len(openai.queries) + len(anthropic.queries); got != 4 {
		t.Fatalf("importer fetched %d pages, want 4", got)
	}
	// The scoped OpenAI request carries project_ids[]=[] empty (org-wide).
	if got := openai.call(0).Get("bucket_width"); got != "1d" {
		t.Fatalf("openai query %v", openai.call(0))
	}
	// Anthropic hit the /v1-normalized path and the documented group_bys.
	if got := anthropic.call(0)["group_by[]"]; len(got) != 2 {
		t.Fatalf("anthropic group_by[] %v", got)
	}

	// Exact invoiced costs.
	rangeStart := day1.Format("2006-01-02")
	rangeEnd := day3.AddDate(0, 0, 1).Format("2006-01-02")
	costsList := h.want(owner, "GET", fmt.Sprintf("/api/v1/providers/%s/invoiced-costs?start=%s&end=%s", openaiID, rangeStart, rangeEnd), nil, nil, 200)
	items := costsList["items"].([]any)
	if len(items) != 4 {
		t.Fatalf("openai invoiced items %v", items)
	}
	byItem := map[string]string{}
	for _, item := range items {
		m := item.(map[string]any)
		byItem[m["day"].(string)+"|"+m["line_item"].(string)] = m["amount"].(string)
	}
	if byItem[d1+"|gpt-4o, input_tokens"] != "0.300000000000" &&
		byItem[d1+"|gpt-4o, input_tokens"] != "0.3" {
		t.Fatalf("exact input sum: %v", byItem)
	}
	if byItem[d3+"|gpt-4o-mini, input_tokens"] != "0.0005000001" &&
		byItem[d3+"|gpt-4o-mini, input_tokens"] != "0.000500000100" {
		t.Fatalf("exact mini sum: %v", byItem)
	}
	acosts := h.want(owner, "GET", fmt.Sprintf("/api/v1/providers/%s/invoiced-costs?start=%s&end=%s", anthropicID, rangeStart, rangeEnd), nil, nil, 200)
	aByItem := map[string]string{}
	for _, item := range acosts["items"].([]any) {
		m := item.(map[string]any)
		aByItem[m["day"].(string)+"|"+m["line_item"].(string)] = m["amount"].(string)
	}
	if got := aByItem[d1+"|claude-sonnet-4-5 input tokens"]; got != "1.913400000000" && got != "1.9134" {
		t.Fatalf("cents conversion: %v", aByItem)
	}

	// Reconciliation: attributed 0.4 vs invoiced lines on day1; caller and
	// budget-exempt rows are excluded.
	report := h.want(owner, "GET", fmt.Sprintf("/api/v1/usage/reconciliation?start=%s&end=%s&provider_id=%s", rangeStart, rangeEnd, openaiID), nil, nil, 200)
	var dayItem map[string]any
	for _, item := range report["items"].([]any) {
		m := item.(map[string]any)
		if m["day"] == d1 {
			dayItem = m
		}
	}
	if dayItem == nil {
		t.Fatalf("reconciliation items %v", report["items"])
	}
	if dayItem["attributed_cost"] != "0.400000000000" && dayItem["attributed_cost"] != "0.4" {
		t.Fatalf("attributed_cost %v (caller and exempt must be excluded)", dayItem)
	}
	// excluded_caller_attempts counts billable attempts excluded for either
	// reason: the caller-paid attempt and the budget-exempt attempt.
	if dayItem["priced_attempts"] != float64(2) || dayItem["unpriced_attempts"] != float64(1) ||
		dayItem["incomplete_attempts"] != float64(1) || dayItem["excluded_caller_attempts"] != float64(2) {
		t.Fatalf("attempt counts %v", dayItem)
	}
	if dayItem["capture_rate_unavailable_reason"] != nil {
		if dayItem["capture_rate"] == nil {
			t.Fatalf("invoiced day must carry a capture rate: %v", dayItem)
		}
	}
	if dayItem["unpriced_share"] != "0.333333" {
		t.Fatalf("unpriced_share %v", dayItem)
	}

	// The Anthropic provider records an invoice day with no attributed usage:
	// its capture rate is a clean zero rather than null.
	areport := h.want(owner, "GET", fmt.Sprintf("/api/v1/usage/reconciliation?start=%s&end=%s&provider_id=%s", rangeStart, rangeEnd, anthropicID), nil, nil, 200)
	var aDay, aTotals map[string]any
	for _, item := range areport["items"].([]any) {
		m := item.(map[string]any)
		if m["day"] == d1 {
			aDay = m
		}
	}
	for _, item := range areport["totals"].([]any) {
		m := item.(map[string]any)
		if m["provider_id"] == anthropicID {
			aTotals = m
		}
	}
	if aDay == nil || aTotals == nil {
		t.Fatalf("anthropic reconciliation %v %v", areport["items"], areport["totals"])
	}
	if aDay["attributed_cost"] != "0" && aDay["attributed_cost"] != "0.000000000000" {
		t.Fatalf("anthropic attributed %v", aDay)
	}
	if aDay["capture_rate"] != "0" && aDay["capture_rate"] != "0.000000" {
		t.Fatalf("anthropic capture_rate %v (invoiced day, zero usage)", aDay)
	}
	if aTotals["provider_id"] != anthropicID || aTotals["invoiced_cost"] == nil {
		t.Fatalf("anthropic totals %v", aTotals)
	}
	// attributed_cost_covered compares like with like: attributed spend on
	// days the import covered. Anthropic attributed nothing, so both are zero.
	if aTotals["attributed_cost_covered"] == nil {
		t.Fatalf("totals lack attributed_cost_covered: %v", aTotals)
	}
	// Items are sorted by provider, then day.
	var prevProvider, prevDay string
	for _, item := range report["items"].([]any) {
		m := item.(map[string]any)
		if prevProvider != "" && (m["provider_id"].(string) < prevProvider ||
			(m["provider_id"] == prevProvider && m["day"].(string) <= prevDay)) {
			t.Fatalf("items not sorted: %v after %s/%s", m["day"], prevProvider, prevDay)
		}
		prevProvider, prevDay = m["provider_id"].(string), m["day"].(string)
	}

	// A 401 marks the import 'credential' and backs it off. The run endpoint
	// makes the import due again, since the success above scheduled it an
	// hour out.
	headers := etagHeader(h.want(owner, "GET", "/api/v1/providers/"+openaiID+"/billing-import", nil, nil, 200))
	headers["Idempotency-Key"] = uuid.NewString()
	runImport := h.want(owner, "POST", "/api/v1/providers/"+openaiID+"/billing-import/run", nil, headers, 202)
	if runImport["status"] == nil {
		t.Fatalf("run response %v", runImport)
	}
	openai.failOnce()
	if err := importer.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	var code *string
	var next time.Time
	if err := h.Pool.QueryRow(t.Context(), `SELECT last_error_code,next_import_at FROM olp.provider_billing_imports WHERE provider_id=$1`, openaiID).
		Scan(&code, &next); err != nil || code == nil || *code != "credential" {
		t.Fatalf("failure code %v err %v", code, err)
	}
	if !next.After(time.Now().Add(4 * time.Minute)) {
		t.Fatalf("backoff too short: %s", next)
	}
}

// Export carries billing_import with the deterministic ref and no key.

// Applying without the binding blocks; supplying it recreates the
// import on a fresh installation.

// Every credential reference the document names needs a binding; the
// billing ref is the one this feature adds, the provider's credential
// slot another.

// Dropping billing_import from the entry deletes the import on apply.

func TestBillingImportPreservesCurrencies(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	day1, day2, day3 := billingDays()
	fake := newBillingFake(t, "/v1/organization/costs", map[string]string{
		"": "openai_costs_page1.json", "page-2-token": "openai_costs_page2.json",
	}, map[string]string{
		"1759104000": fmt.Sprint(day1.Unix()),
		"1759190400": fmt.Sprint(day2.Unix()),
		"1759276800": fmt.Sprint(day3.Unix()),
		"\"value\": 0.2,\n            \"currency\": \"usd\"": "\"value\": -0.1,\n            \"currency\": \"eur\"",
		"\"value\": 1234.5678901234,":                        "\"value\": 0,",
		"\"value\": 5,":                                      "\"value\": 0,",
	})
	provider := billingProvider(h, owner, "Currencies", "openai", fake.server.URL+"/v1", nil)
	providerID := billingProviderID(provider)
	billingPut(h, owner, providerID, map[string]any{"credential": "sk-admin-" + strings.Repeat("k", 30)}, provider["etag"].(string), 201)
	if err := billingImporter(h, t).Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	start := day1.Format("2006-01-02")
	end := day1.AddDate(0, 0, 1).Format("2006-01-02")
	costs := h.want(owner, "GET", fmt.Sprintf("/api/v1/providers/%s/invoiced-costs?start=%s&end=%s", providerID, start, end), nil, nil, 200)
	amounts := map[string]decimal.Decimal{}
	for _, raw := range costs["items"].([]any) {
		item := raw.(map[string]any)
		if item["line_item"] != "gpt-4o, input_tokens" {
			continue
		}
		amount, err := decimal.NewFromString(item["amount"].(string))
		if err != nil {
			t.Fatal(err)
		}
		amounts[item["currency"].(string)] = amount
	}
	if len(amounts) != 2 || !amounts["USD"].Equal(decimal.RequireFromString("0.1")) || !amounts["EUR"].Equal(decimal.RequireFromString("-0.1")) {
		t.Fatalf("same line item must preserve its amounts by currency: %v", amounts)
	}
	report := h.want(owner, "GET", fmt.Sprintf("/api/v1/usage/reconciliation?start=%s&end=%s&provider_id=%s", start, end, providerID), nil, nil, 200)
	items := report["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("reconciliation items: %v", items)
	}
	item := items[0].(map[string]any)
	if item["capture_rate"] != nil || item["capture_rate_unavailable_reason"] != "currency_mismatch" {
		t.Fatalf("multi-currency day must not have a capture rate: %v", item)
	}
	totals := report["totals"].([]any)[0].(map[string]any)
	if totals["capture_rate"] != nil || totals["capture_rate_unavailable_reason"] != "currency_mismatch" {
		t.Fatalf("multi-currency total must not have a capture rate: %v", totals)
	}
	fake.mu.Lock()
	fake.shift = map[string]string{
		"1759104000":                  fmt.Sprint(day1.Unix()),
		"1759190400":                  fmt.Sprint(day2.Unix()),
		"1759276800":                  fmt.Sprint(day3.Unix()),
		"\"value\": 0.1,":             "\"value\": 0,",
		"\"value\": 0.2,":             "\"value\": 0,",
		"\"value\": 1234.5678901234,": "\"value\": 0,",
		"\"value\": 5,":               "\"value\": 0,",
		"\"currency\": \"usd\"":       "\"currency\": \"eur\"",
	}
	fake.mu.Unlock()
	path := "/api/v1/providers/" + providerID + "/billing-import"
	current := h.want(owner, "GET", path, nil, nil, 200)
	headers := etagHeader(current)
	headers["Idempotency-Key"] = uuid.NewString()
	h.want(owner, "POST", path+"/run", nil, headers, 202)
	if err := billingImporter(h, t).Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	report = h.want(owner, "GET", fmt.Sprintf("/api/v1/usage/reconciliation?start=%s&end=%s&provider_id=%s", start, end, providerID), nil, nil, 200)
	item = report["items"].([]any)[0].(map[string]any)
	if item["capture_rate"] != nil || item["capture_rate_unavailable_reason"] != "zero_invoice" {
		t.Fatalf("single-currency zero invoice must keep its reason: %v", item)
	}
	totals = report["totals"].([]any)[0].(map[string]any)
	if totals["capture_rate"] != nil || totals["capture_rate_unavailable_reason"] != "zero_invoice" {
		t.Fatalf("single-currency zero total must keep its reason: %v", totals)
	}
}

func TestBillingImportPreservesPrecision(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	day1, day2, day3 := billingDays()
	fake := newBillingFake(t, "/v1/organization/costs", map[string]string{
		"": "openai_costs_page1.json", "page-2-token": "openai_costs_page2.json",
	}, map[string]string{
		"1759104000":      fmt.Sprint(day1.Unix()),
		"1759190400":      fmt.Sprint(day2.Unix()),
		"1759276800":      fmt.Sprint(day3.Unix()),
		"\"value\": 0.1,": "\"value\": 0.00000000000000000001,",
	})
	provider := billingProvider(h, owner, "Precision", "openai", fake.server.URL+"/v1", nil)
	providerID := billingProviderID(provider)
	billingPut(h, owner, providerID, map[string]any{"credential": "sk-admin-" + strings.Repeat("k", 30)}, provider["etag"].(string), 201)
	if err := billingImporter(h, t).Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	start := day1.Format("2006-01-02")
	end := day1.AddDate(0, 0, 1).Format("2006-01-02")
	costs := h.want(owner, "GET", fmt.Sprintf("/api/v1/providers/%s/invoiced-costs?start=%s&end=%s", providerID, start, end), nil, nil, 200)
	for _, raw := range costs["items"].([]any) {
		item := raw.(map[string]any)
		if item["line_item"] != "gpt-4o, input_tokens" {
			continue
		}
		amount, err := decimal.NewFromString(item["amount"].(string))
		if err != nil || !amount.Equal(decimal.RequireFromString("0.20000000000000000001")) {
			t.Fatalf("invoice amount lost precision: %v, %v", item, err)
		}
		return
	}
	t.Fatalf("invoice line item missing: %v", costs)
}

func TestBillingImportCredentialCompatibility(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	day1, day2, day3 := billingDays()
	fake := newBillingFake(t, "/v1/organization/costs", map[string]string{
		"": "openai_costs_page1.json", "page-2-token": "openai_costs_page2.json",
	}, map[string]string{
		"1759104000": fmt.Sprint(day1.Unix()),
		"1759190400": fmt.Sprint(day2.Unix()),
		"1759276800": fmt.Sprint(day3.Unix()),
	})
	provider := billingProvider(h, owner, "Compatibility", "openai", fake.server.URL+"/v1", nil)
	providerID := billingProviderID(provider)
	path := "/api/v1/providers/" + providerID + "/billing-import"
	billingPut(h, owner, providerID, map[string]any{}, provider["etag"].(string), 422)
	billingPut(h, owner, providerID, map[string]any{"credential": nil}, provider["etag"].(string), 422)
	key := `{"source":"openai_costs","api_key":"` + strings.Repeat("k", 30) + `"}`
	fake.expectedAuthorization = "Bearer " + key
	created := billingPut(h, owner, providerID, map[string]any{"credential": key}, provider["etag"].(string), 201)
	if strings.Contains(fmt.Sprint(created), key) {
		t.Fatal("create response exposed the billing credential")
	}
	importer := billingImporter(h, t)
	checkImport := func() {
		t.Helper()
		if err := importer.Pass(t.Context()); err != nil {
			t.Fatal(err)
		}
		result := h.want(owner, "GET", path, nil, nil, 200)
		status := result["status"].(map[string]any)
		if _, ok := status["source_revision"]; !ok {
			t.Fatal("billing import status did not include the source revision")
		}
		if status["last_success_at"] == nil || status["last_error_code"] != nil {
			t.Fatalf("billing credential did not authenticate: %v", status)
		}
		if strings.Contains(fmt.Sprint(result), key) {
			t.Fatal("import response exposed the billing credential")
		}
	}
	schedule := func() {
		t.Helper()
		current := h.want(owner, "GET", path, nil, nil, 200)
		headers := etagHeader(current)
		headers["Idempotency-Key"] = uuid.NewString()
		h.want(owner, "POST", path+"/run", nil, headers, 202)
	}
	checkImport()
	current := h.want(owner, "GET", path, nil, nil, 200)
	billingPut(h, owner, providerID, map[string]any{}, current["etag"].(string), 200)
	schedule()
	checkImport()
	var credentialID string
	if err := h.Pool.QueryRow(t.Context(), `SELECT credential_id::text FROM olp.provider_billing_imports WHERE provider_id=$1::uuid`, providerID).Scan(&credentialID); err != nil {
		t.Fatal(err)
	}
	tx, err := h.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if err := h.Ring2().Store(t.Context(), tx, h.Server.Installation, credentialID, secrets.BillingCredential, []byte(key), nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	schedule()
	checkImport()
	export := h.want(owner, "GET", "/api/v1/configuration/export", nil, nil, 200)
	if strings.Contains(fmt.Sprint(export), key) {
		t.Fatal("configuration export exposed the billing credential")
	}
	body := map[string]any{"document": export["document"], "secret_bindings": map[string]any{"Compatibility/billing": key}}
	h.want(owner, "POST", "/api/v1/configuration/plan", body, nil, 200)
	h.want(owner, "POST", "/api/v1/configuration/apply", body, idem("billing-compatibility"), 200)
	schedule()
	checkImport()
	rotated := "sk-admin-" + strings.Repeat("r", 30)
	rotatedBody := map[string]any{"document": export["document"], "secret_bindings": map[string]any{"Compatibility/billing": rotated}}
	h.want(owner, "POST", "/api/v1/configuration/apply", rotatedBody, idem("billing-rotate"), 200)
	fake.mu.Lock()
	fake.expectedAuthorization = "Bearer " + rotated
	fake.mu.Unlock()
	schedule()
	checkImport()
	rotatedExport := h.want(owner, "GET", "/api/v1/configuration/export", nil, nil, 200)
	if strings.Contains(fmt.Sprint(rotatedExport), rotated) {
		t.Fatal("configuration export exposed the rotated billing credential")
	}
	h.want(owner, "POST", "/api/v1/configuration/apply", rotatedBody, idem("billing-unchanged"), 200)
	schedule()
	checkImport()
}

func TestBillingImportDiscardsChangedInFlightFetch(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	day1, day2, day3 := billingDays()
	fake := newBillingFake(t, "/v1/organization/costs", map[string]string{
		"": "openai_costs_page1.json", "page-2-token": "openai_costs_page2.json",
	}, map[string]string{"1759104000": fmt.Sprint(day1.Unix()), "1759190400": fmt.Sprint(day2.Unix()), "1759276800": fmt.Sprint(day3.Unix())})
	provider := billingProvider(h, owner, "Changing scope", "openai", fake.server.URL+"/v1", nil)
	providerID := billingProviderID(provider)
	path := "/api/v1/providers/" + providerID + "/billing-import"
	key := "sk-admin-" + strings.Repeat("k", 30)
	billingPut(h, owner, providerID, map[string]any{"credential": key, "scope": map[string]any{"project_ids": []string{"proj_aaa111"}}}, provider["etag"].(string), 201)
	begin := func() (chan struct{}, <-chan error) {
		t.Helper()
		started, release, done := make(chan struct{}), make(chan struct{}, 1), make(chan error, 1)
		fake.mu.Lock()
		fake.started, fake.release = started, release
		fake.mu.Unlock()
		t.Cleanup(func() {
			select {
			case release <- struct{}{}:
			default:
			}
		})
		importer := billingImporter(h, t)
		go func() { done <- importer.Pass(t.Context()) }()
		select {
		case <-started:
		case <-time.After(10 * time.Second):
			t.Fatal("billing fetch did not start")
		}
		return release, done
	}
	finish := func(release chan struct{}, done <-chan error) {
		t.Helper()
		release <- struct{}{}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	invoicePath := fmt.Sprintf("/api/v1/providers/%s/invoiced-costs?start=%s&end=%s", providerID, day1.Format("2006-01-02"), day3.AddDate(0, 0, 1).Format("2006-01-02"))
	release, done := begin()
	current := h.want(owner, "GET", path, nil, nil, 200)
	billingPut(h, owner, providerID, map[string]any{"scope": map[string]any{"project_ids": []string{"proj_bbb222"}}}, current["etag"].(string), 200)
	finish(release, done)
	items := h.want(owner, "GET", invoicePath, nil, nil, 200)["items"].([]any)
	current = h.want(owner, "GET", path, nil, nil, 200)
	if len(items) != 0 || current["status"].(map[string]any)["last_success_at"] != nil {
		t.Fatalf("old scope committed after a change: invoices=%v status=%v", items, current["status"])
	}
	headers := etagHeader(current)
	headers["Idempotency-Key"] = uuid.NewString()
	h.want(owner, "POST", path+"/run", nil, headers, 202)
	if err := billingImporter(h, t).Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	items = h.want(owner, "GET", invoicePath, nil, nil, 200)["items"].([]any)
	if len(items) == 0 {
		t.Fatal("new scope did not import its rows")
	}
	current = h.want(owner, "GET", path, nil, nil, 200)
	billingPut(h, owner, providerID, map[string]any{"scope": map[string]any{"project_ids": []string{"proj_aaa111"}}}, current["etag"].(string), 200)
	current = h.want(owner, "GET", path, nil, nil, 200)
	status := current["status"].(map[string]any)
	if status["imported_from"] != nil || status["imported_through"] != nil {
		t.Fatalf("previous scope coverage was retained: %v", status)
	}
	retained := h.want(owner, "GET", invoicePath, nil, nil, 200)["items"].([]any)
	if !reflect.DeepEqual(retained, items) {
		t.Fatalf("scope change removed invoice history: %v", retained)
	}
	fake.mu.Lock()
	queryIndex := len(fake.queries)
	fake.mu.Unlock()
	headers = etagHeader(current)
	headers["Idempotency-Key"] = uuid.NewString()
	h.want(owner, "POST", path+"/run", nil, headers, 202)
	if err := billingImporter(h, t).Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	query := fake.call(queryIndex)
	start, startErr := strconv.ParseInt(query.Get("start_time"), 10, 64)
	end, endErr := strconv.ParseInt(query.Get("end_time"), 10, 64)
	if startErr != nil || endErr != nil || end-start != int64(30*24*time.Hour/time.Second) || query.Get("project_ids[]") != "proj_aaa111" {
		t.Fatalf("returning to the prior scope did not use its first-import window: %v", query)
	}
	items = h.want(owner, "GET", invoicePath, nil, nil, 200)["items"].([]any)
	fake.mu.Lock()
	fake.shift[`"value": 0.2,`] = `"value": 9.9,`
	fake.mu.Unlock()
	current = h.want(owner, "GET", path, nil, nil, 200)
	headers = etagHeader(current)
	headers["Idempotency-Key"] = uuid.NewString()
	h.want(owner, "POST", path+"/run", nil, headers, 202)
	release, done = begin()
	current = h.want(owner, "GET", path, nil, nil, 200)
	h.want(owner, "DELETE", path, nil, etagHeader(current), 204)
	finish(release, done)
	afterDelete := h.want(owner, "GET", invoicePath, nil, nil, 200)["items"].([]any)
	if !reflect.DeepEqual(afterDelete, items) {
		t.Fatalf("fetch changed retained invoices after deletion: %v", afterDelete)
	}
}

func TestBillingImportCredentialCompatibilityAcrossKinds(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	day1, day2, day3 := billingDays()
	openai := newBillingFake(t, "/v1/organization/costs", map[string]string{
		"": "openai_costs_page1.json", "page-2-token": "openai_costs_page2.json",
	}, map[string]string{"1759104000": fmt.Sprint(day1.Unix()), "1759190400": fmt.Sprint(day2.Unix()), "1759276800": fmt.Sprint(day3.Unix())})
	anthropic := newBillingFake(t, "/v1/organizations/cost_report", map[string]string{
		"": "anthropic_cost_report_page1.json", "page-2-token": "anthropic_cost_report_page2.json",
	}, map[string]string{
		`"starting_at": "2025-10-01T`: `"starting_at": "` + day1.Format("2006-01-02") + "T",
		`"ending_at": "2025-10-02T`:   `"ending_at": "` + day2.Format("2006-01-02") + "T",
		`"starting_at": "2025-10-02T`: `"starting_at": "` + day2.Format("2006-01-02") + "T",
		`"ending_at": "2025-10-03T`:   `"ending_at": "` + day3.Format("2006-01-02") + "T",
		`"starting_at": "2025-10-03T`: `"starting_at": "` + day3.Format("2006-01-02") + "T",
		`"ending_at": "2025-10-04T`:   `"ending_at": "` + day3.AddDate(0, 0, 1).Format("2006-01-02") + "T",
	})
	oldKey := "sk-admin-" + strings.Repeat("o", 30)
	anthropic.expectedAPIKey = oldKey
	provider := billingProvider(h, owner, "Changed kind", "openai", openai.server.URL+"/v1", nil)
	providerID := billingProviderID(provider)
	path := "/api/v1/providers/" + providerID + "/billing-import"
	billingPut(h, owner, providerID, map[string]any{"credential": oldKey}, provider["etag"].(string), 201)
	providerPath := "/api/v1/providers/" + providerID
	credentials := h.want(owner, "GET", providerPath+"/credentials", nil, nil, 200)["items"].([]any)
	credentialID := credentials[0].(map[string]any)["id"].(string)
	headers := etagHeader(provider)
	headers["Idempotency-Key"] = uuid.NewString()
	h.want(owner, "POST", providerPath+"/credentials/"+credentialID+"/revoke", nil, headers, 200)
	provider = h.want(owner, "GET", providerPath, nil, nil, 200)
	provider = h.want(owner, "PATCH", "/api/v1/providers/"+providerID, map[string]any{"name": "Changed kind", "configuration": map[string]any{
		"kind": "anthropic", "auth_mode": "api_key", "endpoint": anthropic.server.URL + "/v1",
	}}, etagHeader(provider), 200)
	current := h.want(owner, "GET", path, nil, nil, 200)
	billingPut(h, owner, providerID, map[string]any{}, current["etag"].(string), 200)
	if err := billingImporter(h, t).Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	current = h.want(owner, "GET", path, nil, nil, 200)
	status := current["status"].(map[string]any)
	if status["last_success_at"] == nil || status["last_error_code"] != nil {
		t.Fatalf("retained polling credential failed after changing kind: %v", status)
	}
	newAnthropicKey := "sk-ant-" + strings.Repeat("a", 30)
	billingPut(h, owner, providerID, map[string]any{"credential": newAnthropicKey}, current["etag"].(string), 200)
	h.want(owner, "PATCH", "/api/v1/providers/"+providerID, map[string]any{"name": "Changed kind", "configuration": map[string]any{
		"kind": "openai", "auth_mode": "api_key", "endpoint": openai.server.URL + "/v1",
	}}, etagHeader(provider), 200)
	export := h.want(owner, "GET", "/api/v1/configuration/export", nil, nil, 200)
	replacement := "sk-admin-" + strings.Repeat("n", 30)
	body := map[string]any{"document": export["document"], "secret_bindings": map[string]any{"Changed kind/billing": replacement, "Changed kind/default": "sk-provider-" + strings.Repeat("n", 30)}}
	h.want(owner, "POST", "/api/v1/configuration/apply", body, idem("changed-kind"), 200)
	openai.mu.Lock()
	openai.expectedAuthorization = "Bearer " + replacement
	openai.mu.Unlock()
	current = h.want(owner, "GET", path, nil, nil, 200)
	headers = etagHeader(current)
	headers["Idempotency-Key"] = uuid.NewString()
	h.want(owner, "POST", path+"/run", nil, headers, 202)
	if err := billingImporter(h, t).Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	current = h.want(owner, "GET", path, nil, nil, 200)
	status = current["status"].(map[string]any)
	if status["last_success_at"] == nil || status["last_error_code"] != nil {
		t.Fatalf("replacement credential failed after configuration apply: %v", status)
	}
}

func TestBillingImportMigrationPreservesHistory(t *testing.T) {
	pool, dbURL := accessDatabase(t)
	if _, err := pool.Exec(t.Context(), `CREATE SCHEMA olp; CREATE TABLE olp.migrations
		(version text PRIMARY KEY, checksum bytea NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir("../../internal/database/migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		if entry.Name() >= "0123_billing_currency_revision.sql" {
			break
		}
		data, err := os.ReadFile("../../internal/database/migrations/" + entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(t.Context(), string(data)); err != nil {
			t.Fatalf("migration %s: %v", entry.Name(), err)
		}
		checksum := sha256.Sum256(data)
		if _, err := pool.Exec(t.Context(), `INSERT INTO olp.migrations(version,checksum) VALUES($1,$2)`, entry.Name(), checksum[:]); err != nil {
			t.Fatal(err)
		}
	}
	userID, providerID := uuid.NewString(), uuid.NewString()
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.users(id,email,display_name,role,etag)
		VALUES($1::uuid,'history@example.test','History','viewer',$2::uuid)`, userID, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.providers(id,name,kind,state,configuration,etag,slots_etag,created_by)
		VALUES($1::uuid,'Migration','openai','draft','{"kind":"openai","auth_mode":"api_key"}', $2::uuid,$3::uuid,$4::uuid)`,
		providerID, uuid.NewString(), uuid.NewString(), userID); err != nil {
		t.Fatal(err)
	}
	day1, day2, day3 := billingDays()
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.provider_invoiced_costs
		(provider_id,day,line_item,currency,amount,source,fetched_at) VALUES
		($1::uuid,$2::date,'tokens','USD',0.123456789012,'openai_costs','2025-01-01T12:00:00Z'),
		($1::uuid,$3::date,'storage','EUR',12.34,'openai_costs','2025-01-02T12:00:00Z')`,
		providerID, day1.Format("2006-01-02"), day2.Format("2006-01-02")); err != nil {
		t.Fatal(err)
	}
	legacyProvider := uuid.NewString()
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.providers(id,name,kind,state,configuration,etag,slots_etag,created_by)
		VALUES($1::uuid,'Legacy range','openai','draft','{"kind":"openai","auth_mode":"api_key"}', $2::uuid,$3::uuid,$4::uuid)`,
		legacyProvider, uuid.NewString(), uuid.NewString(), userID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.provider_billing_imports(provider_id,source,etag,created_by,imported_from,imported_through,last_success_at)
		VALUES($1::uuid,'openai_costs',$2::uuid,$3::uuid,$4::date,$5::date,now())`, legacyProvider, uuid.NewString(), userID, day1.AddDate(0, 0, -1), day2); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.provider_invoiced_costs(provider_id,day,line_item,currency,amount,source)
		VALUES($1::uuid,$2::date,'tokens','USD',1,'openai_costs')`, legacyProvider, day1); err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	h := newAccessHarnessOn(t, pool, dbURL)
	owner := h.owner()
	legacyDay := day1.AddDate(0, 0, -1)
	legacyPrice := "0.15"
	billingFact(t, h, legacyProvider, uuid.NewString(), legacyDay, "billable", true, &legacyPrice, false, false, false)
	legacyReport := h.want(owner, "GET", fmt.Sprintf("/api/v1/usage/reconciliation?start=%s&end=%s&provider_id=%s", legacyDay.Format("2006-01-02"), day1.Format("2006-01-02"), legacyProvider), nil, nil, 200)
	legacyItem := legacyReport["items"].([]any)[0].(map[string]any)
	if legacyItem["invoiced_cost"] != nil || legacyItem["capture_rate_unavailable_reason"] != "not_imported" {
		t.Fatalf("migration invented coverage from a legacy range: %v", legacyReport)
	}
	legacyStatus := h.want(owner, "GET", "/api/v1/providers/"+legacyProvider+"/billing-import", nil, nil, 200)["status"].(map[string]any)
	if legacyStatus["imported_from"] != nil || legacyStatus["imported_through"] != nil {
		t.Fatalf("migration attributed unknown historical scope to the current import: %v", legacyStatus)
	}
	path := fmt.Sprintf("/api/v1/providers/%s/invoiced-costs?start=%s&end=%s", providerID, day1.Format("2006-01-02"), day3.Format("2006-01-02"))
	items := h.want(owner, "GET", path, nil, nil, 200)["items"].([]any)
	expected := []map[string]string{
		{"day": day1.Format("2006-01-02"), "line_item": "tokens", "currency": "USD", "amount": "0.123456789012", "source": "openai_costs", "fetched_at": "2025-01-01T12:00:00Z"},
		{"day": day2.Format("2006-01-02"), "line_item": "storage", "currency": "EUR", "amount": "12.34", "source": "openai_costs", "fetched_at": "2025-01-02T12:00:00Z"},
	}
	if len(items) != len(expected) {
		t.Fatalf("invoice history changed after migration: %v", items)
	}
	for index, expectedItem := range expected {
		item := items[index].(map[string]any)
		for _, field := range []string{"day", "line_item", "currency", "source", "fetched_at"} {
			if item[field] != expectedItem[field] {
				t.Fatalf("invoice history changed after migration: %v", item)
			}
		}
		amount, err := decimal.NewFromString(item["amount"].(string))
		expectedAmount, expectedErr := decimal.NewFromString(expectedItem["amount"])
		if err != nil || expectedErr != nil || !amount.Equal(expectedAmount) {
			t.Fatalf("invoice amount changed after migration: %v", item)
		}
	}
	report := h.want(owner, "GET", fmt.Sprintf("/api/v1/usage/reconciliation?start=%s&end=%s&provider_id=%s", day1.Format("2006-01-02"), day3.Format("2006-01-02"), providerID), nil, nil, 200)
	reconciled := report["items"].([]any)
	if len(reconciled) != len(expected) {
		t.Fatalf("invoice history was not reconcilable after migration: %v", report)
	}
	for index, want := range expected {
		item := reconciled[index].(map[string]any)
		amount, err := decimal.NewFromString(item["invoiced_cost"].(string))
		expectedAmount, expectedErr := decimal.NewFromString(want["amount"])
		if err != nil || expectedErr != nil || !amount.Equal(expectedAmount) {
			t.Fatalf("reconciled amount changed after migration: %v", item)
		}
	}
}

func TestBillingImportAWSScope(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	provider := h.want(owner, "POST", "/api/v1/providers", map[string]any{
		"name":          "Bedrock billing",
		"configuration": map[string]any{"kind": "bedrock", "auth_mode": "static", "endpoint": "https://bedrock-runtime.us-east-1.amazonaws.com", "cloud_region": "us-east-1"},
		"credential":    `{"access_key_id":"BEDROCKKEY1234567890","secret_access_key":"bedrock-secret-123456789"}`,
		"model":         "amazon.titan-text-express-v1",
	}, idem("aws-billing-provider"), 201)
	id := billingProviderID(provider)
	scope := map[string]any{"payer_account_id": "111122223333", "bucket": "billing-report-bucket", "report_prefix": "billing/cur", "region": "us-east-1", "linked_account_ids": []string{"444455556666"}}
	credential := map[string]any{"auth_mode": "static", "access_key_id": "BILLINGKEY1234567890", "secret_access_key": "billing-secret-example"}
	billingPut(h, owner, id, map[string]any{"scope": map[string]any{"payer_account_id": "invalid", "bucket": "billing-report-bucket", "report_prefix": "billing/cur", "region": "us-east-1"}, "credential": credential}, provider["etag"].(string), 422)
	billingPut(h, owner, id, map[string]any{"scope": map[string]any{"project_ids": []string{"proj_abc"}}, "credential": credential}, provider["etag"].(string), 422)
	billingPut(h, owner, id, map[string]any{"credential": credential}, provider["etag"].(string), 422)
	billingPut(h, owner, id, map[string]any{"scope": scope}, provider["etag"].(string), 422)
	billingPut(h, owner, id, map[string]any{"scope": scope, "credential": nil}, provider["etag"].(string), 422)
	created := billingPut(h, owner, id, map[string]any{"scope": scope, "credential": credential}, provider["etag"].(string), 201)
	if created["source"] != "aws_cur" || created["credential"] != nil {
		t.Fatalf("billing import must have the AWS source and keep the credential write-only: %v", created)
	}
}

func TestBillingImportPollingReconciliationAfterDeletion(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	day1, day2, day3 := billingDays()
	fake := newBillingFake(t, "/v1/organization/costs", map[string]string{
		"": "openai_costs_page1.json", "page-2-token": "openai_costs_page2.json",
	}, map[string]string{"1759104000": fmt.Sprint(day1.Unix()), "1759190400": fmt.Sprint(day2.Unix()), "1759276800": fmt.Sprint(day3.Unix())})
	provider := billingProvider(h, owner, "Polling history", "openai", fake.server.URL+"/v1", nil)
	id := billingProviderID(provider)
	billingPut(h, owner, id, map[string]any{"credential": "sk-admin-" + strings.Repeat("k", 30)}, provider["etag"].(string), 201)
	price := "0.15"
	for _, day := range []time.Time{day1.AddDate(0, 0, -1), day1, day2} {
		billingFact(t, h, id, uuid.NewString(), day, "billable", true, &price, false, false, false)
	}
	if err := billingImporter(h, t).Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	start, end := day1.AddDate(0, 0, -1).Format("2006-01-02"), day3.AddDate(0, 0, 1).Format("2006-01-02")
	path := fmt.Sprintf("/api/v1/usage/reconciliation?start=%s&end=%s&provider_id=%s", start, end, id)
	before := h.want(owner, "GET", path, nil, nil, 200)
	importPath := "/api/v1/providers/" + id + "/billing-import"
	h.want(owner, "DELETE", importPath, nil, etagHeader(h.want(owner, "GET", importPath, nil, nil, 200)), 204)
	after := h.want(owner, "GET", path, nil, nil, 200)
	check := func(report map[string]any) {
		t.Helper()
		for i := range before["items"].([]any) {
			old := before["items"].([]any)[i].(map[string]any)
			item := report["items"].([]any)[i].(map[string]any)
			if item["invoiced_cost"] != old["invoiced_cost"] || item["capture_rate"] != old["capture_rate"] || item["capture_rate_unavailable_reason"] != old["capture_rate_unavailable_reason"] || item["provisional"] != old["provisional"] {
				t.Fatalf("historical polling reconciliation lost coverage on day %d: before=%v after=%v", i, old, item)
			}
		}
	}
	check(after)
	key := "sk-admin-" + strings.Repeat("k", 30)
	recreate := func() {
		t.Helper()
		provider := h.want(owner, "GET", "/api/v1/providers/"+id, nil, nil, 200)
		billingPut(h, owner, id, map[string]any{"credential": key}, provider["etag"].(string), 201)
	}
	recreate()
	check(h.want(owner, "GET", path, nil, nil, 200))
	export := h.want(owner, "GET", "/api/v1/configuration/export", nil, nil, 200)
	document := export["document"].(map[string]any)
	for _, value := range document["providers"].([]any) {
		entry := value.(map[string]any)
		if entry["name"] == "Polling history" {
			delete(entry, "billing_import")
		}
	}
	h.want(owner, "POST", "/api/v1/configuration/apply", map[string]any{"document": document, "secret_bindings": map[string]any{}}, idem("polling-history-remove"), 200)
	h.want(owner, "GET", importPath, nil, nil, 404)
	check(h.want(owner, "GET", path, nil, nil, 200))
	setRetention := func(value string) {
		t.Helper()
		setting := h.want(owner, "GET", "/api/v1/settings/retention.usage_days", nil, nil, 200)
		h.want(owner, "PUT", "/api/v1/settings/retention.usage_days", map[string]any{"value": value}, etagHeader(setting), 200)
	}
	setRetention("1")
	if err := billingImporter(h, t).Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	setRetention("90")
	recreate()
	retained := h.want(owner, "GET", path, nil, nil, 200)
	for _, value := range retained["items"].([]any) {
		item := value.(map[string]any)
		if item["day"] == day1.Format("2006-01-02") && (item["invoiced_cost"] != nil || item["capture_rate_unavailable_reason"] != "not_imported") {
			t.Fatalf("recreated polling import reported purged coverage: %v", item)
		}
	}
	current := h.want(owner, "GET", importPath, nil, nil, 200)
	billingPut(h, owner, id, map[string]any{"scope": map[string]any{"project_ids": []string{"proj_another"}}}, current["etag"].(string), 200)
	changed := h.want(owner, "GET", path, nil, nil, 200)
	for index, value := range changed["items"].([]any) {
		item := value.(map[string]any)
		previous := retained["items"].([]any)[index].(map[string]any)
		if item["invoiced_cost"] != previous["invoiced_cost"] || item["capture_rate_unavailable_reason"] != previous["capture_rate_unavailable_reason"] {
			t.Fatalf("scope change altered historical reconciliation: before=%v after=%v", retained, changed)
		}
	}
	status := h.want(owner, "GET", importPath, nil, nil, 200)["status"].(map[string]any)
	if status["imported_from"] != nil || status["imported_through"] != nil {
		t.Fatalf("changed scope inherited historical coverage: %v", status)
	}
}

func TestBillingImportPollingDisjointRecreation(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	today := time.Now().UTC().Truncate(24 * time.Hour)
	old := today.AddDate(0, 0, -20)
	recent := today.AddDate(0, 0, -3)
	gap := today.AddDate(0, 0, -10)
	shift := func(start time.Time) map[string]string {
		return map[string]string{
			"1759104000": fmt.Sprint(start.Unix()),
			"1759190400": fmt.Sprint(start.AddDate(0, 0, 1).Unix()),
			"1759276800": fmt.Sprint(start.AddDate(0, 0, 2).Unix()),
		}
	}
	fake := newBillingFake(t, "/v1/organization/costs", map[string]string{"": "openai_costs_page1.json", "page-2-token": "openai_costs_page2.json"}, shift(old))
	provider := billingProvider(h, owner, "Polling gap", "openai", fake.server.URL+"/v1", nil)
	id := billingProviderID(provider)
	credential := "sk-admin-" + strings.Repeat("k", 30)
	path := "/api/v1/providers/" + id + "/billing-import"
	billingPut(h, owner, id, map[string]any{"credential": credential}, provider["etag"].(string), 201)
	importer := billingImporter(h, t)
	if err := importer.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	current := h.want(owner, "GET", path, nil, nil, 200)
	headers := etagHeader(current)
	headers["Idempotency-Key"] = uuid.NewString()
	h.want(owner, "DELETE", path, nil, headers, 204)
	provider = h.want(owner, "GET", "/api/v1/providers/"+id, nil, nil, 200)
	billingPut(h, owner, id, map[string]any{"credential": credential}, provider["etag"].(string), 201)
	fake.mu.Lock()
	fake.shift = shift(recent)
	fake.mu.Unlock()
	if err := importer.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	amount := "0.2"
	billingFact(t, h, id, uuid.NewString(), gap.Add(time.Hour), "billable", true, &amount, false, false, false)
	report := h.want(owner, "GET", fmt.Sprintf("/api/v1/usage/reconciliation?start=%s&end=%s&provider_id=%s", gap.Format("2006-01-02"), gap.AddDate(0, 0, 1).Format("2006-01-02"), id), nil, nil, 200)
	items := report["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["capture_rate_unavailable_reason"] != "not_imported" || items[0].(map[string]any)["invoiced_cost"] != nil {
		t.Fatalf("unfetched gap appeared covered: %v", report)
	}
	status := h.want(owner, "GET", path, nil, nil, 200)["status"].(map[string]any)
	if status["imported_from"] != recent.Format("2006-01-02") || status["imported_through"] != recent.AddDate(0, 0, 2).Format("2006-01-02") {
		t.Fatalf("status must report a contiguous fetched range: %v", status)
	}
}
