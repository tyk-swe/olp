//go:build integration

package integration_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/catalog"
	"github.com/tyk-swe/olp/internal/management"
	"github.com/tyk-swe/olp/internal/signing"
	"github.com/tyk-swe/olp/internal/usage"
)

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
