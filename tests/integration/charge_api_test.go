//go:build integration

package integration_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
)

func idemKey(name string) map[string]string { return map[string]string{"Idempotency-Key": name} }

// TestDiscountsOverHTTP creates, lists and replays pricing discounts, refusing
// a scope whose kind and vendor disagree and members without the settings
// permission.
func TestDiscountsOverHTTP(t *testing.T) {
	f := repSetup(t)
	created := f.h.want(f.Owner, http.MethodPost, "/api/v1/pricing/discounts",
		map[string]any{"provider_kind": "openai", "provider_id": f.P1, "multiplier": "0.9"},
		idemKey("discount-first"), 201)
	if created["multiplier"].(string) != "0.900000000000" {
		t.Fatalf("multiplier = %v", created["multiplier"])
	}
	if created["provider_id"].(string) != f.P1 || created["provider_name"].(string) != "primary" {
		t.Fatalf("scope = %v", created)
	}
	if location := created["id"].(string); location == "" {
		t.Fatal("no discount id returned")
	}

	// A replayed key returns the stored reply and creates nothing else.
	replayed := f.h.want(f.Owner, http.MethodPost, "/api/v1/pricing/discounts",
		map[string]any{"provider_kind": "openai", "provider_id": f.P1, "multiplier": "0.9"},
		idemKey("discount-first"), 201)
	if replayed["id"].(string) != created["id"].(string) {
		t.Fatal("a replayed key created a second discount")
	}
	if total := f.count("SELECT count(*) FROM olp.pricing_discounts"); total != 1 {
		t.Fatalf("discounts = %d, want 1", total)
	}

	listed := f.h.want(f.Owner, http.MethodGet, "/api/v1/pricing/discounts", nil, nil, 200)
	if len(listed["items"].([]any)) != 1 {
		t.Fatalf("listed = %v", listed["items"])
	}

	// A vendor must belong to the declared kind; a plugin needs a provider.
	bad := f.h.want(f.Owner, http.MethodPost, "/api/v1/pricing/discounts",
		map[string]any{"provider_kind": "anthropic", "vendor_id": "openai", "multiplier": "0.5"},
		idemKey("discount-vendor"), 422)
	if code := repProblemCode(t, bad); code != "validation_failed" {
		t.Fatalf("vendor problem = %s", code)
	}
	plugin := f.h.want(f.Owner, http.MethodPost, "/api/v1/pricing/discounts",
		map[string]any{"provider_kind": "plugin", "multiplier": "0.5"},
		idemKey("discount-plugin"), 422)
	if code := repProblemCode(t, plugin); code != "validation_failed" {
		t.Fatalf("plugin problem = %s", code)
	}
	future := f.h.want(f.Owner, http.MethodPost, "/api/v1/pricing/discounts",
		map[string]any{"provider_kind": "openai", "multiplier": "0.75",
			"effective_at": time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)},
		idemKey("discount-past"), 422)
	if code := repProblemCode(t, future); code != "validation_failed" {
		t.Fatalf("past effective problem = %s", code)
	}

	// Settings-free members read but never create.
	viewer := f.h.invite(f.Owner, "viewer@example.test", "viewer")
	f.h.want(viewer, http.MethodGet, "/api/v1/pricing/discounts", nil, nil, 200)
	f.h.want(viewer, http.MethodPost, "/api/v1/pricing/discounts",
		map[string]any{"provider_kind": "openai", "multiplier": "0.5"},
		idemKey("discount-viewer"), 403)
}

// TestRateCardsOverHTTP exercises card creation, ownership conflicts,
// revision publishing, listing and authorization.
func TestRateCardsOverHTTP(t *testing.T) {
	f := repSetup(t)
	h := f.h
	org := h.want(f.Owner, http.MethodPost, "/api/v1/organizations",
		map[string]any{"name": "Charges"}, idemKey("cards-org"), 201)["id"].(string)
	project := h.want(f.Owner, http.MethodPost, "/api/v1/organizations/"+org+"/projects",
		map[string]any{"name": "Billed"}, idemKey("cards-project"), 201)["id"].(string)

	card := h.want(f.Owner, http.MethodPost, "/api/v1/rate-cards",
		map[string]any{"owner": map[string]any{"kind": "installation"}}, idemKey("card-install"), 201)
	if card["owner"].(map[string]any)["kind"].(string) != "installation" {
		t.Fatalf("owner = %v", card["owner"])
	}
	cardID := card["id"].(string)
	duplicate := h.want(f.Owner, http.MethodPost, "/api/v1/rate-cards",
		map[string]any{"owner": map[string]any{"kind": "installation"}}, idemKey("card-install-2"), 409)
	if code := repProblemCode(t, duplicate); code != "rate_card_exists" {
		t.Fatalf("duplicate owner = %s", code)
	}

	// Organization and project cards resolve their owners by id.
	h.want(f.Owner, http.MethodPost, "/api/v1/rate-cards",
		map[string]any{"owner": map[string]any{"kind": "organization", "id": org}}, idemKey("card-org"), 201)
	h.want(f.Owner, http.MethodPost, "/api/v1/rate-cards",
		map[string]any{"owner": map[string]any{"kind": "project", "id": project}}, idemKey("card-project"), 201)

	revisionBody := map[string]any{"markup": "1.25",
		"prices": []any{map[string]any{"provider_kind": "openai", "model": "m", "operation": "generation",
			"currency": "usd", "input_per_million": "5"}}}
	revision := h.want(f.Owner, http.MethodPost, "/api/v1/rate-cards/"+cardID+"/revisions",
		revisionBody, idemKey("card-rev-1"), 201)
	if revision["revision"].(float64) != 1 || revision["markup"].(string) != "1.250000000000" {
		t.Fatalf("revision = %v", revision)
	}
	prices := revision["prices"].([]any)
	if len(prices) != 1 || prices[0].(map[string]any)["currency"].(string) != "USD" {
		t.Fatalf("card prices = %v", prices)
	}

	// Replay stores nothing twice.
	replayed := h.want(f.Owner, http.MethodPost, "/api/v1/rate-cards/"+cardID+"/revisions",
		revisionBody, idemKey("card-rev-1"), 201)
	if replayed["id"].(string) != revision["id"].(string) {
		t.Fatal("a replayed key created a second revision")
	}

	// A provider-scoped card price is refused.
	scoped := h.want(f.Owner, http.MethodPost, "/api/v1/rate-cards/"+cardID+"/revisions",
		map[string]any{"markup": "1.25",
			"prices": []any{map[string]any{"provider_kind": "openai", "provider_id": f.P1, "model": "m",
				"operation": "generation", "currency": "USD", "input_per_million": "5"}}},
		idemKey("card-rev-scoped"), 422)
	if code := repProblemCode(t, scoped); code != "validation_failed" {
		t.Fatalf("scoped price problem = %s", code)
	}

	listed := h.want(f.Owner, http.MethodGet, "/api/v1/rate-cards", nil, nil, 200)
	if len(listed["items"].([]any)) != 3 {
		t.Fatalf("cards = %v", listed["items"])
	}
	revisions := h.want(f.Owner, http.MethodGet, "/api/v1/rate-cards/"+cardID+"/revisions", nil, nil, 200)
	if len(revisions["items"].([]any)) != 1 {
		t.Fatalf("revisions = %v", revisions["items"])
	}
	detail := h.want(f.Owner, http.MethodGet, "/api/v1/rate-cards/"+cardID, nil, nil, 200)
	if len(detail["revisions"].([]any)) != 1 {
		t.Fatalf("card revisions = %v", detail["revisions"])
	}
	h.want(f.Owner, http.MethodGet, "/api/v1/rate-cards/"+uuid.NewString(), nil, nil, 404)
	h.want(f.Owner, http.MethodPost, "/api/v1/rate-cards/"+uuid.NewString()+"/revisions",
		map[string]any{"markup": "2"}, idemKey("card-rev-missing"), 404)

	viewer := f.h.invite(f.Owner, "cards-viewer@example.test", "viewer")
	f.h.want(viewer, http.MethodGet, "/api/v1/rate-cards", nil, nil, 200)
	f.h.want(viewer, http.MethodPost, "/api/v1/rate-cards",
		map[string]any{"owner": map[string]any{"kind": "installation"}}, idemKey("card-viewer"), 403)
	f.h.want(viewer, http.MethodPost, "/api/v1/rate-cards/"+cardID+"/revisions",
		map[string]any{"markup": "2"}, idemKey("card-viewer-rev"), 403)
}
