//go:build integration

package integration_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/batches"
	"github.com/tyk-swe/olp/internal/resources"
)

type managedBatchFixture struct {
	server        *httptest.Server
	adapter       string
	mu            sync.Mutex
	files         map[string]string
	job           map[string]any
	loseCreateAck bool
}

func newManagedBatchFixture(t *testing.T, adapter string) *managedBatchFixture {
	t.Helper()
	fixture := &managedBatchFixture{adapter: adapter, files: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/files", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"data": []any{}})
	})
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"data": []any{map[string]any{"id": vendorModel, "object": "model"}}})
	})
	fileUpload := func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(16 << 20); err != nil {
			http.Error(w, "invalid multipart", http.StatusBadRequest)
			return
		}
		input, _, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "missing file", http.StatusBadRequest)
			return
		}
		body, err := io.ReadAll(input)
		_ = input.Close()
		if err != nil {
			http.Error(w, "invalid file", http.StatusBadRequest)
			return
		}
		fixture.mu.Lock()
		fixture.files["file-input"] = string(body)
		fixture.mu.Unlock()
		purpose := r.FormValue("purpose")
		if adapter == "together" {
			purpose = "batch-api"
		}
		writeJSON(w, map[string]any{"id": "file-input", "object": "file", "bytes": len(body),
			"created_at": time.Now().Unix(), "filename": "input.jsonl", "purpose": purpose})
	}
	mux.HandleFunc("POST /v1/files/upload", fileUpload)
	mux.HandleFunc("POST /v1/files", fileUpload)
	mux.HandleFunc("GET /v1/files/{id}", func(w http.ResponseWriter, r *http.Request) {
		purpose := "batch"
		if adapter == "together" {
			purpose = "batch-api"
		}
		writeJSON(w, map[string]any{"id": r.PathValue("id"), "object": "file", "bytes": 128,
			"created_at": time.Now().Unix(), "filename": r.PathValue("id") + ".jsonl", "purpose": purpose, "provider_only": true})
	})
	mux.HandleFunc("DELETE /v1/files/{id}", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"id": r.PathValue("id"), "object": "file", "deleted": true})
	})
	mux.HandleFunc("GET /v1/files/{id}/content", func(w http.ResponseWriter, r *http.Request) {
		fixture.mu.Lock()
		body, ok := fixture.files[r.PathValue("id")]
		fixture.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/jsonl")
		_, _ = io.WriteString(w, body)
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"id": "probe", "object": "chat.completion", "model": vendorModel,
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "ok"}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}})
	})
	create := func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		var request map[string]any
		_ = json.Unmarshal(body, &request)
		inputID := request["input_file_id"]
		if adapter == "mistral" {
			inputs, _ := request["input_files"].([]any)
			if len(inputs) > 0 {
				inputID = inputs[0]
			}
		}
		job := map[string]any{
			"id": "job-managed", "status": "COMPLETED", "endpoint": request["endpoint"],
			"input_file_id": inputID, "created_at": time.Now().UTC().Format(time.RFC3339Nano),
			"output_file_id": "file-output", "error_file_id": "file-error",
			"output_file": "file-output", "error_file": "file-error",
			"total_requests": "1", "succeeded_requests": "1", "failed_requests": "0",
		}
		if adapter == "mistral" {
			job["status"] = "SUCCESS"
			delete(job, "input_file_id")
			job["input_files"] = []string{"file-input"}
		}
		fixture.mu.Lock()
		fixture.job = job
		fixture.files["file-output"] = batchChatLine("managed-result", "ok")
		fixture.files["file-error"] = ""
		loseAck := fixture.loseCreateAck
		fixture.mu.Unlock()
		if loseAck {
			w.WriteHeader(http.StatusBadGateway)
			writeJSON(w, map[string]any{"error": map[string]any{"message": "lost acknowledgement"}})
			return
		}
		if adapter == "together" {
			writeJSON(w, map[string]any{"job": job})
		} else {
			writeJSON(w, job)
		}
	}
	mux.HandleFunc("POST /v1/batches", create)
	mux.HandleFunc("POST /v1/batch/jobs", create)
	mux.HandleFunc("GET /v1/batches", func(w http.ResponseWriter, _ *http.Request) {
		fixture.mu.Lock()
		job := fixture.job
		fixture.mu.Unlock()
		if job == nil {
			writeJSON(w, []any{})
			return
		}
		writeJSON(w, []any{job})
	})
	mux.HandleFunc("GET /v1/batch/jobs", func(w http.ResponseWriter, _ *http.Request) {
		fixture.mu.Lock()
		job := fixture.job
		fixture.mu.Unlock()
		if job == nil {
			writeJSON(w, map[string]any{"data": []any{}, "total": 0})
			return
		}
		writeJSON(w, map[string]any{"data": []any{job}, "total": 1})
	})
	mux.HandleFunc("GET /v1/batches/{id}", func(w http.ResponseWriter, _ *http.Request) {
		fixture.mu.Lock()
		job := fixture.job
		fixture.mu.Unlock()
		writeJSON(w, job)
	})
	mux.HandleFunc("GET /v1/batch/jobs/{id}", func(w http.ResponseWriter, _ *http.Request) {
		fixture.mu.Lock()
		job := fixture.job
		fixture.mu.Unlock()
		writeJSON(w, job)
	})
	mux.HandleFunc("POST /v1/batches/{id}/cancel", func(w http.ResponseWriter, _ *http.Request) {
		fixture.mu.Lock()
		job := fixture.job
		fixture.mu.Unlock()
		writeJSON(w, job)
	})
	mux.HandleFunc("POST /v1/batch/jobs/{id}/cancel", func(w http.ResponseWriter, _ *http.Request) {
		fixture.mu.Lock()
		job := fixture.job
		fixture.mu.Unlock()
		writeJSON(w, job)
	})
	fixture.server = httptest.NewServer(mux)
	t.Cleanup(fixture.server.Close)
	return fixture
}

func TestManagedBatchSubmissionAndSettlement(t *testing.T) {
	for _, adapter := range []string{"together", "mistral"} {
		t.Run(adapter, func(t *testing.T) {
			fixture := newManagedBatchFixture(t, adapter)
			h := newAccessHarness(t)
			owner := h.owner()
			profile := adapter + "-files-batches"
			configuration := map[string]any{"kind": "openai_compatible", "auth_mode": "api_key",
				"endpoint": fixture.server.URL + "/v1", "profile_id": profile, "profile_revision": "1",
				"options": map[string]any{"vendor_id": adapter}}
			provider := h.want(owner, http.MethodPost, "/api/v1/providers", map[string]any{
				"name": "Managed " + adapter, "configuration": configuration, "model": vendorModel, "credential": vendorSecret,
			}, map[string]string{"Idempotency-Key": uuid.NewString()}, http.StatusCreated)
			path := "/api/v1/providers/" + provider["id"].(string)
			probe := h.want(owner, http.MethodPost, path+"/probe", nil, etagHeader(provider), http.StatusOK)
			if probe["succeeded"] != true {
				t.Fatalf("provider probe: %v", probe)
			}
			models := h.want(owner, http.MethodGet, path+"/models", nil, nil, http.StatusOK)
			modelID := models["items"].([]any)[0].(map[string]any)["id"].(string)
			capabilities := []any{
				map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"},
				map[string]any{"operation": "batch", "surface": "openai", "mode": "unary"},
			}
			detail := h.want(owner, http.MethodPatch, path+"/models/"+modelID, map[string]any{"enabled": true, "capabilities": capabilities}, etagHeader(provider), http.StatusOK)
			certified := h.want(owner, http.MethodPost, path+"/models/"+modelID+"/certify", nil, etagHeader(detail), http.StatusOK)
			if certified["status"] != "certified" {
				t.Fatalf("batch certification: %v", certified)
			}
			detail = h.want(owner, http.MethodGet, path, nil, nil, http.StatusOK)
			h.want(owner, http.MethodPost, path+"/activate", nil, withMatch(detail, map[string]string{"Idempotency-Key": uuid.NewString()}), http.StatusOK)
			slug := "managed-" + adapter + "-" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
			draft := h.want(owner, http.MethodPost, "/api/v1/route-drafts", map[string]any{
				"slug": slug, "operations": []string{"batch"}, "overall_timeout_ms": 10000, "max_attempts": 1,
				"fidelity": map[string]any{"mode": "transformed"},
				"targets":  []any{map[string]any{"provider_id": provider["id"], "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 5000}},
			}, map[string]string{"Idempotency-Key": uuid.NewString()}, http.StatusCreated)
			h.want(owner, http.MethodPost, "/api/v1/route-drafts/"+draft["id"].(string)+"/activate", nil,
				withMatch(draft, map[string]string{"Idempotency-Key": uuid.NewString()}), http.StatusOK)
			key := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{
				"name": "managed batches", "scopes": []string{"inference"}, "allowed_routes": []string{slug},
			}, map[string]string{"Idempotency-Key": uuid.NewString()}, http.StatusCreated)["secret"].(string)
			h.refresh()
			input := `{"custom_id":"managed-result","method":"POST","url":"/v1/chat/completions","body":{"model":"` + vendorModel + `","messages":[{"role":"user","content":"hi"}]}}` + "\n"
			uploadStatus, uploaded := h.uploadTestFile(slug, key, input)
			if uploadStatus != http.StatusOK {
				t.Fatalf("managed upload: %d %v", uploadStatus, uploaded)
			}
			fileStatus, retrieved, _ := h.gateway(http.MethodGet, "/v1/files/"+uploaded["id"].(string), key, nil)
			if fileStatus != http.StatusOK || retrieved["purpose"] != "batch" || retrieved["provider_only"] != nil {
				t.Fatalf("managed file retrieval lost normalized projection: %d %v", fileStatus, retrieved)
			}
			fixture.mu.Lock()
			translated := fixture.files["file-input"]
			fixture.mu.Unlock()
			var translatedLine map[string]json.RawMessage
			if err := json.Unmarshal([]byte(strings.TrimSpace(translated)), &translatedLine); err != nil {
				t.Fatal(err)
			}
			if len(translatedLine) != 2 || len(translatedLine["method"]) != 0 || len(translatedLine["url"]) != 0 {
				t.Fatalf("%s input file was not projected to custom_id/body: %s", adapter, translated)
			}
			var translatedBody map[string]json.RawMessage
			if err := json.Unmarshal(translatedLine["body"], &translatedBody); err != nil {
				t.Fatal(err)
			}
			_, modelPresent := translatedBody["model"]
			if adapter == "together" && !modelPresent || adapter == "mistral" && modelPresent {
				t.Fatalf("%s model placement in translated batch body is wrong: %s", adapter, translated)
			}
			inputContentStatus, inputContent, _ := h.gatewayRaw(http.MethodGet, "/v1/files/"+uploaded["id"].(string)+"/content", key, nil,
				map[string]string{"X-OLP-Route": slug})
			if inputContentStatus != http.StatusOK || !bytes.Equal(bytes.TrimSpace(inputContent), bytes.TrimSpace([]byte(translated))) {
				t.Fatalf("managed input content proxy: %d %s", inputContentStatus, inputContent)
			}
			fixture.mu.Lock()
			fixture.loseCreateAck = adapter == "together"
			fixture.mu.Unlock()
			request, _ := json.Marshal(map[string]any{"input_file_id": uploaded["id"], "endpoint": "/v1/chat/completions", "completion_window": "24h", "metadata": map[string]string{"client": "preserved"}})
			status, response, _ := h.gatewayRaw(http.MethodPost, "/v1/batches", key, bytes.NewReader(request),
				map[string]string{"Content-Type": "application/json", "X-OLP-Route": slug})
			wantStatus := http.StatusOK
			if adapter == "together" {
				wantStatus = http.StatusBadGateway
			}
			if status != wantStatus {
				t.Fatalf("managed batch create: %d %s", status, response)
			}
			localID := ""
			if adapter == "together" {
				var intentID uuid.UUID
				if err := h.Pool.QueryRow(t.Context(), `SELECT resource_id FROM olp.batch_settlements WHERE adapter='together' AND state='submission_unknown'`).Scan(&intentID); err != nil {
					t.Fatal(err)
				}
				localID = resources.LocalID(resources.KindBatch, intentID)
			} else {
				var created map[string]any
				if err := json.Unmarshal(response, &created); err != nil {
					t.Fatal(err)
				}
				localID = created["id"].(string)
			}
			worker := batchWorker(h, nil)
			if _, err := worker.ReconcileOnce(t.Context(), batches.WorkerBatchSize); err != nil {
				t.Fatal(err)
			}
			resourceID := batchResourceID(t, localID)
			assertBatchSettlement(t, h, resourceID, "settled", 1, 0)
			getStatus, batch, _ := h.gateway(http.MethodGet, "/v1/batches/"+localID, key, nil)
			if getStatus != http.StatusOK || batch["status"] != "completed" {
				t.Fatalf("managed batch get: %d %v", getStatus, batch)
			}
			if adapter == "together" {
				metadata, _ := batch["metadata"].(map[string]any)
				if metadata["client"] != "preserved" {
					t.Fatalf("recovered Together metadata was lost: %v", batch)
				}
			}
			outputID, _ := batch["output_file_id"].(string)
			contentStatus, content, _ := h.gatewayRaw(http.MethodGet, "/v1/files/"+outputID+"/content", key, nil,
				map[string]string{"X-OLP-Route": slug})
			if contentStatus != http.StatusOK || !bytes.Contains(content, []byte(`"custom_id":"managed-result"`)) {
				t.Fatalf("managed result content: %d %s", contentStatus, content)
			}
		})
	}
}
