//go:build integration

package integration_test

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// publishFidelity publishes a new revision of slug with the given mode.
func publishFidelity(t *testing.T, h *accessHarness, owner *browser, slug string, provider any, operations []string, mode string) {
	t.Helper()
	body := fidelityDraft(slug, provider)
	body["operations"] = operations
	body["fidelity"] = map[string]any{"mode": mode}
	draft := h.want(owner, "POST", "/api/v1/route-drafts", body, idem(uuid.NewString()), 201)
	h.want(owner, "POST", "/api/v1/route-drafts/"+draft["id"].(string)+"/activate", nil, withMatch(draft, idem(uuid.NewString())), 200)
	h.refresh()
}

// requireRouteChanged asserts the refusal for a retained resource whose
// contract the route no longer promises.
func requireRouteChanged(t *testing.T, what string, status int, raw []byte, detail string) {
	t.Helper()
	if status != http.StatusConflict || !strings.Contains(string(raw), "provider_resource_unavailable") || !strings.Contains(string(raw), detail) {
		t.Fatalf("%s was not refused after the route changed fidelity: %d %s", what, status, raw)
	}
}

func TestRetainedFilesAndBatchesFollowTheirRouteFidelity(t *testing.T) {
	fixture := newOpenAIFixture(t, "")
	h := newAccessHarness(t)
	operations := []string{"batch", "embeddings"}
	owner, provider, slug, _ := provisionOpenAIWith(t, h, fixture.URL,
		[]any{map[string]any{"operation": "batch", "surface": "openai", "mode": "unary"}, map[string]any{"operation": "embeddings", "surface": "openai", "mode": "unary"}}, operations,
		map[string]any{"fidelity": map[string]any{"mode": "strict"}},
		map[string]any{"profile_id": "azure-legacy-chat", "profile_revision": "1"})
	key := stateKey(t, h, owner, slug, true)
	h.refresh()
	input := `{"custom_id":"first","method":"POST","url":"/v1/embeddings","body":{"model":"` + vendorModel + `","input":"alpha"}}` + "\n"
	upload := func() string {
		t.Helper()
		status, uploaded := h.uploadTestFile(slug, key, input)
		if status != http.StatusOK {
			t.Fatalf("upload: %d %v", status, uploaded)
		}
		return uploaded["id"].(string)
	}
	createBatch := func(file string) (int, []byte) {
		t.Helper()
		status, raw, _ := h.gatewayRaw(http.MethodPost, "/v1/batches", key, strings.NewReader(`{"input_file_id":"`+file+`","endpoint":"/v1/embeddings","completion_window":"24h"}`), map[string]string{"Content-Type": "application/json"})
		return status, raw
	}
	listed := func(path string, ids ...string) {
		t.Helper()
		status, raw, _ := h.gatewayRaw(http.MethodGet, path, key, nil, nil)
		if status != http.StatusOK {
			t.Fatalf("GET %s failed after a fidelity switch: %d %s", path, status, raw)
		}
		for _, id := range ids {
			if !strings.Contains(string(raw), `"id":"`+id+`"`) {
				t.Fatalf("GET %s omitted %s: %s", path, id, raw)
			}
		}
	}
	strictFile := upload()
	status, raw := createBatch(strictFile)
	strictBatch, _ := jsonStringField(raw, "id")
	if status != http.StatusOK || !strings.HasPrefix(strictFile, "strict_file_") || !strings.HasPrefix(strictBatch, "strict_batch_") {
		t.Fatalf("strict route did not retain a strict file and batch: %s %d %s", strictFile, status, raw)
	}

	publishFidelity(t, h, owner, slug, provider["id"], operations, "transformed")
	// Lists keep every retained resource, and nothing reaches the provider
	// for a strict resource the transformed route no longer serves.
	before := fixture.dials.Load()
	listed("/v1/files", strictFile)
	listed("/v1/batches", strictBatch)
	for _, path := range []string{"/v1/files/" + strictFile, "/v1/files/" + strictFile + "/content", "/v1/batches/" + strictBatch} {
		status, raw, _ := h.gatewayRaw(http.MethodGet, path, key, nil, nil)
		requireRouteChanged(t, "GET "+path, status, raw, "now transformed")
	}
	status, raw = createBatch(strictFile)
	requireRouteChanged(t, "a batch from a strict file", status, raw, "now transformed")
	if fixture.dials.Load() != before {
		t.Fatal("a strict resource reached the provider through a transformed route")
	}
	// Its owner can still stop the work and remove the provider-held file.
	status, raw, _ = h.gatewayRaw(http.MethodPost, "/v1/batches/"+strictBatch+"/cancel", key, nil, nil)
	if status != http.StatusOK || !strings.Contains(string(raw), `"status":"cancelling"`) {
		t.Fatalf("cancel of a strict batch on a transformed route: %d %s", status, raw)
	}
	status, raw, _ = h.gatewayRaw(http.MethodDelete, "/v1/files/"+strictFile, key, nil, nil)
	if status != http.StatusOK || !strings.Contains(string(raw), `"deleted":true`) {
		t.Fatalf("delete of a strict file on a transformed route: %d %s", status, raw)
	}
	if fixture.dials.Load() != before+2 {
		t.Fatal("strict cancel and delete did not reach the provider")
	}

	plainFile := upload()
	if !strings.HasPrefix(plainFile, "file_") {
		t.Fatalf("transformed route did not retain a metadata-only file: %s", plainFile)
	}
	publishFidelity(t, h, owner, slug, provider["id"], operations, "strict")
	// A transformed file cannot start strict batch work, but it is still
	// listed and retrievable as before.
	before = fixture.dials.Load()
	status, raw = createBatch(plainFile)
	requireRouteChanged(t, "a batch from a transformed file", status, raw, "now that the route is strict")
	if fixture.dials.Load() != before {
		t.Fatal("a transformed file started batch work on a strict route")
	}
	listed("/v1/files", plainFile)
	listed("/v1/batches", strictBatch)
	if status, raw, _ := h.gatewayRaw(http.MethodGet, "/v1/files/"+plainFile, key, nil, nil); status != http.StatusOK {
		t.Fatalf("transformed file retrieval on a strict route: %d %s", status, raw)
	}
	// The strict batch is served again once its route is strict.
	if status, raw, _ := h.gatewayRaw(http.MethodGet, "/v1/batches/"+strictBatch, key, nil, nil); status != http.StatusOK {
		t.Fatalf("strict batch retrieval after the route became strict again: %d %s", status, raw)
	}
}

func TestRetainedResponsesFollowTheirRouteFidelity(t *testing.T) {
	fixture := newOpenAIFixture(t, "")
	h := newAccessHarness(t)
	operations := []string{"generation"}
	owner, provider, slug, _ := provisionOpenAIWith(t, h, fixture.URL,
		[]any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}}, operations,
		map[string]any{"fidelity": map[string]any{"mode": "strict"}},
		map[string]any{"profile_id": "azure-legacy-responses", "profile_revision": "1"})
	key := stateKey(t, h, owner, slug, true)
	h.refresh()
	create := func(extra string) (int, []byte) {
		t.Helper()
		status, raw, _ := h.gatewayRaw(http.MethodPost, "/v1/responses", key, strings.NewReader(`{"model":"`+slug+`","input":"retain this","store":true`+extra+`}`), map[string]string{"Content-Type": "application/json"})
		return status, raw
	}
	status, raw := create("")
	strict, _ := jsonStringField(raw, "id")
	if status != http.StatusOK || !strings.HasPrefix(strict, "strict_response_") {
		t.Fatalf("strict route did not retain a strict response: %d %s", status, raw)
	}

	publishFidelity(t, h, owner, slug, provider["id"], operations, "transformed")
	before := fixture.dials.Load()
	for _, path := range []string{"/v1/responses/" + strict, "/v1/responses/" + strict + "/input_items"} {
		status, raw, _ := h.gatewayRaw(http.MethodGet, path, key, nil, nil)
		requireRouteChanged(t, "GET "+path, status, raw, "now transformed")
	}
	status, raw = create(`,"previous_response_id":"` + strict + `"`)
	requireRouteChanged(t, "a continuation of a strict response", status, raw, "now transformed")
	if fixture.dials.Load() != before {
		t.Fatal("a strict response reached the provider through a transformed route")
	}
	status, raw, _ = h.gatewayRaw(http.MethodPost, "/v1/responses/"+strict+"/cancel", key, nil, nil)
	if status != http.StatusOK || !strings.Contains(string(raw), `"status":"cancelled"`) || !strings.Contains(string(raw), strict) {
		t.Fatalf("cancel of a strict response on a transformed route: %d %s", status, raw)
	}
	status, raw, _ = h.gatewayRaw(http.MethodDelete, "/v1/responses/"+strict, key, nil, nil)
	if status != http.StatusOK || !strings.Contains(string(raw), `"deleted":true`) {
		t.Fatalf("delete of a strict response on a transformed route: %d %s", status, raw)
	}
	if fixture.dials.Load() != before+2 {
		t.Fatal("strict cancel and delete did not reach the provider")
	}

	fixture.respID.Store("resp-up-2")
	fixture.resps["resp-up-2"] = fixture.resps["resp-up-1"]
	status, raw = create("")
	plain, _ := jsonStringField(raw, "id")
	if status != http.StatusOK || !strings.HasPrefix(plain, "response_") {
		t.Fatalf("transformed route did not retain a metadata-only response: %d %s", status, raw)
	}
	publishFidelity(t, h, owner, slug, provider["id"], operations, "strict")
	before = fixture.dials.Load()
	status, raw = create(`,"previous_response_id":"` + plain + `"`)
	requireRouteChanged(t, "a continuation of a transformed response", status, raw, "now that the route is strict")
	if fixture.dials.Load() != before {
		t.Fatal("a transformed response started work on a strict route")
	}
	if status, raw, _ := h.gatewayRaw(http.MethodGet, "/v1/responses/"+plain, key, nil, nil); status != http.StatusOK {
		t.Fatalf("transformed response retrieval on a strict route: %d %s", status, raw)
	}
}

func TestContinuationRecoveryFollowsItsRouteFidelity(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	stream, err := os.ReadFile("../fixtures/fidelity/v1/anthropic-tool-workflow.sse")
	if err != nil {
		t.Fatal(err)
	}
	slug, key := continuationBarrierFixtureOwner(t, h, owner, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(stream)
	})
	headers := continuationHeaders()
	status, first, _ := h.gatewayRaw("POST", "/v1/chat/completions", key, strings.NewReader(strings.Replace(continuationInput, "ROUTE", slug, 1)), headers)
	if status != http.StatusOK {
		t.Fatalf("strict continuation: %d %s", status, first)
	}
	var handle string
	for line := range strings.SplitSeq(string(first), "\n") {
		var chunk struct {
			OLP struct {
				Handle string `json:"handle"`
			} `json:"olp"`
		}
		if strings.HasPrefix(line, "data: {") && json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk) == nil && chunk.OLP.Handle != "" {
			handle = chunk.OLP.Handle
		}
	}
	if handle == "" {
		t.Fatal("committed continuation omitted its handle")
	}
	route := h.want(owner, "GET", "/api/v1/routes", nil, nil, 200)["items"].([]any)[0].(map[string]any)
	provider := route["latest_revision"].(map[string]any)["targets"].([]any)[0].(map[string]any)["provider_id"]
	recovery := func() (int, []byte) {
		t.Helper()
		status, raw, _ := h.gatewayRaw("GET", "/v1/continuations/"+handle, key, nil, map[string]string{"X-OLP-Continuation": continuationClientVersion})
		return status, raw
	}

	publishFidelity(t, h, owner, slug, provider, []string{"generation"}, "transformed")
	status, raw := recovery()
	requireRouteChanged(t, "continuation recovery", status, raw, "now transformed")
	status, raw, _ = h.gatewayRaw("GET", "/v1/continuation-submissions/"+headers["X-OLP-Submission-ID"], key, nil, map[string]string{"X-OLP-Continuation": continuationClientVersion})
	requireRouteChanged(t, "continuation submission recovery", status, raw, "now transformed")

	publishFidelity(t, h, owner, slug, provider, []string{"generation"}, "strict")
	if status, raw = recovery(); status != http.StatusOK || !strings.Contains(string(raw), `"state":"ready"`) {
		t.Fatalf("continuation recovery after the route became strict again: %d %s", status, raw)
	}
}
