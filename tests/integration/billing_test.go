//go:build integration

package integration_test

import (
	"bytes"
	"compress/gzip"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/billing"
	"github.com/tyk-swe/olp/internal/database"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/tests/harness"
)

type billingFixture struct {
	pool         *pgxpool.Pool
	keys         *secrets.KeyRing
	installation string
	ownerID      string
	providerID   string
}

func newBillingFixture(t *testing.T, kind, configuration string) billingFixture {
	t.Helper()
	ctx := t.Context()
	pool, _ := harness.Database(t)
	installation, err := database.Installation(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	f := billingFixture{pool: pool, keys: keyRing(t, "01"), installation: installation, ownerID: uuid.NewString(), providerID: uuid.NewString()}
	if err := access.PrepareInstallation(ctx, pool, installation, secrets.NewAuthKey([]byte(strings.Repeat("k", 32)), installation), f.keys); err != nil {
		t.Fatal(err)
	}
	projectID := uuid.NewString()
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO olp.users (id,email,display_name,role,etag) VALUES ($1::uuid,$1::text || '@example.com','Billing fixture','owner',$1::uuid)`, []any{f.ownerID}},
		{`INSERT INTO olp.projects (id,name,etag,created_by) VALUES ($1::uuid,$1::text,$1::uuid,$2)`, []any{projectID, f.ownerID}},
		{`INSERT INTO olp.providers (id,name,kind,state,configuration,etag,slots_etag,created_by,project_id)
			VALUES ($1::uuid,$1::text,$2,'draft',$3::jsonb,$1::uuid,$1::uuid,$4,$5)`, []any{f.providerID, kind, configuration, f.ownerID, projectID}},
		{`INSERT INTO olp.settings (key,value,etag,updated_by) VALUES ('retention.usage_days','15',$1::uuid,$1::uuid)`, []any{f.ownerID}},
	} {
		if _, err := pool.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f billingFixture) enable(t *testing.T, source, scope, storedCredential string) {
	t.Helper()
	ctx := t.Context()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	credentialID := uuid.NewString()
	if err := f.keys.Store(ctx, tx, f.installation, credentialID, secrets.BillingCredential, []byte(storedCredential), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO olp.provider_billing_imports (provider_id,source,credential_id,scope,enabled,etag,created_by,next_import_at)
		VALUES ($1::uuid,$2,$3::uuid,$4::jsonb,true,$5::uuid,$6::uuid,now())`,
		f.providerID, source, credentialID, scope, uuid.NewString(), f.ownerID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func (f billingFixture) retainUsage(t *testing.T, days int) {
	t.Helper()
	if _, err := f.pool.Exec(t.Context(), `UPDATE olp.settings SET value=$1 WHERE key='retention.usage_days'`, strconv.Itoa(days)); err != nil {
		t.Fatal(err)
	}
}

func (f billingFixture) importDue(t *testing.T, s3Endpoint string) {
	t.Helper()
	if _, err := f.pool.Exec(t.Context(), `UPDATE olp.provider_billing_imports SET next_import_at=now() WHERE provider_id=$1::uuid`, f.providerID); err != nil {
		t.Fatal(err)
	}
	importer := &billing.Importer{
		Pool: f.pool, Keys: f.keys, Installation: f.installation, Log: slog.New(slog.DiscardHandler),
		Egress: &egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}},
		AWSClient: func(config aws.Config) *s3.Client {
			return s3.NewFromConfig(config, func(o *s3.Options) { o.BaseEndpoint, o.UsePathStyle = aws.String(s3Endpoint), true })
		},
	}
	if err := importer.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
}

type billingImportStatus struct {
	succeeded bool
	errorCode string
	failures  int
	from      string
	through   string
	nextIn    time.Duration
}

func (f billingFixture) status(t *testing.T) billingImportStatus {
	t.Helper()
	var status billingImportStatus
	var nextIn float64
	if err := f.pool.QueryRow(t.Context(), `SELECT last_success_at IS NOT NULL,COALESCE(last_error_code,''),consecutive_failures,
		COALESCE(imported_from::text,''),COALESCE(imported_through::text,''),EXTRACT(EPOCH FROM next_import_at-now())::float8
		FROM olp.provider_billing_imports WHERE provider_id=$1::uuid`, f.providerID).Scan(
		&status.succeeded, &status.errorCode, &status.failures, &status.from, &status.through, &nextIn); err != nil {
		t.Fatal(err)
	}
	status.nextIn = time.Duration(nextIn * float64(time.Second))
	return status
}

func (f billingFixture) invoiced(t *testing.T) []string {
	t.Helper()
	rows, err := f.pool.Query(t.Context(), `SELECT concat_ws(' / ',day::text,line_item,currency,trim_scale(amount)::text)
		FROM olp.provider_invoiced_costs WHERE provider_id=$1::uuid ORDER BY day,line_item,currency`, f.providerID)
	if err != nil {
		t.Fatal(err)
	}
	lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return lines
}

func wantBillingStatus(t *testing.T, got billingImportStatus, errorCode string, failures int, nextAfter, nextBefore time.Duration) {
	t.Helper()
	if got.errorCode != errorCode || got.failures != failures || got.nextIn < nextAfter || got.nextIn > nextBefore {
		t.Fatalf("import status %+v, want error %q after %d failures and the next import in %v to %v", got, errorCode, failures, nextAfter, nextBefore)
	}
}

func day(at time.Time) string { return at.Format(time.DateOnly) }

func TestBillingImportStoresEachInvoicedDayAndRefreshesTheLastWeek(t *testing.T) {
	t.Parallel()
	const key = "sk-admin-billing-fixture-0123456789"
	today := time.Now().UTC().Truncate(24 * time.Hour)
	first, second := today.AddDate(0, 0, -2), today.AddDate(0, 0, -1)
	var mu sync.Mutex
	var queries []url.Values
	secondAmount := "2"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/organization/costs" || r.Header.Get("Authorization") != "Bearer "+key {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		queries = append(queries, r.URL.Query())
		result := func(value any, currency, item string) map[string]any {
			return map[string]any{"amount": map[string]any{"value": value, "currency": currency}, "line_item": item}
		}
		json.NewEncoder(w).Encode(map[string]any{"object": "list", "has_more": false, "data": []any{
			map[string]any{"start_time": first.Unix(), "results": []any{result("1.25", "USD", "gpt-5, input"), result(0.5, "usd", "gpt-5, input")}},
			map[string]any{"start_time": second.Unix(), "results": []any{result(secondAmount, "USD", "gpt-5, output")}},
		}})
	}))
	t.Cleanup(server.Close)
	f := newBillingFixture(t, "openai", fmt.Sprintf(`{"kind":"openai","endpoint":%q}`, server.URL+"/v1"))
	f.enable(t, "openai_costs", `{"project_ids":["proj_abc123"]}`, "{\n  \"source\": \"openai_costs\",\n  \"api_key\": \""+key+"\"\n}")

	f.importDue(t, "")
	status := f.status(t)
	wantBillingStatus(t, status, "", 0, 55*time.Minute, 65*time.Minute)
	if !status.succeeded || status.from != day(first) || status.through != day(second) {
		t.Fatalf("first import status %+v, want success covering %s to %s", status, day(first), day(second))
	}
	if got, want := f.invoiced(t), []string{day(first) + " / gpt-5, input / USD / 1.75", day(second) + " / gpt-5, output / USD / 2"}; !slices.Equal(got, want) {
		t.Fatalf("invoiced costs after the first import:\n%q\nwant\n%q", got, want)
	}
	mu.Lock()
	if len(queries) != 1 || queries[0].Get("start_time") != strconv.FormatInt(today.AddDate(0, 0, -30).Unix(), 10) ||
		!slices.Equal(queries[0]["group_by[]"], []string{"line_item"}) || !slices.Equal(queries[0]["project_ids[]"], []string{"proj_abc123"}) {
		t.Fatalf("the first import asked for %v, want a month of line items for the scoped project", queries)
	}
	secondAmount = "3"
	mu.Unlock()

	f.importDue(t, "")
	if got, want := f.invoiced(t), []string{day(first) + " / gpt-5, input / USD / 1.75", day(second) + " / gpt-5, output / USD / 3"}; !slices.Equal(got, want) {
		t.Fatalf("invoiced costs after a restatement:\n%q\nwant\n%q", got, want)
	}
	mu.Lock()
	defer mu.Unlock()
	if got, want := queries[len(queries)-1].Get("start_time"), strconv.FormatInt(today.AddDate(0, 0, -7).Unix(), 10); got != want {
		t.Fatalf("a refresh started at %s, want the last week from %s", got, want)
	}
}

func TestBillingImportBacksOffWhenTheVendorRejectsALegacyRawKey(t *testing.T) {
	t.Parallel()
	const rawKey = "sk-ant-admin-legacy-raw-key-0123456789"
	var sent sync.Map
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent.Store(r.Header.Get("x-api-key"), true)
		http.Error(w, "revoked", http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)
	f := newBillingFixture(t, "anthropic", fmt.Sprintf(`{"kind":"anthropic","endpoint":%q}`, server.URL))
	f.enable(t, "anthropic_cost_report", `{}`, rawKey)

	f.importDue(t, "")
	wantBillingStatus(t, f.status(t), "credential", 1, 4*time.Minute, 6*time.Minute)
	if _, ok := sent.Load(rawKey); !ok {
		t.Fatal("the importer did not send the stored legacy key to the vendor")
	}
	if got := f.invoiced(t); len(got) != 0 {
		t.Fatalf("a rejected import stored %q", got)
	}
}

func TestBillingImportReadsBedrockLinesFromAnAWSCostReport(t *testing.T) {
	t.Parallel()
	const payer, bucket, prefix = "123456789012", "olp-cur", "cur/olp"
	today := time.Now().UTC().Truncate(24 * time.Hour)
	earliest := today.AddDate(0, 0, -15)
	bucketHandler, _ := fakeCostReportBucket(t, bucket, prefix, payer, earliest, today)
	server := httptest.NewServer(bucketHandler)
	t.Cleanup(server.Close)
	f := newBillingFixture(t, "bedrock", `{"kind":"bedrock"}`)
	f.enable(t, "aws_cur", fmt.Sprintf(`{"payer_account_id":%q,"bucket":%q,"report_prefix":%q,"region":"us-east-1"}`, payer, bucket, prefix), staticAWSCredential)

	f.importDue(t, server.URL)
	status := f.status(t)
	wantBillingStatus(t, status, "", 0, 0, 65*time.Minute)
	if !status.succeeded || status.from != day(earliest) || status.through != day(today.AddDate(0, 0, -1)) {
		t.Fatalf("import status %+v, want success covering %s to yesterday", status, day(earliest))
	}
	var want []string
	for at := earliest; at.Before(today); at = at.AddDate(0, 0, 1) {
		want = append(want, day(at)+" / AmazonBedrock | Usage | USE1-Claude-input-tokens | InvokeModel | SKU1 / USD / 0.5")
	}
	if got := f.invoiced(t); !slices.Equal(got, want) {
		t.Fatalf("invoiced costs:\n%q\nwant\n%q", got, want)
	}
}

func TestBillingImportRereadsOnlyTheOpenMonthsOnceOlderMonthsAreCovered(t *testing.T) {
	t.Parallel()
	const payer, bucket, prefix = "123456789012", "olp-cur", "cur/olp"
	today := time.Now().UTC().Truncate(24 * time.Hour)
	earliest := today.AddDate(0, 0, -100)
	bucketHandler, listed := fakeCostReportBucket(t, bucket, prefix, payer, earliest, today)
	server := httptest.NewServer(bucketHandler)
	t.Cleanup(server.Close)
	f := newBillingFixture(t, "bedrock", `{"kind":"bedrock"}`)
	f.retainUsage(t, 100)
	f.enable(t, "aws_cur", fmt.Sprintf(`{"payer_account_id":%q,"bucket":%q,"report_prefix":%q,"region":"us-east-1"}`, payer, bucket, prefix), staticAWSCredential)

	f.importDue(t, server.URL)
	backfilled := listed()
	f.importDue(t, server.URL)
	reread := slices.Sorted(slices.Values(listed()[len(backfilled):]))

	current := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC)
	period := func(month time.Time) string {
		return month.Format("20060102") + "-" + month.AddDate(0, 1, 0).Format("20060102")
	}
	if want := []string{period(current.AddDate(0, -1, 0)), period(current)}; len(backfilled) < 4 || !slices.Equal(reread, want) {
		t.Fatalf("listed %q on backfill and %q on the next pass, want only %q on the next pass", backfilled, reread, want)
	}
	if status := f.status(t); !status.succeeded || status.from != day(earliest) || status.through != day(today.AddDate(0, 0, -1)) {
		t.Fatalf("import status %+v, want success covering %s to yesterday", status, day(earliest))
	}
}

const staticAWSCredential = "{\n  \"source\": \"aws_cur\",\n  \"auth_mode\": \"static\",\n  \"access_key_id\": \"AKIAIOSFODNN7EXAMPLE\",\n  \"secret_access_key\": \"wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY\"\n}"

func fakeCostReportBucket(t *testing.T, bucket, prefix, payer string, earliest, today time.Time) (http.Handler, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var listed []string
	snapshot := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(listed)
	}
	const manifestTime = "20060102T150405.000Z"
	delivered := time.Now().UTC().Add(-time.Minute)
	month := func(period string) (time.Time, time.Time) {
		start, err := time.Parse("20060102", period[:8])
		if err != nil {
			t.Error(err)
		}
		return start, start.AddDate(0, 1, 0)
	}
	report := func(start, end time.Time) []byte {
		var buffer bytes.Buffer
		zipped := gzip.NewWriter(&buffer)
		rows := csv.NewWriter(zipped)
		rows.Write([]string{"identity/LineItemId", "bill/PayerAccountId", "lineItem/UsageAccountId", "lineItem/ProductCode", "lineItem/UsageStartDate",
			"lineItem/LineItemType", "lineItem/UsageType", "lineItem/Operation", "product/sku", "lineItem/CurrencyCode", "lineItem/UnblendedCost"})
		for at := start; at.Before(end); at = at.AddDate(0, 0, 1) {
			stamp := at.Format(time.RFC3339)
			rows.Write([]string{"bedrock-" + stamp, payer, payer, "AmazonBedrock", stamp, "Usage", "USE1-Claude-input-tokens", "InvokeModel", "SKU1", "USD", "0.5"})
			rows.Write([]string{"bedrock-" + stamp, payer, payer, "AmazonBedrock", stamp, "Usage", "USE1-Claude-input-tokens", "InvokeModel", "SKU1", "USD", "0.5"})
			rows.Write([]string{"tax-" + stamp, payer, payer, "AmazonBedrock", stamp, "Tax", "USE1-Claude-input-tokens", "InvokeModel", "SKU1", "USD", "9"})
			rows.Write([]string{"ec2-" + stamp, payer, payer, "AmazonEC2", stamp, "Usage", "BoxUsage", "RunInstances", "SKU2", "USD", "9"})
		}
		rows.Flush()
		zipped.Close()
		return buffer.Bytes()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, ok := strings.CutPrefix(r.URL.Path, "/"+bucket+"/")
		switch {
		case r.URL.Path == "/"+bucket && r.URL.Query().Get("list-type") == "2":
			listPrefix := r.URL.Query().Get("prefix")
			period := strings.TrimSuffix(strings.TrimPrefix(listPrefix, prefix+"/"), "/")
			mu.Lock()
			listed = append(listed, period)
			mu.Unlock()
			w.Header().Set("Content-Type", "application/xml")
			fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>%s</Name><Prefix>%s</Prefix><KeyCount>1</KeyCount><MaxKeys>1000</MaxKeys><IsTruncated>false</IsTruncated><Contents><Key>%s</Key><LastModified>%s</LastModified><Size>1</Size></Contents></ListBucketResult>`,
				bucket, listPrefix, listPrefix+"assembly-"+period+"/olp-Manifest.json", delivered.Format("2006-01-02T15:04:05.000Z"))
		case ok && strings.HasSuffix(key, "-Manifest.json"):
			period := strings.Split(strings.TrimPrefix(key, prefix+"/"), "/")[0]
			start, end := month(period)
			json.NewEncoder(w).Encode(map[string]any{
				"assemblyId":    "assembly-" + period,
				"billingPeriod": map[string]string{"start": start.Format(manifestTime), "end": end.Format(manifestTime)},
				"reportKeys":    []string{strings.TrimSuffix(key, "olp-Manifest.json") + "olp-00001.csv.gz"},
			})
		case ok && strings.HasSuffix(key, ".csv.gz"):
			start, end := month(strings.Split(strings.TrimPrefix(key, prefix+"/"), "/")[0])
			if start.Before(earliest) {
				start = earliest
			}
			if today.Before(end) {
				end = today
			}
			w.Write(report(start, end))
		default:
			http.NotFound(w, r)
		}
	}), snapshot
}
