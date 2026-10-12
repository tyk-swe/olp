//go:build integration

package integration_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestConfigurationChargePromotion exports discounts and rate cards, applies
// them to a fresh installation, then exports again for a no-change plan. A
// document that omits them merges: nothing is ended or neutralized.
func TestConfigurationChargePromotion(t *testing.T) {
	source := newAccessHarness(t)
	sourceOwner := source.owner()
	sourceUsage := usageMuxFor(source)
	usageWant := func(h *accessHarness, usage *httptest.Server, owner *browser, method, path string, body any, headers map[string]string, want int) map[string]any {
		status, out := h.browserOn(usage.URL, owner, method, path, body, headers)
		if status != want {
			t.Fatalf("%s %s: %d %v, want %d", method, path, status, out, want)
		}
		return out
	}
	providers := createScopedProvider(source, sourceOwner, "Chargeable", "https://api.example.test/v1", nil, 201)
	providerID := providers["id"].(string)
	usageWant(source, sourceUsage, sourceOwner, http.MethodPost, "/api/v1/pricing/discounts",
		map[string]any{"provider_kind": "openai_compatible", "provider_id": providerID, "multiplier": "0.8"},
		idem("charge-discount"), 201)
	org := source.want(sourceOwner, http.MethodPost, "/api/v1/organizations",
		map[string]any{"name": "Charge org"}, idem("charge-org"), 201)["id"].(string)
	projectID := source.want(sourceOwner, http.MethodPost, "/api/v1/organizations/"+org+"/projects",
		map[string]any{"name": "Charge project"}, idem("charge-project"), 201)["id"].(string)
	card := usageWant(source, sourceUsage, sourceOwner, http.MethodPost, "/api/v1/rate-cards",
		map[string]any{"owner": map[string]any{"kind": "project", "id": projectID}},
		idem("charge-card"), 201)
	usageWant(source, sourceUsage, sourceOwner, http.MethodPost,
		"/api/v1/rate-cards/"+card["id"].(string)+"/revisions",
		map[string]any{"markup": "1.5", "prices": []any{repPrice("openai_compatible", "promoted-model", "generation")}},
		idem("charge-revision"), 201)

	exported := source.want(sourceOwner, http.MethodGet, "/api/v1/configuration/export", nil, nil, 200)
	document := exported["document"].(map[string]any)
	discounts, _ := document["discounts"].([]any)
	if len(discounts) != 1 {
		t.Fatalf("exported discounts = %v", discounts)
	}
	if discounts[0].(map[string]any)["provider"].(string) != "Chargeable" {
		t.Fatalf("discount provider ref = %v", discounts[0])
	}
	cards, _ := document["rate_cards"].([]any)
	if len(cards) != 1 {
		t.Fatalf("exported rate cards = %v", cards)
	}
	owner := cards[0].(map[string]any)["owner"].(map[string]any)
	if owner["kind"].(string) != "project" || owner["project"].(string) != "Charge project" {
		t.Fatalf("card owner = %v", owner)
	}

	destination := newAccessHarness(t)
	destinationOwner := destination.owner()
	destinationUsage := usageMuxFor(destination)
	planBody := map[string]any{"document": document}
	planned := destination.want(destinationOwner, http.MethodPost, "/api/v1/configuration/plan", planBody, nil, 200)
	found := map[string]bool{}
	var bindingKey string
	for _, item := range planned["actions"].([]any) {
		entry := item.(map[string]any)
		found[entry["kind"].(string)+"/"+entry["action"].(string)] = true
	}
	for _, item := range planned["blockers"].([]any) {
		entry := item.(map[string]any)
		if entry["detail"] == "secret_binding_required" {
			bindingKey = entry["key"].(string)
		}
	}
	if !found["discount/create"] || !found["rate_card/create"] {
		t.Fatalf("plan must create the discount and card: %v", planned["actions"])
	}
	applyBody := planBody
	if bindingKey != "" {
		applyBody = map[string]any{"document": document, "secret_bindings": map[string]any{bindingKey: vendorSecret}}
	}
	destination.want(destinationOwner, http.MethodPost, "/api/v1/configuration/apply", applyBody, idem("charge-apply"), 200)

	listed := usageWant(destination, destinationUsage, destinationOwner, http.MethodGet,
		"/api/v1/pricing/discounts", nil, nil, 200)
	if len(listed["items"].([]any)) != 1 {
		t.Fatalf("destination discounts = %v", listed["items"])
	}
	listedCards := usageWant(destination, destinationUsage, destinationOwner, http.MethodGet,
		"/api/v1/rate-cards", nil, nil, 200)
	if len(listedCards["items"].([]any)) != 1 {
		t.Fatalf("destination cards = %v", listedCards["items"])
	}

	reexported := destination.want(destinationOwner, http.MethodGet, "/api/v1/configuration/export", nil, nil, 200)
	reDoc := reexported["document"].(map[string]any)
	rePlan := destination.want(destinationOwner, http.MethodPost, "/api/v1/configuration/plan",
		map[string]any{"document": reDoc}, nil, 200)
	for _, item := range rePlan["actions"].([]any) {
		entry := item.(map[string]any)
		if kind := entry["kind"].(string); (kind == "discount" || kind == "rate_card") && entry["action"].(string) != "noop" {
			t.Fatalf("round trip must not change charges: %v", entry)
		}
	}
	discountsAgain, _ := reDoc["discounts"].([]any)
	if len(discountsAgain) != 1 {
		t.Fatalf("re-exported discounts = %v", discountsAgain)
	}

	// A document without discounts or rate cards leaves them untouched, like
	// every other collection member.
	changed := map[string]any{}
	encoded, _ := json.Marshal(reDoc)
	if err := json.Unmarshal(encoded, &changed); err != nil {
		t.Fatal(err)
	}
	delete(changed, "discounts")
	delete(changed, "rate_cards")
	var discountRows, revisionRows int64
	destination.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.pricing_discounts`).Scan(&discountRows)
	destination.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.rate_card_revisions`).Scan(&revisionRows)
	destination.want(destinationOwner, http.MethodPost, "/api/v1/configuration/apply",
		map[string]any{"document": changed}, idem("charge-strip"), 200)
	var nowDiscounts, nowRevisions int64
	destination.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.pricing_discounts`).Scan(&nowDiscounts)
	destination.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.rate_card_revisions`).Scan(&nowRevisions)
	if nowDiscounts != discountRows || nowRevisions != revisionRows {
		t.Fatalf("an omitted document touched discounts %d→%d or revisions %d→%d",
			discountRows, nowDiscounts, revisionRows, nowRevisions)
	}

	// Ending a discount promotes as a listed multiplier-one scope; the card is
	// neutralized by a listed markup-one revision with no prices.
	changed["discounts"] = []any{map[string]any{
		"provider_kind": "openai_compatible", "provider": "Chargeable", "multiplier": "1"}}
	changed["rate_cards"] = []any{map[string]any{
		"owner": map[string]any{"kind": "project", "project": "Charge project"}, "markup": "1"}}
	destination.want(destinationOwner, http.MethodPost, "/api/v1/configuration/apply",
		map[string]any{"document": changed}, idem("charge-end"), 200)
	endList := usageWant(destination, destinationUsage, destinationOwner, http.MethodGet,
		"/api/v1/pricing/discounts", nil, nil, 200)
	items := endList["items"].([]any)
	if len(items) < 2 {
		t.Fatalf("discounts after ending = %v", items)
	}
	if newest := items[0].(map[string]any); newest["multiplier"].(string) != "1.000000000000" {
		t.Fatalf("scope ended with multiplier %v", newest["multiplier"])
	}
	if err := destination.Pool.QueryRow(t.Context(),
		`SELECT count(*) FROM olp.rate_card_revisions`).Scan(&nowRevisions); err != nil {
		t.Fatal(err)
	}
	if nowRevisions != revisionRows+1 {
		t.Fatalf("card revisions = %d, want a neutral revision", nowRevisions)
	}

	// The stale-digest check exports inside the apply transaction, where every
	// export query shares one connection. It must refuse 409, not 503.
	status, refused, _ := destination.request(destinationOwner, http.MethodPost, "/api/v1/configuration/apply",
		map[string]any{"document": reDoc, "expected_digest": "stale-digest"}, idem("charge-stale"))
	if status != http.StatusConflict {
		t.Fatalf("a stale digest over pricing state must refuse 409: %d %v", status, refused)
	}
}
