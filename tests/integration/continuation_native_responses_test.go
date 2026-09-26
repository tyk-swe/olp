//go:build integration

package integration_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestStrictNativeResponseContinuationKeepsHistoricalProvider(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	historical := newStrictProviderFixture(t, "azure-v1-responses")
	replacement := newStrictProviderFixture(t, "azure-v1-responses")
	options := map[string]any{"operation_defaults": map[string]any{"generation": map[string]any{"dialect": "openai-responses", "values": map[string]any{"max_output_tokens": 32}, "native_options": map[string]any{"historical_marker": "original"}}}}
	slug, _ := publishStrictProvider(t, h, owner, historical, options, nil, "strict")
	key := stateKey(t, h, owner, slug, true)
	h.refresh()
	status, first, _ := h.gatewayRaw("POST", "/v1/responses", key, strings.NewReader(`{"model":"`+slug+`","input":"native first","store":true}`), map[string]string{"Content-Type": "application/json"})
	if status != 200 {
		t.Fatalf("first: %d %s", status, first)
	}
	var response struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(first, &response) != nil || !strings.HasPrefix(response.ID, "strict_response_") {
		t.Fatalf("missing strict local identifier: %s", first)
	}
	var encrypted bool
	if err := h.Pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM olp.provider_resources r JOIN olp.secrets s ON s.id=r.id WHERE r.kind='strict_response' AND r.contract_version='native-responses-v1' AND s.purpose='provider_continuation')`).Scan(&encrypted); err != nil || !encrypted {
		t.Fatalf("contract not committed: %t %v", encrypted, err)
	}
	oldCalls := len(historical.captured())
	providerPath := "/api/v1/providers/" + historical.providerID
	detail := h.want(owner, "GET", providerPath, nil, nil, 200)
	slots := h.want(owner, "GET", providerPath+"/credential-slots", nil, nil, 200)
	oldCredential := slots["items"].([]any)[0].(map[string]any)["credential_version_id"].(string)
	configuration := detail["configuration"].(map[string]any)
	configuration["endpoint"] = replacement.URL
	configured := configuration["options"].(map[string]any)
	defaults := configured["operation_defaults"].(map[string]any)["generation"].(map[string]any)
	defaults["values"] = map[string]any{"max_output_tokens": 999}
	defaults["native_options"] = map[string]any{"historical_marker": "replacement"}
	detail = h.want(owner, "PATCH", providerPath, map[string]any{"name": "Replacement serving config", "configuration": configuration}, etagHeader(detail), 200)
	// Existing certification is connection-specific; certify the new endpoint
	// through the public management boundary before publishing the changed config.
	modelID := h.want(owner, "GET", providerPath+"/models", nil, nil, 200)["items"].([]any)[0].(map[string]any)["id"].(string)
	h.want(owner, "POST", providerPath+"/models/"+modelID+"/certify", nil, etagHeader(detail), 200)
	detail = h.want(owner, "GET", providerPath, nil, nil, 200)
	h.want(owner, "POST", providerPath+"/activate", nil, withMatch(detail, idem(uuid.NewString())), 200)
	h.refresh()
	beforeNew := len(replacement.captured())
	// Recreate the gateway dependencies and manager against the same durable
	// authority, without retaining the old runtime/template caches.
	h = newAccessHarnessOn(t, h.Pool, h.DBURL)
	h.refresh()
	continued := `{"model":"` + slug + `","input":[{"type":"function_call_output","call_id":"native-call","output":"native result"}],"previous_response_id":"` + response.ID + `","store":false}`
	status, next, _ := h.gatewayRaw("POST", "/v1/responses", key, strings.NewReader(continued), map[string]string{"Content-Type": "application/json"})
	if status != 200 {
		t.Fatalf("historical next: %d %s", status, next)
	}
	if len(historical.captured()) != oldCalls+1 || len(replacement.captured()) != beforeNew {
		t.Fatal("retained response substituted current provider endpoint")
	}
	sent := historical.captured()[oldCalls].body
	if !bytes.Contains(sent, []byte(`"max_output_tokens":32`)) || !bytes.Contains(sent, []byte(`"historical_marker":"original"`)) || bytes.Contains(sent, []byte(response.ID)) {
		t.Fatalf("historical native invocation changed: %s", sent)
	}
	detail = h.want(owner, "GET", providerPath, nil, nil, 200)
	h.want(owner, "POST", providerPath+"/credentials/"+oldCredential+"/revoke", nil, withMatch(detail, idem(uuid.NewString())), 200)
	h.refresh()
	status, rejected, _ := h.gatewayRaw("POST", "/v1/responses", key, strings.NewReader(continued), map[string]string{"Content-Type": "application/json"})
	if status != 409 || len(historical.captured()) != oldCalls+1 || !bytes.Contains(rejected, []byte("provider_resource_credential_unavailable")) {
		t.Fatalf("revoked historical credential: %d %s", status, rejected)
	}
}
