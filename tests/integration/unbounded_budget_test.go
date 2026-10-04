//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/connectors"
)

func TestStrictBatchBudgetRejectsBeforeDispatch(t *testing.T) {
	fixture := newOpenAIFixture(t, "")
	h := newAccessHarness(t)
	owner, _, slug, _ := provisionOpenAIWith(t, h, fixture.URL,
		[]any{map[string]any{"operation": "batch", "surface": "openai", "mode": "unary"}, map[string]any{"operation": "embeddings", "surface": "openai", "mode": "unary"}}, []string{"batch", "embeddings"},
		map[string]any{"fidelity": map[string]any{"mode": "strict"}}, map[string]any{"profile_id": "azure-legacy-chat", "profile_revision": "1"})
	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "batch budget", "scopes": []string{"inference"}, "allowed_routes": []string{slug}, "allow_provider_state": true}, idem(uuid.NewString()), 201)
	secret := key["secret"].(string)
	h.refresh()
	input := fmt.Sprintf("{\"custom_id\":\"one\",\"method\":\"POST\",\"url\":\"/v1/embeddings\",\"body\":{\"model\":%q,\"input\":[1,2,3]}}\n", vendorModel)
	status, file := h.uploadTestFile(slug, secret, input)
	if status != http.StatusOK {
		t.Fatalf("upload: %d %v", status, file)
	}
	keyPath := "/api/v1/api-keys/" + key["id"].(string)
	record := h.want(owner, "GET", keyPath, nil, nil, 200)
	h.want(owner, "PATCH", keyPath, map[string]any{"tokens_per_minute": 100}, etagHeader(record), 200)
	h.refresh()
	before := fixture.dials.Load()
	body := []byte(fmt.Sprintf(`{"input_file_id":%q,"endpoint":"/v1/embeddings","completion_window":"24h"}`, file["id"]))
	status, raw, _ := h.gatewayRaw(http.MethodPost, "/v1/batches", secret, strings.NewReader(string(body)), map[string]string{"Content-Type": "application/json"})
	if status != http.StatusBadRequest || !strings.Contains(string(raw), "unbounded_work_budget") || fixture.dials.Load() != before {
		t.Fatalf("batch admission: %d %s calls %d -> %d", status, raw, before, fixture.dials.Load())
	}
}

func TestGeminiLiveBudgetRejectsBeforeDispatch(t *testing.T) {
	h := newAccessHarness(t)
	provider := newGeminiLifecycleProvider(t)
	owner := h.owner()
	slug, _, _ := provisionGeminiLifecycle(t, h, owner, "gemini-live", provider)
	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "Live budget", "scopes": []string{"inference"}, "allowed_routes": []string{slug}, "tokens_per_minute": 100}, idem(uuid.NewString()), 201)
	h.refresh()
	before := len(provider.captured())
	address := "ws" + strings.TrimPrefix(h.HTTP.URL, "http") + "/gemini/ws/" + connectors.GeminiLiveMethod + "?key=" + url.QueryEscape(key["secret"].(string))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	client, _, err := websocket.Dial(ctx, address, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseNow()
	if err := client.Write(ctx, websocket.MessageText, []byte(fmt.Sprintf(`{"setup":{"model":"models/%s"}}`, slug))); err != nil {
		t.Fatal(err)
	}
	_, _, err = client.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusPolicyViolation || len(provider.captured()) != before {
		t.Fatalf("Live budget dispatched: %v calls %d -> %d", err, before, len(provider.captured()))
	}
}
