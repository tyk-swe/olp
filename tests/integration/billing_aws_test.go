//go:build integration

package integration_test

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/tyk-swe/olp/internal/billing"
)

type awsObject struct {
	body     []byte
	modified time.Time
}

type awsBillingFake struct {
	mu       sync.Mutex
	objects  map[string]awsObject
	server   *httptest.Server
	pauseKey string
	started  chan struct{}
	release  chan struct{}
}

func newAWSBillingFake(t *testing.T) *awsBillingFake {
	t.Helper()
	f := &awsBillingFake{objects: map[string]awsObject{}}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Authorization"), "Credential=BILLINGKEY1234567890/") {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		f.mu.Lock()
		pause := f.pauseKey != "" && strings.HasSuffix(r.URL.Path, f.pauseKey)
		started, release := f.started, f.release
		if pause {
			f.pauseKey = ""
		}
		f.mu.Unlock()
		if pause {
			close(started)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.URL.Query().Get("list-type") == "2" {
			w.Header().Set("Content-Type", "application/xml")
			fmt.Fprint(w, `<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><IsTruncated>false</IsTruncated>`)
			for key, object := range f.objects {
				if strings.HasPrefix(key, r.URL.Query().Get("prefix")) {
					fmt.Fprint(w, "<Contents><Key>")
					_ = xml.EscapeText(w, []byte(key))
					fmt.Fprintf(w, "</Key><LastModified>%s</LastModified><Size>%d</Size></Contents>", object.modified.UTC().Format(time.RFC3339), len(object.body))
				}
			}
			fmt.Fprint(w, "</ListBucketResult>")
			return
		}
		key := strings.TrimPrefix(r.URL.Path, "/billing-report-bucket/")
		object, ok := f.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		http.ServeContent(w, r, key, object.modified, bytes.NewReader(object.body))
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *awsBillingFake) delivery(t *testing.T, day time.Time, assembly string, modified time.Time, amount string) {
	t.Helper()
	start := time.Date(day.Year(), day.Month(), 1, 0, 0, 0, 0, time.UTC)
	period := start.Format("20060102") + "-" + start.AddDate(0, 1, 0).Format("20060102")
	prefix := "billing/cur/" + period + "/" + assembly + "/"
	part := prefix + "cur-1.csv.gz"
	manifest, _ := json.Marshal(map[string]any{"assemblyId": assembly, "billingPeriod": map[string]string{"start": start.Format(time.RFC3339), "end": start.AddDate(0, 1, 0).Format(time.RFC3339)}, "reportKeys": []string{part}})
	header := "bill/PayerAccountId,lineItem/UsageAccountId,lineItem/ProductCode,lineItem/UsageStartDate,lineItem/LineItemType,lineItem/UsageType,lineItem/Operation,product/sku,lineItem/CurrencyCode,lineItem/UnblendedCost\n"
	row := func(account, product, hour, amount string) string {
		return fmt.Sprintf("111122223333,%s,%s,%sT%s:00:00Z,Usage,InputTokens,InvokeModel,sku1,USD,%s\n", account, product, day.Format("2006-01-02"), hour, amount)
	}
	csv := header + row("444455556666", "AmazonBedrock", "01", amount) + row("444455556666", "AmazonBedrock", "02", "0.20000000000000000001") + row("777788889999", "AmazonBedrock", "03", "10") + row("444455556666", "AmazonEC2", "04", "100")
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	if _, err := gz.Write([]byte(csv)); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[prefix+"cur-Manifest.json"] = awsObject{body: manifest, modified: modified}
	f.objects[part] = awsObject{body: compressed.Bytes(), modified: modified}
}

func (f *awsBillingFake) rewriteCSV(t *testing.T, replace func(string) string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for key, object := range f.objects {
		if !strings.HasSuffix(key, ".csv.gz") {
			continue
		}
		reader, err := gzip.NewReader(bytes.NewReader(object.body))
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		if err := reader.Close(); err != nil {
			t.Fatal(err)
		}
		var compressed bytes.Buffer
		writer := gzip.NewWriter(&compressed)
		if _, err := writer.Write([]byte(replace(string(data)))); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		f.objects[key] = awsObject{body: compressed.Bytes(), modified: object.modified}
	}
}

func TestBillingImportAWSDelivery(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	fake := newAWSBillingFake(t)
	today := time.Now().UTC().Truncate(24 * time.Hour)
	day := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -1, 3)
	fake.delivery(t, day, "assembly-one", day.AddDate(0, 0, 4), "0.1")
	fake.rewriteCSV(t, func(csv string) string {
		rows := strings.Split(strings.TrimSuffix(csv, "\n"), "\n")
		result := rows[0] + ",identity/LineItemId,reservation/EffectiveCost,savingsPlan/SavingsPlanEffectiveCost,lineItem/UsageEndDate\n"
		for index, row := range rows[1:] {
			result += fmt.Sprintf("%s,id-%d,,,\n", row, index)
		}
		result += rows[1] + ",id-0,,,\n"
		prefix := "111122223333,444455556666,AmazonBedrock," + day.Format("2006-01-02") + "T03:00:00Z,"
		result += prefix + "DiscountedUsage,InputTokens,InvokeModel,sku-discount,USD,900,id-discount,0.12,,\n"
		result += prefix + "SavingsPlanCoveredUsage,InputTokens,InvokeModel,sku-plan,USD,800,id-plan,,0.33,\n"
		result += prefix + "SavingsPlanNegation,InputTokens,InvokeModel,sku-plan,USD,-800,id-negation,,,\n"
		result += "111122223333,444455556666,AmazonBedrock," + day.Format("2006-01-02") + "T23:30:00Z,Usage,InputTokens,InvokeModel,sku1,USD,0.05,id-midnight,,," + day.AddDate(0, 0, 1).Format("2006-01-02") + "T00:30:00Z\n"
		next := "111122223333,444455556666,AmazonBedrock," + day.AddDate(0, 0, 1).Format("2006-01-02") + "T01:00:00Z,"
		result += next + "Usage,InputTokens,InvokeModel,sku2,USD,0.4,id-next,,,\n"
		result += next + "Credit,InputTokens,InvokeModel,sku2,EUR,-0.2,id-credit,,,\n"
		result += "111122223333,444455556666,AmazonBedrock," + day.AddDate(0, 0, 2).Format("2006-01-02") + "T01:00:00Z,Usage,InputTokens,InvokeModel,sku3,USD,0.25,id-no-usage,,,\n"
		return result
	})
	provider := h.want(owner, "POST", "/api/v1/providers", map[string]any{"name": "AWS delivery", "configuration": map[string]any{"kind": "bedrock", "auth_mode": "static", "endpoint": "https://bedrock-runtime.us-east-1.amazonaws.com", "cloud_region": "us-east-1"}, "credential": `{"access_key_id":"BEDROCKKEY1234567890","secret_access_key":"bedrock-secret-123456789"}`, "model": "amazon.titan-text-express-v1"}, idem("aws-delivery-provider"), 201)
	id := billingProviderID(provider)
	scope := map[string]any{"payer_account_id": "111122223333", "bucket": "billing-report-bucket", "report_prefix": "billing/cur", "region": "us-east-1", "linked_account_ids": []string{"444455556666"}}
	credential := map[string]any{"auth_mode": "static", "access_key_id": "BILLINGKEY1234567890", "secret_access_key": "billing-secret-example"}
	billingPut(h, owner, id, map[string]any{"scope": scope, "credential": credential}, provider["etag"].(string), 201)
	priced := "0.15"
	billingFact(t, h, id, uuid.NewString(), day, "billable", true, &priced, false, false, false)
	billingFact(t, h, id, uuid.NewString(), day, "billable", true, nil, true, false, false)
	billingFact(t, h, id, uuid.NewString(), day, "billable", false, nil, false, false, false)
	billingFact(t, h, id, uuid.NewString(), day, "billable", true, &priced, false, true, false)
	billingFact(t, h, id, uuid.NewString(), day, "billable", true, &priced, false, false, true)
	billingFact(t, h, id, uuid.NewString(), day.AddDate(0, 0, 3), "billable", true, &priced, false, false, false)
	importer := billingImporter(h, t)
	importer.AWSClient = func(config aws.Config) *s3.Client {
		return s3.NewFromConfig(config, func(o *s3.Options) { o.BaseEndpoint = aws.String(fake.server.URL); o.UsePathStyle = true })
	}
	if err := importer.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	status := h.want(owner, "GET", "/api/v1/providers/"+id+"/billing-import", nil, nil, 200)["status"].(map[string]any)
	if status["last_error_code"] != nil || status["source_revision"] != "assembly-one" {
		t.Fatalf("delivery status: %v", status)
	}
	date := day.Format("2006-01-02")
	result := h.want(owner, "GET", "/api/v1/providers/"+id+"/invoiced-costs?start="+date+"&end="+day.AddDate(0, 0, 1).Format("2006-01-02"), nil, nil, 200)
	items := result["items"].([]any)
	if len(items) != 3 {
		t.Fatalf("expected scoped Bedrock lines only: %v", items)
	}
	expected := map[string]decimal.Decimal{
		"Usage":                   decimal.RequireFromString("0.35000000000000000001"),
		"DiscountedUsage":         decimal.RequireFromString("0.12"),
		"SavingsPlanCoveredUsage": decimal.RequireFromString("0.33"),
	}
	for _, item := range items {
		entry := item.(map[string]any)
		label := entry["line_item"].(string)
		parts := strings.Split(label, " | ")
		amount, err := decimal.NewFromString(entry["amount"].(string))
		if len(parts) < 2 || err != nil || !amount.Equal(expected[parts[1]]) {
			t.Fatalf("unexpected exact or effective cost: %v", entry)
		}
	}
	report := h.want(owner, "GET", fmt.Sprintf("/api/v1/usage/reconciliation?start=%s&end=%s&provider_id=%s", date, day.AddDate(0, 0, 1).Format("2006-01-02"), id), nil, nil, 200)
	item := report["items"].([]any)[0].(map[string]any)
	rate, err := decimal.NewFromString(item["capture_rate"].(string))
	if err != nil || !rate.Equal(decimal.RequireFromString("0.1875")) || item["unpriced_share"] != "0.5" || item["priced_attempts"] != float64(1) || item["unpriced_attempts"] != float64(1) || item["incomplete_attempts"] != float64(1) || item["excluded_caller_attempts"] != float64(2) {
		t.Fatalf("AWS reconciliation: %v", item)
	}
	nextDay := day.AddDate(0, 0, 1)
	nextReport := h.want(owner, "GET", fmt.Sprintf("/api/v1/usage/reconciliation?start=%s&end=%s&provider_id=%s", nextDay.Format("2006-01-02"), nextDay.AddDate(0, 0, 3).Format("2006-01-02"), id), nil, nil, 200)
	byDay := map[string]map[string]any{}
	for _, result := range nextReport["items"].([]any) {
		entry := result.(map[string]any)
		byDay[entry["day"].(string)] = entry
	}
	mixed := byDay[nextDay.Format("2006-01-02")]
	if mixed["capture_rate"] != nil || mixed["capture_rate_unavailable_reason"] != "currency_mismatch" {
		t.Fatalf("mixed currencies were reconciled: %v", mixed)
	}
	unused := byDay[nextDay.AddDate(0, 0, 1).Format("2006-01-02")]
	if unused["capture_rate"] != "0" || unused["capture_rate_unavailable_reason"] != nil {
		t.Fatalf("invoice with no attributed usage: %v", unused)
	}
	empty := byDay[nextDay.AddDate(0, 0, 2).Format("2006-01-02")]
	if empty["capture_rate"] != nil || empty["capture_rate_unavailable_reason"] != "zero_invoice" {
		t.Fatalf("empty covered day: %v", empty)
	}
	nextCosts := h.want(owner, "GET", fmt.Sprintf("/api/v1/providers/%s/invoiced-costs?start=%s&end=%s", id, nextDay.Format("2006-01-02"), nextDay.AddDate(0, 0, 1).Format("2006-01-02")), nil, nil, 200)
	amounts := map[string]decimal.Decimal{}
	for _, cost := range nextCosts["items"].([]any) {
		entry := cost.(map[string]any)
		value, err := decimal.NewFromString(entry["amount"].(string))
		if err != nil {
			t.Fatal(err)
		}
		amounts[entry["currency"].(string)] = value
	}
	if len(amounts) != 2 || !amounts["USD"].Equal(decimal.RequireFromString("0.4")) || !amounts["EUR"].Equal(decimal.RequireFromString("-0.2")) {
		t.Fatalf("midnight usage or credit has an incorrect amount: %v", nextCosts)
	}
	export := h.want(owner, "GET", "/api/v1/configuration/export", nil, nil, 200)
	if strings.Contains(fmt.Sprint(export), "billing-secret-example") {
		t.Fatal("configuration export disclosed the billing credential")
	}
	binding, err := json.Marshal(credential)
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"document": export["document"], "secret_bindings": map[string]any{"AWS delivery/billing": string(binding)}}
	importPath := "/api/v1/providers/" + id + "/billing-import"
	currentImport := h.want(owner, "GET", importPath, nil, nil, 200)
	h.want(owner, "DELETE", importPath, nil, etagHeader(currentImport), 204)
	h.want(owner, "POST", "/api/v1/configuration/plan", body, nil, 200)
	h.want(owner, "POST", "/api/v1/configuration/apply", body, idem("aws-config-apply"), 200)
	recreated := h.want(owner, "GET", importPath, nil, nil, 200)
	if recreated["source"] != "aws_cur" || strings.Contains(fmt.Sprint(recreated), "billing-secret-example") {
		t.Fatalf("configuration did not recreate the write-only AWS import: %v", recreated)
	}
	if err := importer.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	status = h.want(owner, "GET", importPath, nil, nil, 200)["status"].(map[string]any)
	if status["last_error_code"] != nil || status["source_revision"] != "assembly-one" {
		t.Fatalf("recreated AWS import did not authenticate: %v", status)
	}
	h.want(owner, "POST", "/api/v1/configuration/apply", body, idem("aws-config-repeat"), 200)
}

func TestBillingImportAWSRevisions(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	fake := newAWSBillingFake(t)
	today := time.Now().UTC().Truncate(24 * time.Hour)
	day := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -1, 3)
	newer := day.AddDate(0, 0, 1).Add(2 * time.Hour)
	fake.delivery(t, day, "newer", newer, "0.1")
	fake.rewriteCSV(t, func(csv string) string {
		return csv + fmt.Sprintf("111122223333,444455556666,AmazonBedrock,%sT01:00:00Z,Usage,InputTokens,InvokeModel,sku1,USD,99\n", day.AddDate(0, 0, 2).Format("2006-01-02"))
	})
	provider := h.want(owner, "POST", "/api/v1/providers", map[string]any{"name": "AWS revisions", "configuration": map[string]any{"kind": "bedrock", "auth_mode": "static", "endpoint": "https://bedrock-runtime.us-east-1.amazonaws.com", "cloud_region": "us-east-1"}, "credential": `{"access_key_id":"BEDROCKKEY1234567890","secret_access_key":"bedrock-secret-123456789"}`, "model": "amazon.titan-text-express-v1"}, idem("aws-revision-provider"), 201)
	id := billingProviderID(provider)
	scope := map[string]any{"payer_account_id": "111122223333", "bucket": "billing-report-bucket", "report_prefix": "billing/cur", "region": "us-east-1", "linked_account_ids": []string{"444455556666"}}
	credential := map[string]any{"auth_mode": "static", "access_key_id": "BILLINGKEY1234567890", "secret_access_key": "billing-secret-example"}
	billingPut(h, owner, id, map[string]any{"scope": scope, "credential": credential}, provider["etag"].(string), 201)
	importer := billingImporter(h, t)
	importer.AWSClient = func(config aws.Config) *s3.Client {
		return s3.NewFromConfig(config, func(o *s3.Options) { o.BaseEndpoint = aws.String(fake.server.URL); o.UsePathStyle = true })
	}
	current := day.AddDate(0, 0, 1)
	importer.Now = func() time.Time { return current }
	pass := func() {
		t.Helper()
		if err := importer.Pass(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	schedule := func() {
		t.Helper()
		headers := etagHeader(h.want(owner, "GET", "/api/v1/providers/"+id+"/billing-import", nil, nil, 200))
		headers["Idempotency-Key"] = fmt.Sprintf("aws-run-%d", time.Now().UnixNano())
		h.want(owner, "POST", "/api/v1/providers/"+id+"/billing-import/run", nil, headers, 202)
	}
	status := func() map[string]any {
		t.Helper()
		return h.want(owner, "GET", "/api/v1/providers/"+id+"/billing-import", nil, nil, 200)["status"].(map[string]any)
	}
	amounts := func() []any {
		t.Helper()
		return h.want(owner, "GET", "/api/v1/providers/"+id+"/invoiced-costs?start="+day.Format("2006-01-02")+"&end="+day.AddDate(0, 0, 1).Format("2006-01-02"), nil, nil, 200)["items"].([]any)
	}
	pass()
	if status()["source_revision"] != "newer" || status()["imported_through"] != day.Format("2006-01-02") {
		t.Fatalf("initial status %v", status())
	}
	current = current.AddDate(0, 0, 1)
	schedule()
	pass()
	if status()["imported_through"] != day.Format("2006-01-02") || len(amounts()) != 1 {
		t.Fatalf("stale equal revision extended coverage: %v %v", status(), amounts())
	}
	openDay := day.AddDate(0, 0, 2)
	openCosts := h.want(owner, "GET", fmt.Sprintf("/api/v1/providers/%s/invoiced-costs?start=%s&end=%s", id, openDay.Format("2006-01-02"), openDay.AddDate(0, 0, 1).Format("2006-01-02")), nil, nil, 200)
	if len(openCosts["items"].([]any)) != 0 {
		t.Fatalf("open UTC day was imported: %v", openCosts)
	}
	headers := etagHeader(h.want(owner, "GET", "/api/v1/providers/"+id+"/billing-import", nil, nil, 200))
	h.want(owner, "DELETE", "/api/v1/providers/"+id+"/billing-import", nil, headers, 204)
	provider = h.want(owner, "GET", "/api/v1/providers/"+id, nil, nil, 200)
	billingPut(h, owner, id, map[string]any{"scope": scope, "credential": credential}, provider["etag"].(string), 201)
	if status()["source_revision"] != "newer" || status()["imported_through"] != day.Format("2006-01-02") {
		t.Fatalf("recreated import lost coverage: %v", status())
	}
	fake.mu.Lock()
	fake.objects = map[string]awsObject{}
	fake.mu.Unlock()
	fake.delivery(t, day, "older", newer.Add(-time.Hour), "5")
	pass()
	if status()["source_revision"] != "newer" || len(amounts()) != 1 || amounts()[0].(map[string]any)["amount"] != "0.30000000000000000001" {
		t.Fatalf("older revision replaced history: %v %v", status(), amounts())
	}
	fake.mu.Lock()
	fake.objects = map[string]awsObject{}
	fake.mu.Unlock()
	fake.delivery(t, day, "newer", newer, "0.1")
	scopeB := map[string]any{"payer_account_id": "111122223333", "bucket": "billing-report-bucket", "report_prefix": "billing/cur", "region": "us-east-1", "linked_account_ids": []string{"777788889999"}}
	currentImport := h.want(owner, "GET", "/api/v1/providers/"+id+"/billing-import", nil, nil, 200)
	billingPut(h, owner, id, map[string]any{"scope": scopeB}, currentImport["etag"].(string), 200)
	schedule()
	pass()
	if len(amounts()) != 1 || amounts()[0].(map[string]any)["amount"] != "10" {
		t.Fatalf("scope B: %v %v", status(), amounts())
	}
	currentImport = h.want(owner, "GET", "/api/v1/providers/"+id+"/billing-import", nil, nil, 200)
	billingPut(h, owner, id, map[string]any{"scope": scope}, currentImport["etag"].(string), 200)
	if status()["imported_through"] != nil {
		t.Fatalf("scope A inherited B coverage: %v", status())
	}
	fake.mu.Lock()
	fake.objects = map[string]awsObject{}
	fake.mu.Unlock()
	fake.delivery(t, day, "older", newer.Add(-time.Hour), "5")
	schedule()
	pass()
	if status()["imported_through"] != nil || amounts()[0].(map[string]any)["amount"] != "10" {
		t.Fatalf("older A revision applied: %v %v", status(), amounts())
	}
	fake.mu.Lock()
	fake.objects = map[string]awsObject{}
	fake.mu.Unlock()
	fake.delivery(t, day, "newer", newer, "0.1")
	schedule()
	pass()
	if status()["source_revision"] != "newer" || status()["imported_through"] == nil || amounts()[0].(map[string]any)["amount"] != "0.30000000000000000001" {
		t.Fatalf("equal A revision was not restored: %v %v", status(), amounts())
	}
}

func TestBillingImportAWSDiscardsChangedInFlightDelivery(t *testing.T) {
	for _, action := range []string{"scope", "delete"} {
		t.Run(action, func(t *testing.T) {
			h := newAccessHarness(t)
			owner := h.owner()
			fake := newAWSBillingFake(t)
			today := time.Now().UTC().Truncate(24 * time.Hour)
			day := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -1, 3)
			fake.delivery(t, day, "in-flight", day.AddDate(0, 0, 1).Add(time.Hour), "0.1")
			fake.pauseKey = ".csv.gz"
			fake.started, fake.release = make(chan struct{}), make(chan struct{})
			provider := h.want(owner, "POST", "/api/v1/providers", map[string]any{"name": "AWS in flight", "configuration": map[string]any{"kind": "bedrock", "auth_mode": "static", "endpoint": "https://bedrock-runtime.us-east-1.amazonaws.com", "cloud_region": "us-east-1"}, "credential": `{"access_key_id":"BEDROCKKEY1234567890","secret_access_key":"bedrock-secret-123456789"}`, "model": "amazon.titan-text-express-v1"}, idem("aws-flight-provider"), 201)
			id := billingProviderID(provider)
			scope := map[string]any{"payer_account_id": "111122223333", "bucket": "billing-report-bucket", "report_prefix": "billing/cur", "region": "us-east-1", "linked_account_ids": []string{"444455556666"}}
			credential := map[string]any{"auth_mode": "static", "access_key_id": "BILLINGKEY1234567890", "secret_access_key": "billing-secret-example"}
			billingPut(h, owner, id, map[string]any{"scope": scope, "credential": credential}, provider["etag"].(string), 201)
			importer := billingImporter(h, t)
			importer.AWSClient = func(config aws.Config) *s3.Client {
				return s3.NewFromConfig(config, func(o *s3.Options) { o.BaseEndpoint = aws.String(fake.server.URL); o.UsePathStyle = true })
			}
			done := make(chan error, 1)
			go func() { done <- importer.Pass(context.Background()) }()
			select {
			case <-fake.started:
			case <-time.After(10 * time.Second):
				t.Fatal("delivery did not begin")
			}
			path := "/api/v1/providers/" + id + "/billing-import"
			current := h.want(owner, "GET", path, nil, nil, 200)
			if action == "scope" {
				scope["linked_account_ids"] = []string{"777788889999"}
				billingPut(h, owner, id, map[string]any{"scope": scope}, current["etag"].(string), 200)
			} else {
				h.want(owner, "DELETE", path, nil, etagHeader(current), 204)
			}
			close(fake.release)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			costs := h.want(owner, "GET", fmt.Sprintf("/api/v1/providers/%s/invoiced-costs?start=%s&end=%s", id, day.Format("2006-01-02"), day.AddDate(0, 0, 1).Format("2006-01-02")), nil, nil, 200)
			if len(costs["items"].([]any)) != 0 {
				t.Fatalf("stale delivery wrote costs: %v", costs)
			}
			if action == "scope" {
				status := h.want(owner, "GET", path, nil, nil, 200)["status"].(map[string]any)
				if status["source_revision"] != nil || status["imported_through"] != nil {
					t.Fatalf("stale delivery wrote status: %v", status)
				}
			}
		})
	}
}

func TestBillingImportAWSFormatsAndLabels(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	fake := newAWSBillingFake(t)
	today := time.Now().UTC().Truncate(24 * time.Hour)
	day := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -1, 3)
	provider := h.want(owner, "POST", "/api/v1/providers", map[string]any{"name": "AWS formats", "configuration": map[string]any{"kind": "bedrock", "auth_mode": "static", "endpoint": "https://bedrock-runtime.us-east-1.amazonaws.com", "cloud_region": "us-east-1"}, "credential": `{"access_key_id":"BEDROCKKEY1234567890","secret_access_key":"bedrock-secret-123456789"}`, "model": "amazon.titan-text-express-v1"}, idem("aws-formats-provider"), 201)
	id := billingProviderID(provider)
	scope := map[string]any{"payer_account_id": "111122223333", "bucket": "billing-report-bucket", "report_prefix": "billing/cur", "region": "us-east-1", "linked_account_ids": []string{"444455556666"}}
	credential := map[string]any{"auth_mode": "static", "access_key_id": "BILLINGKEY1234567890", "secret_access_key": "billing-secret-example"}
	billingPut(h, owner, id, map[string]any{"scope": scope, "credential": credential}, provider["etag"].(string), 201)
	importer := billingImporter(h, t)
	importer.AWSClient = func(config aws.Config) *s3.Client {
		return s3.NewFromConfig(config, func(o *s3.Options) { o.BaseEndpoint = aws.String(fake.server.URL); o.UsePathStyle = true })
	}
	path := "/api/v1/providers/" + id + "/billing-import"
	schedule := func() {
		t.Helper()
		headers := etagHeader(h.want(owner, "GET", path, nil, nil, 200))
		headers["Idempotency-Key"] = uuid.NewString()
		h.want(owner, "POST", path+"/run", nil, headers, 202)
	}
	status := func() map[string]any { return h.want(owner, "GET", path, nil, nil, 200)["status"].(map[string]any) }
	costs := func() []any {
		return h.want(owner, "GET", fmt.Sprintf("/api/v1/providers/%s/invoiced-costs?start=%s&end=%s", id, day.Format("2006-01-02"), day.AddDate(0, 0, 1).Format("2006-01-02")), nil, nil, 200)["items"].([]any)
	}
	pass := func() {
		if err := importer.Pass(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	fake.delivery(t, day, "parquet", day.AddDate(0, 0, 1).Add(time.Hour), "0.1")
	fake.mu.Lock()
	for key, object := range fake.objects {
		if strings.HasSuffix(key, "-Manifest.json") {
			fake.objects[key] = awsObject{body: bytes.ReplaceAll(object.body, []byte(".csv.gz"), []byte(".parquet")), modified: object.modified}
		}
	}
	fake.mu.Unlock()
	pass()
	if status()["last_error_code"] != "response" || status()["imported_through"] != nil || len(costs()) != 0 {
		t.Fatalf("unsupported format: %v %v", status(), costs())
	}
	fake.mu.Lock()
	fake.objects = map[string]awsObject{}
	fake.mu.Unlock()
	fake.delivery(t, day, "bad-currency", day.AddDate(0, 0, 1).Add(2*time.Hour), "0.1")
	fake.rewriteCSV(t, func(csv string) string { return strings.Replace(csv, "sku1,USD,0.1", "sku1,US,0.1", 1) })
	schedule()
	pass()
	if status()["last_error_code"] != "response" || status()["imported_through"] != nil || len(costs()) != 0 {
		t.Fatalf("invalid currency: %v %v", status(), costs())
	}
	fake.mu.Lock()
	fake.objects = map[string]awsObject{}
	fake.mu.Unlock()
	fake.delivery(t, day, "zip-delivery", day.AddDate(0, 0, 1).Add(3*time.Hour), "0.1")
	fake.rewriteCSV(t, func(csv string) string {
		long := strings.Repeat("x", 300)
		csv = strings.Replace(csv, "sku1,USD,0.1", long+"one,USD,0.1", 1)
		return strings.Replace(csv, "sku1,USD,0.20000000000000000001", long+"two,USD,0.20000000000000000001", 1)
	})
	fake.mu.Lock()
	for key, object := range fake.objects {
		if strings.HasSuffix(key, ".csv.gz") {
			reader, err := gzip.NewReader(bytes.NewReader(object.body))
			if err != nil {
				t.Fatal(err)
			}
			var compressed bytes.Buffer
			writer := zip.NewWriter(&compressed)
			part, err := writer.Create("report.csv")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.Copy(part, reader); err != nil {
				t.Fatal(err)
			}
			if err := reader.Close(); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			delete(fake.objects, key)
			fake.objects[strings.TrimSuffix(key, ".gz")+".zip"] = awsObject{body: compressed.Bytes(), modified: object.modified}
		}
		if strings.HasSuffix(key, "-Manifest.json") {
			fake.objects[key] = awsObject{body: bytes.ReplaceAll(object.body, []byte(".csv.gz"), []byte(".csv.zip")), modified: object.modified}
		}
	}
	fake.mu.Unlock()
	schedule()
	pass()
	items := costs()
	if status()["last_error_code"] != nil || len(items) != 2 {
		t.Fatalf("ZIP import: %v %v", status(), items)
	}
	first := items[0].(map[string]any)["line_item"].(string)
	second := items[1].(map[string]any)["line_item"].(string)
	if first == second || len([]rune(first)) > 256 || len([]rune(second)) > 256 {
		t.Fatalf("long labels collided or exceeded the limit: %q %q", first, second)
	}
	previous := fmt.Sprint(items)
	checkFailure := func(expected string, backoff time.Duration) {
		t.Helper()
		current := status()
		last, lastErr := time.Parse(time.RFC3339Nano, current["last_import_at"].(string))
		next, nextErr := time.Parse(time.RFC3339Nano, current["next_import_at"].(string))
		if current["last_error_code"] != expected || fmt.Sprint(costs()) != previous || lastErr != nil || nextErr != nil || next.Sub(last) != backoff {
			t.Fatalf("failed delivery changed costs or backoff: status=%v costs=%v", current, costs())
		}
	}
	fake.mu.Lock()
	fake.objects = map[string]awsObject{}
	fake.mu.Unlock()
	schedule()
	pass()
	checkFailure("status", 5*time.Minute)
	fake.delivery(t, day, "truncated", day.AddDate(0, 0, 1).Add(4*time.Hour), "0.1")
	fake.mu.Lock()
	for key, object := range fake.objects {
		if !strings.HasSuffix(key, "-Manifest.json") {
			continue
		}
		var manifest map[string]any
		if err := json.Unmarshal(object.body, &manifest); err != nil {
			t.Fatal(err)
		}
		parts := manifest["reportKeys"].([]any)
		original := fake.objects[parts[0].(string)]
		truncated := strings.TrimSuffix(key, "cur-Manifest.json") + "cur-2.csv.gz"
		fake.objects[truncated] = awsObject{body: original.body[:len(original.body)/2], modified: original.modified}
		manifest["reportKeys"] = append(parts, truncated)
		body, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		fake.objects[key] = awsObject{body: body, modified: object.modified}
	}
	fake.mu.Unlock()
	schedule()
	pass()
	checkFailure("response", 10*time.Minute)
	fake.mu.Lock()
	fake.objects = map[string]awsObject{}
	fake.mu.Unlock()
	fake.delivery(t, day, "too-many-parts", day.AddDate(0, 0, 1).Add(5*time.Hour), "0.1")
	fake.mu.Lock()
	for key, object := range fake.objects {
		if strings.HasSuffix(key, "-Manifest.json") {
			var manifest map[string]any
			if err := json.Unmarshal(object.body, &manifest); err != nil {
				t.Fatal(err)
			}
			original := fake.objects[manifest["reportKeys"].([]any)[0].(string)]
			parts := make([]string, 1001)
			for index := range parts {
				parts[index] = fmt.Sprintf("%scur-%d.csv.gz", strings.TrimSuffix(key, "cur-Manifest.json"), index)
				fake.objects[parts[index]] = original
			}
			manifest["reportKeys"] = parts
			body, err := json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			fake.objects[key] = awsObject{body: body, modified: object.modified}
		}
	}
	fake.mu.Unlock()
	schedule()
	pass()
	checkFailure("response", 20*time.Minute)
	current := h.want(owner, "GET", "/api/v1/providers/"+id+"/billing-import", nil, nil, 200)
	badCredential := map[string]any{"auth_mode": "static", "access_key_id": "REVOKEDKEY1234567890", "secret_access_key": "revoked-secret-example"}
	billingPut(h, owner, id, map[string]any{"scope": scope, "credential": badCredential}, current["etag"].(string), 200)
	schedule()
	pass()
	checkFailure("credential", 40*time.Minute)
	var metrics strings.Builder
	billing.WriteMetrics(t.Context(), h.Pool, importer, &metrics)
	for _, sample := range []string{
		fmt.Sprintf("olp_billing_imports_total{provider=%q,outcome=\"response\"} 4", id),
		fmt.Sprintf("olp_billing_imports_total{provider=%q,outcome=\"status\"} 1", id),
		fmt.Sprintf("olp_billing_imports_total{provider=%q,outcome=\"credential\"} 1", id),
	} {
		if !strings.Contains(metrics.String(), sample) {
			t.Fatalf("AWS failure metrics missing %q: %s", sample, metrics.String())
		}
	}
}

func TestBillingImportAWSSlowDeliveryDoesNotLeaseOtherImports(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	fake := newAWSBillingFake(t)
	today := time.Now().UTC().Truncate(24 * time.Hour)
	day := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -1, 3)
	fake.delivery(t, day, "slow", day.AddDate(0, 0, 1).Add(time.Hour), "0.1")
	fake.pauseKey = ".csv.gz"
	fake.started, fake.release = make(chan struct{}), make(chan struct{})
	scope := map[string]any{"payer_account_id": "111122223333", "bucket": "billing-report-bucket", "report_prefix": "billing/cur", "region": "us-east-1", "linked_account_ids": []string{"444455556666"}}
	credential := map[string]any{"auth_mode": "static", "access_key_id": "BILLINGKEY1234567890", "secret_access_key": "billing-secret-example"}
	var ids []string
	for _, name := range []string{"Slow", "Other"} {
		provider := h.want(owner, "POST", "/api/v1/providers", map[string]any{"name": "AWS " + name, "configuration": map[string]any{"kind": "bedrock", "auth_mode": "static", "endpoint": "https://bedrock-runtime.us-east-1.amazonaws.com", "cloud_region": "us-east-1"}, "credential": `{"access_key_id":"BEDROCKKEY1234567890","secret_access_key":"bedrock-secret-123456789"}`, "model": "amazon.titan-text-express-v1"}, idem("aws-batch-"+name), 201)
		id := billingProviderID(provider)
		ids = append(ids, id)
		billingPut(h, owner, id, map[string]any{"scope": scope, "credential": credential}, provider["etag"].(string), 201)
	}
	importer := billingImporter(h, t)
	importer.AWSClient = func(config aws.Config) *s3.Client {
		return s3.NewFromConfig(config, func(o *s3.Options) { o.BaseEndpoint = aws.String(fake.server.URL); o.UsePathStyle = true })
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- importer.Pass(ctx) }()
	select {
	case <-fake.started:
	case <-time.After(10 * time.Second):
		t.Fatal("slow delivery did not begin")
	}
	cancel()
	<-done
	close(fake.release)
	if err := importer.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	status := h.want(owner, "GET", "/api/v1/providers/"+ids[1]+"/billing-import", nil, nil, 200)["status"].(map[string]any)
	if status["source_revision"] != "slow" || status["last_error_code"] != nil {
		t.Fatalf("other import was leased without being attempted: %v", status)
	}
}

func TestBillingImportAWSUsesNewestCompleteDelivery(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	fake := newAWSBillingFake(t)
	today := time.Now().UTC().Truncate(24 * time.Hour)
	day := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -1, 3)
	fake.delivery(t, day, "complete", day.AddDate(0, 0, 1).Add(time.Hour), "0.1")
	fake.delivery(t, day, "uploading", day.AddDate(0, 0, 1).Add(2*time.Hour), "0.5")
	var partKey string
	var missing awsObject
	fake.mu.Lock()
	for key, object := range fake.objects {
		if strings.Contains(key, "/uploading/") && strings.HasSuffix(key, ".csv.gz") {
			partKey, missing = key, object
			delete(fake.objects, key)
		}
	}
	fake.mu.Unlock()
	provider := h.want(owner, "POST", "/api/v1/providers", map[string]any{"name": "AWS complete", "configuration": map[string]any{"kind": "bedrock", "auth_mode": "static", "endpoint": "https://bedrock-runtime.us-east-1.amazonaws.com", "cloud_region": "us-east-1"}, "credential": `{"access_key_id":"BEDROCKKEY1234567890","secret_access_key":"bedrock-secret-123456789"}`, "model": "amazon.titan-text-express-v1"}, idem("aws-complete-provider"), 201)
	id := billingProviderID(provider)
	scope := map[string]any{"payer_account_id": "111122223333", "bucket": "billing-report-bucket", "report_prefix": "billing/cur", "region": "us-east-1", "linked_account_ids": []string{"444455556666"}}
	credential := map[string]any{"auth_mode": "static", "access_key_id": "BILLINGKEY1234567890", "secret_access_key": "billing-secret-example"}
	billingPut(h, owner, id, map[string]any{"scope": scope, "credential": credential}, provider["etag"].(string), 201)
	importer := billingImporter(h, t)
	importer.AWSClient = func(config aws.Config) *s3.Client {
		return s3.NewFromConfig(config, func(o *s3.Options) { o.BaseEndpoint = aws.String(fake.server.URL); o.UsePathStyle = true })
	}
	if err := importer.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/providers/" + id + "/billing-import"
	status := h.want(owner, "GET", path, nil, nil, 200)["status"].(map[string]any)
	if status["source_revision"] != "complete" || status["last_error_code"] != nil {
		t.Fatalf("incomplete newer delivery hid the complete report: %v", status)
	}
	fake.mu.Lock()
	fake.objects[partKey] = missing
	fake.mu.Unlock()
	current := h.want(owner, "GET", path, nil, nil, 200)
	headers := etagHeader(current)
	headers["Idempotency-Key"] = uuid.NewString()
	h.want(owner, "POST", path+"/run", nil, headers, 202)
	if err := importer.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	status = h.want(owner, "GET", path, nil, nil, 200)["status"].(map[string]any)
	if status["source_revision"] != "uploading" || status["last_error_code"] != nil {
		t.Fatalf("completed newer delivery was not applied: %v", status)
	}
}

func TestBillingImportAWSEgressAndRedirects(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	reached := make(chan struct{}, 1)
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(sink.Close)
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, sink.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirector.Close)
	provider := h.want(owner, "POST", "/api/v1/providers", map[string]any{"name": "AWS egress", "configuration": map[string]any{"kind": "bedrock", "auth_mode": "static", "endpoint": "https://bedrock-runtime.us-east-1.amazonaws.com", "cloud_region": "us-east-1"}, "credential": `{"access_key_id":"BEDROCKKEY1234567890","secret_access_key":"bedrock-secret-123456789"}`, "model": "amazon.titan-text-express-v1"}, idem("aws-egress-provider"), 201)
	id := billingProviderID(provider)
	scope := map[string]any{"payer_account_id": "111122223333", "bucket": "billing-report-bucket", "report_prefix": "billing/cur", "region": "us-east-1"}
	credential := map[string]any{"auth_mode": "static", "access_key_id": "BILLINGKEY1234567890", "secret_access_key": "billing-secret-example"}
	billingPut(h, owner, id, map[string]any{"scope": scope, "credential": credential}, provider["etag"].(string), 201)
	importer := billingImporter(h, t)
	endpoint := "http://169.254.169.254"
	importer.AWSClient = func(config aws.Config) *s3.Client {
		return s3.NewFromConfig(config, func(o *s3.Options) { o.BaseEndpoint = aws.String(endpoint); o.UsePathStyle = true })
	}
	path := "/api/v1/providers/" + id + "/billing-import"
	if err := importer.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	status := h.want(owner, "GET", path, nil, nil, 200)["status"].(map[string]any)
	if status["last_error_code"] != "network" || status["imported_through"] != nil {
		t.Fatalf("blocked destination was imported: %v", status)
	}
	endpoint = redirector.URL
	headers := etagHeader(h.want(owner, "GET", path, nil, nil, 200))
	headers["Idempotency-Key"] = uuid.NewString()
	h.want(owner, "POST", path+"/run", nil, headers, 202)
	if err := importer.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	status = h.want(owner, "GET", path, nil, nil, 200)["status"].(map[string]any)
	if status["last_error_code"] != "network" || status["imported_through"] != nil {
		t.Fatalf("redirect was imported: %v", status)
	}
	select {
	case <-reached:
		t.Fatal("the redirected endpoint received the signed request")
	default:
	}
}

func TestBillingImportAWSWorkloadCredential(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "BILLINGKEY1234567890")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "billing-workload-secret")
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_PROFILE", "")
	h := newAccessHarness(t)
	owner := h.owner()
	fake := newAWSBillingFake(t)
	today := time.Now().UTC().Truncate(24 * time.Hour)
	day := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -1, 3)
	fake.delivery(t, day, "workload", day.AddDate(0, 0, 1).Add(time.Hour), "0.1")
	provider := h.want(owner, "POST", "/api/v1/providers", map[string]any{"name": "AWS workload", "configuration": map[string]any{"kind": "bedrock", "auth_mode": "static", "endpoint": "https://bedrock-runtime.us-east-1.amazonaws.com", "cloud_region": "us-east-1"}, "credential": `{"access_key_id":"BEDROCKKEY1234567890","secret_access_key":"bedrock-secret-123456789"}`, "model": "amazon.titan-text-express-v1"}, idem("aws-workload-provider"), 201)
	id := billingProviderID(provider)
	scope := map[string]any{"payer_account_id": "111122223333", "bucket": "billing-report-bucket", "report_prefix": "billing/cur", "region": "us-east-1"}
	billingPut(h, owner, id, map[string]any{"scope": scope, "credential": map[string]any{"auth_mode": "workload"}}, provider["etag"].(string), 201)
	importer := billingImporter(h, t)
	importer.AWSClient = func(config aws.Config) *s3.Client {
		return s3.NewFromConfig(config, func(o *s3.Options) { o.BaseEndpoint = aws.String(fake.server.URL); o.UsePathStyle = true })
	}
	if err := importer.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	status := h.want(owner, "GET", "/api/v1/providers/"+id+"/billing-import", nil, nil, 200)["status"].(map[string]any)
	if status["last_error_code"] != nil || status["source_revision"] != "workload" {
		t.Fatalf("workload credential did not import: %v", status)
	}
}

func TestBillingImportAWSDeliveryIdentities(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	fake := newAWSBillingFake(t)
	today := time.Now().UTC().Truncate(24 * time.Hour)
	day := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -1, 3)
	fake.delivery(t, day, "partitions", day.AddDate(0, 0, 1), "0.1")
	fake.rewriteCSV(t, func(csv string) string {
		rows := strings.Split(strings.TrimSuffix(csv, "\n"), "\n")
		out := rows[0] + ",identity/LineItemId,identity/TimeInterval,bill/BillType\n"
		interval := func(hour int) string {
			return fmt.Sprintf("%sT%02d:00:00Z/%sT%02d:00:00Z", day.Format("2006-01-02"), hour, day.Format("2006-01-02"), hour+1)
		}
		for index, row := range rows[1:] {
			out += fmt.Sprintf("%s,id-%d,%s,Anniversary\n", row, index, interval(index+1))
		}
		out += fmt.Sprintf("%s,id-0,%s,Anniversary\n", strings.Replace(rows[1], "T01:", "T05:", 1), interval(5))
		out += fmt.Sprintf("%s,id-0,%s,Purchase\n", rows[1], interval(1))
		return out
	})
	fake.mu.Lock()
	for key, object := range fake.objects {
		if strings.HasSuffix(key, ".csv.gz") {
			fake.objects[strings.Replace(key, "cur-1.csv.gz", "cur-2.csv.gz", 1)] = object
		}
		if strings.HasSuffix(key, "-Manifest.json") {
			var manifest map[string]any
			if err := json.Unmarshal(object.body, &manifest); err != nil {
				t.Fatal(err)
			}
			parts := manifest["reportKeys"].([]any)
			manifest["reportKeys"] = append(parts, strings.Replace(parts[0].(string), "cur-1.csv.gz", "cur-2.csv.gz", 1))
			body, err := json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			fake.objects[key] = awsObject{body: body, modified: object.modified}
		}
	}
	fake.mu.Unlock()
	provider := h.want(owner, "POST", "/api/v1/providers", map[string]any{"name": "AWS partitions", "configuration": map[string]any{"kind": "bedrock", "auth_mode": "static", "endpoint": "https://bedrock-runtime.us-east-1.amazonaws.com", "cloud_region": "us-east-1"}, "credential": `{"access_key_id":"BEDROCKKEY1234567890","secret_access_key":"bedrock-secret-123456789"}`, "model": "amazon.titan-text-express-v1"}, idem("aws-partitions-provider"), 201)
	id := billingProviderID(provider)
	scope := map[string]any{"payer_account_id": "111122223333", "bucket": "billing-report-bucket", "report_prefix": "billing/cur", "region": "us-east-1", "linked_account_ids": []string{"444455556666"}}
	credential := map[string]any{"auth_mode": "static", "access_key_id": "BILLINGKEY1234567890", "secret_access_key": "billing-secret-example"}
	billingPut(h, owner, id, map[string]any{"scope": scope, "credential": credential}, provider["etag"].(string), 201)
	importer := billingImporter(h, t)
	importer.AWSClient = func(config aws.Config) *s3.Client {
		return s3.NewFromConfig(config, func(o *s3.Options) { o.BaseEndpoint = aws.String(fake.server.URL); o.UsePathStyle = true })
	}
	if err := importer.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	costs := h.want(owner, "GET", fmt.Sprintf("/api/v1/providers/%s/invoiced-costs?start=%s&end=%s", id, day.Format("2006-01-02"), day.AddDate(0, 0, 1).Format("2006-01-02")), nil, nil, 200)["items"].([]any)
	if len(costs) != 1 || costs[0].(map[string]any)["amount"] != "0.50000000000000000001" {
		t.Fatalf("repeated records were counted or distinct intervals and bill types were lost: %v", costs)
	}
}

func TestBillingImportAWSDoesNotCoverDaysAfterDelivery(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	fake := newAWSBillingFake(t)
	today := time.Now().UTC().Truncate(24 * time.Hour)
	day := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -1, 3)
	fake.delivery(t, day, "stale", day.AddDate(0, 0, 1).Add(3*time.Hour), "0.1")
	provider := h.want(owner, "POST", "/api/v1/providers", map[string]any{"name": "AWS stale delivery", "configuration": map[string]any{"kind": "bedrock", "auth_mode": "static", "endpoint": "https://bedrock-runtime.us-east-1.amazonaws.com", "cloud_region": "us-east-1"}, "credential": `{"access_key_id":"BEDROCKKEY1234567890","secret_access_key":"bedrock-secret-123456789"}`, "model": "amazon.titan-text-express-v1"}, idem("aws-stale-provider"), 201)
	id := billingProviderID(provider)
	scope := map[string]any{"payer_account_id": "111122223333", "bucket": "billing-report-bucket", "report_prefix": "billing/cur", "region": "us-east-1"}
	credential := map[string]any{"auth_mode": "static", "access_key_id": "BILLINGKEY1234567890", "secret_access_key": "billing-secret-example"}
	billingPut(h, owner, id, map[string]any{"scope": scope, "credential": credential}, provider["etag"].(string), 201)
	priced := "0.15"
	uncovered := day.AddDate(0, 0, 1)
	billingFact(t, h, id, uuid.NewString(), uncovered, "billable", true, &priced, false, false, false)
	importer := billingImporter(h, t)
	importer.AWSClient = func(config aws.Config) *s3.Client {
		return s3.NewFromConfig(config, func(o *s3.Options) { o.BaseEndpoint = aws.String(fake.server.URL); o.UsePathStyle = true })
	}
	if err := importer.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	status := h.want(owner, "GET", "/api/v1/providers/"+id+"/billing-import", nil, nil, 200)["status"].(map[string]any)
	if status["imported_through"] != day.Format("2006-01-02") {
		t.Fatalf("stale report covered a later day: %v", status)
	}
	report := h.want(owner, "GET", fmt.Sprintf("/api/v1/usage/reconciliation?start=%s&end=%s&provider_id=%s", uncovered.Format("2006-01-02"), uncovered.AddDate(0, 0, 1).Format("2006-01-02"), id), nil, nil, 200)
	item := report["items"].([]any)[0].(map[string]any)
	if item["capture_rate_unavailable_reason"] != "not_imported" || item["invoiced_cost"] != nil {
		t.Fatalf("stale report treated an unimported day as zero: %v", item)
	}
}

func TestBillingImportAWSMissingPeriodsRemainUncoveredAndRetry(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	setting := h.want(owner, "GET", "/api/v1/settings/retention.usage_days", nil, nil, 200)
	h.want(owner, "PUT", "/api/v1/settings/retention.usage_days", map[string]any{"value": "180"}, etagHeader(setting), 200)
	fake := newAWSBillingFake(t)
	today := time.Now().UTC().Truncate(24 * time.Hour)
	month := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC)
	oldDay := month.AddDate(0, -4, 14)
	missingDay := month.AddDate(0, -2, 14)
	newDay := month.AddDate(0, -1, 14)
	fake.delivery(t, oldDay, "old-period", time.Date(oldDay.Year(), oldDay.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 1), "0.1")
	fake.delivery(t, newDay, "recent-period", newDay.AddDate(0, 0, 1), "0.1")
	provider := h.want(owner, "POST", "/api/v1/providers", map[string]any{"name": "AWS missing periods", "configuration": map[string]any{"kind": "bedrock", "auth_mode": "static", "endpoint": "https://bedrock-runtime.us-east-1.amazonaws.com", "cloud_region": "us-east-1"}, "credential": `{"access_key_id":"BEDROCKKEY1234567890","secret_access_key":"bedrock-secret-123456789"}`, "model": "amazon.titan-text-express-v1"}, idem("aws-missing-period-provider"), 201)
	id := billingProviderID(provider)
	scope := map[string]any{"payer_account_id": "111122223333", "bucket": "billing-report-bucket", "report_prefix": "billing/cur", "region": "us-east-1"}
	credential := map[string]any{"auth_mode": "static", "access_key_id": "BILLINGKEY1234567890", "secret_access_key": "billing-secret-example"}
	billingPut(h, owner, id, map[string]any{"scope": scope, "credential": credential}, provider["etag"].(string), 201)
	priced := "0.15"
	billingFact(t, h, id, uuid.NewString(), missingDay, "billable", true, &priced, false, false, false)
	importer := billingImporter(h, t)
	importer.AWSClient = func(config aws.Config) *s3.Client {
		return s3.NewFromConfig(config, func(o *s3.Options) { o.BaseEndpoint = aws.String(fake.server.URL); o.UsePathStyle = true })
	}
	if err := importer.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	status := h.want(owner, "GET", "/api/v1/providers/"+id+"/billing-import", nil, nil, 200)["status"].(map[string]any)
	if status["imported_from"].(string) <= missingDay.Format("2006-01-02") && status["imported_through"].(string) >= missingDay.Format("2006-01-02") {
		t.Fatalf("disjoint periods produced a coverage range across the gap: %v", status)
	}
	report := func() map[string]any {
		t.Helper()
		return h.want(owner, "GET", fmt.Sprintf("/api/v1/usage/reconciliation?start=%s&end=%s&provider_id=%s", missingDay.Format("2006-01-02"), missingDay.AddDate(0, 0, 1).Format("2006-01-02"), id), nil, nil, 200)["items"].([]any)[0].(map[string]any)
	}
	if item := report(); item["capture_rate_unavailable_reason"] != "not_imported" || item["invoiced_cost"] != nil {
		t.Fatalf("missing period was treated as invoiced zero: %v", item)
	}
	for _, coveredDay := range []time.Time{oldDay, newDay} {
		costs := h.want(owner, "GET", fmt.Sprintf("/api/v1/providers/%s/invoiced-costs?start=%s&end=%s", id, coveredDay.Format("2006-01-02"), coveredDay.AddDate(0, 0, 1).Format("2006-01-02")), nil, nil, 200)
		if len(costs["items"].([]any)) == 0 {
			t.Fatalf("expected imported costs on both sides of the gap: %v", costs)
		}
	}
	fake.delivery(t, missingDay, "recovered-period", missingDay.AddDate(0, 0, 1), "0.4")
	var missingKey string
	var missingPart awsObject
	fake.mu.Lock()
	for key, object := range fake.objects {
		if strings.Contains(key, "/recovered-period/") && strings.HasSuffix(key, ".csv.gz") {
			missingKey, missingPart = key, object
			delete(fake.objects, key)
		}
	}
	fake.mu.Unlock()
	if missingKey == "" {
		t.Fatal("expected a historical report part")
	}
	fake.mu.Lock()
	for key, object := range fake.objects {
		if strings.Contains(key, "/old-period/") && strings.HasSuffix(key, "-Manifest.json") {
			fake.objects[key] = awsObject{body: []byte("malformed"), modified: object.modified}
		}
	}
	fake.mu.Unlock()
	path := "/api/v1/providers/" + id + "/billing-import"
	headers := etagHeader(h.want(owner, "GET", path, nil, nil, 200))
	headers["Idempotency-Key"] = uuid.NewString()
	h.want(owner, "POST", path+"/run", nil, headers, 202)
	if err := importer.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	status = h.want(owner, "GET", path, nil, nil, 200)["status"].(map[string]any)
	if status["last_error_code"] != "status" || report()["capture_rate_unavailable_reason"] != "not_imported" {
		t.Fatalf("incomplete historical period did not remain retryable: %v", status)
	}
	fake.mu.Lock()
	fake.objects[missingKey] = missingPart
	fake.mu.Unlock()
	headers = etagHeader(h.want(owner, "GET", path, nil, nil, 200))
	headers["Idempotency-Key"] = uuid.NewString()
	h.want(owner, "POST", path+"/run", nil, headers, 202)
	if err := importer.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if item := report(); item["capture_rate_unavailable_reason"] != nil || item["invoiced_cost"] == nil {
		t.Fatalf("missing historical period was not retried: %v", item)
	}
	status = h.want(owner, "GET", path, nil, nil, 200)["status"].(map[string]any)
	if status["last_error_code"] != nil {
		t.Fatalf("completed old period was unnecessarily fetched: %v", status)
	}
}

func TestBillingImportAWSListsOnlyRetainedPeriods(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	fake := newAWSBillingFake(t)
	today := time.Now().UTC().Truncate(24 * time.Hour)
	day := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -1, 3)
	fake.delivery(t, day, "current", day.AddDate(0, 0, 1), "0.1")
	old := day.AddDate(-1, 0, 0)
	oldStart := time.Date(old.Year(), old.Month(), 1, 0, 0, 0, 0, time.UTC)
	oldPrefix := fmt.Sprintf("billing/cur/%s-%s/old/", oldStart.Format("20060102"), oldStart.AddDate(0, 1, 0).Format("20060102"))
	fake.mu.Lock()
	for index := range 4001 {
		fake.objects[fmt.Sprintf("%spart-%d.csv.gz", oldPrefix, index)] = awsObject{body: []byte("old"), modified: old}
	}
	fake.mu.Unlock()
	provider := h.want(owner, "POST", "/api/v1/providers", map[string]any{"name": "AWS retained periods", "configuration": map[string]any{"kind": "bedrock", "auth_mode": "static", "endpoint": "https://bedrock-runtime.us-east-1.amazonaws.com", "cloud_region": "us-east-1"}, "credential": `{"access_key_id":"BEDROCKKEY1234567890","secret_access_key":"bedrock-secret-123456789"}`, "model": "amazon.titan-text-express-v1"}, idem("aws-retained-provider"), 201)
	id := billingProviderID(provider)
	scope := map[string]any{"payer_account_id": "111122223333", "bucket": "billing-report-bucket", "report_prefix": "billing/cur", "region": "us-east-1"}
	credential := map[string]any{"auth_mode": "static", "access_key_id": "BILLINGKEY1234567890", "secret_access_key": "billing-secret-example"}
	billingPut(h, owner, id, map[string]any{"scope": scope, "credential": credential}, provider["etag"].(string), 201)
	importer := billingImporter(h, t)
	importer.AWSClient = func(config aws.Config) *s3.Client {
		return s3.NewFromConfig(config, func(o *s3.Options) { o.BaseEndpoint = aws.String(fake.server.URL); o.UsePathStyle = true })
	}
	if err := importer.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	status := h.want(owner, "GET", "/api/v1/providers/"+id+"/billing-import", nil, nil, 200)["status"].(map[string]any)
	if status["last_error_code"] != nil || status["source_revision"] != "current" {
		t.Fatalf("historical objects blocked a retained report: %v", status)
	}
}

func TestBillingImportAWSBoundedPeriodsMakeProgress(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	fake := newAWSBillingFake(t)
	now := time.Now().UTC()
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	previous, current := month.AddDate(0, -1, 9), month.AddDate(0, 0, 9)
	fake.delivery(t, previous, "previous", month.AddDate(0, 0, 1), "0.2")
	fake.delivery(t, current, "current", month.AddDate(0, 0, 14), "0.3")
	fake.mu.Lock()
	for key, object := range fake.objects {
		if !strings.HasSuffix(key, ".csv.gz") {
			continue
		}
		var body bytes.Buffer
		body.Write(object.body)
		header := []byte{0x1f, 0x8b, 8, 4, 0, 0, 0, 0, 0, 0xff, 0xff, 0xff}
		padding := make([]byte, 65535)
		footer := []byte{3, 0, 0, 0, 0, 0, 0, 0, 0, 0}
		for range 520 {
			body.Write(header)
			body.Write(padding)
			body.Write(footer)
		}
		fake.objects[key] = awsObject{body: body.Bytes(), modified: object.modified}
	}
	fake.mu.Unlock()
	provider := h.want(owner, "POST", "/api/v1/providers", map[string]any{"name": "AWS bounded periods", "configuration": map[string]any{"kind": "bedrock", "auth_mode": "static", "endpoint": "https://bedrock-runtime.us-east-1.amazonaws.com", "cloud_region": "us-east-1"}, "credential": `{"access_key_id":"BEDROCKKEY1234567890","secret_access_key":"bedrock-secret-123456789"}`, "model": "amazon.titan-text-express-v1"}, idem("aws-bounded-periods"), 201)
	id := billingProviderID(provider)
	scope := map[string]any{"payer_account_id": "111122223333", "bucket": "billing-report-bucket", "report_prefix": "billing/cur", "region": "us-east-1", "linked_account_ids": []string{"444455556666"}}
	credential := map[string]any{"auth_mode": "static", "access_key_id": "BILLINGKEY1234567890", "secret_access_key": "billing-secret-example"}
	billingPut(h, owner, id, map[string]any{"scope": scope, "credential": credential}, provider["etag"].(string), 201)
	importer := billingImporter(h, t)
	importer.Now = func() time.Time { return month.AddDate(0, 0, 20) }
	importer.AWSClient = func(config aws.Config) *s3.Client {
		return s3.NewFromConfig(config, func(o *s3.Options) { o.BaseEndpoint = aws.String(fake.server.URL); o.UsePathStyle = true })
	}
	if err := importer.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/providers/" + id + "/billing-import"
	query := fmt.Sprintf("/api/v1/providers/%s/invoiced-costs?start=%s&end=%s", id, previous.Format("2006-01-02"), current.AddDate(0, 0, 1).Format("2006-01-02"))
	items := h.want(owner, "GET", query, nil, nil, 200)["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["amount"] != "0.40000000000000000001" {
		t.Fatalf("the first bounded pass did not preserve its completed period: %v", items)
	}
	headers := etagHeader(h.want(owner, "GET", path, nil, nil, 200))
	headers["Idempotency-Key"] = uuid.NewString()
	h.want(owner, "POST", path+"/run", nil, headers, 202)
	if err := importer.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	items = h.want(owner, "GET", query, nil, nil, 200)["items"].([]any)
	if len(items) != 2 || items[0].(map[string]any)["amount"] != "0.40000000000000000001" || items[1].(map[string]any)["amount"] != "0.50000000000000000001" {
		t.Fatalf("bounded passes did not eventually refresh both periods: %v", items)
	}
}

func TestBillingImportAWSIncompletePeriodDoesNotBlockOtherPeriods(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	fake := newAWSBillingFake(t)
	now := time.Now().UTC()
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	previous, current := month.AddDate(0, -1, 9), month.AddDate(0, 0, 9)
	fake.delivery(t, previous, "old", month.AddDate(0, 0, 1), "0.1")
	fake.delivery(t, current, "initial", month.AddDate(0, 0, 14), "0.2")
	provider := h.want(owner, "POST", "/api/v1/providers", map[string]any{"name": "AWS incomplete period", "configuration": map[string]any{"kind": "bedrock", "auth_mode": "static", "endpoint": "https://bedrock-runtime.us-east-1.amazonaws.com", "cloud_region": "us-east-1"}, "credential": `{"access_key_id":"BEDROCKKEY1234567890","secret_access_key":"bedrock-secret-123456789"}`, "model": "amazon.titan-text-express-v1"}, idem("aws-incomplete-period"), 201)
	id := billingProviderID(provider)
	scope := map[string]any{"payer_account_id": "111122223333", "bucket": "billing-report-bucket", "report_prefix": "billing/cur", "region": "us-east-1", "linked_account_ids": []string{"444455556666"}}
	credential := map[string]any{"auth_mode": "static", "access_key_id": "BILLINGKEY1234567890", "secret_access_key": "billing-secret-example"}
	billingPut(h, owner, id, map[string]any{"scope": scope, "credential": credential}, provider["etag"].(string), 201)
	importer := billingImporter(h, t)
	importer.Now = func() time.Time { return month.AddDate(0, 0, 20) }
	importer.AWSClient = func(config aws.Config) *s3.Client {
		return s3.NewFromConfig(config, func(o *s3.Options) { o.BaseEndpoint = aws.String(fake.server.URL); o.UsePathStyle = true })
	}
	if err := importer.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	fake.delivery(t, previous, "incomplete", month.AddDate(0, 0, 2), "0.5")
	fake.delivery(t, current, "revised", month.AddDate(0, 0, 15), "0.7")
	fake.mu.Lock()
	for key := range fake.objects {
		if (strings.Contains(key, "/old/") || strings.Contains(key, "/incomplete/")) && strings.HasSuffix(key, ".csv.gz") {
			delete(fake.objects, key)
		}
	}
	fake.mu.Unlock()
	path := "/api/v1/providers/" + id + "/billing-import"
	headers := etagHeader(h.want(owner, "GET", path, nil, nil, 200))
	headers["Idempotency-Key"] = uuid.NewString()
	h.want(owner, "POST", path+"/run", nil, headers, 202)
	if err := importer.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	items := h.want(owner, "GET", fmt.Sprintf("/api/v1/providers/%s/invoiced-costs?start=%s&end=%s", id, previous.Format("2006-01-02"), current.AddDate(0, 0, 1).Format("2006-01-02")), nil, nil, 200)["items"].([]any)
	if len(items) != 2 || items[0].(map[string]any)["amount"] != "0.30000000000000000001" || items[1].(map[string]any)["amount"] != "0.90000000000000000001" {
		t.Fatalf("an incomplete period blocked a healthy period or changed existing costs: %v", items)
	}
	status := h.want(owner, "GET", path, nil, nil, 200)["status"].(map[string]any)
	if status["last_error_code"] != "status" {
		t.Fatalf("the failed period did not report a content-free error: %v", status)
	}
}

func TestBillingImportAWSCumulativeByteBudgets(t *testing.T) {
	for _, limit := range []string{"compressed", "expanded"} {
		t.Run(limit, func(t *testing.T) {
			h := newAccessHarness(t)
			owner := h.owner()
			fake := newAWSBillingFake(t)
			today := time.Now().UTC().Truncate(24 * time.Hour)
			day := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -1, 3)
			fake.delivery(t, day, "baseline", day.AddDate(0, 0, 1), "0.1")
			provider := h.want(owner, "POST", "/api/v1/providers", map[string]any{"name": "AWS byte budget", "configuration": map[string]any{"kind": "bedrock", "auth_mode": "static", "endpoint": "https://bedrock-runtime.us-east-1.amazonaws.com", "cloud_region": "us-east-1"}, "credential": `{"access_key_id":"BEDROCKKEY1234567890","secret_access_key":"bedrock-secret-123456789"}`, "model": "amazon.titan-text-express-v1"}, idem("aws-budget-provider"), 201)
			id := billingProviderID(provider)
			scope := map[string]any{"payer_account_id": "111122223333", "bucket": "billing-report-bucket", "report_prefix": "billing/cur", "region": "us-east-1"}
			credential := map[string]any{"auth_mode": "static", "access_key_id": "BILLINGKEY1234567890", "secret_access_key": "billing-secret-example"}
			billingPut(h, owner, id, map[string]any{"scope": scope, "credential": credential}, provider["etag"].(string), 201)
			importer := billingImporter(h, t)
			importer.AWSClient = func(config aws.Config) *s3.Client {
				return s3.NewFromConfig(config, func(o *s3.Options) { o.BaseEndpoint = aws.String(fake.server.URL); o.UsePathStyle = true })
			}
			if err := importer.Pass(t.Context()); err != nil {
				t.Fatal(err)
			}
			fake.delivery(t, day, "over-budget", day.AddDate(0, 0, 1).Add(time.Hour), "0.7")
			fake.mu.Lock()
			for key, object := range fake.objects {
				if !strings.Contains(key, "/over-budget/") || !strings.HasSuffix(key, "-Manifest.json") {
					continue
				}
				var manifest map[string]any
				if err := json.Unmarshal(object.body, &manifest); err != nil {
					t.Fatal(err)
				}
				first := manifest["reportKeys"].([]any)[0].(string)
				second := strings.TrimSuffix(first, ".csv.gz") + "-second.csv.gz"
				var body bytes.Buffer
				if limit == "compressed" {
					body.Write(fake.objects[first].body)
					header := []byte{0x1f, 0x8b, 8, 4, 0, 0, 0, 0, 0, 0xff, 0xff, 0xff}
					padding := make([]byte, 65535)
					footer := []byte{3, 0, 0, 0, 0, 0, 0, 0, 0, 0}
					for range 520 {
						body.Write(header)
						body.Write(padding)
						body.Write(footer)
					}
				} else {
					writer := gzip.NewWriter(&body)
					fmt.Fprintln(writer, "bill/PayerAccountId,lineItem/UsageAccountId,lineItem/ProductCode,lineItem/UsageStartDate,lineItem/LineItemType,lineItem/UsageType,lineItem/Operation,product/sku,lineItem/CurrencyCode,lineItem/UnblendedCost,unused")
					row := "111122223333,444455556666,AmazonEC2," + day.Format("2006-01-02") + "T01:00:00Z,Usage,InputTokens,InvokeModel,sku,USD,0.1," + strings.Repeat("x", 256<<10) + "\n"
					for range 1030 {
						if _, err := io.WriteString(writer, row); err != nil {
							t.Fatal(err)
						}
					}
					if err := writer.Close(); err != nil {
						t.Fatal(err)
					}
				}
				fake.objects[first] = awsObject{body: body.Bytes(), modified: object.modified}
				fake.objects[second] = awsObject{body: body.Bytes(), modified: object.modified}
				manifest["reportKeys"] = []string{first, second}
				encoded, err := json.Marshal(manifest)
				if err != nil {
					t.Fatal(err)
				}
				fake.objects[key] = awsObject{body: encoded, modified: object.modified}
			}
			fake.mu.Unlock()
			path := "/api/v1/providers/" + id + "/billing-import"
			headers := etagHeader(h.want(owner, "GET", path, nil, nil, 200))
			headers["Idempotency-Key"] = uuid.NewString()
			h.want(owner, "POST", path+"/run", nil, headers, 202)
			if err := importer.Pass(t.Context()); err != nil {
				t.Fatal(err)
			}
			status := h.want(owner, "GET", path, nil, nil, 200)["status"].(map[string]any)
			if status["last_error_code"] != "response" || status["source_revision"] != "baseline" {
				t.Fatalf("%s byte cap did not preserve the previous revision: %v", limit, status)
			}
		})
	}
}

func TestBillingImportAWSReconciliationAfterDeletion(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := createProject(h, owner, "Billing history")
	otherProject := createProject(h, owner, "Other billing history")
	h.invite(owner, "billing-member@example.com", "operator")
	h.invite(owner, "billing-outsider@example.com", "operator")
	for _, value := range h.want(owner, "GET", "/api/v1/users", nil, nil, 200)["items"].([]any) {
		user := value.(map[string]any)
		email := user["email"].(string)
		if email != "billing-member@example.com" && email != "billing-outsider@example.com" {
			continue
		}
		id := user["id"].(string)
		h.want(owner, "PATCH", "/api/v1/users/"+id, map[string]any{"access_scope": "assigned"}, etagHeader(user), 200)
		if email == "billing-member@example.com" {
			addMember(h, owner, project, id, "viewer")
		} else {
			addMember(h, owner, otherProject, id, "viewer")
		}
	}
	member := login(h, "billing-member@example.com")
	outsider := login(h, "billing-outsider@example.com")
	fake := newAWSBillingFake(t)
	today := time.Now().UTC().Truncate(24 * time.Hour)
	day := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -1, 3)
	fake.delivery(t, day, "history", day.AddDate(0, 0, 2), "0.1")
	provider := h.want(owner, "POST", "/api/v1/providers", map[string]any{"name": "AWS history", "project_id": project, "configuration": map[string]any{"kind": "bedrock", "auth_mode": "static", "endpoint": "https://bedrock-runtime.us-east-1.amazonaws.com", "cloud_region": "us-east-1"}, "credential": `{"access_key_id":"BEDROCKKEY1234567890","secret_access_key":"bedrock-secret-123456789"}`, "model": "amazon.titan-text-express-v1"}, idem("aws-history-provider"), 201)
	id := billingProviderID(provider)
	scope := map[string]any{"payer_account_id": "111122223333", "bucket": "billing-report-bucket", "report_prefix": "billing/cur", "region": "us-east-1", "linked_account_ids": []string{"444455556666"}}
	credential := map[string]any{"auth_mode": "static", "access_key_id": "BILLINGKEY1234567890", "secret_access_key": "billing-secret-example"}
	billingPut(h, owner, id, map[string]any{"scope": scope, "credential": credential}, provider["etag"].(string), 201)
	price := "0.15"
	for _, current := range []time.Time{day, day.AddDate(0, 0, 2)} {
		billingFact(t, h, id, uuid.NewString(), current, "billable", true, &price, false, false, false)
	}
	importer := billingImporter(h, t)
	importer.AWSClient = func(config aws.Config) *s3.Client {
		return s3.NewFromConfig(config, func(o *s3.Options) { o.BaseEndpoint = aws.String(fake.server.URL); o.UsePathStyle = true })
	}
	if err := importer.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	start, end := day.Format("2006-01-02"), day.AddDate(0, 0, 3).Format("2006-01-02")
	path := fmt.Sprintf("/api/v1/usage/reconciliation?start=%s&end=%s&provider_id=%s", start, end, id)
	before := h.want(member, "GET", path, nil, nil, 200)
	if len(before["items"].([]any)) != 3 {
		t.Fatalf("covered day without usage or invoice is missing: %v", before)
	}
	importPath := "/api/v1/providers/" + id + "/billing-import"
	h.want(owner, "DELETE", importPath, nil, etagHeader(h.want(owner, "GET", importPath, nil, nil, 200)), 204)
	h.want(owner, "GET", importPath, nil, nil, 404)
	after := h.want(member, "GET", path, nil, nil, 200)
	for index, want := range []struct{ amount, reason any }{{"0.30000000000000000001", nil}, {"0", "zero_invoice"}, {nil, "not_imported"}} {
		old := before["items"].([]any)[index].(map[string]any)
		item := after["items"].([]any)[index].(map[string]any)
		if item["invoiced_cost"] != want.amount || item["capture_rate_unavailable_reason"] != want.reason || item["capture_rate"] != old["capture_rate"] {
			t.Fatalf("historical reconciliation lost coverage on day %d: before=%v after=%v", index, old, item)
		}
	}
	h.want(outsider, "GET", path, nil, nil, 404)
	all := h.want(outsider, "GET", fmt.Sprintf("/api/v1/usage/reconciliation?start=%s&end=%s", start, end), nil, nil, 200)
	if len(all["items"].([]any)) != 0 || len(all["totals"].([]any)) != 0 {
		t.Fatalf("reconciliation leaked another project's history: %v", all)
	}
}

func TestBillingImportAWSRestatesPeriods(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	fake := newAWSBillingFake(t)
	today := time.Now().UTC().Truncate(24 * time.Hour)
	day := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -1, 3)
	fake.delivery(t, day, "initial", day.AddDate(0, 0, 2), "0.1")
	fake.rewriteCSV(t, func(value string) string {
		return value + fmt.Sprintf("111122223333,444455556666,AmazonBedrock,%sT03:00:00Z,Usage,Retired,InvokeModel,old-sku,USD,4\n", day.Format("2006-01-02")) + fmt.Sprintf("111122223333,444455556666,AmazonBedrock,%sT03:00:00Z,Usage,Disappeared,InvokeModel,old-sku,USD,3\n", day.AddDate(0, 0, 1).Format("2006-01-02"))
	})
	provider := h.want(owner, "POST", "/api/v1/providers", map[string]any{"name": "AWS restatement", "configuration": map[string]any{"kind": "bedrock", "auth_mode": "static", "endpoint": "https://bedrock-runtime.us-east-1.amazonaws.com", "cloud_region": "us-east-1"}, "credential": `{"access_key_id":"BEDROCKKEY1234567890","secret_access_key":"bedrock-secret-123456789"}`, "model": "amazon.titan-text-express-v1"}, idem("aws-restatement-provider"), 201)
	id := billingProviderID(provider)
	scope := map[string]any{"payer_account_id": "111122223333", "bucket": "billing-report-bucket", "report_prefix": "billing/cur", "region": "us-east-1", "linked_account_ids": []string{"444455556666"}}
	credential := map[string]any{"auth_mode": "static", "access_key_id": "BILLINGKEY1234567890", "secret_access_key": "billing-secret-example"}
	billingPut(h, owner, id, map[string]any{"scope": scope, "credential": credential}, provider["etag"].(string), 201)
	price := "0.3"
	billingFact(t, h, id, uuid.NewString(), day.AddDate(0, 0, 1), "billable", true, &price, false, false, false)
	importer := billingImporter(h, t)
	importer.AWSClient = func(config aws.Config) *s3.Client {
		return s3.NewFromConfig(config, func(o *s3.Options) { o.BaseEndpoint = aws.String(fake.server.URL); o.UsePathStyle = true })
	}
	path := "/api/v1/providers/" + id + "/billing-import"
	run := func() {
		t.Helper()
		headers := etagHeader(h.want(owner, "GET", path, nil, nil, 200))
		headers["Idempotency-Key"] = uuid.NewString()
		h.want(owner, "POST", path+"/run", nil, headers, 202)
		if err := importer.Pass(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	costs := func() map[string]string {
		t.Helper()
		result := map[string]string{}
		for _, value := range h.want(owner, "GET", fmt.Sprintf("/api/v1/providers/%s/invoiced-costs?start=%s&end=%s", id, day.Format("2006-01-02"), day.AddDate(0, 0, 1).Format("2006-01-02")), nil, nil, 200)["items"].([]any) {
			item := value.(map[string]any)
			result[item["line_item"].(string)] = item["amount"].(string)
		}
		return result
	}
	if err := importer.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(costs()) != 2 {
		t.Fatalf("initial invoice: %v", costs())
	}
	fake.delivery(t, day, "corrected", day.AddDate(0, 0, 3), "0.7")
	fake.rewriteCSV(t, func(value string) string {
		if !strings.Contains(value, ",USD,0.7\n") {
			return value
		}
		prefix := "111122223333,444455556666,AmazonBedrock," + day.Format("2006-01-02") + "T03:00:00Z,"
		return value + prefix + "Credit,InputTokens,InvokeModel,sku1,USD,-0.03\n" + prefix + "Refund,InputTokens,InvokeModel,sku1,USD,-0.04\n" + prefix + "Tax,InputTokens,InvokeModel,sku1,USD,90\n"
	})
	run()
	corrected := costs()
	expected := map[string]string{
		"AmazonBedrock | Usage | InputTokens | InvokeModel | sku1":  "0.90000000000000000001",
		"AmazonBedrock | Credit | InputTokens | InvokeModel | sku1": "-0.03",
		"AmazonBedrock | Refund | InputTokens | InvokeModel | sku1": "-0.04",
	}
	if !reflect.DeepEqual(corrected, expected) {
		t.Fatalf("corrected report was not replaced exactly: %v", corrected)
	}
	if status := h.want(owner, "GET", path, nil, nil, 200)["status"].(map[string]any); status["source_revision"] != "corrected" {
		t.Fatalf("incorrect revision: %v", status)
	}
	emptyDay := day.AddDate(0, 0, 1)
	empty := h.want(owner, "GET", fmt.Sprintf("/api/v1/usage/reconciliation?start=%s&end=%s&provider_id=%s", emptyDay.Format("2006-01-02"), emptyDay.AddDate(0, 0, 1).Format("2006-01-02"), id), nil, nil, 200)["items"].([]any)[0].(map[string]any)
	if empty["invoiced_cost"] != "0" || empty["capture_rate_unavailable_reason"] != "zero_invoice" {
		t.Fatalf("restated empty day did not replace prior costs: %v", empty)
	}
	run()
	if !reflect.DeepEqual(costs(), expected) {
		t.Fatalf("equal revision changed the invoice: %v", costs())
	}
	fake.mu.Lock()
	for key := range fake.objects {
		if strings.Contains(key, "/corrected/") {
			delete(fake.objects, key)
		}
	}
	fake.mu.Unlock()
	run()
	if !reflect.DeepEqual(costs(), expected) {
		t.Fatalf("older revision regressed the invoice: %v", costs())
	}
	if status := h.want(owner, "GET", path, nil, nil, 200)["status"].(map[string]any); status["source_revision"] != "corrected" {
		t.Fatalf("older revision regressed the import status: %v", status)
	}
	var metrics strings.Builder
	billing.WriteMetrics(t.Context(), h.Pool, importer, &metrics)
	if !strings.Contains(metrics.String(), fmt.Sprintf("olp_billing_imports_total{provider=%q,outcome=\"success\"} 4", id)) {
		t.Fatalf("AWS imports missing from metrics: %s", metrics.String())
	}
}

func TestBillingImportAWSRetentionAndBackfill(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	fake := newAWSBillingFake(t)
	today := time.Now().UTC().Truncate(24 * time.Hour)
	month := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC)
	oldDay := month.AddDate(0, -2, 3)
	previousDay := month.AddDate(0, -1, 3)
	currentDay := month
	clock := month.AddDate(0, 0, 3)
	fake.delivery(t, oldDay, "old", month.AddDate(0, -1, 1), "1")
	fake.delivery(t, previousDay, "previous", month.AddDate(0, 0, 1), "2")
	fake.delivery(t, currentDay, "current", month.AddDate(0, 0, 1), "3")
	provider := h.want(owner, "POST", "/api/v1/providers", map[string]any{"name": "AWS retention", "configuration": map[string]any{"kind": "bedrock", "auth_mode": "static", "endpoint": "https://bedrock-runtime.us-east-1.amazonaws.com", "cloud_region": "us-east-1"}, "credential": `{"access_key_id":"BEDROCKKEY1234567890","secret_access_key":"bedrock-secret-123456789"}`, "model": "amazon.titan-text-express-v1"}, idem("aws-retention-provider"), 201)
	id := billingProviderID(provider)
	scope := map[string]any{"payer_account_id": "111122223333", "bucket": "billing-report-bucket", "report_prefix": "billing/cur", "region": "us-east-1", "linked_account_ids": []string{"444455556666"}}
	credential := map[string]any{"auth_mode": "static", "access_key_id": "BILLINGKEY1234567890", "secret_access_key": "billing-secret-example"}
	billingPut(h, owner, id, map[string]any{"scope": scope, "credential": credential}, provider["etag"].(string), 201)
	price := "0.1"
	billingFact(t, h, id, uuid.NewString(), oldDay, "billable", true, &price, false, false, false)
	importer := billingImporter(h, t)
	importer.Now = func() time.Time { return clock }
	importer.AWSClient = func(config aws.Config) *s3.Client {
		return s3.NewFromConfig(config, func(o *s3.Options) { o.BaseEndpoint = aws.String(fake.server.URL); o.UsePathStyle = true })
	}
	path := "/api/v1/providers/" + id + "/billing-import"
	run := func() {
		t.Helper()
		headers := etagHeader(h.want(owner, "GET", path, nil, nil, 200))
		headers["Idempotency-Key"] = uuid.NewString()
		h.want(owner, "POST", path+"/run", nil, headers, 202)
		if err := importer.Pass(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	amount := func(day time.Time) string {
		t.Helper()
		items := h.want(owner, "GET", fmt.Sprintf("/api/v1/providers/%s/invoiced-costs?start=%s&end=%s", id, day.Format("2006-01-02"), day.AddDate(0, 0, 1).Format("2006-01-02")), nil, nil, 200)["items"].([]any)
		if len(items) == 0 {
			return ""
		}
		if len(items) != 1 {
			t.Fatalf("unexpected invoice lines: %v", items)
		}
		return items[0].(map[string]any)["amount"].(string)
	}
	if err := importer.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if amount(oldDay) != "1.20000000000000000001" || amount(previousDay) != "2.20000000000000000001" || amount(currentDay) != "3.20000000000000000001" {
		t.Fatalf("initial retention backfill missed a period: old=%s previous=%s current=%s", amount(oldDay), amount(previousDay), amount(currentDay))
	}
	fake.delivery(t, previousDay, "previous-corrected", month.AddDate(0, 0, 1).Add(time.Hour), "4")
	fake.delivery(t, currentDay, "current-corrected", month.AddDate(0, 0, 2), "5")
	run()
	if amount(previousDay) != "4.20000000000000000001" || amount(currentDay) != "5.20000000000000000001" {
		t.Fatalf("open periods were not refreshed: previous=%s current=%s", amount(previousDay), amount(currentDay))
	}
	setRetention := func(value string) {
		t.Helper()
		setting := h.want(owner, "GET", "/api/v1/settings/retention.usage_days", nil, nil, 200)
		h.want(owner, "PUT", "/api/v1/settings/retention.usage_days", map[string]any{"value": value}, etagHeader(setting), 200)
	}
	setRetention("1")
	if err := importer.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if amount(oldDay) != "" {
		t.Fatalf("expired invoice was not purged: %s", amount(oldDay))
	}
	report := h.want(owner, "GET", fmt.Sprintf("/api/v1/usage/reconciliation?start=%s&end=%s&provider_id=%s", oldDay.Format("2006-01-02"), oldDay.AddDate(0, 0, 1).Format("2006-01-02"), id), nil, nil, 200)
	item := report["items"].([]any)[0].(map[string]any)
	if item["invoiced_cost"] != nil || item["capture_rate_unavailable_reason"] != "not_imported" {
		t.Fatalf("purged invoice was reported as a covered zero: %v", item)
	}
	setRetention("90")
	run()
	if amount(oldDay) != "1.20000000000000000001" {
		t.Fatalf("wider retention did not restore the old period: %s", amount(oldDay))
	}
}

func TestBillingImportAWSCaptureRateMetric(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	fake := newAWSBillingFake(t)
	today := time.Now().UTC().Truncate(24 * time.Hour)
	start := today.AddDate(0, 0, -7)
	yesterday := today.AddDate(0, 0, -1)
	fake.delivery(t, start, "window-start", today, "0.1")
	fake.delivery(t, yesterday, "window-end", today.Add(time.Second), "0.1")
	provider := h.want(owner, "POST", "/api/v1/providers", map[string]any{"name": "AWS metrics", "configuration": map[string]any{"kind": "bedrock", "auth_mode": "static", "endpoint": "https://bedrock-runtime.us-east-1.amazonaws.com", "cloud_region": "us-east-1"}, "credential": `{"access_key_id":"BEDROCKKEY1234567890","secret_access_key":"bedrock-secret-123456789"}`, "model": "amazon.titan-text-express-v1"}, idem("aws-metrics-provider"), 201)
	id := billingProviderID(provider)
	scope := map[string]any{"payer_account_id": "111122223333", "bucket": "billing-report-bucket", "report_prefix": "billing/cur", "region": "us-east-1", "linked_account_ids": []string{"444455556666"}}
	credential := map[string]any{"auth_mode": "static", "access_key_id": "BILLINGKEY1234567890", "secret_access_key": "billing-secret-example"}
	billingPut(h, owner, id, map[string]any{"scope": scope, "credential": credential}, provider["etag"].(string), 201)
	price := "0.3"
	billingFact(t, h, id, uuid.NewString(), yesterday, "billable", true, &price, false, false, false)
	importer := billingImporter(h, t)
	importer.AWSClient = func(config aws.Config) *s3.Client {
		return s3.NewFromConfig(config, func(o *s3.Options) { o.BaseEndpoint = aws.String(fake.server.URL); o.UsePathStyle = true })
	}
	if err := importer.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	items := h.want(owner, "GET", fmt.Sprintf("/api/v1/providers/%s/invoiced-costs?start=%s&end=%s", id, start.Format("2006-01-02"), today.Format("2006-01-02")), nil, nil, 200)["items"].([]any)
	total := decimal.Zero
	for _, value := range items {
		amount, err := decimal.NewFromString(value.(map[string]any)["amount"].(string))
		if err != nil {
			t.Fatal(err)
		}
		total = total.Add(amount)
	}
	if total.IsZero() {
		t.Fatal("expected invoiced costs for capture rate")
	}
	want := decimal.RequireFromString(price).Div(total).Round(6).String()
	var metrics strings.Builder
	billing.WriteMetrics(t.Context(), h.Pool, importer, &metrics)
	if sample := fmt.Sprintf("olp_billing_capture_rate{provider=%q} %s", id, want); !strings.Contains(metrics.String(), sample) {
		t.Fatalf("AWS capture rate metric missing %q: %s status=%v", sample, metrics.String(), h.want(owner, "GET", "/api/v1/providers/"+id+"/billing-import", nil, nil, 200)["status"])
	}
}

func TestBillingImportAWSHistoryAcrossScopeChanges(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	fake := newAWSBillingFake(t)
	today := time.Now().UTC().Truncate(24 * time.Hour)
	month := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC)
	oldDay := month.AddDate(0, -2, 3)
	newDay := month.AddDate(0, -1, 3)
	fake.delivery(t, oldDay, "scope-a", oldDay.AddDate(0, 0, 1), "0.1")
	provider := h.want(owner, "POST", "/api/v1/providers", map[string]any{"name": "AWS scope history", "configuration": map[string]any{"kind": "bedrock", "auth_mode": "static", "endpoint": "https://bedrock-runtime.us-east-1.amazonaws.com", "cloud_region": "us-east-1"}, "credential": `{"access_key_id":"BEDROCKKEY1234567890","secret_access_key":"bedrock-secret-123456789"}`, "model": "amazon.titan-text-express-v1"}, idem("aws-scope-history-provider"), 201)
	id := billingProviderID(provider)
	scopeA := map[string]any{"payer_account_id": "111122223333", "bucket": "billing-report-bucket", "report_prefix": "billing/cur", "region": "us-east-1", "linked_account_ids": []string{"444455556666"}}
	scopeB := map[string]any{"payer_account_id": "111122223333", "bucket": "billing-report-bucket", "report_prefix": "billing/cur", "region": "us-east-1", "linked_account_ids": []string{"777788889999"}}
	credential := map[string]any{"auth_mode": "static", "access_key_id": "BILLINGKEY1234567890", "secret_access_key": "billing-secret-example"}
	path := "/api/v1/providers/" + id + "/billing-import"
	billingPut(h, owner, id, map[string]any{"scope": scopeA, "credential": credential}, provider["etag"].(string), 201)
	price := "0.15"
	billingFact(t, h, id, uuid.NewString(), newDay, "billable", true, &price, false, false, false)
	importer := billingImporter(h, t)
	importer.AWSClient = func(config aws.Config) *s3.Client {
		return s3.NewFromConfig(config, func(o *s3.Options) { o.BaseEndpoint = aws.String(fake.server.URL); o.UsePathStyle = true })
	}
	if err := importer.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	report := func(day time.Time) map[string]any {
		t.Helper()
		result := h.want(owner, "GET", fmt.Sprintf("/api/v1/usage/reconciliation?start=%s&end=%s&provider_id=%s", day.Format("2006-01-02"), day.AddDate(0, 0, 1).Format("2006-01-02"), id), nil, nil, 200)
		items := result["items"].([]any)
		if len(items) != 1 {
			t.Fatalf("missing historical day: %v", result)
		}
		return items[0].(map[string]any)
	}
	old := report(oldDay)
	if old["invoiced_cost"] != "0.30000000000000000001" {
		t.Fatalf("scope A did not import: %v", old)
	}
	current := h.want(owner, "GET", path, nil, nil, 200)
	changed := billingPut(h, owner, id, map[string]any{"scope": scopeB}, current["etag"].(string), 200)
	status := changed["status"].(map[string]any)
	if status["last_success_at"] != nil || status["imported_from"] != nil || status["imported_through"] != nil {
		t.Fatalf("scope B inherited scope A success: %v", status)
	}
	h.want(owner, "DELETE", path, nil, etagHeader(changed), 204)
	pending := report(newDay)
	if pending["provisional"] != true || pending["capture_rate_unavailable_reason"] != "not_imported" {
		t.Fatalf("unsuccessful scope B was reported as final: %v", pending)
	}
	if historical := report(oldDay); historical["invoiced_cost"] != old["invoiced_cost"] || historical["provisional"] != old["provisional"] {
		t.Fatalf("scope A history was lost after reconfiguration: before=%v after=%v", old, historical)
	}
	provider = h.want(owner, "GET", "/api/v1/providers/"+id, nil, nil, 200)
	billingPut(h, owner, id, map[string]any{"scope": scopeB, "credential": credential}, provider["etag"].(string), 201)
	fake.mu.Lock()
	fake.objects = map[string]awsObject{}
	fake.mu.Unlock()
	fake.delivery(t, newDay, "scope-b", newDay.AddDate(0, 0, 1), "0.1")
	if err := importer.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	status = h.want(owner, "GET", path, nil, nil, 200)["status"].(map[string]any)
	if status["source_revision"] != "scope-b" || status["imported_from"].(string) < month.AddDate(0, -1, 0).Format("2006-01-02") {
		t.Fatalf("scope B status included scope A coverage: %v", status)
	}
	if historical := report(oldDay); historical["invoiced_cost"] != old["invoiced_cost"] || historical["provisional"] != old["provisional"] {
		t.Fatalf("scope B erased scope A history: before=%v after=%v", old, historical)
	}
	if current := report(newDay); current["invoiced_cost"] != "10" {
		t.Fatalf("scope B costs missing: %v", current)
	}
}
