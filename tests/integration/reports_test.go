//go:build integration

package integration_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
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
	        model_incomplete_counted, target_incomplete_counted)
	    VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18,
	        $19::text::numeric, $20::text::numeric, $21, $22, $23, $24, $25, $26, $27, $28, $29, $30,
	        $31, $32, $33, $34, $35)`,
		access.NewID(), access.NewID(), v.RequestID, v.StartedAt, v.Ordinal, f.Key, v.ProviderID,
		v.Route, v.Model, v.Operation, v.Surface, v.ObservedAt, v.Charge, v.Observed, v.Complete,
		v.Input, v.Output, v.Cached, v.Media, v.Cost, v.Unpriced, v.RevisionID, v.Currency,
		v.CountRequest, v.CountProvider, v.CountModel, v.CountTarget,
		v.UnpricedRequest, v.UnpricedProvider, v.UnpricedModel, v.UnpricedTarget,
		v.IncompleteRequest, v.IncompleteProvider, v.IncompleteModel, v.IncompleteTarget)
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
}

func (f *repFixture) hourly(v repHourly) {
	f.exec(`INSERT INTO olp.attempt_usage_hourly (bucket, route_slug, provider_id, upstream_model,
	        operation, surface, api_key_id, request_count, provider_request_count, model_request_count,
	        target_request_count, input_tokens, output_tokens, cached_input_tokens, media_units,
	        estimated_cost, request_unpriced_count, provider_unpriced_count, model_unpriced_count,
	        target_unpriced_count, request_incomplete_count, provider_incomplete_count,
	        model_incomplete_count, target_incomplete_count, currency, unpriced_attempt_count)
	    VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15::text::numeric,
	        $16::text::numeric, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26)`,
		v.Bucket, v.Route, v.ProviderID, v.Model, v.Operation, v.Surface, v.APIKey, v.Requests,
		v.ProviderRequests, v.ModelRequests, v.TargetRequests, v.Input, v.Output, v.Cached, v.Media,
		v.Cost, v.UnpricedRequests, v.UnpricedProvider, v.UnpricedModel, v.UnpricedTarget,
		v.IncompleteRequests, v.IncompleteProvider, v.IncompleteModel, v.IncompleteTarget,
		v.Currency, v.UnpricedAttempts)
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

func repText(value string) *string { return &value }
func repInt(value int) *int        { return &value }
func repCount(value int64) *int64  { return &value }

// repSeedReports lays out the reporting fixture: two retained buckets, two live
// facts in a third hour, one clean hour, and the gap evidence that overlaps the
// first two hours.
func repSeedReports(f *repFixture) {
	base := f.Base
	f.hourly(repHourly{Bucket: base, Route: "alpha", ProviderID: f.P1, Model: "m1",
		Operation: "generation", Surface: "openai", APIKey: &f.Key,
		Requests: 2, ProviderRequests: 2, ModelRequests: 2, TargetRequests: 1,
		Input: 100, Output: 20, Cached: 5, Media: "1.500000", Cost: repText("0.250000000000"),
		Currency: repText("USD")})
	f.hourly(repHourly{Bucket: base.Add(time.Hour), Route: "beta", ProviderID: f.P2, Model: "m2",
		Operation: "embeddings", Surface: "openai",
		Requests: 1, ProviderRequests: 1, ModelRequests: 1, TargetRequests: 1,
		Input: 10, Output: 2, Media: "0.000000",
		UnpricedRequests: 1, UnpricedProvider: 1, UnpricedModel: 1, UnpricedTarget: 1,
		UnpricedAttempts: 1})

	first, second := access.NewID(), access.NewID()
	f.request(repRequest{ID: first, StartedAt: base.Add(time.Hour), Route: "alpha",
		Operation: "generation", Surface: "openai", StatusCode: repInt(200), AttemptCount: 2,
		Latency: repInt(1200), FirstByte: repInt(300)})
	f.attempt(repAttempt{RequestID: first, StartedAt: base.Add(time.Hour), Ordinal: 1,
		ProviderID: f.P2, Model: "m2", StatusCode: repInt(503),
		ErrorClass: repText("upstream_unavailable"),
		Routing:    repText(`{"provider_revision_id":"` + access.NewID() + `","mode":"streaming"}`)})
	f.attempt(repAttempt{RequestID: first, StartedAt: base.Add(time.Hour), Ordinal: 2,
		ProviderID: f.P1, Model: "m1", Committed: true, StatusCode: repInt(200),
		Routing: repText(`{"provider_revision_id":"` + access.NewID() + `","mode":"unary",` +
			`"streamed_output_tokens":3,"credential_slot_id":null,"credential_version_id":null}`)})
	f.fact(repFact{RequestID: first, StartedAt: base.Add(time.Hour), Ordinal: 2,
		ObservedAt: base.Add(2*time.Hour + 15*time.Minute), Route: "alpha", ProviderID: f.P1,
		Model: "m1", Operation: "generation", Surface: "openai", Charge: "billable",
		Observed: true, Complete: true, Input: repCount(7), Output: repCount(3), Cached: repCount(1),
		Media: repText("0.500000"), Cost: repText("0.125000000000"), Currency: repText("USD"),
		CountRequest: true, CountProvider: true, CountModel: true})

	f.request(repRequest{ID: second, StartedAt: base.Add(2 * time.Hour), Route: "alpha",
		Operation: "generation", Surface: "openai", StatusCode: repInt(200), AttemptCount: 1})
	f.attempt(repAttempt{RequestID: second, StartedAt: base.Add(2 * time.Hour), Ordinal: 1,
		ProviderID: f.P1, Model: "m1", Committed: true, StatusCode: repInt(200)})
	f.fact(repFact{RequestID: second, StartedAt: base.Add(2 * time.Hour), Ordinal: 1,
		ObservedAt: base.Add(2*time.Hour + 15*time.Minute), Route: "alpha", ProviderID: f.P1,
		Model: "m1", Operation: "generation", Surface: "openai", Charge: "billing_uncertain",
		Observed: true, Unpriced: true,
		CountRequest: true, CountProvider: true, CountModel: true, CountTarget: true,
		UnpricedRequest: true, UnpricedProvider: true, UnpricedModel: true, UnpricedTarget: true,
		IncompleteRequest: true, IncompleteProvider: true, IncompleteModel: true,
		IncompleteTarget: true})

	third := access.NewID()
	f.request(repRequest{ID: third, StartedAt: base.Add(3 * time.Hour), Route: "beta",
		Operation: "embeddings", Surface: "openai", StatusCode: repInt(429),
		ErrorClass: repText("rate_limited")})

	// A clean hour with no gap evidence, used to prove a report can report itself
	// complete.
	clean := access.NewID()
	f.request(repRequest{ID: clean, StartedAt: base.Add(4 * time.Hour), Route: "gamma",
		Operation: "generation", Surface: "openai", StatusCode: repInt(200), AttemptCount: 1})
	f.attempt(repAttempt{RequestID: clean, StartedAt: base.Add(4 * time.Hour), Ordinal: 1, ProviderID: f.P1, Model: "m3", Committed: true, StatusCode: repInt(200)})
	f.fact(repFact{RequestID: clean, StartedAt: base.Add(4 * time.Hour), Ordinal: 1,
		ObservedAt: base.Add(4*time.Hour + 15*time.Minute), Route: "gamma", ProviderID: f.P1,
		Model: "m3", Operation: "generation", Surface: "openai", Charge: "billable",
		Observed: true, Complete: true, Input: repCount(1), Output: repCount(1),
		Cost: repText("1.000000000000"), Currency: repText("USD"),
		CountRequest: true, CountProvider: true, CountModel: true, CountTarget: true})

	f.exec(`INSERT INTO olp.request_metadata_ingestion_gaps (id, gateway_instance, event_count,
	        reason, first_observed_at, last_observed_at, reported_at, certainty)
	    VALUES ($1, 'gw-1', 3, 'writer_dropped', $2, $3, $3, 'lower_bound')`,
		access.NewID(), base.Add(time.Hour), base.Add(time.Hour+5*time.Minute))
	f.exec(`INSERT INTO olp.request_metadata_gap_hourly (bucket, gateway_instance, reason,
	        event_count, uncertain_gap_count, first_observed_at, last_observed_at)
	    VALUES ($1, 'gw-1', 'restart', 2, 0, $2, $3)`,
		base, base.Add(10*time.Minute), base.Add(20*time.Minute))
	f.exec(`INSERT INTO olp.request_metadata_consumer_health (singleton, pending_events,
	        lag_events, checked_at) VALUES (true, 0, 0, now())`)
}

func repRange(f *repFixture, fromHours, toHours float64) usage.Filters {
	return usage.Filters{
		Start:       f.Base.Add(time.Duration(fromHours * float64(time.Hour))),
		End:         f.Base.Add(time.Duration(toHours * float64(time.Hour))),
		AllProjects: true,
	}
}

func TestReportsAgreeWithSeededUsage(t *testing.T) {
	f := repSetup(t)
	repSeedReports(f)
	ctx := context.Background()
	now := time.Now().UTC()

	summary, err := usage.ReadSummary(ctx, f.pool, repRange(f, 0, 3), now)
	if err != nil {
		t.Fatalf("read summary: %v", err)
	}
	if summary.RequestCount != 5 {
		t.Fatalf("request count = %d, want 5", summary.RequestCount)
	}
	for _, c := range []struct{ name, got, want string }{
		{"input", summary.InputTokens, "117"},
		{"output", summary.OutputTokens, "25"},
		{"cached", summary.CachedInputTokens, "6"},
		{"media", summary.MediaUnits, "2.000000"},
	} {
		if c.got != c.want {
			t.Fatalf("%s tokens = %q, want %q", c.name, c.got, c.want)
		}
	}
	if summary.EstimatedCost == nil || *summary.EstimatedCost != "0.375000000000" {
		t.Fatalf("estimated cost = %v, want 0.375000000000", summary.EstimatedCost)
	}
	if summary.Currency == nil || *summary.Currency != "USD" {
		t.Fatalf("currency = %v, want USD", summary.Currency)
	}
	if summary.UnpricedCount != 2 || summary.IncompleteCount != 1 {
		t.Fatalf("unpriced/incomplete = %d/%d, want 2/1", summary.UnpricedCount, summary.IncompleteCount)
	}
	if summary.GapEvents != 5 || summary.UncertainGapCount != 1 {
		t.Fatalf("gap evidence = %d/%d, want 5/1", summary.GapEvents, summary.UncertainGapCount)
	}
	if !summary.Coverage.RangeComplete || summary.Coverage.Approximate ||
		summary.Coverage.ExcludedBoundaries != 0 {
		t.Fatalf("coverage = %+v, want a complete range", summary.Coverage)
	}
	if summary.Consumer.State != usage.ConsumerHealthy {
		t.Fatalf("consumer state = %q, want healthy", summary.Consumer.State)
	}
	if summary.Complete {
		t.Fatal("a range with unpriced, incomplete and lost events reported itself complete")
	}

	clean, err := usage.ReadSummary(ctx, f.pool, repRange(f, 4, 5), now)
	if err != nil {
		t.Fatalf("read clean summary: %v", err)
	}
	if !clean.Complete || clean.RequestCount != 1 || clean.GapEvents != 0 {
		t.Fatalf("clean summary = %+v, want one complete request", clean)
	}

	report, err := usage.ReadCompleteness(ctx, f.pool, repRange(f, 0, 3), now)
	if err != nil {
		t.Fatalf("read completeness: %v", err)
	}
	if report.RequestCount != 5 || report.PricedCount != 3 || report.UnpricedCount != 2 {
		t.Fatalf("completeness = %d/%d/%d, want 5/3/2",
			report.RequestCount, report.PricedCount, report.UnpricedCount)
	}
	if report.IncompleteCount != 1 || report.GapEvents != 5 || report.UncertainGapCount != 1 {
		t.Fatalf("completeness evidence = %+v", report)
	}
}

func TestReportsCountScopeFollowsTheFilter(t *testing.T) {
	f := repSetup(t)
	repSeedReports(f)
	ctx := context.Background()
	now := time.Now().UTC()
	for _, c := range []struct {
		name     string
		provider bool
		model    bool
		want     int64
	}{
		{"unfiltered counts requests", false, false, 5},
		{"a provider filter counts provider requests", true, false, 4},
		{"a model filter counts model requests", false, true, 4},
		{"both filters count targets", true, true, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			filters := repRange(f, 0, 3)
			if c.provider {
				filters.ProviderID = &f.P1
			}
			if c.model {
				filters.Model = repText("m1")
			}
			summary, err := usage.ReadSummary(ctx, f.pool, filters, now)
			if err != nil {
				t.Fatalf("read summary: %v", err)
			}
			if summary.RequestCount != c.want {
				t.Fatalf("request count = %d, want %d", summary.RequestCount, c.want)
			}
		})
	}
}

func TestReportsExcludePartialBucketsAndSaySo(t *testing.T) {
	f := repSetup(t)
	repSeedReports(f)
	summary, err := usage.ReadSummary(context.Background(), f.pool, repRange(f, 0.5, 2.5), time.Now().UTC())
	if err != nil {
		t.Fatalf("read summary: %v", err)
	}
	if summary.Coverage.RangeComplete || !summary.Coverage.Approximate {
		t.Fatalf("coverage = %+v, want an approximate range", summary.Coverage)
	}
	if summary.Coverage.ExcludedBoundaries != 1 {
		t.Fatalf("excluded boundaries = %d, want 1", summary.Coverage.ExcludedBoundaries)
	}
	// The whole bucket inside the range plus both live facts; the half bucket the
	// range starts in is left out rather than counted in part.
	if summary.RequestCount != 3 {
		t.Fatalf("request count = %d, want 3", summary.RequestCount)
	}
	if summary.Complete {
		t.Fatal("an approximate range reported itself complete")
	}
}

func TestReportsGroupByDimensionAndBucket(t *testing.T) {
	f := repSetup(t)
	repSeedReports(f)
	ctx := context.Background()

	byRoute, err := usage.ReadBreakdown(ctx, f.pool, repRange(f, 0, 3), usage.DimensionRoute, 10)
	if err != nil {
		t.Fatalf("read breakdown: %v", err)
	}
	if len(byRoute.Items) != 2 {
		t.Fatalf("routes = %d, want 2", len(byRoute.Items))
	}
	if byRoute.Items[0].Dimension != "alpha" || byRoute.Items[0].RequestCount != 4 {
		t.Fatalf("first route = %+v, want alpha with 4 requests", byRoute.Items[0])
	}
	if byRoute.Items[1].Dimension != "beta" || byRoute.Items[1].RequestCount != 1 {
		t.Fatalf("second route = %+v, want beta with 1 request", byRoute.Items[1])
	}

	byKey, err := usage.ReadBreakdown(ctx, f.pool, repRange(f, 0, 3), usage.DimensionAPIKey, 10)
	if err != nil {
		t.Fatalf("read breakdown: %v", err)
	}
	keys := map[string]int64{}
	for _, item := range byKey.Items {
		keys[item.Dimension] = item.RequestCount
	}
	if keys[f.Key] != 4 || keys["unknown"] != 1 {
		t.Fatalf("api key breakdown = %v, want the seeded key with 4 and unknown with 1", keys)
	}

	series, err := usage.ReadSeries(ctx, f.pool, repRange(f, 0, 3), usage.GranularityHour)
	if err != nil {
		t.Fatalf("read series: %v", err)
	}
	if len(series.Items) != 3 {
		t.Fatalf("buckets = %d, want 3", len(series.Items))
	}
	for i, want := range []int64{2, 1, 2} {
		bucket := f.Base.Add(time.Duration(i) * time.Hour)
		if !series.Items[i].Bucket.Equal(bucket) {
			t.Fatalf("bucket %d = %s, want %s", i, series.Items[i].Bucket, bucket)
		}
		if series.Items[i].RequestCount != want {
			t.Fatalf("bucket %d count = %d, want %d", i, series.Items[i].RequestCount, want)
		}
	}

	daily, err := usage.ReadSeries(ctx, f.pool, repRange(f, 0, 3), usage.GranularityDay)
	if err != nil {
		t.Fatalf("read daily series: %v", err)
	}
	total := int64(0)
	for _, point := range daily.Items {
		total += point.RequestCount
	}
	if total != 5 {
		t.Fatalf("daily total = %d, want 5", total)
	}
}

func TestUsageReportsOverHTTP(t *testing.T) {
	// Keep both windows safely in the past at any hour while exercising each
	// side of the UTC day boundary.
	day := time.Now().UTC().Truncate(24 * time.Hour).Add(-48 * time.Hour)
	for _, c := range []struct {
		name string
		base time.Time
	}{
		{"within UTC day", day.Add(12 * time.Hour)},
		{"across UTC midnight", day.Add(22 * time.Hour)},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := repSetup(t)
			f.Base = c.base
			repSeedReports(f)
			repCheckUsageReportsOverHTTP(t, f)
		})
	}
}

func repCheckUsageReportsOverHTTP(t *testing.T, f *repFixture) {
	t.Helper()
	window := "start=" + f.Base.Format(time.RFC3339) +
		"&end=" + f.Base.Add(3*time.Hour).Format(time.RFC3339)

	body := f.h.want(f.Owner, http.MethodGet, "/api/v1/usage/summary?"+window, nil, nil, 200)
	if body["request_count"].(float64) != 5 || body["input_tokens"].(string) != "117" {
		t.Fatalf("summary body = %v", body)
	}
	if body["complete"].(bool) {
		t.Fatal("the summary reported itself complete")
	}
	consumer := body["request_metadata_consumer"].(map[string]any)
	if consumer["state"].(string) != "healthy" {
		t.Fatalf("consumer = %v", consumer)
	}
	coverage := body["coverage"].(map[string]any)
	if !coverage["range_complete"].(bool) || coverage["excluded_partial_aggregate_boundaries"].(float64) != 0 {
		t.Fatalf("coverage = %v", coverage)
	}

	body = f.h.want(f.Owner, http.MethodGet, "/api/v1/usage/completeness?"+window, nil, nil, 200)
	if body["priced_count"].(float64) != 3 {
		t.Fatalf("completeness body = %v", body)
	}

	body = f.h.want(f.Owner, http.MethodGet,
		"/api/v1/usage/breakdown?"+window+"&dimension=provider&limit=5", nil, nil, 200)
	items := body["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("breakdown items = %v", items)
	}

	body = f.h.want(f.Owner, http.MethodGet,
		"/api/v1/usage/time-series?"+window+"&granularity=day", nil, nil, 200)
	wantByBucket := map[string]float64{}
	for _, point := range []struct {
		at    time.Time
		count float64
	}{
		{f.Base, 2},
		{f.Base.Add(time.Hour), 1},
		{f.Base.Add(2*time.Hour + 15*time.Minute), 2},
	} {
		bucket := point.at.UTC().Truncate(24 * time.Hour).Format(time.RFC3339)
		wantByBucket[bucket] += point.count
	}
	wantBuckets := make([]string, 0, len(wantByBucket))
	for bucket := range wantByBucket {
		wantBuckets = append(wantBuckets, bucket)
	}
	sort.Strings(wantBuckets)
	series := body["items"].([]any)
	if len(series) != len(wantBuckets) {
		t.Fatalf("daily series = %v, want %d UTC buckets", series, len(wantBuckets))
	}
	var total float64
	for i, raw := range series {
		point := raw.(map[string]any)
		if point["bucket"] != wantBuckets[i] || point["request_count"] != wantByBucket[wantBuckets[i]] {
			t.Fatalf("daily bucket %d = %v, want %s with %v requests", i, point,
				wantBuckets[i], wantByBucket[wantBuckets[i]])
		}
		total += point["request_count"].(float64)
	}
	if total != 5 {
		t.Fatalf("daily series total = %v, want 5", total)
	}

	for _, c := range []struct{ query, code string }{
		{"start=" + f.Base.Format(time.RFC3339), "invalid_range"},
		{window + "&operation=telepathy", "invalid_operation"},
		{window + "&dimension=sideways", "invalid_dimension"},
	} {
		path := "/api/v1/usage/summary?" + c.query
		if strings.Contains(c.query, "dimension") {
			path = "/api/v1/usage/breakdown?" + c.query
		}
		problem := f.h.want(f.Owner, http.MethodGet, path, nil, nil, 400)
		if code := repProblemCode(t, problem); code != c.code {
			t.Fatalf("query %q gave %s, want %s", c.query, code, c.code)
		}
	}
	problem := f.h.want(f.Owner, http.MethodGet,
		"/api/v1/usage/breakdown?"+window+"&dimension=route&limit=0", nil, nil, 400)
	if code := repProblemCode(t, problem); code != "invalid_limit" {
		t.Fatalf("limit problem = %s", code)
	}
}

func TestRequestHistoryPagesAndDetails(t *testing.T) {
	f := repSetup(t)
	repSeedReports(f)

	body := f.h.want(f.Owner, http.MethodGet, "/api/v1/requests?limit=2", nil, nil, 200)
	items := body["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("page = %v", items)
	}
	newest := items[0].(map[string]any)
	if newest["route"].(string) != "gamma" {
		t.Fatalf("newest request = %v, want the clean gamma request", newest)
	}
	if newest["input_tokens"].(float64) != 1 || !newest["usage_complete"].(bool) {
		t.Fatalf("the newest request lost its settled usage: %v", newest)
	}
	unserved := items[1].(map[string]any)
	if unserved["route"].(string) != "beta" || unserved["input_tokens"] != nil ||
		unserved["estimated_cost"] != nil || unserved["usage_complete"] != nil {
		t.Fatalf("a request with no attempt usage reported totals: %v", unserved)
	}
	cursor, ok := body["next_cursor"].(string)
	if !ok || cursor == "" {
		t.Fatalf("next cursor = %v, want a page token", body["next_cursor"])
	}
	body = f.h.want(f.Owner, http.MethodGet, "/api/v1/requests?limit=2&cursor="+cursor, nil, nil, 200)
	second := body["items"].([]any)
	if len(second) != 2 {
		t.Fatalf("second page = %v", second)
	}
	if second[0].(map[string]any)["id"] == newest["id"] {
		t.Fatal("the second page repeated the first page")
	}
	if body["next_cursor"] != nil {
		t.Fatalf("the last page offered another cursor: %v", body["next_cursor"])
	}

	body = f.h.want(f.Owner, http.MethodGet, "/api/v1/requests?provider_id="+f.P1, nil, nil, 200)
	items = body["items"].([]any)
	if len(items) != 3 {
		t.Fatalf("provider filtered page = %v, want the three requests with facts", items)
	}
	body = f.h.want(f.Owner, http.MethodGet, "/api/v1/requests?status_code=429", nil, nil, 200)
	items = body["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["error_class"].(string) != "rate_limited" {
		t.Fatalf("status filtered page = %v", items)
	}

	// Both generation requests on the alpha route, newest first: the older one
	// carries the committed usage and the two attempt timeline.
	body = f.h.want(f.Owner, http.MethodGet, "/api/v1/requests?route=alpha&operation=generation",
		nil, nil, 200)
	alpha := body["items"].([]any)
	if len(alpha) != 2 {
		t.Fatalf("route filtered page = %v", alpha)
	}
	detailed := alpha[1].(map[string]any)
	if detailed["estimated_cost"].(string) != "0.125000000000" ||
		detailed["currency"].(string) != "USD" {
		t.Fatalf("committed usage = %v", detailed)
	}
	if detailed["unpriced"].(bool) || !detailed["usage_complete"].(bool) {
		t.Fatalf("usage flags = %v", detailed)
	}
	uncertain := alpha[0].(map[string]any)
	if !uncertain["unpriced"].(bool) || uncertain["usage_complete"].(bool) {
		t.Fatalf("an uncertain request reported settled usage: %v", uncertain)
	}

	detail := f.h.want(f.Owner, http.MethodGet, "/api/v1/requests/"+detailed["id"].(string),
		nil, nil, 200)
	attempts := detail["attempts"].([]any)
	if len(attempts) != 2 {
		t.Fatalf("attempts = %v", attempts)
	}
	firstAttempt := attempts[0].(map[string]any)
	secondAttempt := attempts[1].(map[string]any)
	if firstAttempt["ordinal"].(float64) != 1 || secondAttempt["ordinal"].(float64) != 2 {
		t.Fatalf("attempts are out of order: %v", attempts)
	}
	if firstAttempt["committed"].(bool) || !secondAttempt["committed"].(bool) {
		t.Fatalf("commitment = %v", attempts)
	}
	if firstAttempt["provider_name"].(string) != "secondary" ||
		secondAttempt["provider_name"].(string) != "primary" {
		t.Fatalf("provider names = %v", attempts)
	}
	if firstAttempt["charge_status"] != nil {
		t.Fatalf("an attempt with no fact carried charge evidence: %v", firstAttempt)
	}
	if secondAttempt["charge_status"].(string) != "billable" ||
		secondAttempt["estimated_cost"].(string) != "0.125000000000" {
		t.Fatalf("charge evidence = %v", secondAttempt)
	}
	routing := secondAttempt["routing"].(map[string]any)
	if routing["provider_revision_id"] == nil || routing["mode"].(string) != "unary" {
		t.Fatalf("routing provenance = %v", routing)
	}

	missing := f.h.want(f.Owner, http.MethodGet, "/api/v1/requests/"+access.NewID(), nil, nil, 404)
	if code := repProblemCode(t, missing); code != "not_found" {
		t.Fatalf("missing request = %s", code)
	}
	bad := f.h.want(f.Owner, http.MethodGet, "/api/v1/requests?cursor=nonsense", nil, nil, 400)
	if code := repProblemCode(t, bad); code != "invalid_cursor" {
		t.Fatalf("cursor problem = %s", code)
	}
}

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

func TestGatewayEpochsOverHTTP(t *testing.T) {
	f := repSetup(t)
	open, closed, stale := access.NewID(), access.NewID(), access.NewID()
	start := f.Base
	closedAt, detectedAt := start.Add(2*time.Minute), start.Add(3*time.Minute)
	f.epoch(open, start, 10, 10, 0, nil, nil, start.Add(time.Minute))
	f.epoch(closed, start, 8, 8, 0, &closedAt, nil, closedAt)
	f.epoch(stale, start, 12, 5, 2, nil, &detectedAt, detectedAt)

	body := f.h.want(f.Owner, http.MethodGet, "/api/v1/request-metadata/gateway-epochs", nil, nil, 200)
	items := body["items"].([]any)
	if len(items) != 3 {
		t.Fatalf("epochs = %v", items)
	}
	states := map[string]string{}
	bounds := map[string]float64{}
	for _, item := range items {
		row := item.(map[string]any)
		states[row["process_epoch"].(string)] = row["state"].(string)
		bounds[row["process_epoch"].(string)] = row["uncertain_event_lower_bound"].(float64)
	}
	if states[open] != "open" || states[closed] != "gracefully_closed" || states[stale] != "unresolved" {
		t.Fatalf("states = %v", states)
	}
	if bounds[stale] != 5 || bounds[open] != 0 {
		t.Fatalf("uncertain lower bounds = %v, want 5 for the stale epoch", bounds)
	}
	// Newest first, so the stale epoch (detected last) leads.
	if items[0].(map[string]any)["process_epoch"].(string) != stale {
		t.Fatalf("ordering = %v", items)
	}

	filtered := f.h.want(f.Owner, http.MethodGet,
		"/api/v1/request-metadata/gateway-epochs?state=unresolved", nil, nil, 200)
	if len(filtered["items"].([]any)) != 1 {
		t.Fatalf("unresolved epochs = %v", filtered["items"])
	}
	f.h.want(f.Owner, http.MethodGet,
		"/api/v1/request-metadata/gateway-epochs?state=elsewhere", nil, nil, 400)

	path := "/api/v1/request-metadata/gateway-epochs/"
	acknowledged := f.h.want(f.Owner, http.MethodPost, path+stale+"/acknowledge", nil, nil, 200)
	if acknowledged["acknowledged_by"].(string) != f.OwnerID {
		t.Fatalf("acknowledged by = %v", acknowledged["acknowledged_by"])
	}
	if acknowledged["gateway_instance"].(string) != "gw-1" {
		t.Fatalf("acknowledgement = %v", acknowledged)
	}
	again := f.h.want(f.Owner, http.MethodPost, path+stale+"/acknowledge", nil, nil, 200)
	if again["acknowledged_at"].(string) != acknowledged["acknowledged_at"].(string) {
		t.Fatal("a repeated acknowledgement moved the timestamp")
	}
	f.h.want(f.Owner, http.MethodPost, path+open+"/acknowledge", nil, nil, 404)
	f.h.want(f.Owner, http.MethodPost, path+access.NewID()+"/acknowledge", nil, nil, 404)
	f.h.want(f.Owner, http.MethodPost, path+"not-a-uuid/acknowledge", nil, nil, 400)

	resolved := f.h.want(f.Owner, http.MethodGet,
		"/api/v1/request-metadata/gateway-epochs?state=acknowledged", nil, nil, 200)
	if len(resolved["items"].([]any)) != 1 {
		t.Fatalf("acknowledged epochs = %v", resolved["items"])
	}
	if total := f.count(`SELECT count(*) FROM olp.audit
	    WHERE action = 'request_metadata.gateway_epoch_acknowledge'`); total != 2 {
		t.Fatalf("audit rows = %d, want one per acknowledgement", total)
	}
}

// repSeedExpiring seeds one expired row in every table maintenance drains, plus
// a live neighbour that must survive.
func repSeedExpiring(f *repFixture, now time.Time) {
	f.exec(`INSERT INTO olp.request_metadata_event_receipts (event_id, request_id, event_sha256,
	        status, observed_at, recorded_at) VALUES ($1, $2, $3, 'fact_persisted', $4, $4)`,
		access.NewID(), access.NewID(), make([]byte, 32), now.Add(-30*24*time.Hour))
	f.exec(`INSERT INTO olp.request_metadata_event_receipts (event_id, request_id, event_sha256,
	        status, observed_at, recorded_at) VALUES ($1, $2, $3, 'pending', $4, $4)`,
		access.NewID(), access.NewID(), make([]byte, 32), now.Add(-time.Hour))
	f.exec(`INSERT INTO olp.sessions (id, user_id, digest, expires_at)
	    VALUES ($1, $2, $3, $4)`, access.NewID(), f.OwnerID, []byte("expired session digest "),
		now.Add(-time.Hour))
	f.exec(`INSERT INTO olp.invitations (id, email, role, digest, invited_by, expires_at)
	    VALUES ($1, 'stale@example.test', 'viewer', $2, $3, $4)`,
		access.NewID(), []byte("expired invitation digest"), f.OwnerID, now.Add(-time.Hour))
	f.exec(`INSERT INTO olp.replays (actor, key, fingerprint, expires_at)
	    VALUES ($1, 'expired', $2, $3)`, f.OwnerID, []byte("fingerprint"), now.Add(-time.Hour))
	secret := access.NewID()
	f.exec(`INSERT INTO olp.secrets (id, purpose, key_version, ciphertext, expires_at)
	    VALUES ($1, 'oidc_flow', 1, $2, $3)`, secret, []byte("ciphertext"), now.Add(-time.Hour))
	f.exec(`INSERT INTO olp.oidc_flows (id, state_digest, cookie_digest, configuration_etag, expires_at)
	    VALUES ($1, $2, $3, $4, $5)`, secret, []byte("state digest"), []byte("cookie digest"),
		access.NewID(), now.Add(-time.Hour))
	f.exec(`INSERT INTO olp.audit (id, actor_user_id, action, resource_type, resource_id, outcome,
	        occurred_at) VALUES ($1, $2, 'api_key.create', 'api_key', $3, 'success', $4)`,
		access.NewID(), f.OwnerID, access.NewID(), now.Add(-400*24*time.Hour))
	f.exec(`INSERT INTO olp.request_metadata_ingestion_gaps (id, gateway_instance, event_count,
	        reason, first_observed_at, last_observed_at, reported_at, certainty)
	    VALUES ($1, 'gw-1', 4, 'writer_dropped', $2, $2, $2, 'lower_bound')`,
		access.NewID(), now.Add(-100*24*time.Hour))
	drained := now.Add(-100 * 24 * time.Hour)
	f.epoch(access.NewID(), drained.Add(-time.Hour), 4, 4, 0, &drained, nil, drained)
}

// repRollupFacts seeds `count` identical priced facts in one retained bucket.
func repRollupFacts(f *repFixture, bucket time.Time, count int) {
	for i := 0; i < count; i++ {
		id := access.NewID()
		f.request(repRequest{ID: id, StartedAt: bucket, Route: "alpha", Operation: "generation",
			Surface: "openai", StatusCode: repInt(200), AttemptCount: 1})
		f.fact(repFact{RequestID: id, StartedAt: bucket, Ordinal: 1,
			ObservedAt: bucket.Add(time.Duration(i) * time.Minute), Route: "alpha",
			ProviderID: f.P1, Model: "m1", Operation: "generation", Surface: "openai",
			Charge: "billable", Observed: true, Complete: true, Input: repCount(7),
			Output: repCount(3), Cached: repCount(1), Media: repText("0.500000"),
			Cost: repText("0.125000000000"), Currency: repText("USD"),
			CountRequest: true, CountProvider: true, CountModel: true, CountTarget: true})
	}
}

// repSameTotals asserts that a rollup preserved every reconstructable total, so
// a budget rebuilt from retained buckets matches the one built from live facts.
func repSameTotals(t *testing.T, before, after usage.Summary) {
	t.Helper()
	if before.RequestCount != after.RequestCount || before.UnpricedCount != after.UnpricedCount ||
		before.IncompleteCount != after.IncompleteCount {
		t.Fatalf("counts changed: before %+v after %+v", before.Totals, after.Totals)
	}
	if before.InputTokens != after.InputTokens || before.OutputTokens != after.OutputTokens ||
		before.CachedInputTokens != after.CachedInputTokens || before.MediaUnits != after.MediaUnits {
		t.Fatalf("tokens changed: before %+v after %+v", before.Totals, after.Totals)
	}
	if repOptional(before.EstimatedCost) != repOptional(after.EstimatedCost) {
		t.Fatalf("cost changed: before %v after %v",
			repOptional(before.EstimatedCost), repOptional(after.EstimatedCost))
	}
	if repOptional(before.Currency) != repOptional(after.Currency) {
		t.Fatalf("currency changed: before %v after %v",
			repOptional(before.Currency), repOptional(after.Currency))
	}
	if before.Coverage != after.Coverage {
		t.Fatalf("coverage changed: before %+v after %+v", before.Coverage, after.Coverage)
	}
}

func repOptional(value *string) string {
	if value == nil {
		return "<none>"
	}
	return *value
}

func TestUsageMaintenanceRollsUpAndPurges(t *testing.T) {
	f := repSetup(t)
	ctx := context.Background()
	now := time.Now().UTC()
	bucket := now.Add(-100 * 24 * time.Hour).Truncate(time.Hour)
	f.hourly(repHourly{Bucket: bucket, Route: "alpha", ProviderID: f.P1, Model: "m1",
		Operation: "generation", Surface: "openai", APIKey: &f.Key,
		Requests: 1, ProviderRequests: 1, ModelRequests: 1, TargetRequests: 1,
		Input: 10, Output: 5, Cached: 1, Media: "2.000000", Cost: repText("1.000000000000"),
		Currency: repText("USD")})
	repRollupFacts(f, bucket, 2)
	repSeedExpiring(f, now)
	live := access.NewID()
	f.request(repRequest{ID: live, StartedAt: now.Add(-time.Hour), Route: "alpha",
		Operation: "generation", Surface: "openai", StatusCode: repInt(200)})

	window := usage.Filters{Start: bucket, End: bucket.Add(time.Hour), AllProjects: true}
	before, err := usage.ReadSummary(ctx, f.pool, window, now)
	if err != nil {
		t.Fatalf("read summary: %v", err)
	}

	// A second holder of the maintenance lock means this run does nothing at all.
	blocker, err := f.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if _, err = blocker.Exec(ctx, "SELECT pg_advisory_lock($1)", int64(0x4f4c505f4d54)); err != nil {
		t.Fatalf("hold maintenance lock: %v", err)
	}
	skipped, err := usage.RunMaintenance(ctx, f.pool, now)
	if err != nil {
		t.Fatalf("run maintenance: %v", err)
	}
	if skipped.LockAcquired || skipped.UsageRows != 0 || skipped.ReceiptRows != 0 {
		t.Fatalf("a second maintenance run did work: %+v", skipped)
	}
	if _, err = blocker.Exec(ctx, "SELECT pg_advisory_unlock($1)", int64(0x4f4c505f4d54)); err != nil {
		t.Fatalf("release maintenance lock: %v", err)
	}
	blocker.Release()
	if total := f.count("SELECT count(*) FROM olp.attempt_usage_facts"); total != 2 {
		t.Fatalf("facts = %d, want the two the skipped run left alone", total)
	}

	report, err := usage.RunMaintenance(ctx, f.pool, now)
	if err != nil {
		t.Fatalf("run maintenance: %v", err)
	}
	if !report.LockAcquired {
		t.Fatal("maintenance could not take its lock")
	}
	if report.UsageRows != 2 || report.RollupRows != 1 {
		t.Fatalf("rollup = %d facts into %d buckets, want 2 into 1", report.UsageRows, report.RollupRows)
	}
	if report.ReceiptRows != 1 || report.SessionRows != 1 || report.InvitationRows != 1 ||
		report.ReplayRows != 1 || report.OIDCFlowRows != 1 || report.AuditRows != 1 ||
		report.EpochRows != 1 || report.GapRows != 1 || report.GapRollupRows != 1 {
		t.Fatalf("purge report = %+v", report)
	}
	if report.RequestRows < 2 {
		t.Fatalf("expired requests = %d, want the seeded pair", report.RequestRows)
	}

	after, err := usage.ReadSummary(ctx, f.pool, window, now)
	if err != nil {
		t.Fatalf("read summary: %v", err)
	}
	repSameTotals(t, before, after)
	if total := f.count("SELECT count(*) FROM olp.attempt_usage_facts"); total != 0 {
		t.Fatalf("facts left after the rollup = %d", total)
	}
	if total := f.count("SELECT count(*) FROM olp.attempt_usage_hourly"); total != 1 {
		t.Fatalf("hourly rows = %d, want the one the facts folded into", total)
	}
	if total := f.count("SELECT count(*) FROM olp.request_metadata_event_receipts"); total != 1 {
		t.Fatalf("receipts = %d, want the recent one to survive", total)
	}
	if total := f.count("SELECT count(*) FROM olp.requests WHERE id = $1", live); total != 1 {
		t.Fatalf("a request inside the retention window was purged")
	}
	if total := f.count("SELECT count(*) FROM olp.usage_request_anchors"); total != 0 {
		t.Fatalf("orphaned anchors = %d, want none", total)
	}
	if total := f.count(`SELECT count(*) FROM olp.request_metadata_gap_hourly`); total != 1 {
		t.Fatalf("gap rollup rows = %d, want one", total)
	}

	// A second batch folds into the same bucket rather than replacing it.
	repRollupFacts(f, bucket, 2)
	expected, err := usage.ReadSummary(ctx, f.pool, window, now)
	if err != nil {
		t.Fatalf("read summary: %v", err)
	}
	if _, err = usage.RunMaintenance(ctx, f.pool, now); err != nil {
		t.Fatalf("run maintenance: %v", err)
	}
	settled, err := usage.ReadSummary(ctx, f.pool, window, now)
	if err != nil {
		t.Fatalf("read summary: %v", err)
	}
	repSameTotals(t, expected, settled)
	if settled.RequestCount != 5 {
		t.Fatalf("request count = %d, want 1 retained plus 4 rolled up", settled.RequestCount)
	}
}

func TestUsageRetentionHonoursTheConfiguredWindow(t *testing.T) {
	f := repSetup(t)
	ctx := context.Background()
	now := time.Now().UTC()
	f.exec(`UPDATE olp.settings SET value = '2' WHERE key = 'retention.requests_days'`)
	recent, old := access.NewID(), access.NewID()
	f.request(repRequest{ID: recent, StartedAt: now.Add(-24 * time.Hour), Route: "alpha",
		Operation: "generation", Surface: "openai", StatusCode: repInt(200)})
	f.request(repRequest{ID: old, StartedAt: now.Add(-72 * time.Hour), Route: "alpha",
		Operation: "generation", Surface: "openai", StatusCode: repInt(200)})
	report, err := usage.RunMaintenance(ctx, f.pool, now)
	if err != nil {
		t.Fatalf("run maintenance: %v", err)
	}
	if report.RequestRows != 1 {
		t.Fatalf("purged %d requests, want only the one past the two day window", report.RequestRows)
	}
	if f.count("SELECT count(*) FROM olp.requests WHERE id = $1", recent) != 1 {
		t.Fatal("a request inside the window was purged")
	}
	if f.count("SELECT count(*) FROM olp.requests WHERE id = $1", old) != 0 {
		t.Fatal("a request past the window survived")
	}

	f.exec(`UPDATE olp.settings SET value = '0' WHERE key = 'retention.usage_days'`)
	if _, err = usage.RunMaintenance(ctx, f.pool, now); err == nil {
		t.Fatal("maintenance ran with a retention setting outside its bounds")
	}
}
