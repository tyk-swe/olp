//go:build integration

package integration_test

import (
	"encoding/json"
	"math"
	"net/http"
	"net/url"
	"testing"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/limits"
)

func TestAPIKeyListingsPreserveIntegerPrecision(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	expected := map[string]*int64{}
	for _, tokens := range []int64{limits.MaxCounter, limits.MaxCounter - 1, 0} {
		input := map[string]any{"name": "precise token limit"}
		var limit *int64
		if tokens != 0 {
			limit = &tokens
			input["tokens_per_minute"] = tokens
		}
		created := h.want(owner, "POST", "/api/v1/api-keys", input, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
		expected[created["id"].(string)] = limit
	}
	cursor := ""
	for page := 0; page < 3; page++ {
		request, err := http.NewRequest("GET", h.HTTP.URL+"/api/v1/api-keys?limit=1&cursor="+url.QueryEscape(cursor), nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, cookie := range owner.Cookies {
			request.AddCookie(cookie)
		}
		response, err := h.HTTP.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		var listing struct {
			Items []struct {
				ID              string `json:"id"`
				TokensPerMinute *int64 `json:"tokens_per_minute"`
			} `json:"items"`
			NextCursor *string `json:"next_cursor"`
		}
		err = json.NewDecoder(response.Body).Decode(&listing)
		response.Body.Close()
		if response.StatusCode != 200 || err != nil {
			t.Fatalf("typed API-key listing: status %d, decode error %v", response.StatusCode, err)
		}
		if len(listing.Items) != 1 {
			t.Fatalf("page %d returned %d keys, want 1", page, len(listing.Items))
		}
		item := listing.Items[0]
		want, exists := expected[item.ID]
		if !exists {
			t.Fatal("pagination repeated a key or returned an unknown key")
		}
		if (item.TokensPerMinute == nil) != (want == nil) || want != nil && *item.TokensPerMinute != *want {
			t.Fatal("listing changed the token limit")
		}
		delete(expected, item.ID)
		if len(expected) == 0 {
			if listing.NextCursor != nil {
				t.Fatal("the last page returned a cursor")
			}
		} else {
			if listing.NextCursor == nil || *listing.NextCursor == "" {
				t.Fatal("pagination ended before all keys were returned")
			}
			cursor = *listing.NextCursor
		}
	}
}

func TestAPIKeyBudgetAndTokenLimitContract(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	created := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{
		"name": "precise budgets", "daily_cost_limit": "01.50",
		"monthly_cost_limit": "1.000000000001", "tokens_per_minute": int64(math.MaxInt32) + 1,
		"requests_per_minute": math.MaxInt32, "max_concurrency": math.MaxInt32,
	}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	path := "/api/v1/api-keys/" + created["id"].(string)
	secret := created["secret"].(string)
	assertPolicy := func(daily, monthly string, tokens int64) map[string]any {
		t.Helper()
		record := h.want(owner, "GET", path, nil, nil, 200)
		budget := record["budget"].(map[string]any)
		if budget["daily"].(map[string]any)["limit"] != daily || budget["monthly"].(map[string]any)["limit"] != monthly {
			t.Fatal("the API did not preserve the budget amounts")
		}
		// Read durable authority as int64, independently of the HTTP harness's
		// floating-point JSON decoder.
		authority, err := h.authority(secret)
		if err != nil {
			t.Fatal(err)
		}
		policy := authority.Policy
		if policy.TokensPerMinute == nil || *policy.TokensPerMinute != tokens ||
			policy.RequestsPerMinute == nil || *policy.RequestsPerMinute != math.MaxInt32 ||
			policy.MaxConcurrency == nil || *policy.MaxConcurrency != math.MaxInt32 ||
			policy.DailyCostLimit == nil || *policy.DailyCostLimit != daily ||
			policy.MonthlyCostLimit == nil || *policy.MonthlyCostLimit != monthly {
			t.Fatal("the stored policy lost precision or changed an omitted limit")
		}
		return record
	}
	record := assertPolicy("01.50", "1.000000000001", int64(math.MaxInt32)+1)
	h.want(owner, "PATCH", path, map[string]any{
		"daily_cost_limit": "000000000001.000000000001", "monthly_cost_limit": "999999999999.999999999999",
		"tokens_per_minute": limits.MaxCounter,
	}, etagHeader(record), 200)
	record = assertPolicy("000000000001.000000000001", "999999999999.999999999999", limits.MaxCounter)
	for _, patch := range []map[string]any{
		{"tokens_per_minute": 0},
		{"tokens_per_minute": -1},
		{"tokens_per_minute": limits.MaxCounter + 1},
		{"tokens_per_minute": math.MaxInt64},
		{"tokens_per_minute": uint64(math.MaxInt64) + 1},
		{"requests_per_minute": int64(math.MaxInt32) + 1},
		{"max_concurrency": int64(math.MaxInt32) + 1},
		{"daily_cost_limit": "0.000000000000"},
		{"monthly_cost_limit": "1000000000000"},
		{"daily_cost_limit": "1.0000000000001"},
	} {
		h.want(owner, "PATCH", path, patch, etagHeader(record), 422)
		if current := assertPolicy("000000000001.000000000001", "999999999999.999999999999", limits.MaxCounter); current["etag"] != record["etag"] {
			t.Fatal("an invalid policy changed the key")
		}
	}
	headers := etagHeader(record)
	headers["Idempotency-Key"] = uuid.NewString()
	rotated := h.want(owner, "POST", path+"/rotate", map[string]any{
		"daily_cost_limit": " 08 ", "monthly_cost_limit": "0.000000000001",
	}, headers, 200)
	secret = rotated["secret"].(string)
	record = assertPolicy("08", "0.000000000001", limits.MaxCounter)
	h.want(owner, "PATCH", path, map[string]any{
		"daily_cost_limit": nil, "monthly_cost_limit": nil, "tokens_per_minute": nil,
	}, etagHeader(record), 200)
	authority, err := h.authority(secret)
	if err != nil {
		t.Fatal(err)
	}
	if authority.Policy.DailyCostLimit != nil || authority.Policy.MonthlyCostLimit != nil || authority.Policy.TokensPerMinute != nil {
		t.Fatal("explicit null did not clear the limits")
	}
}

func TestAPIKeyResponseMetadataPolicy(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	created := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "metadata default"}, idem("metadata-default"), 201)
	path := "/api/v1/api-keys/" + created["id"].(string)
	secret := created["secret"].(string)
	// The management detail and the durable authority the gateway reads must
	// agree, and every other policy field must survive each change.
	assertMetadata := func(want bool) map[string]any {
		t.Helper()
		record := h.want(owner, "GET", path, nil, nil, 200)
		if record["response_metadata"] != want {
			t.Fatalf("detail response_metadata = %v, want %v", record["response_metadata"], want)
		}
		authority, err := h.authority(secret)
		if err != nil {
			t.Fatal(err)
		}
		if authority.Policy.ResponseMetadata != want {
			t.Fatalf("authority response_metadata = %v, want %v", authority.Policy.ResponseMetadata, want)
		}
		return record
	}
	record := assertMetadata(false)
	// A key stored before the policy existed reads as opted out.
	if _, err := h.Pool.Exec(t.Context(), `UPDATE olp.api_keys SET policy=policy-'response_metadata' WHERE id=$1`, created["id"]); err != nil {
		t.Fatal(err)
	}
	record = assertMetadata(false)
	h.want(owner, "PATCH", path, map[string]any{"response_metadata": "yes"}, etagHeader(record), 422)
	h.want(owner, "PATCH", path, map[string]any{"response_metadata": true, "requests_per_minute": 7}, etagHeader(record), 200)
	record = assertMetadata(true)
	if record["requests_per_minute"] != float64(7) {
		t.Fatalf("opting in changed another limit: %v", record)
	}
	// An omitted field keeps the stored choice.
	h.want(owner, "PATCH", path, map[string]any{"name": "renamed"}, etagHeader(record), 200)
	record = assertMetadata(true)
	headers := etagHeader(record)
	headers["Idempotency-Key"] = uuid.NewString()
	secret = h.want(owner, "POST", path+"/rotate", nil, headers, 200)["secret"].(string)
	record = assertMetadata(true)
	h.want(owner, "PATCH", path, map[string]any{"response_metadata": false}, etagHeader(record), 200)
	assertMetadata(false)

	optedIn := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "metadata opt-in", "response_metadata": true}, idem("metadata-opt-in"), 201)
	path, secret = "/api/v1/api-keys/"+optedIn["id"].(string), optedIn["secret"].(string)
	assertMetadata(true)
}
