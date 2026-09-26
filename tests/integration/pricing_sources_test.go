//go:build integration

package integration_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/management"
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
 {"provider_kind":"openai","model":"embed-source","operation":"embeddings","currency":"USD","unit_price":"0.020000000000"}
]}`

const sourceDocumentV2 = `{"currency":"USD","prices":[
 {"provider_kind":"openai","model":"gpt-source","operation":"generation","currency":"USD","input_per_million":"2.000000","output_per_million":"4.000000"},
 {"provider_kind":"openai","model":"embed-source","operation":"embeddings","currency":"USD","unit_price":"0.020000000000"},
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
