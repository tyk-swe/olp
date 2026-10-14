//go:build integration

package export

import (
	"bytes"
	"compress/gzip"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/export/focusspec"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/usage"
)

// focusFixture seeds the full metering chain: two projects, one key each,
// three providers (plain, discounted, committed), and the prices they bill.
type focusFixture struct {
	owner     string
	org       string
	projectA  string
	projectB  string
	keyA      string
	keyB      string
	keyCaller string
	plain     string
	discount  string
	committed string
	effective time.Time
}

func seedFocusFixture(t *testing.T, pool *pgxpool.Pool, day time.Time) focusFixture {
	t.Helper()
	f := focusFixture{
		owner:     uuid.NewString(),
		org:       uuid.NewString(),
		projectA:  uuid.NewString(),
		projectB:  uuid.NewString(),
		keyA:      uuid.NewString(),
		keyB:      uuid.NewString(),
		keyCaller: uuid.NewString(),
		plain:     uuid.NewString(),
		discount:  uuid.NewString(),
		committed: uuid.NewString(),
		effective: day.Add(-2 * time.Hour),
	}
	provider := func(id, name, config string) {
		rev, etag, slots := uuid.NewString(), uuid.NewString(), uuid.NewString()
		if _, err := pool.Exec(t.Context(), `INSERT INTO olp.providers(id,name,kind,state,configuration,etag,slots_etag,active_revision,active_revision_id,created_by)
			VALUES($1::uuid,$2,'openai','active',$3::jsonb,$4::uuid,$5::uuid,1,NULL,$6::uuid)`,
			id, name, config, etag, slots, f.owner); err != nil {
			t.Fatalf("provider %s: %v", name, err)
		}
		if _, err := pool.Exec(t.Context(), `INSERT INTO olp.provider_revisions(id,provider_id,revision,name,configuration,models,slots,source_etag,activated_by)
			VALUES($1::uuid,$2::uuid,1,$3,$4::jsonb,'[]'::jsonb,'[]'::jsonb,$5::uuid,$6::uuid)`,
			rev, id, name, config, uuid.NewString(), f.owner); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(t.Context(), `UPDATE olp.providers SET active_revision_id=$1::uuid WHERE id=$2::uuid`, rev, id); err != nil {
			t.Fatal(err)
		}
	}
	statements := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO olp.users(id,email,display_name,role,etag) VALUES($1::uuid,$2,'Focus','owner',$3::uuid)`,
			[]any{f.owner, "focus-" + f.owner + "@example.test", uuid.NewString()}},
		{`INSERT INTO olp.organizations(id,name,etag,created_by) VALUES($1::uuid,$2,$3::uuid,$4::uuid)`,
			[]any{f.org, "focus-org", uuid.NewString(), f.owner}},
		{`INSERT INTO olp.projects(id,organization_id,name,etag,created_by) VALUES($1::uuid,$2,$3,$4::uuid,$5::uuid)`,
			[]any{f.projectA, f.org, "focus-a", uuid.NewString(), f.owner}},
		{`INSERT INTO olp.projects(id,organization_id,name,etag,created_by) VALUES($1::uuid,$2,$3,$4::uuid,$5::uuid)`,
			[]any{f.projectB, f.org, "focus-b", uuid.NewString(), f.owner}},
		{`INSERT INTO olp.api_keys(id,lookup_id,digest,name,project_id,created_by,policy,etag)
			VALUES($1::uuid,$2,$3,'focus-a',$4::uuid,$5::uuid,'{}'::jsonb,$6::uuid)`,
			[]any{f.keyA, "lk-" + f.keyA, []byte("d"), f.projectA, f.owner, uuid.NewString()}},
		{`INSERT INTO olp.api_keys(id,lookup_id,digest,name,project_id,created_by,policy,etag)
			VALUES($1::uuid,$2,$3,'focus-b',$4::uuid,$5::uuid,'{}'::jsonb,$6::uuid)`,
			[]any{f.keyB, "lk-" + f.keyB, []byte("d"), f.projectB, f.owner, uuid.NewString()}},
		{`INSERT INTO olp.api_keys(id,lookup_id,digest,name,project_id,created_by,policy,etag)
			VALUES($1::uuid,$2,$3,'focus-caller',$4::uuid,$5::uuid,'{}'::jsonb,$6::uuid)`,
			[]any{f.keyCaller, "lk-" + f.keyCaller, []byte("d"), f.projectA, f.owner, uuid.NewString()}},
		{`INSERT INTO olp.pricing_revisions(id,revision,effective_at,created_by) VALUES($1::uuid,1,$2,$3::uuid)`,
			[]any{uuid.NewString(), f.effective, f.owner}},
	}
	for _, st := range statements {
		if _, err := pool.Exec(t.Context(), st.sql, st.args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	provider(f.plain, "plain", `{"kind":"openai","auth_mode":"api_key","endpoint":"https://127.0.0.1:9","options":{"vendor_id":"openai"}}`)
	provider(f.discount, "discount", `{"kind":"openai","auth_mode":"api_key","endpoint":"https://127.0.0.1:9","options":{"vendor_id":"openai"}}`)
	provider(f.committed, "committed", `{"kind":"openai","auth_mode":"api_key","endpoint":"https://127.0.0.1:9","options":{"vendor_id":"openai","commitment":{"amount":"10","period":"day"}}}`)

	// Prices: input 3 + output 12 + audio 6 + tool fee for the plain provider;
	// input 2 for the committed one; the discounted provider shares plain's
	// rates with a 0.5 multiplier; tiered rates exercise the tier branch.
	var revision string
	if err := pool.QueryRow(t.Context(), `SELECT id::text FROM olp.pricing_revisions WHERE effective_at=$1`, f.effective).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	insertPrice := func(providerID *string, model string, columns map[string]any) {
		cols, binds, args := []string{"pricing_revision_id", "provider_kind", "model", "operation", "currency"},
			[]string{"$1::uuid", "$2", "$3", "$4", "$5"},
			[]any{revision, "openai", model, "generation", "USD"}
		if providerID != nil {
			cols = append(cols, "provider_id")
			binds = append(binds, fmt.Sprintf("$%d::uuid", len(args)+1))
			args = append(args, *providerID)
		}
		i := len(args)
		for k, v := range columns {
			cols = append(cols, k)
			binds = append(binds, fmt.Sprintf("$%d", i+1))
			args = append(args, v)
			i++
		}
		sql := fmt.Sprintf("INSERT INTO olp.prices (%s) VALUES (%s)", strings.Join(cols, ","), strings.Join(binds, ","))
		if _, err := pool.Exec(t.Context(), sql, args...); err != nil {
			t.Fatalf("price %s: %v", model, err)
		}
	}
	insertPrice(nil, "focus-model", map[string]any{
		"input_per_million": 3.0, "output_per_million": 12.0, "audio_input_per_million": 6.0,
		"tool_calls": `{"web_search":0.5}`,
	})
	insertPrice(nil, "focus-tiered", map[string]any{
		"input_per_million": 3.0,
		"tiers":             `[{"above_input_tokens":1000,"input_per_million":2}]`,
	})
	insertPrice(&f.committed, "focus-committed", map[string]any{"input_per_million": 2.0})
	// Half-price discount scoped to the discount provider.
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.pricing_discounts
		(id,provider_kind,provider_id,multiplier,effective_at,created_by)
		VALUES($1::uuid,'openai',$2::uuid,0.5,$3,$4::uuid)`,
		uuid.NewString(), f.discount, f.effective, f.owner); err != nil {
		t.Fatal(err)
	}
	// Rate cards: project A marks cost up 2x; project B pins an explicit price.
	cardA := uuid.NewString()
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.rate_cards(id,project_id,created_by) VALUES($1::uuid,$2::uuid,$3::uuid)`,
		cardA, f.projectA, f.owner); err != nil {
		t.Fatal(err)
	}
	cardARev := uuid.NewString()
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.rate_card_revisions(id,rate_card_id,revision,markup,effective_at,created_by)
		VALUES($1::uuid,$2::uuid,1,2,$3,$4::uuid)`, cardARev, cardA, f.effective, f.owner); err != nil {
		t.Fatal(err)
	}
	cardB := uuid.NewString()
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.rate_cards(id,project_id,created_by) VALUES($1::uuid,$2::uuid,$3::uuid)`,
		cardB, f.projectB, f.owner); err != nil {
		t.Fatal(err)
	}
	cardBRev := uuid.NewString()
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.rate_card_revisions(id,rate_card_id,revision,markup,effective_at,created_by)
		VALUES($1::uuid,$2::uuid,1,1,$3,$4::uuid)`, cardBRev, cardB, f.effective, f.owner); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.rate_card_prices
		(rate_card_revision_id,provider_kind,vendor_id,model,operation,input_per_million,currency)
		VALUES($1::uuid,'openai','openai','focus-model','generation',7::numeric,'USD')`, cardBRev); err != nil {
		t.Fatal(err)
	}
	return f
}

// focusEvent persists a one-attempt event at `observed`.
func focusEvent(t *testing.T, pool *pgxpool.Pool, key, provider, model string, observed time.Time,
	att *usage.AttemptUsage, credential string, attribution map[string]string) *usage.Event {
	t.Helper()
	status := 200
	event := &usage.Event{
		Version: usage.WireVersion, EventID: uuid.NewString(), RequestID: uuid.NewString(),
		RuntimeGenerationID: uuid.NewString(), APIKeyID: key, RouteSlug: "focus-route",
		Operation: "generation", Surface: "openai",
		RequestStartedAt: observed.Add(-time.Second), RequestCompletedAt: observed,
		ObservedAt: observed, StatusCode: &status, LatencyMS: 1000,
	}
	event.Attribution = attribution
	attempt := usage.Attempt{
		ID: uuid.NewString(), Ordinal: 1, ProviderID: provider, UpstreamModel: model,
		StartedAt: observed.Add(-time.Second), CompletedAt: observed.Add(-500 * time.Millisecond),
		StatusCode: &status, Committed: true, LatencyMS: 500, Usage: att,
	}
	if credential != "" {
		attempt.Routing = &usage.Routing{CredentialSource: credential}
	}
	event.Attempts = append(event.Attempts, attempt)
	event.ProviderID, event.UpstreamModel, event.Committed = &provider, &model, true
	payload, err := usage.Encode(event)
	if err != nil {
		t.Fatal(err)
	}
	result, err := usage.PersistEvent(t.Context(), pool, event, payload)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != usage.PersistOutcomePersisted {
		t.Fatalf("persist outcome %v", result.Outcome)
	}
	return event
}

func i64(v int64) *int64 { return &v }

func mustGunzip(t *testing.T, body []byte) []byte {
	t.Helper()
	r, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := io.Copy(&out, r); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// focusRowMaps re-decodes the day parts into header-keyed maps.
func focusRowMaps(t *testing.T, store *objectStore, day time.Time) []map[string]string {
	t.Helper()
	var rows []map[string]string
	for _, hit := range store.received() {
		if !strings.Contains(hit.path, fmt.Sprintf("charge_date=%s/revision=", day.Format("2006-01-02"))) ||
			!strings.HasSuffix(hit.path, ".csv.gz") {
			continue
		}
		data, err := csv.NewReader(bytes.NewReader(mustGunzip(t, hit.body))).ReadAll()
		if err != nil {
			t.Fatal(err)
		}
		for _, raw := range data[1:] {
			m := map[string]string{}
			for i, c := range data[0] {
				m[c] = raw[i]
			}
			rows = append(rows, m)
		}
	}
	return rows
}

func sumColumn(rows []map[string]string, column string) decimal.Decimal {
	total := decimal.Zero
	for _, row := range rows {
		if v := row[column]; v != "" {
			total = total.Add(decimal.RequireFromString(v))
		}
	}
	return total
}

func sumFacts(t *testing.T, pool *pgxpool.Pool, day time.Time) decimal.Decimal {
	t.Helper()
	var sum *string
	if err := pool.QueryRow(t.Context(), `SELECT trim_scale(SUM(f.estimated_cost))::text
		FROM olp.attempt_usage_facts f
		WHERE f.observed_at >= $1::date AND f.observed_at < ($1::date+1)::timestamptz
		AND f.charge_status='billable' AND f.usage_complete
		AND f.estimated_cost IS NOT NULL AND NOT f.caller_credential AND NOT f.budget_exempt`,
		day.Format("2006-01-02")).Scan(&sum); err != nil {
		t.Fatal(err)
	}
	if sum == nil {
		return decimal.Zero
	}
	return decimal.RequireFromString(*sum)
}

func TestIntegrationFocusExport(t *testing.T) {
	ctx := t.Context()
	pool := matrixPool(t)
	ring := matrixRing(t)
	const installation = "focus-installation"

	// D is a finished UTC day inside the retention window.
	now := time.Now().UTC()
	day := now.AddDate(0, 0, -3).Truncate(24 * time.Hour)
	f := seedFocusFixture(t, pool, day)

	observed := day.Add(10 * time.Hour)
	// Plain priced attempt: modality shares, a tool call, attribution.
	focusEvent(t, pool, f.keyA, f.plain, "focus-model", observed,
		&usage.AttemptUsage{Observed: true, Complete: true,
			InputTokens: i64(2000), AudioInputTokens: i64(300), OutputTokens: i64(500),
			ToolCalls: map[string]int64{"web_search": 2}},
		"", map[string]string{"team": "core"})
	// Discounted provider: same model, half multiplier.
	focusEvent(t, pool, f.keyA, f.discount, "focus-model", observed.Add(time.Minute),
		&usage.AttemptUsage{Observed: true, Complete: true, InputTokens: i64(1000)}, "", nil)
	// Committed provider: two attempts each weighing 2 → each allocates 5 of 10.
	for i := range 2 {
		focusEvent(t, pool, f.keyA, f.committed, "focus-committed", observed.Add(time.Duration(i)*time.Minute),
			&usage.AttemptUsage{Observed: true, Complete: true, InputTokens: i64(1000)}, "", nil)
	}
	// Caller-paid attempt on project A: excluded from the cost export, included
	// in project A's charge export.
	focusEvent(t, pool, f.keyCaller, f.plain, "focus-model", observed.Add(2*time.Minute),
		&usage.AttemptUsage{Observed: true, Complete: true, InputTokens: i64(1000)}, "caller", nil)
	// Unpriced attempt on project B (no price row for the model).
	focusEvent(t, pool, f.keyB, f.plain, "focus-unpriced", observed.Add(3*time.Minute),
		&usage.AttemptUsage{Observed: true, Complete: true, InputTokens: i64(1000)}, "", nil)
	// Explicit-price attempt on project B (card B sets 7/M).
	focusEvent(t, pool, f.keyB, f.plain, "focus-model", observed.Add(4*time.Minute),
		&usage.AttemptUsage{Observed: true, Complete: true, InputTokens: i64(1000)}, "", nil)

	// A committed fact whose cost_lines the generator resolves by fallback.
	focusEvent(t, pool, f.keyA, f.plain, "focus-model", observed.Add(5*time.Minute),
		&usage.AttemptUsage{Observed: true, Complete: true, InputTokens: i64(1000), OutputTokens: i64(100)}, "", nil)
	fallbackEvent := focusEvent(t, pool, f.keyA, f.plain, "focus-tiered", observed.Add(6*time.Minute),
		&usage.AttemptUsage{Observed: true, Complete: true, InputTokens: i64(3000)}, "", nil)
	_ = fallbackEvent
	// Force one fact's cost_lines NULL so the fallback line is exercised.
	if _, err := pool.Exec(ctx, `UPDATE olp.attempt_usage_facts SET cost_lines=NULL
		WHERE attempt_id=(SELECT a.id FROM olp.attempts a JOIN olp.attempt_usage_facts f ON f.attempt_id=a.id
			WHERE f.upstream_model='focus-model' AND f.estimated_cost IS NOT NULL LIMIT 1)`); err != nil {
		t.Fatal(err)
	}

	costStore := &objectStore{}
	costServer := httptest.NewServer(costStore.handler())
	t.Cleanup(costServer.Close)
	chargeStore := &objectStore{}
	chargeServer := httptest.NewServer(chargeStore.handler())
	t.Cleanup(chargeServer.Close)

	// The installation cost-basis sink and a project charge-basis sink.
	// S3 destinations need a static credential; seal one per sink.
	sealCredential := func() *string {
		credential, _ := json.Marshal(map[string]any{"auth_mode": "static", "region": "us-east-1",
			"secret": map[string]any{"access_key_id": "AKIAIOSFODNN7EXAMPLE", "secret_access_key": "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"}})
		secretID := uuid.NewString()
		sealed, err := ring.Seal(installation, secrets.SinkCredential, secretID, credential)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO olp.secrets(id,purpose,key_version,ciphertext)
			VALUES($1,'sink_credential',1,$2)`, secretID, sealed); err != nil {
			t.Fatal(err)
		}
		return &secretID
	}
	costSink := insertSink(t, pool, "focus-cost", "s3", "csv_gzip", costServer.URL+"/bucket",
		[]string{"focus"}, fmt.Sprintf(`{"cost_basis":"cost","start_date":%q}`, day.Format("2006-01-02")), sealCredential(), nil)
	chargeSink := insertSink(t, pool, "focus-charge", "s3", "csv_gzip", chargeServer.URL+"/bucket",
		[]string{"focus"}, fmt.Sprintf(`{"cost_basis":"charge","start_date":%q}`, day.Format("2006-01-02")), sealCredential(), &f.projectA)

	sender := NewSender(matrixPolicy())
	workerNow := day.Add(27 * time.Hour) // the day is well past D+1d+2h
	worker := &FocusWorker{Pool: pool, Keys: ring, Installation: installation, Sender: sender,
		Log: slog.New(slog.DiscardHandler), Now: func() time.Time { return workerNow }}
	progress, err := worker.Pass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !progress {
		t.Fatal("pass reported no progress")
	}

	// Collect each sink's hits by the day directory.
	hits := append(costStore.received(), chargeStore.received()...)
	if len(hits) == 0 {
		rows, _ := pool.Query(ctx, `SELECT sink_id::text,charge_date::text,attempts,last_error_code,next_attempt_at::text,delivered_at::text,revision FROM olp.focus_datasets`)
		for rows.Next() {
			var sID, d, n, del string
			var a, rev int
			var e *string
			if err := rows.Scan(&sID, &d, &a, &e, &n, &del, &rev); err != nil {
				t.Logf("scan: %v", err)
			}
			var ev string
			if e != nil {
				ev = *e
			}
			t.Logf("ledger: sink=%s day=%s attempts=%d err=%s next=%s delivered=%s rev=%d", sID, d, a, ev, n, del, rev)
		}
		rows.Close()
		t.Fatal("no objects uploaded")
	}
	// Part keys identify the sink only by prefix; both sinks share the fake
	// bucket, so split hits by manifest sink_id.
	manifests := map[string]map[string]any{}
	var partsByManifestSink = map[string][]objectHit{}
	_ = partsByManifestSink
	for _, hit := range hits {
		if strings.HasSuffix(hit.path, "manifest.json") {
			var doc map[string]any
			if err := json.Unmarshal(hit.body, &doc); err != nil {
				t.Fatalf("manifest json: %v", err)
			}
			manifests[doc["sink_id"].(string)] = doc
		}
	}
	if len(manifests) != 2 {
		t.Fatalf("manifests = %d, want 2", len(manifests))
	}

	// Assertion 1: every part gunzips and validates with zero violations.
	seenEvaluated := false
	for _, hit := range hits {
		if !strings.HasSuffix(hit.path, ".csv.gz") {
			continue
		}
		report, err := focusspec.Validate(bytes.NewReader(mustGunzip(t, hit.body)), FocusCriteria)
		if err != nil {
			t.Fatalf("validate %s: %v", hit.path, err)
		}
		t.Logf("part %s: evaluated=%d dynamic=%d not-applicable=%d violations=%d",
			hit.path, report.Evaluated, report.Dynamic, report.NotApplicable, len(report.Violations))
		if !report.OK() {
			for _, v := range report.Violations {
				t.Logf("violation %s", v)
			}
			t.Fatalf("part %s has FOCUS violations", hit.path)
		}
		seenEvaluated = true
	}
	if !seenEvaluated {
		t.Fatal("no csv parts uploaded")
	}

	costRows := focusRowMaps(t, costStore, day)
	chargeRows := focusRowMaps(t, chargeStore, day)

	// Assertion 2 (cost basis): billed = Σ estimated_cost of included facts.
	var usageRows []map[string]string
	for _, row := range costRows {
		if row["ChargeCategory"] == "Usage" {
			usageRows = append(usageRows, row)
		}
	}
	if len(usageRows) == 0 {
		t.Fatal("no usage rows")
	}
	var billed decimal.Decimal
	var committedEffective decimal.Decimal
	var callerSeen, attributionSeen, toolSeen, tierSeen, fallbackSeen bool
	for _, row := range usageRows {
		if row["x_Model"] == "focus-caller" {
			callerSeen = true
		}
		if row["x_PriceComponent"] == "tool_calls.web_search" && row["SkuMeter"] == "Tool Calls (web_search)" {
			toolSeen = true
		}
		if row["Tags"] != "" {
			var tags map[string]string
			if err := json.Unmarshal([]byte(row["Tags"]), &tags); err == nil && tags["team"] == "core" {
				attributionSeen = true
			}
		}
		if row["x_PriceTier"] != "" {
			tierSeen = true
		}
		if row["x_PriceComponent"] == "attempt" && row["SkuMeter"] == "Attempts" {
			fallbackSeen = true
		}
		if row["PricingCategory"] == "Committed" {
			if row["BilledCost"] != "0" {
				t.Fatalf("committed BilledCost %s", row["BilledCost"])
			}
			committedEffective = committedEffective.Add(decimal.RequireFromString(row["EffectiveCost"]))
		} else {
			billed = billed.Add(decimal.RequireFromString(row["BilledCost"]))
		}
	}
	if callerSeen {
		t.Fatal("caller-credential usage appeared in the cost-basis dataset")
	}
	if !toolSeen || !attributionSeen || !tierSeen || !fallbackSeen {
		t.Fatalf("missing shapes: tool=%v attribution=%v tier=%v fallback=%v", toolSeen, attributionSeen, tierSeen, fallbackSeen)
	}
	want := sumFacts(t, pool, day)
	// Non-committed billed = committed-adjusted facts: subtract committed facts' cost.
	var committedCost *string
	if err := pool.QueryRow(ctx, `SELECT trim_scale(SUM(f.estimated_cost))::text FROM olp.attempt_usage_facts f
		WHERE f.observed_at >= $1::date AND f.observed_at < ($1::date+1)::timestamptz
		AND f.charge_status='billable' AND f.usage_complete AND f.commitment_period IS NOT NULL`,
		day.Format("2006-01-02")).Scan(&committedCost); err != nil {
		t.Fatal(err)
	}
	if committedCost != nil {
		want = want.Sub(decimal.RequireFromString(*committedCost))
	}
	// Committed facts store estimated_cost=0; their cost carries through the
	// fallback columns instead. Assert the plain-fact total.
	var nonCommitted *string
	if err := pool.QueryRow(ctx, `SELECT trim_scale(SUM(f.estimated_cost))::text FROM olp.attempt_usage_facts f
		WHERE f.observed_at >= $1::date AND f.observed_at < ($1::date+1)::timestamptz
		AND f.charge_status='billable' AND f.usage_complete
		AND f.estimated_cost IS NOT NULL AND NOT f.caller_credential AND NOT f.budget_exempt
		AND f.commitment_period IS NULL`,
		day.Format("2006-01-02")).Scan(&nonCommitted); err != nil {
		t.Fatal(err)
	}
	if nonCommitted == nil || !billed.Equal(decimal.RequireFromString(*nonCommitted)) {
		t.Fatalf("Σ BilledCost = %s, want Σ estimated_cost %v", billed, nonCommitted)
	}
	if !committedEffective.Equal(decimal.RequireFromString("10")) {
		t.Fatalf("Σ committed EffectiveCost = %s, want 10", committedEffective)
	}

	// Purchase row on the bucket start date.
	var purchase bool
	for _, row := range costRows {
		if row["ChargeCategory"] == "Purchase" && row["SkuId"] == "olp/commitment/day" {
			purchase = true
			if row["BilledCost"] != "10" || row["EffectiveCost"] != "0" {
				t.Fatalf("purchase amounts: %v", row)
			}
		}
	}
	if !purchase {
		t.Fatal("no Purchase row for the committed bucket")
	}

	// Assertion 3 (charge basis): project A only, billed = Σ charge.
	var chargeBilled decimal.Decimal
	var chargeCaller bool
	for _, row := range chargeRows {
		if row["ChargeCategory"] != "Usage" {
			continue
		}
		chargeBilled = chargeBilled.Add(decimal.RequireFromString(row["BilledCost"]))
		if row["SubAccountId"] != f.projectA {
			t.Fatalf("charge row outside project A: %v", row["SubAccountId"])
		}
		if row["x_ApiKeyId"] == f.keyCaller {
			chargeCaller = true
		}
	}
	if !chargeCaller {
		t.Fatal("caller-credential attempt missing from the charge-basis dataset")
	}
	var chargeSum *string
	if err := pool.QueryRow(ctx, `SELECT trim_scale(SUM(f.charge))::text FROM olp.attempt_usage_facts f
		LEFT JOIN olp.api_keys k ON k.id=f.api_key_id
		LEFT JOIN olp.providers pr ON pr.id=f.provider_id
		LEFT JOIN olp.routes rt ON rt.slug=f.route_slug
		WHERE f.observed_at >= $1::date AND f.observed_at < ($1::date+1)::timestamptz
		AND f.charge_status='billable' AND f.usage_complete AND f.charge IS NOT NULL
		AND COALESCE(k.project_id, pr.project_id, rt.project_id)=$2::uuid`,
		day.Format("2006-01-02"), f.projectA).Scan(&chargeSum); err != nil {
		t.Fatal(err)
	}
	if chargeSum == nil || !chargeBilled.Equal(decimal.RequireFromString(*chargeSum)) {
		t.Fatalf("charge Σ BilledCost = %s, want %v", chargeBilled, chargeSum)
	}

	// Ledger rows and manifest totals.
	for _, sink := range []string{costSink, chargeSink} {
		var revision, rowCount, partCount int64
		var lastErr *string
		var delivered *time.Time
		if err := pool.QueryRow(ctx, `SELECT revision, row_count, part_count, delivered_at, last_error_code
			FROM olp.focus_datasets WHERE sink_id=$1::uuid AND charge_date=$2::date`,
			sink, day.Format("2006-01-02")).Scan(&revision, &rowCount, &partCount, &delivered, &lastErr); err != nil {
			t.Fatalf("ledger %s: %v", sink, err)
		}
		if revision != 1 || delivered == nil || lastErr != nil {
			t.Fatalf("ledger for %s: revision=%d delivered=%v err=%v", sink, revision, delivered, lastErr)
		}
		doc := manifests[sink]
		if doc == nil {
			t.Fatalf("no manifest for %s", sink)
		}
		if doc["revision"] != float64(1) || doc["charge_date"] != day.Format("2006-01-02") {
			t.Fatalf("manifest %s: %v", sink, doc)
		}
		parts := doc["parts"].([]any)
		if int64(len(parts)) != partCount {
			t.Fatalf("manifest %s part_count %v != %d", sink, partCount, len(parts))
		}
		var manifestRows int64
		for _, p := range parts {
			manifestRows += int64(p.(map[string]any)["rows"].(float64))
		}
		if manifestRows != rowCount {
			t.Fatalf("manifest %s row total %d != %d", sink, manifestRows, rowCount)
		}
	}
	// Completeness: the cost basis counts the priced caller attempt it
	// excluded; the charge basis reports 0 (it bills callers).
	costDoc := manifests[costSink]["completeness"].(map[string]any)
	if costDoc["caller_paid_excluded_attempts"] != float64(1) {
		t.Fatalf("cost completeness %v", costDoc)
	}
	chargeDoc := manifests[chargeSink]["completeness"].(map[string]any)
	if chargeDoc["caller_paid_excluded_attempts"] != float64(0) {
		t.Fatalf("charge completeness %v", chargeDoc)
	}
	if costDoc["fallback_line_attempts"].(float64) < 1 || costDoc["unpriced_attempts"].(float64) < 1 {
		t.Fatalf("cost completeness %v", costDoc)
	}
}

// flakyStore fails the first PUT for manifest.json, then passes every request;
// it keeps the hit log the objectStore does.
type flakyStore struct {
	objectStore
	failed bool
}

func (f *flakyStore) handler() func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		if !f.failed && strings.HasSuffix(r.URL.Path, "manifest.json") {
			f.failed = true
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		f.objectStore.handler()(w, r)
	}
}

// TestIntegrationFocusLifecycle covers restatement, manifest-failure retry,
// empty days, the retention floor, and the datasets API/status.
func TestIntegrationFocusLifecycle(t *testing.T) {
	ctx := t.Context()
	pool := matrixPool(t)
	ring := matrixRing(t)
	const installation = "focus-life"

	now := time.Now().UTC()
	day := now.AddDate(0, 0, -3).Truncate(24 * time.Hour)
	f := seedFocusFixture(t, pool, day)
	observed := day.Add(10 * time.Hour)
	focusEvent(t, pool, f.keyA, f.plain, "focus-model", observed,
		&usage.AttemptUsage{Observed: true, Complete: true, InputTokens: i64(1000)}, "", nil)

	store := &flakyStore{}
	server := httptest.NewServer(http.HandlerFunc(store.handler()))
	t.Cleanup(server.Close)
	sink := insertSink(t, pool, "focus-life", "s3", "csv_gzip", server.URL+"/bucket",
		[]string{"focus"}, fmt.Sprintf(`{"cost_basis":"cost","start_date":%q}`, day.Format("2006-01-02")), sealTestCredential(t, pool, ring, installation), nil)

	workerNow := day.Add(27 * time.Hour)
	worker := &FocusWorker{Pool: pool, Keys: ring, Installation: installation, Sender: NewSender(matrixPolicy()),
		Log: slog.New(slog.DiscardHandler), Now: func() time.Time { return workerNow }}

	// First pass: the manifest PUT fails → the day stays undelivered, parts are
	// still written, and the error lands on the ledger.
	if _, err := worker.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	var attempts int
	var code *string
	var delivered *time.Time
	if err := pool.QueryRow(ctx, `SELECT attempts,last_error_code,delivered_at FROM olp.focus_datasets
		WHERE sink_id=$1::uuid AND charge_date=$2::date`, sink, day.Format("2006-01-02")).Scan(&attempts, &code, &delivered); err != nil {
		t.Fatal(err)
	}
	if delivered != nil || code == nil || *code != "status" {
		t.Fatalf("after failed manifest: attempts=%d code=%v delivered=%v", attempts, code, delivered)
	}
	var firstKeys []string
	for _, hit := range store.received() {
		firstKeys = append(firstKeys, hit.path)
	}
	if len(firstKeys) != 1 || !strings.HasSuffix(firstKeys[0], "revision=0001/part-00000.csv.gz") {
		t.Fatalf("first attempt uploads: %v", firstKeys)
	}

	// Retry: the same keys are rewritten, the manifest lands last, revision 1.
	if _, err := pool.Exec(ctx, `UPDATE olp.focus_datasets SET next_attempt_at=now() WHERE sink_id=$1::uuid`, sink); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	hits := store.received()
	var lastKey string
	var rev1Keys []string
	for _, hit := range hits {
		if strings.Contains(hit.path, "revision=0001/") {
			rev1Keys = append(rev1Keys, hit.path)
		}
		lastKey = hit.path
	}
	// The first attempt's part PUT plus the retry's = the same key twice, and
	// the manifest PUT is last.
	if len(rev1Keys) != 2 || rev1Keys[0] != rev1Keys[1] || rev1Keys[0] != firstKeys[0] {
		t.Fatalf("retry uploads: keys=%v want %s twice", rev1Keys, firstKeys[0])
	}
	if !strings.HasSuffix(lastKey, "manifest.json") {
		t.Fatalf("manifest was not the last PUT: %s", lastKey)
	}
	var revision int64
	if err := pool.QueryRow(ctx, `SELECT revision FROM olp.focus_datasets
		WHERE sink_id=$1::uuid AND charge_date=$2::date`, sink, day.Format("2006-01-02")).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if revision != 1 {
		t.Fatalf("revision = %d, want 1 (no gap after a failed manifest)", revision)
	}

	// Restatement: a late fact bumps the dirty version but does not regenerate
	// before the delivered row is an hour old.
	focusEvent(t, pool, f.keyA, f.plain, "focus-model", observed.Add(10*time.Minute),
		&usage.AttemptUsage{Observed: true, Complete: true, InputTokens: i64(4000)}, "", nil)
	if _, err := worker.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	if revisionNow(t, pool, sink, day) != 1 {
		t.Fatal("restated before the one-hour window")
	}
	if _, err := pool.Exec(ctx, `UPDATE olp.focus_datasets SET delivered_at=now()-interval '61 minutes',
		next_attempt_at=now() WHERE sink_id=$1::uuid`, sink); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	if revisionNow(t, pool, sink, day) != 2 {
		t.Fatalf("revision = %d, want 2", revisionNow(t, pool, sink, day))
	}
	var rev2 bool
	for _, hit := range store.received() {
		if strings.Contains(hit.path, "revision=0002/part-00000.csv.gz") {
			rev2 = true
		}
	}
	if !rev2 {
		t.Fatal("no revision=0002 part uploaded")
	}
	// The restated day totals include the late fact: the manifest's accounting
	// total equals the full fact sum.
	var manifestHit *objectHit
	for i, hit := range store.received() {
		if strings.HasSuffix(hit.path, fmt.Sprintf("charge_date=%s/manifest.json", day.Format("2006-01-02"))) &&
			strings.Contains(hit.path, "revision=0002") || strings.HasSuffix(hit.path, "manifest.json") {
			manifestHit = &store.received()[i]
		}
	}
	if manifestHit == nil {
		t.Fatal("no manifest after restatement")
	}
	var doc map[string]any
	if err := json.Unmarshal(manifestHit.body, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["revision"] != float64(2) {
		t.Fatalf("manifest revision %v", doc["revision"])
	}
	var fullSum *string
	if err := pool.QueryRow(ctx, `SELECT trim_scale(SUM(f.estimated_cost))::text FROM olp.attempt_usage_facts f
		WHERE f.observed_at >= $1::date AND f.observed_at < ($1::date+1)::timestamptz
		AND f.charge_status='billable' AND f.usage_complete AND f.estimated_cost IS NOT NULL
		AND NOT f.caller_credential AND NOT f.budget_exempt`, day.Format("2006-01-02")).Scan(&fullSum); err != nil {
		t.Fatal(err)
	}
	accounting := doc["accounting_totals"].(map[string]any)
	if accounting["cost"] != *fullSum {
		t.Fatalf("manifest accounting cost %v, want %s", accounting["cost"], *fullSum)
	}

	// Empty day: D+1 inside the window generates a header-only part.
	emptyDay := day.AddDate(0, 0, 1)
	workerNow = emptyDay.Add(27 * time.Hour)
	if _, err := worker.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	var emptyRows []string
	for _, hit := range store.received() {
		if strings.Contains(hit.path, "charge_date="+emptyDay.Format("2006-01-02")) && strings.HasSuffix(hit.path, ".csv.gz") {
			data, err := csv.NewReader(bytes.NewReader(mustGunzip(t, hit.body))).ReadAll()
			if err != nil {
				t.Fatal(err)
			}
			emptyRows = append(emptyRows, fmt.Sprint(len(data)))
			if len(data) != 1 {
				t.Fatalf("empty day has %d rows", len(data))
			}
		}
	}
	if len(emptyRows) == 0 {
		t.Fatal("empty day produced no part")
	}

	// Days before the retention-safe floor never generate: dirty one manually
	// and confirm the pass deletes the marker without a dataset row.
	if _, err := pool.Exec(ctx, `INSERT INTO olp.focus_dirty_days(day,version) VALUES($1::date,1)`,
		now.AddDate(0, 0, -400).Format("2006-01-02")); err != nil {
		t.Fatal(err)
	}
	workerNow = now
	if _, err := worker.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	var staleDirty, staleDataset int64
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM olp.focus_dirty_days WHERE day < $1::date),
		(SELECT count(*) FROM olp.focus_datasets WHERE sink_id=$2::uuid AND charge_date < $1::date)`,
		now.AddDate(0, 0, -89).Format("2006-01-02"), sink).Scan(&staleDirty, &staleDataset); err != nil {
		t.Fatal(err)
	}
	if staleDirty != 0 || staleDataset != 0 {
		t.Fatalf("pre-retention state: dirty=%d datasets=%d", staleDirty, staleDataset)
	}

	// The datasets endpoint and the sink status reflect the deliveries.
	rows, err := pool.Query(ctx, focusDatasetsSQL, sink, emptyDay.AddDate(0, 0, 1).Format("2006-01-02"), 51)
	if err != nil {
		t.Fatal(err)
	}
	items, err := access.JSONRows(rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("focus-datasets items: %v", items)
	}
	// Newest charge_date first: the empty day precedes the fact day.
	if items[0]["charge_date"] != emptyDay.Format("2006-01-02") ||
		items[1]["charge_date"] != day.Format("2006-01-02") {
		t.Fatalf("ordering: %v %v", items[0]["charge_date"], items[1]["charge_date"])
	}
	if fmt.Sprint(items[1]["revision"]) != "2" {
		t.Fatalf("item revision: %v", items[1])
	}
	if items[1]["current"] != true || items[0]["current"] != true {
		t.Fatalf("item current flags: %v %v", items[0], items[1])
	}
	mgmt := &Management{Pool: pool}
	status, err := mgmt.sinkStatus(httptest.NewRequest("GET", "/", nil), pool, sink)
	if err != nil {
		t.Fatal(err)
	}
	var focusStatus map[string]any
	for _, st := range status {
		if st["stream"] == "focus" {
			focusStatus = st
		}
	}
	if focusStatus == nil {
		t.Fatalf("no focus stream status: %v", status)
	}
	// Every later empty day is eligible too once the clock is past 02:00
	// UTC, so assert the minimum the test controls rather than the exact
	// delivered-days count.
	deliveredDays, _ := strconv.Atoi(fmt.Sprint(focusStatus["delivered_days"]))
	if deliveredDays < 2 ||
		fmt.Sprint(focusStatus["latest_delivered_date"]) < emptyDay.Format("2006-01-02") {
		t.Fatalf("focus status: %v", focusStatus)
	}

	// Claim ABA: attempts is a monotonic counter, so a finisher holding an
	// older attempts value cannot overwrite a newer claim or delivery.
	newClaim, err := worker.claimDay(ctx, sink, day)
	if err != nil {
		t.Fatal(err)
	}
	staleResult := &dayResult{dirtyVersion: 999, rowCount: 999, partCount: 9,
		billed: decimal.RequireFromString("999"), effective: decimal.RequireFromString("999")}
	if err := worker.finishDay(ctx, focusSink{Destination: Destination{ID: sink}}, day, newClaim-1, staleResult, nil); err != nil {
		t.Fatal(err)
	}
	var finalRevision, dirtyVersion, rowCount int64
	if err := pool.QueryRow(ctx, `SELECT revision,COALESCE(dirty_version,0),row_count FROM olp.focus_datasets
		WHERE sink_id=$1::uuid AND charge_date=$2::date`, sink, day.Format("2006-01-02")).Scan(&finalRevision, &dirtyVersion, &rowCount); err != nil {
		t.Fatal(err)
	}
	if finalRevision != 2 || dirtyVersion == 999 || rowCount == 999 {
		t.Fatalf("stale finisher overwrote the row: rev=%d dirty=%d rows=%d", finalRevision, dirtyVersion, rowCount)
	}
}

func revisionNow(t *testing.T, pool *pgxpool.Pool, sink string, day time.Time) int64 {
	t.Helper()
	var revision int64
	if err := pool.QueryRow(t.Context(), `SELECT revision FROM olp.focus_datasets
		WHERE sink_id=$1::uuid AND charge_date=$2::date`, sink, day.Format("2006-01-02")).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	return revision
}

func sealTestCredential(t *testing.T, pool *pgxpool.Pool, ring *secrets.KeyRing, installation string) *string {
	t.Helper()
	credential, _ := json.Marshal(map[string]any{"auth_mode": "static", "region": "us-east-1",
		"secret": map[string]any{"access_key_id": "AKIAIOSFODNN7EXAMPLE", "secret_access_key": "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"}})
	secretID := uuid.NewString()
	sealed, err := ring.Seal(installation, secrets.SinkCredential, secretID, credential)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.secrets(id,purpose,key_version,ciphertext)
		VALUES($1,'sink_credential',1,$2)`, secretID, sealed); err != nil {
		t.Fatal(err)
	}
	return &secretID
}
