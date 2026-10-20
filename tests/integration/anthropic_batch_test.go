//go:build integration

package integration_test

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/batches"
)

const anthropicFixtureModel = "claude-batch-fixture"

type anthropicBatchFixture struct {
	server *httptest.Server
	mu     sync.Mutex
	files  map[string][]byte
	calls  int
}

func newAnthropicBatchFixture(t *testing.T) *anthropicBatchFixture {
	t.Helper()
	fixture := &anthropicBatchFixture{files: map[string][]byte{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/files", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"data": []any{}, "has_more": false})
	})
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"data": []any{map[string]any{"id": anthropicFixtureModel, "type": "model"}}, "has_more": false})
	})
	mux.HandleFunc("POST /v1/messages", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"id": "msg_probe", "type": "message", "role": "assistant", "model": anthropicFixtureModel,
			"content": []any{map[string]any{"type": "text", "text": "ok"}}, "stop_reason": "end_turn",
			"usage": map[string]any{"input_tokens": 1, "output_tokens": 1},
		})
	})
	mux.HandleFunc("POST /v1/files", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Anthropic-Beta") != "files-api-2025-04-14" {
			http.Error(w, "missing files beta", http.StatusBadRequest)
			return
		}
		if err := r.ParseMultipartForm(16 << 20); err != nil {
			http.Error(w, "invalid multipart", http.StatusBadRequest)
			return
		}
		part, _, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "missing file part", http.StatusBadRequest)
			return
		}
		body, _ := io.ReadAll(part)
		_ = part.Close()
		fixture.mu.Lock()
		fixture.files["file_anthropic_input"] = body
		fixture.mu.Unlock()
		writeJSON(w, map[string]any{"id": "file_anthropic_input", "type": "file", "filename": "input.jsonl",
			"mime_type": "application/jsonl", "size_bytes": len(body), "created_at": time.Now().UTC(), "downloadable": false})
	})
	mux.HandleFunc("GET /v1/files/{id}", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"id": r.PathValue("id"), "type": "file", "filename": "input.jsonl",
			"mime_type": "application/jsonl", "size_bytes": 100, "created_at": time.Now().UTC(), "downloadable": false})
	})
	mux.HandleFunc("GET /v1/files/{id}/content", func(w http.ResponseWriter, r *http.Request) {
		fixture.mu.Lock()
		body := fixture.files[r.PathValue("id")]
		fixture.mu.Unlock()
		w.Header().Set("Content-Type", "application/jsonl")
		_, _ = w.Write(body)
	})
	mux.HandleFunc("POST /v1/messages/batches", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Anthropic-Beta") != "files-api-2025-04-14" {
			http.Error(w, "missing file-ref beta", http.StatusBadRequest)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		fixture.mu.Lock()
		fixture.calls++
		fileID := ""
		requests, _ := body["requests"].([]any)
		if len(requests) > 0 {
			request, _ := requests[0].(map[string]any)
			params, _ := request["params"].(map[string]any)
			messages, _ := params["messages"].([]any)
			if len(messages) > 0 {
				message, _ := messages[0].(map[string]any)
				content, _ := message["content"].([]any)
				if len(content) > 0 {
					block, _ := content[0].(map[string]any)
					source, _ := block["source"].(map[string]any)
					fileID, _ = source["file_id"].(string)
				}
			}
		}
		fixture.mu.Unlock()
		if fileID != "file_anthropic_input" {
			http.Error(w, "file id was not rewritten", http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]any{"id": "msgbatch_fixture", "type": "message_batch", "processing_status": "in_progress",
			"request_counts": map[string]int{"processing": 1, "succeeded": 0, "errored": 0, "canceled": 0, "expired": 0},
			"created_at":     time.Now().UTC(), "expires_at": time.Now().UTC().Add(24 * time.Hour), "results_url": nil})
	})
	mux.HandleFunc("GET /v1/messages/batches", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"data": []any{}, "has_more": false})
	})
	mux.HandleFunc("GET /v1/messages/batches/{id}", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"id": "msgbatch_fixture", "type": "message_batch", "processing_status": "ended",
			"request_counts": map[string]int{"processing": 0, "succeeded": 1, "errored": 1, "canceled": 1, "expired": 1},
			"created_at":     time.Now().UTC(), "expires_at": time.Now().UTC().Add(24 * time.Hour),
			"results_url": fixture.server.URL + "/v1/messages/batches/msgbatch_fixture/results"})
	})
	mux.HandleFunc("GET /v1/messages/batches/{id}/results", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/jsonl")
		_, _ = io.WriteString(w, `{"custom_id":"success","result":{"type":"succeeded","message":{"id":"msg_result","type":"message","role":"assistant","model":"claude-batch-fixture","content":[],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":1}}}}`+"\n"+
			`{"custom_id":"error","result":{"type":"errored","error":{"type":"invalid_request_error","message":"bad request"}}}`+"\n"+
			`{"custom_id":"cancel","result":{"type":"canceled"}}`+"\n"+
			`{"custom_id":"expire","result":{"type":"expired"}}`+"\n")
	})
	fixture.server = httptest.NewServer(mux)
	t.Cleanup(fixture.server.Close)
	return fixture
}

func uploadAnthropicFile(t *testing.T, h *accessHarness, key, route string) (int, map[string]any) {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("file", "input.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte("fixture input"))
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	status, raw, _ := h.gatewayRaw(http.MethodPost, "/anthropic/v1/files", key, &body,
		map[string]string{"Content-Type": form.FormDataContentType(), "X-OLP-Route": route})
	var response map[string]any
	_ = json.Unmarshal(raw, &response)
	return status, response
}

func TestAnthropicNativeBatchResourcesAndSettlement(t *testing.T) {
	fixture := newAnthropicBatchFixture(t)
	h := newAccessHarness(t)
	owner := h.owner()
	provider := h.want(owner, http.MethodPost, "/api/v1/providers", map[string]any{
		"name": "Anthropic batches", "configuration": map[string]any{
			"kind": "anthropic", "auth_mode": "api_key", "endpoint": fixture.server.URL + "/v1",
			"profile_id": "anthropic-messages", "profile_revision": "1",
		}, "model": anthropicFixtureModel, "credential": "sk-ant-fixture-secret",
	}, idem(uuid.NewString()), http.StatusCreated)
	providerPath := "/api/v1/providers/" + provider["id"].(string)
	if probe := h.want(owner, http.MethodPost, providerPath+"/probe", nil, etagHeader(provider), http.StatusOK); probe["succeeded"] != true {
		t.Fatalf("Anthropic probe: %v", probe)
	}
	models := h.want(owner, http.MethodGet, providerPath+"/models", nil, nil, http.StatusOK)
	modelID := models["items"].([]any)[0].(map[string]any)["id"].(string)
	model := h.want(owner, http.MethodGet, providerPath+"/models/"+modelID, nil, nil, http.StatusOK)
	updated := h.want(owner, http.MethodPatch, providerPath+"/models/"+modelID, map[string]any{"enabled": true, "capabilities": []any{
		map[string]any{"operation": "generation", "surface": "anthropic", "mode": "unary"},
		map[string]any{"operation": "batch", "surface": "anthropic", "mode": "unary"},
	}}, etagHeader(model), http.StatusOK)
	certified := h.want(owner, http.MethodPost, providerPath+"/models/"+modelID+"/certify", nil, etagHeader(updated), http.StatusOK)
	if certified["status"] != "certified" {
		t.Fatalf("Anthropic batch certification: %v", certified)
	}
	provider = h.want(owner, http.MethodGet, providerPath, nil, nil, http.StatusOK)
	h.want(owner, http.MethodPost, providerPath+"/activate", nil, withMatch(provider, idem(uuid.NewString())), http.StatusOK)
	h.want(owner, http.MethodPost, "/api/v1/pricing/revisions", map[string]any{
		"effective_at": time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano),
		"prices": []any{map[string]any{
			"provider_kind": "anthropic", "provider_id": provider["id"], "model": anthropicFixtureModel,
			"operation": "generation", "currency": "USD", "input_per_million": "1000000",
			"output_per_million": "1000000", "batch_multiplier": "1",
		}},
	}, idem(uuid.NewString()), http.StatusCreated)

	slug := "anthropic-batch-" + uuid.NewString()[:8]
	makeRoute := func(routeSlug string, fidelity string) map[string]any {
		draft := h.want(owner, http.MethodPost, "/api/v1/route-drafts", map[string]any{
			"slug": routeSlug, "operations": []string{"generation", "batch"}, "overall_timeout_ms": 10000, "max_attempts": 1,
			"fidelity": map[string]any{"mode": fidelity},
			"targets":  []any{map[string]any{"provider_id": provider["id"], "provider_model": anthropicFixtureModel, "priority": 0, "weight": 1, "timeout_ms": 5000}},
		}, idem(uuid.NewString()), http.StatusCreated)
		return h.want(owner, http.MethodPost, "/api/v1/route-drafts/"+draft["id"].(string)+"/activate", nil,
			withMatch(draft, idem(uuid.NewString())), http.StatusOK)
	}
	makeRoute(slug, "transformed")
	strictSlug := slug + "-strict"
	makeRoute(strictSlug, "strict")
	key := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{
		"name": "Anthropic batch", "scopes": []string{"inference"}, "allowed_routes": []string{slug, strictSlug},
	}, idem(uuid.NewString()), http.StatusCreated)["secret"].(string)
	otherKey := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{
		"name": "Foreign file", "scopes": []string{"inference"}, "allowed_routes": []string{slug},
	}, idem(uuid.NewString()), http.StatusCreated)["secret"].(string)
	h.refresh()

	status, file := uploadAnthropicFile(t, h, key, slug)
	if status != http.StatusOK {
		t.Fatalf("Anthropic file upload: status=%d file=%v", status, file)
	}
	localFileID := file["id"].(string)
	request := func(fileID string) []byte {
		raw, _ := json.Marshal(map[string]any{"requests": []any{map[string]any{
			"custom_id": "success", "params": map[string]any{"model": slug, "max_tokens": 16,
				"messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{
					"type": "document", "source": map[string]any{"type": "file", "file_id": fileID},
				}}}}},
		}}})
		return raw
	}
	fixture.mu.Lock()
	dispatches := fixture.calls
	fixture.mu.Unlock()
	status, _, _ = h.gatewayRaw(http.MethodPost, "/anthropic/v1/messages/batches", otherKey,
		bytes.NewReader(request(localFileID)), map[string]string{"Content-Type": "application/json", "X-OLP-Route": slug})
	fixture.mu.Lock()
	afterForeign := fixture.calls
	fixture.mu.Unlock()
	if status != http.StatusNotFound || afterForeign != dispatches {
		t.Fatalf("foreign file reference status=%d dispatches=%d/%d", status, dispatches, afterForeign)
	}
	status, _, _ = h.gatewayRaw(http.MethodPost, "/anthropic/v1/messages/batches", key,
		bytes.NewReader(request(localFileID)), map[string]string{"Content-Type": "application/json", "X-OLP-Route": strictSlug})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("strict route status=%d, want 422", status)
	}
	status, raw, _ := h.gatewayRaw(http.MethodPost, "/anthropic/v1/messages/batches", key,
		bytes.NewReader(request(localFileID)), map[string]string{"Content-Type": "application/json", "X-OLP-Route": slug})
	if status != http.StatusOK {
		t.Fatalf("Anthropic batch create: %d %s", status, raw)
	}
	var batch map[string]any
	if err := json.Unmarshal(raw, &batch); err != nil {
		t.Fatal(err)
	}
	localBatchID := batch["id"].(string)
	resourceID := anthropicBatchResourceID(t, localBatchID)
	if pass, err := batchWorker(h, nil).ReconcileOnce(t.Context(), batches.WorkerBatchSize); err != nil || pass.Settled != 1 {
		t.Fatalf("Anthropic worker settlement: pass=%+v err=%v", pass, err)
	}
	var counts map[batches.ResultKind]int64
	counts, err := batches.NewStore(h.Pool).ResultCounts(t.Context(), resourceID)
	if err != nil || counts[batches.ResultSucceeded] != 1 || counts[batches.ResultErrored] != 1 ||
		counts[batches.ResultCancelled] != 1 || counts[batches.ResultExpired] != 1 {
		t.Fatalf("Anthropic result counts = %v, %v", counts, err)
	}
	rows, err := h.Pool.Query(t.Context(), `SELECT r.result_kind,count(f.attempt_id),
		count(*) FILTER(WHERE f.charge_status='billable'),
		COALESCE(sum(f.estimated_cost),0)::text
		FROM olp.batch_settlement_results r
		LEFT JOIN olp.attempt_usage_facts f ON f.event_id=r.event_id
		WHERE r.resource_id=$1 GROUP BY r.result_kind`, resourceID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind batches.ResultKind
		var facts, billable int64
		var cost string
		if err := rows.Scan(&kind, &facts, &billable, &cost); err != nil {
			t.Fatal(err)
		}
		if kind == batches.ResultSucceeded && (facts != 1 || billable != 1 || cost == "0.000000000000") {
			t.Fatalf("successful Anthropic line facts=%d billable=%d cost=%s", facts, billable, cost)
		}
		if kind != batches.ResultSucceeded && billable != 0 {
			t.Fatalf("non-success Anthropic line %s was charged: facts=%d cost=%s", kind, facts, cost)
		}
	}
	resultsStatus, results, _ := h.gatewayRaw(http.MethodGet, "/anthropic/v1/messages/batches/"+localBatchID+"/results", key, nil, nil)
	if resultsStatus != http.StatusOK || strings.Count(string(results), "\n") != 4 {
		t.Fatalf("Anthropic result stream status=%d body=%s", resultsStatus, results)
	}
}

func anthropicBatchResourceID(t *testing.T, localID string) uuid.UUID {
	t.Helper()
	raw := strings.TrimPrefix(localID, "anthropic_batch_")
	if len(raw) != 32 {
		t.Fatalf("invalid local Anthropic batch ID %q", localID)
	}
	id, err := uuid.Parse(raw[:8] + "-" + raw[8:12] + "-" + raw[12:16] + "-" + raw[16:20] + "-" + raw[20:])
	if err != nil {
		t.Fatal(err)
	}
	return id
}
