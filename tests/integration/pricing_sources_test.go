//go:build integration

package integration_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/catalog"
	"github.com/tyk-swe/olp/internal/configuration"
	"github.com/tyk-swe/olp/internal/management"
	"github.com/tyk-swe/olp/internal/signing"
	"github.com/tyk-swe/olp/internal/usage"
)

func pricingHarness(t *testing.T) *accessHarness {
	t.Helper()
	h := newAccessHarness(t)
	policy := alertPolicy()
	h.Server.Egress = policy
	mux := http.NewServeMux()
	management.Register(mux)
	h.Server.Register(mux)
	(&usage.Server{Access: h.Server, VendorKind: repVendorKind, Egress: policy}).Register(mux)
	// Configuration endpoints exercise the pricing round trip through export,
	// plan and apply; this harness carries no providers, so the credential
	// stores and rate limiter are never reached.
	(&configuration.Server{Access: h.Server, Egress: policy, VendorKind: repVendorKind}).Register(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	h.HTTP = server
	return h
}

type sourceFixture struct {
	*httptest.Server
	document atomic.Value
	fetches  atomic.Int64
}

func newSourceFixture(t *testing.T, document string) *sourceFixture {
	f := &sourceFixture{}
	f.document.Store(document)
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.fetches.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, f.document.Load().(string))
	}))
	t.Cleanup(f.Close)
	return f
}

const sourceDocumentV1 = `{"currency":"USD","prices":[
 {"provider_kind":"openai","model":"gpt-source","operation":"generation","currency":"USD","input_per_million":"1.000000","output_per_million":"4.000000"},
 {"provider_kind":"openai","model":"embed-source","operation":"embeddings","currency":"USD","unit_price":"0.02"}
]}`

const sourceDocumentV2 = `{"currency":"USD","prices":[
 {"provider_kind":"openai","model":"gpt-source","operation":"generation","currency":"USD","input_per_million":"2.000000","output_per_million":"4.000000"},
 {"provider_kind":"openai","model":"embed-source","operation":"embeddings","currency":"USD","unit_price":"0.02"},
 {"provider_kind":"anthropic","model":"claude-source","operation":"generation","currency":"USD","input_per_million":"3.000000","output_per_million":"15.000000"}
]}`

func TestPricingSourceLifecycle(t *testing.T) {
	h := pricingHarness(t)
	owner := h.owner()
	fixture := newSourceFixture(t, sourceDocumentV1)

	status, problem, _ := h.request(owner, "POST", "/api/v1/pricing/sources",
		map[string]any{"name": "internal feed", "url": "http://10.0.0.4/prices.json"},
		map[string]string{"Idempotency-Key": uuid.NewString()})
	if status != 422 {
		t.Fatalf("internal source = %d %v", status, problem)
	}
	status, problem, _ = h.request(owner, "POST", "/api/v1/pricing/sources",
		map[string]any{"name": "credentialed feed", "url": "https://user:pass@127.0.0.1/prices.json"},
		map[string]string{"Idempotency-Key": uuid.NewString()})
	if status != 422 {
		t.Fatalf("credentialed source = %d %v", status, problem)
	}

	source := h.want(owner, "POST", "/api/v1/pricing/sources",
		map[string]any{"name": "vendor feed", "url": fixture.URL + "/prices.json"},
		map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	sourceID := source["id"].(string)

	refresh := h.want(owner, "POST", "/api/v1/pricing/sources/"+sourceID+"/refresh", nil, nil, 200)
	snapshot := refresh["snapshot"].(map[string]any)
	if len(snapshot["sha256"].(string)) != 64 || snapshot["price_count"].(float64) != 2 || snapshot["currency"] != "USD" {
		t.Fatalf("snapshot = %v", snapshot)
	}
	diff := refresh["diff"].(map[string]any)
	if diff["added_count"].(float64) != 2 || diff["removed_count"].(float64) != 0 || diff["changed_count"].(float64) != 0 {
		t.Fatalf("first diff = %v", diff)
	}
	snapshotID := snapshot["id"].(string)

	refresh = h.want(owner, "POST", "/api/v1/pricing/sources/"+sourceID+"/refresh", nil, nil, 200)
	if refresh["snapshot"].(map[string]any)["id"] != snapshotID {
		t.Fatalf("same digest stored a second snapshot")
	}

	published := h.want(owner, "POST", "/api/v1/pricing/source-snapshots/"+snapshotID+"/publish",
		map[string]any{}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	if published["source_snapshot_id"] != snapshotID {
		t.Fatalf("published revision = %v", published)
	}
	revisions := h.want(owner, "GET", "/api/v1/pricing/revisions", nil, nil, 200)
	latest := revisions["items"].([]any)[0].(map[string]any)
	if latest["source_name"] != "vendor feed" || latest["source_snapshot_id"] != snapshotID {
		t.Fatalf("revision provenance = %v", latest)
	}
	if len(latest["prices"].([]any)) != 2 {
		t.Fatalf("revision prices = %v", latest["prices"])
	}

	fixture.document.Store(sourceDocumentV2)
	refresh = h.want(owner, "POST", "/api/v1/pricing/sources/"+sourceID+"/refresh", nil, nil, 200)
	snapshot = refresh["snapshot"].(map[string]any)
	if snapshot["id"] == snapshotID {
		t.Fatalf("changed document reused a snapshot")
	}
	diff = refresh["diff"].(map[string]any)
	if diff["added_count"].(float64) != 1 || diff["changed_count"].(float64) != 1 || diff["removed_count"].(float64) != 0 {
		t.Fatalf("second diff = %v", diff)
	}
	snapshotID = snapshot["id"].(string)

	published = h.want(owner, "POST", "/api/v1/pricing/source-snapshots/"+snapshotID+"/publish",
		map[string]any{"overrides": []any{
			map[string]any{"provider_kind": "openai", "model": "gpt-source", "operation": "generation",
				"currency": "USD", "input_per_million": "9.000000", "output_per_million": "9.000000"},
			map[string]any{"provider_kind": "openai", "model": "embed-source", "operation": "embeddings",
				"currency": "USD"},
		}}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	prices := published["prices"].([]any)
	if len(prices) != 2 {
		t.Fatalf("merged prices = %v", prices)
	}
	for _, entry := range prices {
		price := entry.(map[string]any)
		if price["model"] == "embed-source" {
			t.Fatalf("removal override left its entry: %v", price)
		}
		if price["model"] == "gpt-source" && price["input_per_million"] != "9.000000" {
			t.Fatalf("rate override did not apply: %v", price)
		}
	}

	snapshots := h.want(owner, "GET", "/api/v1/pricing/sources/"+sourceID+"/snapshots", nil, nil, 200)
	if len(snapshots["items"].([]any)) != 2 {
		t.Fatalf("snapshots = %v", snapshots)
	}
	revisions = h.want(owner, "GET", "/api/v1/pricing/revisions", nil, nil, 200)
	if len(revisions["items"].([]any)) != 2 {
		t.Fatalf("revisions = %v", revisions)
	}
}

func TestPricingSourceFetchFailures(t *testing.T) {
	h := pricingHarness(t)
	owner := h.owner()

	create := func(name, url string) map[string]any {
		return h.want(owner, "POST", "/api/v1/pricing/sources",
			map[string]any{"name": name, "url": url},
			map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	}
	refresh := func(source map[string]any, want int) map[string]any {
		status, body, _ := h.request(owner, "POST", "/api/v1/pricing/sources/"+source["id"].(string)+"/refresh", nil, nil)
		if status != want {
			t.Fatalf("refresh = %d, want %d: %v", status, want, body)
		}
		return body
	}

	oversized := newSourceFixture(t, `{"currency":"USD","prices":[]}`+strings.Repeat(" ", 4<<20))
	source := create("oversized feed", oversized.URL+"/prices.json")
	refresh(source, 413)

	badJSON := newSourceFixture(t, `{"unexpected":true}`)
	source = create("schema feed", badJSON.URL+"/prices.json")
	refresh(source, 422)

	badCurrency := newSourceFixture(t, `{"currency":"EUR","prices":[{"provider_kind":"openai","model":"m","operation":"generation","currency":"USD","input_per_million":"1.000000"}]}`)
	source = create("currency feed", badCurrency.URL+"/prices.json")
	refresh(source, 422)

	concatenated := newSourceFixture(t, `{"currency":"USD","prices":[]}{"currency":"USD","prices":[]}`)
	source = create("concatenated feed", concatenated.URL+"/prices.json")
	refresh(source, 422)

	trailing := newSourceFixture(t, `{"currency":"USD","prices":[]} true`)
	source = create("trailing feed", trailing.URL+"/prices.json")
	refresh(source, 422)

	wrongType := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "not json")
	}))
	defer wrongType.Close()
	source = create("media feed", wrongType.URL+"/prices.json")
	refresh(source, 415)

	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer dead.Close()
	source = create("dead feed", dead.URL+"/prices.json")
	refresh(source, 502)

	snapshots := h.want(owner, "GET", "/api/v1/pricing/sources/"+source["id"].(string)+"/snapshots", nil, nil, 200)
	if len(snapshots["items"].([]any)) != 0 {
		t.Fatalf("failed refresh stored snapshots: %v", snapshots)
	}
}

// signedCatalogFixture serves a signed reference catalog and its detached
// signature, as a static host such as raw.githubusercontent.com does.
type signedCatalogFixture struct {
	*httptest.Server
	document, signature atomic.Value
}

func newSignedCatalogFixture(t *testing.T) *signedCatalogFixture {
	f := &signedCatalogFixture{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		switch r.URL.Path {
		case "/catalog.json":
			w.Write(f.document.Load().([]byte))
		case "/catalog.json.sig":
			w.Write(f.signature.Load().([]byte))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func catalogFor(t *testing.T, published time.Time, input string) []byte {
	t.Helper()
	provenance := catalog.Provenance{SourceURL: "https://example.test/pricing", ObservedAt: published}
	rate, output := catalog.Decimal(input), catalog.Decimal("15")
	toolFee := catalog.Decimal("0.01")
	batch := catalog.Decimal("0.5")
	window := catalog.Decimal("0.75")
	document, err := catalog.Encode(&catalog.Catalog{APIVersion: catalog.APIVersion, PublishedAt: published, Currency: "USD", Vendors: []catalog.Vendor{{ID: "anthropic", Name: "Anthropic", Models: []catalog.Model{{
		ID: "claude-catalog", Aliases: []string{"claude-catalog-20261001"}, InputModalities: []string{"text"}, OutputModalities: []string{"text"}, Provenance: provenance,
		Prices: []catalog.Price{{
			Operation: "generation", InputPerMillion: &rate, OutputPerMillion: &output,
			ToolCalls: map[string]catalog.Decimal{"web_search": toolFee}, BatchMultiplier: &batch,
			Tiers:           []catalog.Tier{{AboveInputTokens: 100000, InputPerMillion: &rate, OutputPerMillion: &output}},
			Windows:         &catalog.Windows{TimeZone: "UTC", Periods: []catalog.WindowPeriod{{Days: []int{6, 7}, Start: "00:00", End: "24:00", Multiplier: window}}},
			Unrepresentable: []catalog.Unrepresentable{{Component: "service_tier", Detail: "A priority tier is billed at 1.5x."}},
			Provenance:      provenance}},
	}}}}})
	if err != nil {
		t.Fatal(err)
	}
	return document
}

// TestCatalogPricingSourceLifecycle covers a signed reference catalog as a
// pricing source: refresh verifies the signature, records which catalog a
// snapshot came from, refuses an older signed catalog, and the snapshot
// publishes like any other.
func TestCatalogPricingSourceLifecycle(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keys := signing.MustKeyring(signing.Key{ID: "test-release", Public: public})
	sign := func(document []byte) []byte {
		signature, err := signing.Sign(nil, "test-release", private, document)
		if err != nil {
			t.Fatal(err)
		}
		return signature
	}
	h := newAccessHarness(t)
	policy := alertPolicy()
	h.Server.Egress = policy
	mux := http.NewServeMux()
	management.Register(mux)
	h.Server.Register(mux)
	(&usage.Server{Access: h.Server, VendorKind: repVendorKind, Egress: policy, CatalogKeys: keys}).Register(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	h.HTTP = server
	owner := h.owner()

	fixture := newSignedCatalogFixture(t)
	october := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	current := catalogFor(t, october, "3")
	fixture.document.Store(current)
	fixture.signature.Store(sign(current))

	source := h.want(owner, "POST", "/api/v1/pricing/sources",
		map[string]any{"name": "release catalog", "format": "catalog", "url": fixture.URL + "/catalog.json"},
		map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	if source["format"] != "catalog" {
		t.Fatalf("source = %v", source)
	}
	sourceID := source["id"].(string)
	refresh := h.want(owner, "POST", "/api/v1/pricing/sources/"+sourceID+"/refresh", nil, nil, 200)
	snapshot := refresh["snapshot"].(map[string]any)
	provenance, _ := snapshot["catalog"].(map[string]any)
	if provenance == nil || provenance["key_id"] != "test-release" || provenance["published_at"] != "2026-10-05T12:00:00Z" || snapshot["price_count"].(float64) != 2 {
		t.Fatalf("snapshot = %v", snapshot)
	}
	if unrepresentable := snapshot["document"].(map[string]any)["unrepresentable"].([]any); len(unrepresentable) != 1 {
		t.Fatalf("unrepresentable = %v", unrepresentable)
	}
	// A catalog snapshot carries every price component it lists.
	prices := snapshot["document"].(map[string]any)["prices"].([]any)
	if len(prices) != 2 {
		t.Fatalf("snapshot prices = %v", prices)
	}
	price := prices[0].(map[string]any)
	tiers, _ := price["tiers"].([]any)
	windows, _ := price["windows"].(map[string]any)
	if len(tiers) != 1 || tiers[0].(map[string]any)["above_input_tokens"] != float64(100000) ||
		price["tool_calls"].(map[string]any)["web_search"] != "0.01" ||
		price["batch_multiplier"] != "0.5" ||
		windows["time_zone"] != "UTC" {
		t.Fatalf("snapshot price = %v", price)
	}

	tampered := catalogFor(t, october.Add(time.Hour), "0.3")
	fixture.document.Store(tampered)
	status, problem, _ := h.request(owner, "POST", "/api/v1/pricing/sources/"+sourceID+"/refresh", nil, nil)
	if status != 422 || !strings.HasSuffix(fmt.Sprint(problem["type"]), "/catalog_signature_invalid") {
		t.Fatalf("unsigned edit = %d %v", status, problem)
	}

	newer := catalogFor(t, october.Add(24*time.Hour), "2.5")
	fixture.document.Store(newer)
	fixture.signature.Store(sign(newer))
	h.want(owner, "POST", "/api/v1/pricing/sources/"+sourceID+"/refresh", nil, nil, 200)

	// An attacker replaying the older, validly signed catalog cannot roll the
	// source's prices back.
	fixture.document.Store(current)
	fixture.signature.Store(sign(current))
	status, problem, _ = h.request(owner, "POST", "/api/v1/pricing/sources/"+sourceID+"/refresh", nil, nil)
	if status != 422 || !strings.HasSuffix(fmt.Sprint(problem["type"]), "/catalog_outdated") {
		t.Fatalf("replayed catalog = %d %v", status, problem)
	}

	published := h.want(owner, "POST", "/api/v1/pricing/source-snapshots/"+snapshot["id"].(string)+"/publish",
		map[string]any{}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	if prices := published["prices"].([]any); len(prices) != 2 || prices[0].(map[string]any)["vendor_id"] != "anthropic" {
		t.Fatalf("published = %v", published)
	}

	status, problem, _ = h.request(owner, "PATCH", "/api/v1/pricing/sources/"+sourceID,
		map[string]any{"format": "prices"}, map[string]string{"If-Match": `"` + source["etag"].(string) + `"`})
	if status != 422 {
		t.Fatalf("format change = %d %v", status, problem)
	}
	status, problem, _ = h.request(owner, "POST", "/api/v1/pricing/sources",
		map[string]any{"name": "list without url"}, map[string]string{"Idempotency-Key": uuid.NewString()})
	if status != 422 {
		t.Fatalf("price list without URL = %d %v", status, problem)
	}
}

// componentPrice is one price carrying every H1 component.
func componentPrice() map[string]any {
	return map[string]any{
		"provider_kind": "openai", "model": "component-model", "operation": "generation",
		"currency":                       "USD",
		"input_per_million":              "2.000000000000",
		"cached_input_per_million":       "0.500000000000",
		"output_per_million":             "6.000000000000",
		"audio_input_per_million":        "4.000000000000",
		"cached_audio_input_per_million": "1.000000000000",
		"audio_output_per_million":       "8.000000000000",
		"image_input_per_million":        "3.000000000000",
		"cached_image_input_per_million": "0.750000000000",
		"image_output_per_million":       "12.000000000000",
		"per_request":                    "0.020000000000",
		"batch_multiplier":               "0.500000000000",
		"tool_calls":                     map[string]any{"web_search": "0.010000000000", "file_search": "0.005000000000"},
		"tiers": []any{map[string]any{
			"above_input_tokens":             1000000,
			"input_per_million":              "1.500000000000",
			"cached_input_per_million":       "0.400000000000",
			"output_per_million":             "5.000000000000",
			"audio_input_per_million":        "3.500000000000",
			"cached_audio_input_per_million": "0.900000000000",
			"audio_output_per_million":       "7.000000000000",
			"image_input_per_million":        "2.500000000000",
			"cached_image_input_per_million": "0.600000000000",
			"image_output_per_million":       "10.000000000000"}},
		"windows": map[string]any{"time_zone": "America/New_York", "periods": []any{
			map[string]any{"days": []any{1, 2, 3, 4, 5}, "start": "00:00", "end": "06:00", "multiplier": "0.500000000000"},
			map[string]any{"days": []any{6, 7}, "start": "00:00", "end": "24:00", "multiplier": "0.250000000000"}}},
	}
}

// componentFields are the scalar members of a component-rich price.
func componentFields(entry map[string]any) map[string]string {
	fields := map[string]string{}
	for _, field := range []string{"input_per_million", "cached_input_per_million", "output_per_million",
		"audio_input_per_million", "cached_audio_input_per_million", "audio_output_per_million",
		"image_input_per_million", "cached_image_input_per_million", "image_output_per_million",
		"per_request", "batch_multiplier"} {
		if value, ok := entry[field].(string); ok {
			fields[field] = value
		}
	}
	return fields
}

// checkComponentPrice verifies a component-rich price came back whole.
func checkComponentPrice(t *testing.T, entry map[string]any) {
	t.Helper()
	for field, want := range componentFields(componentPrice()) {
		if entry[field] != want {
			t.Fatalf("price[%s] = %v, want %s in %v", field, entry[field], want, entry)
		}
	}
	tools, ok := entry["tool_calls"].(map[string]any)
	if !ok || len(tools) != 2 || tools["web_search"] != "0.010000000000" {
		t.Fatalf("tool_calls = %v", entry["tool_calls"])
	}
	tiers, ok := entry["tiers"].([]any)
	if !ok || len(tiers) != 1 {
		t.Fatalf("tiers = %v", entry["tiers"])
	}
	tier := tiers[0].(map[string]any)
	if tier["above_input_tokens"].(float64) != 1000000 || tier["input_per_million"] != "1.500000000000" {
		t.Fatalf("tier = %v", tier)
	}
	windows, ok := entry["windows"].(map[string]any)
	if !ok || windows["time_zone"] != "America/New_York" ||
		len(windows["periods"].([]any)) != 2 {
		t.Fatalf("windows = %v", entry["windows"])
	}
}

func TestPricingComponentsRoundTrip(t *testing.T) {
	h := pricingHarness(t)
	owner := h.owner()

	// A revision carries every component through create and list.
	body := map[string]any{
		"effective_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"prices":       []any{componentPrice()},
	}
	created := h.want(owner, "POST", "/api/v1/pricing/revisions", body, idem("components-create"), 201)
	prices := created["prices"].([]any)
	if len(prices) != 1 {
		t.Fatalf("prices = %v", prices)
	}
	checkComponentPrice(t, prices[0].(map[string]any))
	revisions := h.want(owner, "GET", "/api/v1/pricing/revisions", nil, nil, 200)
	listed := revisions["items"].([]any)[0].(map[string]any)
	checkComponentPrice(t, listed["prices"].([]any)[0].(map[string]any))

	// A prices-format source round-trips them through refresh and publish.
	document, _ := json.Marshal(map[string]any{"currency": "USD", "prices": []any{componentPrice()}})
	fixture := newSourceFixture(t, string(document))
	source := h.want(owner, "POST", "/api/v1/pricing/sources",
		map[string]any{"name": "component feed", "url": fixture.URL + "/prices.json"},
		map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	refresh := h.want(owner, "POST", "/api/v1/pricing/sources/"+source["id"].(string)+"/refresh", nil, nil, 200)
	snapshot := refresh["snapshot"].(map[string]any)
	// The stored snapshot document holds the components too.
	var storedText string
	if err := h.Pool.QueryRow(t.Context(),
		`SELECT document::text FROM olp.pricing_source_snapshots WHERE id=$1::uuid`,
		snapshot["id"].(string)).Scan(&storedText); err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	var stored map[string]any
	if err := json.Unmarshal([]byte(storedText), &stored); err != nil {
		t.Fatalf("snapshot document: %v", err)
	}
	checkComponentPrice(t, stored["prices"].([]any)[0].(map[string]any))
	published := h.want(owner, "POST", "/api/v1/pricing/source-snapshots/"+snapshot["id"].(string)+"/publish",
		map[string]any{}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	checkComponentPrice(t, published["prices"].([]any)[0].(map[string]any))

	// Configuration export, plan and apply carry the components without drift.
	exported := h.want(owner, "GET", "/api/v1/configuration/export", nil, nil, 200)
	docEntry := exported["document"].(map[string]any)["pricing"].(map[string]any)["prices"].([]any)[0].(map[string]any)
	checkComponentPrice(t, docEntry)
	planned := h.want(owner, "POST", "/api/v1/configuration/plan",
		map[string]any{"document": exported["document"]}, nil, 200)
	for _, item := range planned["actions"].([]any) {
		action := item.(map[string]any)
		if action["kind"] == "pricing" && action["action"] != "noop" {
			t.Fatalf("export -> plan sees a pricing change: %v", action)
		}
	}
	h.want(owner, "POST", "/api/v1/configuration/apply",
		map[string]any{"document": exported["document"]}, idem("components-apply"), 200)
	revisions = h.want(owner, "GET", "/api/v1/pricing/revisions", nil, nil, 200)
	var found bool
	for _, raw := range revisions["items"].([]any) {
		for _, rawPrice := range raw.(map[string]any)["prices"].([]any) {
			price := rawPrice.(map[string]any)
			if price["model"] == "component-model" {
				checkComponentPrice(t, price)
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("applied configuration lost the component price: %v", revisions)
	}
}
