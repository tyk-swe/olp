//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/management"
	"github.com/tyk-swe/olp/internal/usage"
)

// repVendorKind stands in for the provider catalogue until the process wires the
// real one: pricing only needs to know which connector a vendor belongs to.
func repVendorKind(vendor string) (string, bool) {
	kinds := map[string]string{"openai": "openai", "anthropic": "anthropic", "acme": "openai_compatible"}
	kind, ok := kinds[vendor]
	return kind, ok
}

// repHarness rebuilds the harness HTTP surface with the accounting server
// mounted alongside access, so console sessions authorise usage reads exactly as
// they will in the process.
func repHarness(t *testing.T) *accessHarness {
	t.Helper()
	h := newAccessHarness(t)
	mux := http.NewServeMux()
	management.Register(mux)
	h.Server.Register(mux)
	(&usage.Server{Access: h.Server, VendorKind: repVendorKind}).Register(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	h.HTTP = server
	return h
}

// repFixture owns the identifiers every accounting fixture hangs off.
type repFixture struct {
	t       *testing.T
	h       *accessHarness
	pool    *pgxpool.Pool
	Owner   *browser
	OwnerID string
	Key     string
	P1      string
	P2      string
	Base    time.Time
}

func repSetup(t *testing.T) *repFixture {
	t.Helper()
	h := repHarness(t)
	f := &repFixture{t: t, h: h, pool: h.Pool, Owner: h.owner()}
	f.Base = time.Now().UTC().Truncate(time.Hour).Add(-6 * time.Hour)
	if err := f.pool.QueryRow(context.Background(),
		"SELECT id::text FROM olp.users ORDER BY created_at LIMIT 1").Scan(&f.OwnerID); err != nil {
		t.Fatalf("read owner: %v", err)
	}
	f.P1 = f.provider("primary", "openai")
	f.P2 = f.provider("secondary", "anthropic")
	f.Key = f.apiKey("reports")
	return f
}

func (f *repFixture) exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(context.Background(), query, args...); err != nil {
		f.t.Fatalf("seed %s: %v", strings.SplitN(strings.TrimSpace(query), " ", 4)[2], err)
	}
}

func (f *repFixture) provider(name, kind string) string {
	id := access.NewID()
	f.exec(`INSERT INTO olp.providers (id, name, kind, state, configuration, etag, slots_etag, created_by)
	    VALUES ($1, $2, $3, 'active', '{}'::jsonb, $4, $5, $6)`,
		id, name, kind, access.NewID(), access.NewID(), f.OwnerID)
	return id
}

func (f *repFixture) apiKey(name string) string {
	id := access.NewID()
	digest := make([]byte, 32)
	copy(digest, name)
	f.exec(`INSERT INTO olp.api_keys (id, lookup_id, digest, name, created_by, policy, etag)
	    VALUES ($1, $2, $3, $4, $5, '{}'::jsonb, $6)`,
		id, access.NewID(), digest, name, f.OwnerID, access.NewID())
	return id
}

// repRequest is one seeded gateway request.
type repRequest struct {
	ID           string
	StartedAt    time.Time
	Route        string
	Operation    string
	Surface      string
	StatusCode   *int
	ErrorClass   *string
	AttemptCount int
	Latency      *int
	FirstByte    *int
}

func (f *repFixture) request(r repRequest) {
	completed := r.StartedAt.Add(time.Second)
	f.exec(`INSERT INTO olp.requests (id, runtime_generation_id, api_key_id, route_slug, operation,
	        surface, started_at, completed_at, status_code, error_class, total_latency_ms,
	        first_byte_ms, attempt_count)
	    VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		r.ID, access.NewID(), f.Key, r.Route, r.Operation, r.Surface, r.StartedAt, completed,
		r.StatusCode, r.ErrorClass, r.Latency, r.FirstByte, r.AttemptCount)
}

// repAttempt is one provider attempt of a seeded request.
type repAttempt struct {
	RequestID  string
	StartedAt  time.Time
	Ordinal    int
	ProviderID string
	Model      string
	Committed  bool
	StatusCode *int
	ErrorClass *string
	Routing    *string
}

func (f *repFixture) attempt(a repAttempt) {
	f.exec(`INSERT INTO olp.attempts (id, request_id, request_started_at, ordinal, provider_id,
	        upstream_model, started_at, completed_at, status_code, error_class, committed, latency_ms,
	        first_byte_ms, routing)
	    VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, 120, 40, $12::jsonb)`,
		access.NewID(), a.RequestID, a.StartedAt, a.Ordinal, a.ProviderID, a.Model, a.StartedAt,
		a.StartedAt.Add(120*time.Millisecond), a.StatusCode, a.ErrorClass, a.Committed, a.Routing)
}

func (f *repFixture) anchor(requestID string, startedAt time.Time) {
	f.exec(`INSERT INTO olp.usage_request_anchors (request_id, request_started_at)
	    VALUES ($1, $2) ON CONFLICT DO NOTHING`, requestID, startedAt)
}

// repFact is one attempt usage fact. The counted flags are stored exactly as the
// ingestion writer computes them, so a report never has to recompute them.
type repFact struct {
	RequestID          string
	StartedAt          time.Time
	Ordinal            int
	ObservedAt         time.Time
	Route              string
	ProviderID         string
	Model              string
	Operation          string
	Surface            string
	Charge             string
	Observed           bool
	Complete           bool
	Input              *int64
	Output             *int64
	Cached             *int64
	Media              *string
	Cost               *string
	Unpriced           bool
	Currency           *string
	RevisionID         *string
	CountRequest       bool
	CountProvider      bool
	CountModel         bool
	CountTarget        bool
	UnpricedRequest    bool
	UnpricedProvider   bool
	UnpricedModel      bool
	UnpricedTarget     bool
	IncompleteRequest  bool
	IncompleteProvider bool
	IncompleteModel    bool
	IncompleteTarget   bool
	// Estimate is what admission estimated this attempt's input to be, with
	// the provenance and family it came with; unset is an attempt that was
	// never estimated.
	Estimate   *int64
	Provenance string
	Family     string
}

func (f *repFixture) fact(v repFact) {
	f.anchor(v.RequestID, v.StartedAt)
	f.exec(`INSERT INTO olp.attempt_usage_facts (attempt_id, event_id, request_id, request_started_at,
	        attempt_ordinal, api_key_id, provider_id, route_slug, upstream_model, operation, surface,
	        observed_at, charge_status, usage_observed, usage_complete, input_tokens, output_tokens,
	        cached_input_tokens, media_units, estimated_cost, unpriced, pricing_revision_id, currency,
	        request_counted, provider_request_counted, model_request_counted, target_request_counted,
	        request_unpriced_counted, provider_unpriced_counted, model_unpriced_counted,
	        target_unpriced_counted, request_incomplete_counted, provider_incomplete_counted,
	        model_incomplete_counted, target_incomplete_counted, estimated_input_tokens,
	        estimate_provenance, model_family)
	    VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18,
	        $19::text::numeric, $20::text::numeric, $21, $22, $23, $24, $25, $26, $27, $28, $29, $30,
	        $31, $32, $33, $34, $35, $36, $37, $38)`,
		access.NewID(), access.NewID(), v.RequestID, v.StartedAt, v.Ordinal, f.Key, v.ProviderID,
		v.Route, v.Model, v.Operation, v.Surface, v.ObservedAt, v.Charge, v.Observed, v.Complete,
		v.Input, v.Output, v.Cached, v.Media, v.Cost, v.Unpriced, v.RevisionID, v.Currency,
		v.CountRequest, v.CountProvider, v.CountModel, v.CountTarget,
		v.UnpricedRequest, v.UnpricedProvider, v.UnpricedModel, v.UnpricedTarget,
		v.IncompleteRequest, v.IncompleteProvider, v.IncompleteModel, v.IncompleteTarget,
		v.Estimate, v.Provenance, v.Family)
}

// repHourly is one retained rollup bucket.
type repHourly struct {
	Bucket             time.Time
	Route              string
	ProviderID         string
	Model              string
	Operation          string
	Surface            string
	APIKey             *string
	Requests           int64
	ProviderRequests   int64
	ModelRequests      int64
	TargetRequests     int64
	Input              int64
	Output             int64
	Cached             int64
	Media              string
	Cost               *string
	UnpricedRequests   int64
	UnpricedProvider   int64
	UnpricedModel      int64
	UnpricedTarget     int64
	IncompleteRequests int64
	IncompleteProvider int64
	IncompleteModel    int64
	IncompleteTarget   int64
	Currency           *string
	UnpricedAttempts   int64
	// The estimate dimensions of the bucket, and the sums over the attempts
	// that had both an estimate and reported input.
	Family            string
	Provenance        string
	Estimated         int64
	EstimateReported  int64
	EstimatedAttempts int64
}

func (f *repFixture) hourly(v repHourly) {
	f.exec(`INSERT INTO olp.attempt_usage_hourly (bucket, route_slug, provider_id, upstream_model,
	        operation, surface, api_key_id, request_count, provider_request_count, model_request_count,
	        target_request_count, input_tokens, output_tokens, cached_input_tokens, media_units,
	        estimated_cost, request_unpriced_count, provider_unpriced_count, model_unpriced_count,
	        target_unpriced_count, request_incomplete_count, provider_incomplete_count,
	        model_incomplete_count, target_incomplete_count, currency, unpriced_attempt_count,
	        model_family, estimate_provenance, estimated_input_tokens,
	        estimate_reported_input_tokens, estimate_attempt_count)
	    VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15::text::numeric,
	        $16::text::numeric, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28, $29, $30,
	        $31)`,
		v.Bucket, v.Route, v.ProviderID, v.Model, v.Operation, v.Surface, v.APIKey, v.Requests,
		v.ProviderRequests, v.ModelRequests, v.TargetRequests, v.Input, v.Output, v.Cached, v.Media,
		v.Cost, v.UnpricedRequests, v.UnpricedProvider, v.UnpricedModel, v.UnpricedTarget,
		v.IncompleteRequests, v.IncompleteProvider, v.IncompleteModel, v.IncompleteTarget,
		v.Currency, v.UnpricedAttempts, v.Family, v.Provenance, v.Estimated,
		v.EstimateReported, v.EstimatedAttempts)
}

// repProblemCode reads the code out of an RFC 9457 problem document, where it
// is the last segment of the type URI.
func repProblemCode(t *testing.T, body map[string]any) string {
	t.Helper()
	kind, ok := body["type"].(string)
	if !ok {
		t.Fatalf("response is not a problem document: %v", body)
	}
	return kind[strings.LastIndex(kind, "/")+1:]
}

// The whole bucket inside the range plus both live facts; the half bucket the
// range starts in is left out rather than counted in part.

// A row without a key is the installation's own, reported as system.

// count answers a single-value COUNT query against the fixture database.
func (f *repFixture) count(query string, args ...any) int64 {
	f.t.Helper()
	var total int64
	if err := f.pool.QueryRow(context.Background(), query, args...).Scan(&total); err != nil {
		f.t.Fatalf("count: %v", err)
	}
	return total
}

func repPrice(kind, model, operation string) map[string]any {
	return map[string]any{"provider_kind": kind, "model": model, "operation": operation,
		"currency": "USD", "input_per_million": "1.500000", "output_per_million": "3.000000"}
}

func TestPricingRevisionsOverHTTP(t *testing.T) {
	f := repSetup(t)
	generic := repPrice("openai_compatible", "m2", "embeddings")
	delete(generic, "input_per_million")
	delete(generic, "output_per_million")
	generic["unit_price"] = "0.000200000000"
	vendor := repPrice("openai_compatible", "m2", "embeddings")
	vendor["vendor_id"] = "acme"
	vendor["currency"] = "usd"
	delete(vendor, "input_per_million")
	delete(vendor, "output_per_million")
	vendor["unit_price"] = "0.000100000000"
	body := map[string]any{
		"effective_at": f.Base.Format(time.RFC3339),
		// Insert the vendor override first so reads must impose a stable scope order.
		"prices": []any{repPrice("openai", "m1", "generation"), vendor, generic},
	}
	headers := map[string]string{"Idempotency-Key": "pricing-first"}
	created := f.h.want(f.Owner, http.MethodPost, "/api/v1/pricing/revisions", body, headers, 201)
	if created["revision"].(float64) != 1 {
		t.Fatalf("revision = %v, want 1", created["revision"])
	}
	if created["created_by"].(string) != f.OwnerID {
		t.Fatalf("created by = %v, want the owner", created["created_by"])
	}
	prices := created["prices"].([]any)
	if len(prices) != 3 {
		t.Fatalf("prices = %v", prices)
	}
	for _, entry := range prices {
		if entry.(map[string]any)["currency"].(string) != "USD" {
			t.Fatalf("currency was not normalised: %v", entry)
		}
	}

	replayed := f.h.want(f.Owner, http.MethodPost, "/api/v1/pricing/revisions", body, headers, 201)
	if replayed["id"].(string) != created["id"].(string) {
		t.Fatal("a replayed idempotency key created a second revision")
	}
	if total := f.count("SELECT count(*) FROM olp.pricing_revisions"); total != 1 {
		t.Fatalf("revisions = %d, want 1", total)
	}
	problem := f.h.want(f.Owner, http.MethodPost, "/api/v1/pricing/revisions", body, nil, 400)
	if code := repProblemCode(t, problem); code != "idempotency_key_required" {
		t.Fatalf("missing key gave %s", code)
	}

	listed := f.h.want(f.Owner, http.MethodGet, "/api/v1/pricing/revisions", nil, nil, 200)
	items := listed["items"].([]any)
	if len(items) != 1 || len(items[0].(map[string]any)["prices"].([]any)) != 3 {
		t.Fatalf("listed revisions = %v", items)
	}
	listedPrices := items[0].(map[string]any)["prices"].([]any)
	if first, second := listedPrices[1].(map[string]any), listedPrices[2].(map[string]any); first["vendor_id"] != nil || second["vendor_id"] != "acme" {
		t.Fatalf("scope order = %v, want generic before vendor-specific", listedPrices)
	}
	if listed["next_cursor"] != nil {
		t.Fatalf("next cursor = %v, want none", listed["next_cursor"])
	}
	bad := f.h.want(f.Owner, http.MethodGet, "/api/v1/pricing/revisions?cursor=first", nil, nil, 400)
	if code := repProblemCode(t, bad); code != "invalid_cursor" {
		t.Fatalf("cursor problem = %s", code)
	}

	duplicate := repPrice("openai", "m1", "generation")
	mismatched := repPrice("openai", "m3", "generation")
	mismatched["vendor_id"] = "acme"
	override := repPrice("anthropic", "m4", "generation")
	override["provider_id"] = f.P1
	foreign := repPrice("openai", "m5", "generation")
	foreign["currency"] = "EUR"
	oversized := repPrice("openai", "m6", "generation")
	oversized["input_per_million"] = "1.2345678901234"
	for _, c := range []struct {
		name   string
		prices []any
	}{
		{"duplicate-dimensions", []any{duplicate, repPrice("openai", "m1", "generation")}},
		{"mixed-currencies", []any{repPrice("openai", "m7", "generation"), foreign}},
		{"vendor-from-another-connector", []any{mismatched}},
		{"provider-override-of-another-kind", []any{override}},
		{"too-many-fraction-digits", []any{oversized}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f.h.want(f.Owner, http.MethodPost, "/api/v1/pricing/revisions",
				map[string]any{"effective_at": f.Base.Format(time.RFC3339), "prices": c.prices},
				map[string]string{"Idempotency-Key": c.name}, 422)
		})
	}

	// The installation currency is fixed by the first revision.
	f.h.want(f.Owner, http.MethodPost, "/api/v1/pricing/revisions",
		map[string]any{"effective_at": f.Base.Format(time.RFC3339), "prices": []any{foreign}},
		map[string]string{"Idempotency-Key": "pricing-euro"}, 422)
	if total := f.count("SELECT count(*) FROM olp.pricing_revisions"); total != 1 {
		t.Fatalf("a rejected revision was stored: %d revisions", total)
	}
	if total := f.count(
		`SELECT count(*) FROM olp.audit WHERE action = 'pricing_revision.create'`); total != 1 {
		t.Fatalf("audit rows = %d, want one create", total)
	}

	// A second revision exercises the list cursor, which is the revision number
	// rather than a timestamp: the older revision is reachable only through it.
	second := f.h.want(f.Owner, http.MethodPost, "/api/v1/pricing/revisions",
		map[string]any{"effective_at": f.Base.Add(time.Hour).Format(time.RFC3339),
			"prices": []any{repPrice("openai", "gpt-4o-mini", "generation")}},
		map[string]string{"Idempotency-Key": "pricing-second"}, 201)
	if second["revision"].(float64) != 2 {
		t.Fatalf("second revision numbered %v", second["revision"])
	}
	newest := f.h.want(f.Owner, http.MethodGet, "/api/v1/pricing/revisions?limit=1", nil, nil, 200)
	page := newest["items"].([]any)
	if len(page) != 1 || page[0].(map[string]any)["revision"].(float64) != 2 {
		t.Fatalf("newest page = %v", page)
	}
	next, paged := newest["next_cursor"].(string)
	if !paged || next != "2" {
		t.Fatalf("next cursor = %v", newest["next_cursor"])
	}
	older := f.h.want(f.Owner, http.MethodGet,
		"/api/v1/pricing/revisions?limit=1&cursor="+next, nil, nil, 200)
	page = older["items"].([]any)
	if len(page) != 1 || page[0].(map[string]any)["revision"].(float64) != 1 {
		t.Fatalf("page before the cursor = %v", page)
	}
	if older["next_cursor"] != nil {
		t.Fatalf("trailing cursor = %v", older["next_cursor"])
	}
}

func (f *repFixture) epoch(processEpoch string, started time.Time, accepted, persisted, abandoned int64,
	closed, detected *time.Time, updated time.Time,
) {
	f.exec(`INSERT INTO olp.request_metadata_gateway_epochs (gateway_instance, process_epoch,
	        started_at, accepted, persisted, dropped, abandoned, retrying, writer_closed, updated_at,
	        gracefully_closed_at, stale_detected_at)
	    VALUES ('gw-1', $1, $2, $3, $4, 0, $5, false, $6, $7, $8, $9)`,
		processEpoch, started, accepted, persisted, abandoned, closed != nil, updated, closed, detected)
}

// A second holder of the maintenance lock means this run does nothing at all.

// A second batch folds into the same bucket rather than replacing it.

func TestExplicitCardPricesReserveInstallationCurrency(t *testing.T) {
	for _, ordinaryFirst := range []bool{false, true} {
		t.Run(fmt.Sprint("ordinary-first-", ordinaryFirst), func(t *testing.T) {
			f := repSetup(t)

			price := func(model, currency string) usage.Price {
				return usage.Price{ProviderKind: "openai", Model: model, Operation: "generation", Currency: currency, InputPerMillion: acctPtr("1.500000"), OutputPerMillion: acctPtr("3.000000")}
			}
			ordinary := price("currency-model", map[bool]string{false: "USD", true: "EUR"}[ordinaryFirst])
			publish := func(price usage.Price, key string, want int) map[string]any {
				return f.h.want(f.Owner, "POST", "/api/v1/pricing/revisions", map[string]any{"effective_at": f.Base.Format(time.RFC3339), "prices": []usage.Price{price}}, idem(key), want)
			}
			if ordinaryFirst {
				ordinary["currency"] = "EUR"
			}
			publish := func(price map[string]any, key string, want int) map[string]any {
				return f.h.want(f.Owner, "POST", "/api/v1/pricing/revisions", map[string]any{"effective_at": f.Base.Format(time.RFC3339), "prices": []any{price}}, idem(key), want)
			}
			if ordinaryFirst {
				publish(ordinary, "ordinary-first", 201)
			}
			card := f.h.want(f.Owner, "POST", "/api/v1/rate-cards", map[string]any{"owner": map[string]any{"kind": "installation"}}, idem("currency-card"), 201)
			explicit := map[string]any{"provider_kind": "openai", "model": "currency-model", "operation": "generation", "input_per_million": "1", "output_per_million": "1"}
			revision := f.h.want(f.Owner, "POST", "/api/v1/rate-cards/"+card["id"].(string)+"/revisions", map[string]any{"markup": "1", "prices": []any{explicit}}, idem("currency-card-revision"), 201)
			currency := revision["prices"].([]any)[0].(map[string]any)["currency"].(string)
			f.exec("UPDATE olp.rate_card_revisions SET effective_at=$2 WHERE id=$1::uuid", revision["id"], f.Base)
			foreign := price("conflicting-model", map[string]string{"USD": "EUR", "EUR": "USD"}[currency])
			before := f.count("SELECT count(*) FROM olp.pricing_revisions")
			publish(foreign, "conflicting-currency", 422)
			if after := f.count("SELECT count(*) FROM olp.pricing_revisions"); after != before {
				t.Fatalf("conflicting revision stored: %d -> %d", before, after)
			}
			if !ordinaryFirst {
				publish(ordinary, "ordinary-after-card", 201)
			}
			publish(price("same-currency-model", currency), "same-currency", 201)
			quote := f.h.want(f.Owner, "POST", "/api/v1/pricing/quote", map[string]any{"provider_id": f.P1, "model": "currency-model", "operation": "generation", "usage": map[string]any{"input_tokens": 1000000, "output_tokens": 0}}, nil, 200)
			if quote["currency"] != currency {
				t.Fatalf("card currency %s differs from quote %v", currency, quote)
			}
			charge, err := strconv.ParseFloat(quote["charge"].(string), 64)
			if err != nil || charge != 1 {
				t.Fatalf("quote lost explicit card rate: %v (%v)", quote, err)
			}
			event := acctEvent(t, acctFixture{Pool: f.pool, Key: f.Key}, acctEventOptions{Attempts: []usage.Attempt{acctAttempt(t, f.P1, 1, "currency-model", 200, acctObserved(1000000, 0, nil, nil))}})
			acctPersist(t, acctFixture{Pool: f.pool, Key: f.Key}, event)
			var storedCurrency, storedCharge string
			if err := f.pool.QueryRow(t.Context(), `SELECT btrim(currency), charge::text FROM olp.attempt_usage_facts WHERE request_id=$1::uuid`, event.RequestID).Scan(&storedCurrency, &storedCharge); err != nil {
				t.Fatal(err)
			}
			if storedCurrency != quote["currency"] || storedCharge != quote["charge"] {
				t.Fatalf("accounting %s/%s disagrees with quote %v", storedCurrency, storedCharge, quote)
			}
		})
	}
}

func TestMarkupOnlyCardLeavesCurrencyUnreserved(t *testing.T) {
	f := repSetup(t)
	card := f.h.want(f.Owner, "POST", "/api/v1/rate-cards", map[string]any{"owner": map[string]any{"kind": "installation"}}, idem("markup-card"), 201)
	f.h.want(f.Owner, "POST", "/api/v1/rate-cards/"+card["id"].(string)+"/revisions", map[string]any{"markup": "2"}, idem("markup-only"), 201)
	price := usage.Price{ProviderKind: "openai", Model: "markup-model", Operation: "generation", Currency: "EUR", InputPerMillion: acctPtr("1.500000"), OutputPerMillion: acctPtr("3.000000")}
	f.h.want(f.Owner, "POST", "/api/v1/pricing/revisions", map[string]any{"effective_at": f.Base.Format(time.RFC3339), "prices": []any{price}}, idem("first-eur"), 201)
	quote := f.h.want(f.Owner, "POST", "/api/v1/pricing/quote", map[string]any{"provider_id": f.P1, "model": "markup-model", "operation": "generation", "usage": map[string]any{"input_tokens": 1000000}}, nil, 200)
	if quote["currency"] != price.Currency {
		t.Fatalf("markup-only card reserved another currency: %v", quote)
	}
}
