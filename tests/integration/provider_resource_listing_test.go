//go:build integration

package integration_test

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// Interaction and strict durable identities are listed under the published
// management contract, and each stored kind can be selected.
func TestProviderResourceListingCoversEveryStoredKind(t *testing.T) {
	fixture := newOpenAIFixture(t, "")
	h := newAccessHarness(t)
	owner, provider, slug, _ := provisionOpenAI(t, h, fixture.URL, []any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}}, []string{"generation"})
	var keyID string
	if err := h.Pool.QueryRow(t.Context(), `SELECT id::text FROM olp_go.api_keys ORDER BY created_at DESC LIMIT 1`).Scan(&keyID); err != nil {
		t.Fatal(err)
	}
	contracts := map[string]string{"interaction": "gemini-interaction/v1beta", "strict_file": "native-durable-v1", "strict_batch": "native-durable-v1"}
	for kind, contract := range contracts {
		id := uuid.New()
		if _, err := h.Pool.Exec(t.Context(), `INSERT INTO olp_go.provider_resources
			(id,kind,api_key_id,route_slug,provider_id,provider_revision_id,route_revision_id,slot_id,upstream_id,state,contract_version,expires_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'ready',$10,now()+interval '1 hour')`,
			id, kind, keyID, slug, provider["id"], uuid.New(), uuid.New(), uuid.New(), id.String(), contract); err != nil {
			t.Fatal(err)
		}
	}
	for kind := range contracts {
		items := h.want(owner, "GET", "/api/v3/provider-resources?kind="+kind, nil, nil, http.StatusOK)["items"].([]any)
		if len(items) != 1 || items[0].(map[string]any)["kind"] != kind {
			t.Fatalf("%s listing: %v", kind, items)
		}
	}
	if items := h.want(owner, "GET", "/api/v3/provider-resources", nil, nil, http.StatusOK)["items"].([]any); len(items) != len(contracts) {
		t.Fatalf("unfiltered listing: %v", items)
	}
}
