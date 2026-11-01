//go:build integration

package integration_test

import (
	"strconv"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/usage"
)

// Anthropic surface renders the same refusal in its own envelope.

// No price published: the bound is unknowable, so the ceiling refuses.

// A committed provider's bound is zero and passes any ceiling.

// The pricing quote prices one usage the way accounting records it: discount,
// commitment and charge all match a persisted fact for the same usage.
func TestPricingQuoteMatchesAccounting(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	up := newVendor(t)
	created := h.want(owner, "POST", "/api/v1/providers", map[string]any{
		"name": "Quote vendor", "model": vendorModel, "credential": vendorSecret,
		"configuration": map[string]any{"kind": "openai_compatible", "auth_mode": "api_key", "endpoint": up.URL + "/v1"},
	}, idem("quote-provider"), 201)
	providerID := created["id"].(string)
	h.want(owner, "POST", "/api/v1/pricing/revisions", map[string]any{"effective_at": time.Now().UTC().Format(time.RFC3339Nano), "prices": []any{map[string]any{
		"provider_kind": "openai_compatible", "provider_id": providerID, "model": vendorModel,
		"operation": "generation", "currency": "USD", "input_per_million": "1", "output_per_million": "10"}}}, idem("quote-price"), 201)
	// An installation card at 2x doubles the charge over cost.
	card := h.want(owner, "POST", "/api/v1/rate-cards", map[string]any{"owner": map[string]any{"kind": "installation"}}, idem("quote-card"), 201)
	h.want(owner, "POST", "/api/v1/rate-cards/"+card["id"].(string)+"/revisions", map[string]any{"markup": "2"}, idem("quote-card-revision"), 201)

	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "quote", "scopes": []string{"inference"}}, idem("quote-key"), 201)
	usageBody := map[string]any{"input_tokens": 1000, "output_tokens": 100}
	quote := h.want(owner, "POST", "/api/v1/pricing/quote", map[string]any{
		"provider_id": providerID, "model": vendorModel, "operation": "generation", "usage": usageBody,
	}, nil, 200)
	if quote["cost"] == nil || quote["charge"] == nil || quote["currency"] != "USD" || quote["complete"] != true {
		t.Fatalf("quote: %v", quote)
	}
	cost, _ := strconv.ParseFloat(quote["cost"].(string), 64)
	charge, _ := strconv.ParseFloat(quote["charge"].(string), 64)
	if charge != cost*2 {
		t.Fatalf("charge %v is not cost %v under the 2x card", charge, cost)
	}
	// Persist a real event for the same provider and usage, and read the fact
	// back: the quote and the fact must price identically.
	fixture := acctFixture{Pool: h.Pool, Key: key["id"].(string)}
	event := acctEvent(t, fixture, acctEventOptions{Attempts: []usage.Attempt{
		acctAttempt(t, providerID, 1, vendorModel, 200, acctObserved(1000, 100, nil, nil)),
	}})
	acctPersist(t, fixture, event)
	var storedCost, storedCharge *string
	if err := h.Pool.QueryRow(t.Context(), `SELECT estimated_cost::text, charge::text FROM olp.attempt_usage_facts WHERE request_id = $1::uuid`, event.RequestID).Scan(&storedCost, &storedCharge); err != nil {
		t.Fatalf("fact: %v", err)
	}
	if storedCost == nil || storedCharge == nil ||
		quote["cost"].(string) != *storedCost || quote["charge"].(string) != *storedCharge {
		t.Fatalf("quote %v/%v disagrees with accounting %v/%v", quote["cost"], quote["charge"], storedCost, storedCharge)
	}
}

// The fallback route's target is priced high; the primary is priced low
// enough to plan but will fail in dispatch.

// Cheap on the primary, expensive on the fallback target.

func TestRateCardSummariesTrackBillingOrder(t *testing.T) {
	for _, tc := range []struct {
		name            string
		firstOffset     time.Duration
		secondOffset    time.Duration
		currentRevision int
		count           int
	}{
		{"effective-order", -time.Hour, -2 * time.Hour, 1, 2},
		{"effective-tie", -time.Hour, -time.Hour, 2, 1},
		{"future-only", time.Hour, 2 * time.Hour, 0, 1},
		{"latest-is-current", -2 * time.Hour, -time.Hour, 2, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := repHarness(t)
			owner := h.owner()
			card := h.want(owner, "POST", "/api/v1/rate-cards", map[string]any{"owner": map[string]any{"kind": "installation"}}, idem("summary-card"), 201)
			cardID := card["id"].(string)
			first := h.want(owner, "POST", "/api/v1/rate-cards/"+cardID+"/revisions", map[string]any{"markup": "2"}, idem("summary-first"), 201)
			second := h.want(owner, "POST", "/api/v1/rate-cards/"+cardID+"/revisions", map[string]any{"markup": "3"}, idem("summary-second"), 201)
			base := time.Now().UTC()
			if _, err := h.Pool.Exec(t.Context(), `UPDATE olp.rate_card_revisions SET effective_at=CASE WHEN revision=1 THEN $2::timestamptz ELSE $3::timestamptz END WHERE rate_card_id=$1::uuid`, cardID, base.Add(tc.firstOffset), base.Add(tc.secondOffset)); err != nil {
				t.Fatal(err)
			}
			detail := h.want(owner, "GET", "/api/v1/rate-cards/"+cardID, nil, nil, 200)
			listed := h.want(owner, "GET", "/api/v1/rate-cards", nil, nil, 200)
			for _, summary := range []map[string]any{detail, listed["items"].([]any)[0].(map[string]any)} {
				revisions := summary["revisions"].([]any)
				if len(revisions) != tc.count {
					t.Fatalf("%s revisions=%v", tc.name, revisions)
				}
				if revisions[0].(map[string]any)["id"] != second["id"] {
					t.Fatalf("latest revision missing: %v", revisions)
				}
				if tc.currentRevision == 1 && revisions[1].(map[string]any)["id"] != first["id"] {
					t.Fatalf("active revision missing: %v", revisions)
				}
			}
		})
	}
}
