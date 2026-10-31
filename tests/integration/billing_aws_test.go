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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
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
	fake.delivery(t, day, "assembly-one", day.AddDate(0, 0, 1), "0.1")
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
	newer := day.Add(2 * time.Hour)
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
	if status()["imported_through"] != day.AddDate(0, 0, 1).Format("2006-01-02") || len(amounts()) != 1 {
		t.Fatalf("equal revision did not cover the next closed day: %v %v", status(), amounts())
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
	if status()["source_revision"] != "newer" || status()["imported_through"] != day.AddDate(0, 0, 1).Format("2006-01-02") {
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
			fake.delivery(t, day, "in-flight", day.Add(time.Hour), "0.1")
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
	fake.delivery(t, day, "parquet", day.Add(time.Hour), "0.1")
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
	fake.delivery(t, day, "bad-currency", day.Add(2*time.Hour), "0.1")
	fake.rewriteCSV(t, func(csv string) string { return strings.Replace(csv, "sku1,USD,0.1", "sku1,US,0.1", 1) })
	schedule()
	pass()
	if status()["last_error_code"] != "response" || status()["imported_through"] != nil || len(costs()) != 0 {
		t.Fatalf("invalid currency: %v %v", status(), costs())
	}
	fake.mu.Lock()
	fake.objects = map[string]awsObject{}
	fake.mu.Unlock()
	fake.delivery(t, day, "zip-delivery", day.Add(3*time.Hour), "0.1")
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
	checkFailure := func(expected string) {
		t.Helper()
		if status()["last_error_code"] != expected || fmt.Sprint(costs()) != previous {
			t.Fatalf("failed delivery changed existing costs: status=%v costs=%v", status(), costs())
		}
	}
	fake.mu.Lock()
	fake.objects = map[string]awsObject{}
	fake.mu.Unlock()
	schedule()
	pass()
	checkFailure("status")
	fake.delivery(t, day, "truncated", day.Add(4*time.Hour), "0.1")
	fake.mu.Lock()
	for key, object := range fake.objects {
		if strings.HasSuffix(key, ".csv.gz") {
			fake.objects[key] = awsObject{body: object.body[:len(object.body)/2], modified: object.modified}
		}
	}
	fake.mu.Unlock()
	schedule()
	pass()
	checkFailure("response")
	fake.mu.Lock()
	fake.objects = map[string]awsObject{}
	fake.mu.Unlock()
	fake.delivery(t, day, "too-many-parts", day.Add(5*time.Hour), "0.1")
	fake.mu.Lock()
	for key, object := range fake.objects {
		if strings.HasSuffix(key, "-Manifest.json") {
			var manifest map[string]any
			if err := json.Unmarshal(object.body, &manifest); err != nil {
				t.Fatal(err)
			}
			parts := make([]string, 1001)
			for index := range parts {
				parts[index] = fmt.Sprintf("billing/cur/part-%d.csv.gz", index)
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
	checkFailure("response")
	current := h.want(owner, "GET", "/api/v1/providers/"+id+"/billing-import", nil, nil, 200)
	badCredential := map[string]any{"auth_mode": "static", "access_key_id": "REVOKEDKEY1234567890", "secret_access_key": "revoked-secret-example"}
	billingPut(h, owner, id, map[string]any{"scope": scope, "credential": badCredential}, current["etag"].(string), 200)
	schedule()
	pass()
	checkFailure("credential")
}

func TestBillingImportAWSSlowDeliveryDoesNotLeaseOtherImports(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	fake := newAWSBillingFake(t)
	today := time.Now().UTC().Truncate(24 * time.Hour)
	day := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -1, 3)
	fake.delivery(t, day, "slow", day.Add(time.Hour), "0.1")
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
	fake.delivery(t, day, "complete", day.Add(time.Hour), "0.1")
	fake.delivery(t, day, "uploading", day.Add(2*time.Hour), "0.5")
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
	fake.delivery(t, day, "workload", day.Add(time.Hour), "0.1")
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
