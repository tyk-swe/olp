//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	btypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/gateway"
	"github.com/tyk-swe/olp/internal/limits"
)

type captureSink struct {
	mu     sync.Mutex
	events []gateway.Envelope
}

func (c *captureSink) Terminal(e gateway.Envelope) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, e)
}

func (c *captureSink) last() gateway.Envelope {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.events[len(c.events)-1]
}

func (c *captureSink) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.events)
}

func (c *captureSink) all() []gateway.Envelope {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]gateway.Envelope(nil), c.events...)
}

type openaiFixture struct {
	*httptest.Server
	files          map[string]map[string]any
	batches        map[string]map[string]any
	resps          map[string]map[string]any
	mu             sync.Mutex
	lastReq        atomic.Value
	lastBatchRaw   atomic.Value
	batchCreates   atomic.Int64
	earlyFileReply atomic.Bool
	batchCreateRaw atomic.Value
	batchFetchRaw  atomic.Value
	fileCreateRaw  atomic.Value
	fileFetchRaw   atomic.Value
	contentByID    sync.Map
	lastPath       atomic.Value
	content        atomic.Value
	respID         atomic.Value
	dials          atomic.Int64
}

func newOpenAIFixture(t *testing.T, fileContent string) *openaiFixture {
	f := &openaiFixture{
		files:   map[string]map[string]any{},
		batches: map[string]map[string]any{},
		resps:   map[string]map[string]any{},
	}
	f.files["file-up-1"] = map[string]any{"id": "file-up-1", "object": "file", "bytes": 128, "created_at": 1, "filename": "input.jsonl", "purpose": "batch", "status": "processed"}
	f.batches["batch-up-1"] = map[string]any{"id": "batch-up-1", "object": "batch", "status": "validating", "endpoint": "/v1/chat/completions", "completion_window": "24h"}
	f.resps["resp-up-1"] = map[string]any{"id": "resp-up-1", "object": "response", "created_at": 1, "model": vendorModel, "status": "completed", "error": nil, "incomplete_details": nil, "output": []any{map[string]any{"id": "msg-up-1", "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "OK", "annotations": []any{}}}}}, "usage": map[string]any{"input_tokens": 4, "output_tokens": 6, "total_tokens": 10}}
	f.content.Store(fileContent)
	f.respID.Store("resp-up-1")
	mux := http.NewServeMux()
	mux.HandleFunc("POST /openai/deployments/{dep}/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		body := decodeBody(t, r)
		f.lastReq.Store(body)
		if stream, _ := body["stream"].(bool); stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: {\"id\":\"chatcmpl-up-1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"OK\"},\"finish_reason\":null}]}\n\n", r.PathValue("dep"))
			fmt.Fprintf(w, "data: {\"id\":\"chatcmpl-up-1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\ndata: [DONE]\n\n", r.PathValue("dep"))
			return
		}
		writeJSON(w, map[string]any{"id": "chatcmpl-up-1", "object": "chat.completion", "created": 1, "model": r.PathValue("dep"), "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "OK"}, "finish_reason": "stop"}}, "usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}})
	})
	mux.HandleFunc("POST /openai/deployments/{dep}/embeddings", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"object": "list", "data": []any{map[string]any{"object": "embedding", "index": 0, "embedding": []float64{0.1, 0.2}}}, "model": r.PathValue("dep"), "usage": map[string]any{"prompt_tokens": 2, "total_tokens": 2}})
	})
	mux.HandleFunc("GET /openai/files", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"object": "list", "data": []any{}})
	})
	mux.HandleFunc("POST /openai/files", func(w http.ResponseWriter, r *http.Request) {
		f.dials.Add(1)
		if f.earlyFileReply.Load() {
			_ = http.NewResponseController(w).EnableFullDuplex()
			_, _ = r.Body.Read(make([]byte, 1))
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"file-up-early","object":"file","status":"processed"}`)
			_ = http.NewResponseController(w).Flush()
			return
		}
		_ = r.ParseMultipartForm(1 << 20)
		if raw := f.fileCreateRaw.Load(); raw != nil {
			_, _ = io.WriteString(w, raw.(string))
			return
		}
		writeJSON(w, f.files["file-up-1"])
	})
	mux.HandleFunc("GET /openai/files/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.dials.Add(1)
		file, ok := f.files[r.PathValue("id")]
		if !ok {
			http.Error(w, `{"error":{"message":"no such file"}}`, http.StatusNotFound)
			return
		}
		if raw := f.fileFetchRaw.Load(); raw != nil {
			_, _ = io.WriteString(w, raw.(string))
			return
		}
		writeJSON(w, file)
	})
	mux.HandleFunc("GET /openai/files/{id}/content", func(w http.ResponseWriter, r *http.Request) {
		f.dials.Add(1)
		if _, ok := f.files[r.PathValue("id")]; !ok {
			http.Error(w, `{"error":{"message":"no such file"}}`, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		if custom, ok := f.contentByID.Load(r.PathValue("id")); ok {
			_, _ = io.WriteString(w, custom.(string))
			return
		}
		io.WriteString(w, f.content.Load().(string))
	})
	mux.HandleFunc("DELETE /openai/files/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.dials.Add(1)
		if _, ok := f.files[r.PathValue("id")]; !ok {
			http.Error(w, `{"error":{"message":"no such file"}}`, http.StatusNotFound)
			return
		}
		writeJSON(w, map[string]any{"id": r.PathValue("id"), "object": "file", "deleted": true})
	})
	mux.HandleFunc("GET /openai/batches", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"object": "list", "data": []any{}})
	})
	mux.HandleFunc("POST /openai/batches", func(w http.ResponseWriter, r *http.Request) {
		f.dials.Add(1)
		f.batchCreates.Add(1)
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "invalid batch fixture body", http.StatusBadRequest)
			return
		}
		body := map[string]any{}
		if err := json.Unmarshal(raw, &body); err != nil {
			http.Error(w, "invalid batch fixture JSON", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.lastReq.Store(body)
		f.lastBatchRaw.Store(string(raw))
		f.mu.Unlock()
		if result := f.batchCreateRaw.Load(); result != nil {
			_, _ = io.WriteString(w, result.(string))
			return
		}
		batch := map[string]any{}
		for k, v := range f.batches["batch-up-1"] {
			batch[k] = v
		}
		batch["input_file_id"] = body["input_file_id"]
		writeJSON(w, batch)
	})
	mux.HandleFunc("GET /openai/batches/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.dials.Add(1)
		batch, ok := f.batches[r.PathValue("id")]
		if !ok {
			http.Error(w, `{"error":{"message":"no such batch"}}`, http.StatusNotFound)
			return
		}
		if raw := f.batchFetchRaw.Load(); raw != nil {
			_, _ = io.WriteString(w, raw.(string))
			return
		}
		writeJSON(w, batch)
	})
	mux.HandleFunc("POST /openai/batches/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		f.dials.Add(1)
		batch, ok := f.batches[r.PathValue("id")]
		if !ok {
			http.Error(w, `{"error":{"message":"no such batch"}}`, http.StatusNotFound)
			return
		}
		out := map[string]any{}
		for k, v := range batch {
			out[k] = v
		}
		out["status"] = "cancelling"
		writeJSON(w, out)
	})
	mux.HandleFunc("POST /openai/deployments/{dep}/responses", func(w http.ResponseWriter, r *http.Request) {
		f.dials.Add(1)
		body := decodeBody(t, r)
		f.mu.Lock()
		f.lastReq.Store(body)
		f.mu.Unlock()
		out := map[string]any{}
		for k, v := range f.resps["resp-up-1"] {
			out[k] = v
		}
		out["id"] = f.respID.Load().(string)
		if stream, _ := body["stream"].(bool); stream {
			created, _ := json.Marshal(map[string]any{"type": "response.created", "sequence_number": 0, "response": map[string]any{"id": out["id"], "object": "response", "status": "in_progress", "model": r.PathValue("dep"), "created_at": 1, "output": []any{}}})
			terminal, _ := json.Marshal(map[string]any{"type": "response.completed", "sequence_number": 1, "response": out})
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "event: response.created\ndata: %s\n\nevent: response.completed\ndata: %s\n\n", created, terminal)
			return
		}
		writeJSON(w, out)
	})
	// Azure's reviewed legacy Responses profile uses the resource-level path;
	// the deployment path above remains the older compatibility fixture.
	mux.HandleFunc("POST /openai/responses", func(w http.ResponseWriter, r *http.Request) {
		f.dials.Add(1)
		body := decodeBody(t, r)
		f.lastReq.Store(body)
		out := map[string]any{}
		for k, v := range f.resps["resp-up-1"] {
			out[k] = v
		}
		out["id"] = f.respID.Load().(string)
		if stream, _ := body["stream"].(bool); stream {
			w.Header().Set("Content-Type", "text/event-stream")
			created, _ := json.Marshal(map[string]any{"type": "response.created", "sequence_number": 0, "response": out})
			fmt.Fprintf(w, "event: response.created\ndata: %s\n\n", created)
			return
		}
		writeJSON(w, out)
	})
	mux.HandleFunc("GET /openai/responses/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.dials.Add(1)
		res, ok := f.resps[r.PathValue("id")]
		if !ok {
			http.Error(w, `{"error":{"message":"no such response"}}`, http.StatusNotFound)
			return
		}
		writeJSON(w, res)
	})
	mux.HandleFunc("GET /openai/deployments/{dep}/responses/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.dials.Add(1)
		res, ok := f.resps[r.PathValue("id")]
		if !ok {
			http.Error(w, `{"error":{"message":"no such response"}}`, http.StatusNotFound)
			return
		}
		writeJSON(w, res)
	})
	mux.HandleFunc("POST /openai/deployments/{dep}/responses/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		f.dials.Add(1)
		res, ok := f.resps[r.PathValue("id")]
		if !ok {
			http.Error(w, `{"error":{"message":"no such response"}}`, http.StatusNotFound)
			return
		}
		out := map[string]any{}
		for k, v := range res {
			out[k] = v
		}
		out["status"] = "cancelled"
		writeJSON(w, out)
	})
	mux.HandleFunc("GET /openai/deployments/{dep}/responses/{id}/input_items", func(w http.ResponseWriter, r *http.Request) {
		f.dials.Add(1)
		if _, ok := f.resps[r.PathValue("id")]; !ok {
			http.Error(w, `{"error":{"message":"no such response"}}`, http.StatusNotFound)
			return
		}
		writeJSON(w, map[string]any{"object": "list", "data": []any{map[string]any{"id": "msg-up-1", "type": "message", "role": "user", "status": "completed"}}})
	})
	mux.HandleFunc("DELETE /openai/deployments/{dep}/responses/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.dials.Add(1)
		if _, ok := f.resps[r.PathValue("id")]; !ok {
			http.Error(w, `{"error":{"message":"no such response"}}`, http.StatusNotFound)
			return
		}
		writeJSON(w, map[string]any{"id": r.PathValue("id"), "object": "response.deleted", "deleted": true})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Logf("fixture 404: %s %s", r.Method, r.URL.String())
		http.Error(w, `{"error":{"message":"fixture no such route"}}`, http.StatusNotFound)
	})
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.lastPath.Store(r.URL.Path)
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.Close)
	return f
}

func (h *accessHarness) gatewayRaw(method, path, key string, body io.Reader, headers map[string]string) (int, []byte, http.Header) {
	h.t.Helper()
	req, err := http.NewRequestWithContext(h.t.Context(), method, h.HTTP.URL+path, body)
	if err != nil {
		h.t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw, resp.Header
}

func (h *accessHarness) uploadTestFile(slug, key, content string) (int, map[string]any) {
	h.t.Helper()
	var buf bytes.Buffer
	form := multipart.NewWriter(&buf)
	if err := form.WriteField("purpose", "batch"); err != nil {
		h.t.Fatal(err)
	}
	part, err := form.CreateFormFile("file", "input.jsonl")
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err := part.Write([]byte(content)); err != nil {
		h.t.Fatal(err)
	}
	if err := form.Close(); err != nil {
		h.t.Fatal(err)
	}
	status, raw, _ := h.gatewayRaw("POST", "/v1/files", key, &buf, map[string]string{
		"Content-Type": form.FormDataContentType(), "X-OLP-Route": slug,
	})
	out := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			h.t.Fatalf("upload reply: %q", raw)
		}
	}
	return status, out
}

func provisionOpenAI(t *testing.T, h *accessHarness, endpoint string, capabilities []any, operations []string) (*browser, map[string]any, string, string) {
	t.Helper()
	return provisionOpenAIWith(t, h, endpoint, capabilities, operations, nil)
}

func provisionOpenAIWith(t *testing.T, h *accessHarness, endpoint string, capabilities []any, operations []string, draftFields map[string]any, providerFields ...map[string]any) (*browser, map[string]any, string, string) {
	t.Helper()
	owner := h.owner()
	configuration := map[string]any{"kind": "azure_openai", "auth_mode": "api_key", "endpoint": endpoint, "deployment": vendorModel, "api_version": "2024-10-21"}
	if len(providerFields) > 0 {
		for name, value := range providerFields[0] {
			configuration[name] = value
		}
	}
	create := map[string]any{"name": "Provider state fixture", "configuration": configuration, "model": vendorModel, "credential": vendorSecret}
	detail := h.want(owner, "POST", "/api/v3/providers", create, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	path := "/api/v3/providers/" + detail["id"].(string)
	probe := h.want(owner, "POST", path+"/probe", nil, etagHeader(detail), 200)
	if probe["succeeded"] != true {
		t.Fatalf("provider probe: %v", probe)
	}
	models := h.want(owner, "GET", path+"/models", nil, nil, 200)
	modelID := models["items"].([]any)[0].(map[string]any)["id"].(string)
	detail = h.want(owner, "PATCH", path+"/models/"+modelID, map[string]any{"enabled": true, "capabilities": capabilities}, etagHeader(detail), 200)
	certified := h.want(owner, "POST", path+"/models/"+modelID+"/certify", nil, etagHeader(detail), 200)
	if certified["status"] != "certified" {
		t.Fatalf("capability proof: %v", certified)
	}
	detail = h.want(owner, "GET", path, nil, nil, 200)
	h.want(owner, "POST", path+"/activate", nil, withMatch(detail, map[string]string{"Idempotency-Key": uuid.NewString()}), 200)
	slug := "state-" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	draftBody := map[string]any{"slug": slug, "operations": operations, "overall_timeout_ms": 10000, "max_attempts": 1, "targets": []any{map[string]any{"provider_id": detail["id"], "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 5000}}}
	for key, value := range draftFields {
		draftBody[key] = value
	}
	draft := h.want(owner, "POST", "/api/v3/route-drafts", draftBody, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	h.want(owner, "POST", "/api/v3/route-drafts/"+draft["id"].(string)+"/activate", nil, withMatch(draft, map[string]string{"Idempotency-Key": uuid.NewString()}), 200)
	key := h.want(owner, "POST", "/api/v3/api-keys", map[string]any{"name": "stateful inference", "scopes": []string{"inference"}, "allowed_routes": []string{slug}}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	h.refresh()
	return owner, detail, slug, key["secret"].(string)
}

func stateKey(t *testing.T, h *accessHarness, owner *browser, slug string, allowState bool) string {
	t.Helper()
	key := h.want(owner, "POST", "/api/v3/api-keys", map[string]any{"name": "state key", "scopes": []string{"inference"}, "allowed_routes": []string{slug}, "allow_provider_state": allowState}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	return key["secret"].(string)
}

func TestBatchLifecycle(t *testing.T) {
	fileContent := "batch-download-marker-9\n"
	fixture := newOpenAIFixture(t, fileContent)
	h := newAccessHarness(t)
	sink := &captureSink{}
	h.Gateway.Sink = sink
	owner, detail, slug, secret := provisionOpenAI(t, h, fixture.URL,
		[]any{map[string]any{"operation": "batch", "surface": "openai", "mode": "unary"}},
		[]string{"batch"})
	other := h.want(owner, "POST", "/api/v3/api-keys", map[string]any{"name": "other key", "scopes": []string{"inference"}, "allowed_routes": []string{slug}}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	otherSecret := other["secret"].(string)
	h.refresh()

	status, uploaded := h.uploadTestFile(slug, secret, "upload-content-marker-7\n")
	if status != 200 {
		t.Fatalf("upload: %d %v", status, uploaded)
	}
	fileID, _ := uploaded["id"].(string)
	if !strings.HasPrefix(fileID, "file_") || strings.Contains(fileID, "up-") {
		t.Fatalf("upload returned non-local identifier: %v", uploaded)
	}
	if sink.count() < 1 || len(sink.last().Attempts) != 1 {
		t.Fatalf("upload accounted %d attempts, want exactly 1", len(sink.last().Attempts))
	}

	before := fixture.dials.Load()
	status, list, _ := h.gateway("GET", "/v1/files", secret, nil)
	if status != 200 {
		t.Fatalf("list files: %d %v", status, list)
	}
	items, _ := list["data"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["id"] != fileID {
		t.Fatalf("file list: %v", list)
	}
	if fixture.dials.Load() != before {
		t.Fatal("a metadata-only list must not contact the provider")
	}
	var originalFileMetadata []byte
	if err := h.Pool.QueryRow(t.Context(), `SELECT metadata FROM olp_go.provider_resources WHERE kind='file' AND route_slug=$1`, slug).Scan(&originalFileMetadata); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Pool.Exec(t.Context(), `UPDATE olp_go.provider_resources SET metadata='[]'::jsonb WHERE kind='file' AND route_slug=$1`, slug); err != nil {
		t.Fatal(err)
	}
	status, raw, _ := h.gatewayRaw("GET", "/v1/files", secret, nil, nil)
	if status != http.StatusServiceUnavailable || !bytes.Contains(raw, []byte(`"code":"provider_resource_unavailable"`)) || fixture.dials.Load() != before {
		t.Fatalf("corrupt file was silently omitted from local list: status=%d body=%s", status, raw)
	}
	if _, err := h.Pool.Exec(t.Context(), `UPDATE olp_go.provider_resources SET metadata=$2::jsonb WHERE kind='file' AND route_slug=$1`, slug, string(originalFileMetadata)); err != nil {
		t.Fatal(err)
	}

	status, fetched, _ := h.gateway("GET", "/v1/files/"+fileID, secret, nil)
	if status != 200 || fetched["id"] != fileID {
		t.Fatalf("get file: %d %v", status, fetched)
	}
	status, raw, _ = h.gatewayRaw("GET", "/v1/files/"+fileID+"/content", secret, nil, nil)
	if status != 200 || string(raw) != fileContent {
		t.Fatalf("file content: %d %q", status, raw)
	}

	fixture.content.Store(strings.Repeat("z", (1<<20)+64))
	status, raw, _ = h.gatewayRaw("GET", "/v1/files/"+fileID+"/content", secret, nil, nil)
	if status != http.StatusBadGateway || !bytes.Contains(raw, []byte(`"code":"upstream_response_too_large"`)) || len(raw) >= 1<<20 {
		t.Fatalf("provider file overflow looked like a successful truncated download: %d %d bytes %s", status, len(raw), raw)
	}
	overflowEnvelope := sink.last()
	overflow := overflowEnvelope.Attempts[len(overflowEnvelope.Attempts)-1]
	if overflowEnvelope.Outcome != "failure" || overflowEnvelope.Committed || overflow.Class != "protocol" || overflow.UsageComplete || !overflow.BillingUncertain {
		t.Fatalf("overflow acceptance/observation was not separated: %+v %+v", overflowEnvelope, overflow)
	}
	fixture.content.Store(fileContent)
	status, fetched, _ = h.gateway("GET", "/v1/files/"+fileID, otherSecret, nil)
	if status != 404 {
		t.Fatalf("other key file: %d %v", status, fetched)
	}

	beforeBatch := fixture.dials.Load()
	for _, rejected := range []string{
		`{"input_file_id":"` + fileID + `","input_file_id":"` + fileID + `","endpoint":"/v1/chat/completions"}`,
		`{"input_file_id":"` + fileID + `","endpoint":"/v1/chat/completions","id":"caller-selected"}`,
	} {
		status, raw, _ = h.gatewayRaw("POST", "/v1/batches", secret, strings.NewReader(rejected), map[string]string{"Content-Type": "application/json"})
		if status != http.StatusBadRequest || fixture.dials.Load() != beforeBatch {
			t.Fatalf("ambiguous or caller-owned batch identity dispatched: status=%d body=%s", status, raw)
		}
	}
	batchSource := `{"completion_window":"24h","native":{"rank":9007199254740993,"tiny":-0},"input_file_id":"` + fileID + `","endpoint":"/v1/chat/completions"}`
	status, raw, _ = h.gatewayRaw("POST", "/v1/batches", secret, strings.NewReader(batchSource), map[string]string{"Content-Type": "application/json"})
	var batch map[string]any
	if err := json.Unmarshal(raw, &batch); err != nil {
		t.Fatalf("batch response: %s: %v", raw, err)
	}
	if status != 200 {
		t.Fatalf("create batch: %d %v", status, batch)
	}
	batchID, _ := batch["id"].(string)
	if !strings.HasPrefix(batchID, "batch_") {
		t.Fatalf("batch identifier: %v", batch)
	}
	sent, _ := fixture.lastReq.Load().(map[string]any)
	if sent["input_file_id"] != "file-up-1" {
		t.Fatalf("upstream did not receive the rewritten file identifier: %v", sent)
	}
	wantBatchRaw := `{"completion_window":"24h","native":{"rank":9007199254740993,"tiny":-0},"input_file_id":"file-up-1","endpoint":"/v1/chat/completions","model":"` + vendorModel + `"}`
	if got := fixture.lastBatchRaw.Load().(string); got != wantBatchRaw {
		t.Fatalf("batch source, numeric syntax or overlay order changed:\n got %s\nwant %s", got, wantBatchRaw)
	}

	fixture.mu.Lock()
	fixture.batches["batch-up-1"]["status"] = "completed"
	fixture.batches["batch-up-1"]["output_file_id"] = "file-up-out"
	fixture.batches["batch-up-1"]["error_file_id"] = "file-up-err"
	fixture.mu.Unlock()

	if _, err := h.Pool.Exec(t.Context(), `INSERT INTO olp_go.provider_resources
		(id,kind,api_key_id,route_slug,provider_id,provider_revision_id,route_revision_id,slot_id,credential_id,upstream_id,state,metadata)
		SELECT gen_random_uuid(),'file',$1,route_slug,provider_id,provider_revision_id,route_revision_id,slot_id,credential_id,'file-up-out','processed','{}'
		FROM olp_go.provider_resources WHERE kind='batch' AND upstream_id='batch-up-1'`, other["id"]); err != nil {
		t.Fatal(err)
	}
	status, raw, _ = h.gatewayRaw("GET", "/v1/batches/"+batchID, secret, nil, nil)
	if status < 500 || bytes.Contains(raw, []byte("file-up-out")) {
		t.Fatalf("mapping collision leaked upstream identifier: %d %s", status, raw)
	}
	beforeList := fixture.dials.Load()
	status, raw, _ = h.gatewayRaw("GET", "/v1/batches", secret, nil, nil)
	if status != http.StatusServiceUnavailable || !bytes.Contains(raw, []byte(`"code":"provider_resource_unavailable"`)) || fixture.dials.Load() != beforeList {
		t.Fatalf("unprojectable batch was silently omitted from local list: status=%d body=%s", status, raw)
	}
	if _, err := h.Pool.Exec(t.Context(), `DELETE FROM olp_go.provider_resources WHERE kind='file' AND upstream_id='file-up-out'`); err != nil {
		t.Fatal(err)
	}

	status, batch, _ = h.gateway("GET", "/v1/batches/"+batchID, secret, nil)
	if status != 200 {
		t.Fatalf("get batch: %d %v", status, batch)
	}
	for _, name := range []string{"output_file_id", "error_file_id"} {
		mapped, _ := batch[name].(string)
		if !strings.HasPrefix(mapped, "file_") || strings.Contains(mapped, "up-") {
			t.Fatalf("%s leaked upstream identifier: %v", name, batch)
		}
	}
	status, batch, _ = h.gateway("GET", "/v1/batches/"+batchID, otherSecret, nil)
	if status != 404 {
		t.Fatalf("other key batch: %d %v", status, batch)
	}
	status, batch, _ = h.gateway("POST", "/v1/batches/"+batchID+"/cancel", secret, nil)
	if status != 200 || batch["status"] != "cancelling" {
		t.Fatalf("cancel batch: %d %v", status, batch)
	}

	var metadata string
	if err := h.Pool.QueryRow(t.Context(), "SELECT coalesce(string_agg(metadata::text,''),'') FROM olp_go.provider_resources").Scan(&metadata); err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"upload-content-marker-7", "batch-download-marker-9"} {
		if strings.Contains(metadata, marker) {
			t.Fatalf("provider metadata retained content marker %q", marker)
		}
	}

	detail = h.want(owner, "GET", "/api/v3/providers/"+detail["id"].(string), nil, nil, 200)
	h.want(owner, "PATCH", "/api/v3/providers/"+detail["id"].(string), map[string]any{"name": "Provider state fixture v2", "configuration": map[string]any{"kind": "azure_openai", "auth_mode": "api_key", "endpoint": fixture.URL, "deployment": vendorModel, "api_version": "2024-10-21"}}, etagHeader(detail), 200)
	detail = h.want(owner, "GET", "/api/v3/providers/"+detail["id"].(string), nil, nil, 200)
	h.want(owner, "POST", "/api/v3/providers/"+detail["id"].(string)+"/activate", nil, withMatch(detail, map[string]string{"Idempotency-Key": uuid.NewString()}), 200)
	draft := h.want(owner, "POST", "/api/v3/route-drafts", map[string]any{"slug": slug, "operations": []string{"batch"}, "overall_timeout_ms": 10000, "max_attempts": 1, "targets": []any{map[string]any{"provider_id": detail["id"], "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 5000}}}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	h.want(owner, "POST", "/api/v3/route-drafts/"+draft["id"].(string)+"/activate", nil, withMatch(draft, map[string]string{"Idempotency-Key": uuid.NewString()}), 200)
	h.refresh()
	status, fetched, _ = h.gateway("GET", "/v1/files/"+fileID, secret, nil)
	if status != 200 || fetched["id"] != fileID {
		t.Fatalf("get file after revision churn: %d %v", status, fetched)
	}
	routes := h.want(owner, "GET", "/api/v3/routes", nil, nil, 200)
	var routeID string
	for _, item := range routes["items"].([]any) {
		if item.(map[string]any)["slug"] == slug {
			routeID = item.(map[string]any)["id"].(string)
		}
	}
	route := h.want(owner, "GET", "/api/v3/routes/"+routeID, nil, nil, 200)
	h.want(owner, "POST", "/api/v3/routes/"+routeID+"/retire", nil, withMatch(route, map[string]string{"Idempotency-Key": uuid.NewString()}), 200)
	h.refresh()
	status, fetched, _ = h.gateway("GET", "/v1/files/"+fileID, secret, nil)
	if status != 200 || fetched["id"] != fileID {
		t.Fatalf("get file after retire: %d %v", status, fetched)
	}
	status, deleted, _ := h.gateway("DELETE", "/v1/files/"+fileID, secret, nil)
	if status != 200 || deleted["deleted"] != true {
		t.Fatalf("delete file after retire: %d %v", status, deleted)
	}
}

func TestResponseLifecycle(t *testing.T) {
	fixture := newOpenAIFixture(t, "")
	fixture.resps["resp-up-1"]["output"] = []any{map[string]any{"id": "msg-up-1", "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "response-output-marker-3", "annotations": []any{}}}}}
	h := newAccessHarness(t)
	owner, detail, slug, _ := provisionOpenAI(t, h, fixture.URL,
		[]any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}},
		[]string{"generation"})
	plain := stateKey(t, h, owner, slug, false)
	opted := stateKey(t, h, owner, slug, true)
	otherKey := h.want(owner, "POST", "/api/v3/api-keys", map[string]any{"name": "other state key", "scopes": []string{"inference"}, "allowed_routes": []string{slug}, "allow_provider_state": true}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	other, otherKeyID := otherKey["secret"].(string), otherKey["id"].(string)
	h.refresh()

	status, refused, _ := h.gateway("POST", "/v1/responses", plain, map[string]any{"model": slug, "input": "hi", "background": true})
	if status != 400 {
		t.Fatalf("stateful request without opt-in: %d %v", status, refused)
	}
	status, refused, _ = h.gateway("GET", "/v1/responses/response_missing", plain, nil)
	if status != 403 || h.gatewayCode(status, refused) != "provider_state_forbidden" {
		t.Fatalf("lifecycle without opt-in: %d %v", status, refused)
	}

	status, created, _ := h.gateway("POST", "/v1/responses", opted, map[string]any{"model": slug, "input": "prompt-marker-1", "background": true, "store": true})
	if status != 200 {
		t.Fatalf("create response: %d %v", status, created)
	}
	local, _ := created["id"].(string)
	if !strings.HasPrefix(local, "response_") || strings.Contains(local, "resp-up-1") {
		t.Fatalf("response identifier leaked upstream id: %v", created)
	}
	if raw, _ := json.Marshal(created); bytes.Contains(raw, []byte("resp-up-1")) {
		t.Fatalf("response body leaked upstream id: %s", raw)
	}

	block := func(upstreamID string) {
		t.Helper()
		if _, err := h.Pool.Exec(t.Context(), `INSERT INTO olp_go.provider_resources
			(id,kind,api_key_id,route_slug,provider_id,provider_revision_id,route_revision_id,slot_id,credential_id,upstream_id,state,metadata)
			SELECT gen_random_uuid(),'response',$1,route_slug,provider_id,provider_revision_id,route_revision_id,slot_id,credential_id,$2,'completed','{}'
			FROM olp_go.provider_resources WHERE kind='response' AND upstream_id='resp-up-1'`, otherKeyID, upstreamID); err != nil {
			t.Fatal(err)
		}
	}
	fixture.respID.Store("resp-up-blocked")
	block("resp-up-blocked")
	status, raw, _ := h.gatewayRaw("POST", "/v1/responses", opted, strings.NewReader(`{"model":"`+slug+`","input":"hi","store":true}`), map[string]string{"Content-Type": "application/json"})
	if status < 500 || bytes.Contains(raw, []byte("resp-up-blocked")) {
		t.Fatalf("unary mapping collision leaked upstream identifier: %d %s", status, raw)
	}

	fixture.respID.Store("resp-up-stream")
	block("resp-up-stream")
	status, raw, header := h.gatewayRaw("POST", "/v1/responses", opted, strings.NewReader(`{"model":"`+slug+`","input":"hi","store":true,"stream":true}`), map[string]string{"Content-Type": "application/json"})
	if status < 500 || bytes.Contains(raw, []byte("resp-up-stream")) || strings.Contains(header.Get("Content-Type"), "event-stream") {
		t.Fatalf("streamed mapping collision emitted an offending frame: %d %s %v", status, raw, header.Get("Content-Type"))
	}
	fixture.respID.Store("resp-up-1")

	status, fetched, _ := h.gateway("GET", "/v1/responses/"+local, opted, nil)
	if status != 200 || fetched["id"] != local {
		t.Fatalf("get response: %d %v", status, fetched)
	}
	status, items, _ := h.gateway("GET", "/v1/responses/"+local+"/input_items", opted, nil)
	if status != 200 || items["object"] != "list" {
		t.Fatalf("input items: %d %v", status, items)
	}
	status, fetched, _ = h.gateway("GET", "/v1/responses/"+local, other, nil)
	if status != 404 {
		t.Fatalf("other key response: %d %v", status, fetched)
	}

	status, next, _ := h.gateway("POST", "/v1/responses", opted, map[string]any{"model": slug, "input": "follow-up", "previous_response_id": local, "store": true})
	if status != 200 {
		t.Fatalf("chained response: %d %v", status, next)
	}
	sent, _ := fixture.lastReq.Load().(map[string]any)
	if sent["previous_response_id"] != "resp-up-1" {
		t.Fatalf("previous_response_id was not rewritten upstream: %v", sent)
	}

	status, cancelled, _ := h.gateway("POST", "/v1/responses/"+local+"/cancel", opted, nil)
	if status != 200 || cancelled["status"] != "cancelled" {
		t.Fatalf("cancel response: %d %v", status, cancelled)
	}

	var metadata string
	if err := h.Pool.QueryRow(t.Context(), "SELECT coalesce(string_agg(metadata::text,''),'') FROM olp_go.provider_resources").Scan(&metadata); err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"prompt-marker-1", "response-output-marker-3"} {
		if strings.Contains(metadata, marker) {
			t.Fatalf("provider metadata retained content marker %q", marker)
		}
	}

	detail = h.want(owner, "GET", "/api/v3/providers/"+detail["id"].(string), nil, nil, 200)
	h.want(owner, "PATCH", "/api/v3/providers/"+detail["id"].(string), map[string]any{"name": "Responses fixture v2", "configuration": map[string]any{"kind": "azure_openai", "auth_mode": "api_key", "endpoint": fixture.URL, "deployment": vendorModel, "api_version": "2024-10-21"}}, etagHeader(detail), 200)
	detail = h.want(owner, "GET", "/api/v3/providers/"+detail["id"].(string), nil, nil, 200)
	h.want(owner, "POST", "/api/v3/providers/"+detail["id"].(string)+"/activate", nil, withMatch(detail, map[string]string{"Idempotency-Key": uuid.NewString()}), 200)
	h.refresh()
	status, fetched, _ = h.gateway("GET", "/v1/responses/"+local, opted, nil)
	if status != 200 || fetched["id"] != local {
		t.Fatalf("get response after new revision: %d %v", status, fetched)
	}

	slots := h.want(owner, "GET", "/api/v3/providers/"+detail["id"].(string)+"/credential-slots", nil, nil, 200)
	credentialID := slots["items"].([]any)[0].(map[string]any)["credential_version_id"].(string)
	detail = h.want(owner, "GET", "/api/v3/providers/"+detail["id"].(string), nil, nil, 200)
	h.want(owner, "POST", "/api/v3/providers/"+detail["id"].(string)+"/credentials/"+credentialID+"/revoke", nil, withMatch(detail, map[string]string{"Idempotency-Key": uuid.NewString()}), 200)
	h.refresh()
	status, fetched, _ = h.gateway("GET", "/v1/responses/"+local, opted, nil)
	if status != 409 || h.gatewayCode(status, fetched) != "provider_resource_credential_unavailable" {
		t.Fatalf("revoked credential: %d %v", status, fetched)
	}
}

type realtimeFixture struct {
	*httptest.Server
	dials atomic.Int64
}

func newRealtimeFixture(t *testing.T) *realtimeFixture {
	f := &realtimeFixture{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /openai/deployments/{dep}/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"id": "chatcmpl-up-1", "object": "chat.completion", "created": 1, "model": r.PathValue("dep"), "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "OK"}, "finish_reason": "stop"}}, "usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}})
	})
	mux.HandleFunc("POST /openai/deployments/{dep}/responses", func(w http.ResponseWriter, r *http.Request) {
		writeResponsesFixture(w, r.PathValue("dep"), "OK", false)
	})
	mux.HandleFunc("POST /openai/deployments/{dep}/embeddings", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"object": "list", "data": []any{map[string]any{"object": "embedding", "index": 0, "embedding": []float64{0.1, 0.2}}}, "model": r.PathValue("dep"), "usage": map[string]any{"prompt_tokens": 2, "total_tokens": 2}})
	})
	mux.HandleFunc("GET /openai/realtime", func(w http.ResponseWriter, r *http.Request) {
		f.dials.Add(1)
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer conn.CloseNow()
		for {
			typ, data, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			if string(data) == "BIG" {
				data = bytes.Repeat([]byte("x"), 1<<17)
			}
			if err := conn.Write(r.Context(), typ, data); err != nil {
				return
			}
		}
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func TestRealtimeIngress(t *testing.T) {
	fixture := newRealtimeFixture(t)
	h := newAccessHarness(t)
	sink := &captureSink{}
	h.Gateway.Sink = sink
	owner, _, slug, secret := provisionOpenAI(t, h, fixture.URL,
		[]any{map[string]any{"operation": "realtime", "surface": "openai", "mode": "realtime"}},
		[]string{"realtime"})
	wsURL := strings.Replace(h.HTTP.URL, "http://", "ws://", 1) + "/v1/realtime?model=" + slug
	admissionReleased := func(label string) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
			if h.Gateway.AdmissionPool().Admitted() == 0 {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("%s: admission still held: %d", label, h.Gateway.AdmissionPool().Admitted())
	}

	before := fixture.dials.Load()
	status, _, _ := h.gatewayRaw("GET", "/v1/realtime?model="+slug, secret, nil, nil)
	if status != 400 || fixture.dials.Load() != before {
		t.Fatalf("ordinary GET reached upstream or was not refused: %d", status)
	}
	admissionReleased("ordinary GET")

	headers := http.Header{"Authorization": {"Bearer " + secret}, "Origin": {"https://evil.test"}}
	if _, _, err := websocket.Dial(t.Context(), wsURL, &websocket.DialOptions{HTTPHeader: headers}); err == nil {
		t.Fatal("denied origin opened a realtime session")
	}
	if fixture.dials.Load() != before {
		t.Fatal("denied origin reached the provider dial")
	}
	admissionReleased("denied origin")

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	client, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + secret}}})
	if err != nil {
		t.Fatalf("realtime dial: %v", err)
	}
	if err := client.Write(ctx, websocket.MessageText, []byte("hello upstream")); err != nil {
		t.Fatalf("client write: %v", err)
	}
	_, data, err := client.Read(ctx)
	if err != nil || string(data) != "hello upstream" {
		t.Fatalf("relay did not echo: %v %q", err, data)
	}

	beforeTerminal := sink.count()
	if err := client.Write(ctx, websocket.MessageText, []byte("BIG")); err != nil {
		t.Fatalf("size probe write: %v", err)
	}
	if _, _, err := client.Read(ctx); err == nil {
		t.Fatal("oversized upstream frame was relayed")
	}
	client.Close(websocket.StatusNormalClosure, "")
	admissionReleased("oversized frame")
	if sink.count() != beforeTerminal+1 {
		t.Fatalf("oversized frame terminal count=%d, want one new terminal", sink.count()-beforeTerminal)
	}
	oversized := sink.last()
	if oversized.Outcome != "failure" || oversized.ErrorClass != "realtime_incomplete" || !oversized.Committed || len(oversized.Attempts) != 1 || oversized.Attempts[0].Class != "protocol" {
		t.Fatalf("oversized frame was recorded as success: %+v", oversized)
	}

	client, _, err = websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + secret}}})
	if err != nil {
		t.Fatalf("second realtime dial: %v", err)
	}
	keys := h.want(owner, "GET", "/api/v3/api-keys", nil, nil, 200)
	var keyID string
	for _, item := range keys["items"].([]any) {
		keyID = item.(map[string]any)["id"].(string)
	}
	record := h.want(owner, "GET", "/api/v3/api-keys/"+keyID, nil, nil, 200)
	h.want(owner, "POST", "/api/v3/api-keys/"+keyID+"/revoke", nil, withMatch(record, map[string]string{"Idempotency-Key": uuid.NewString()}), 200)
	h.refresh()
	deadline := time.Now().Add(12 * time.Second)
	var closeErr error
	for time.Now().Before(deadline) {
		if _, _, closeErr = client.Read(ctx); closeErr != nil {
			break
		}
	}
	if closeErr == nil {
		t.Fatal("revoked key kept its realtime session")
	}
	if code := websocket.CloseStatus(closeErr); code != websocket.StatusPolicyViolation {
		t.Fatalf("revocation close status %v, want policy violation", code)
	}
	admissionReleased("revoked key")
	revoked := sink.last()
	if revoked.Outcome != "failure" || revoked.ErrorClass != "key_revoked" || !revoked.Committed || len(revoked.Attempts) != 1 || revoked.Attempts[0].Class != "credential" {
		t.Fatalf("revoked realtime session lost its failure category: %+v", revoked)
	}
}

type bedrockFixture struct {
	*httptest.Server
	lastAuth   atomic.Value
	lastPath   atomic.Value
	big        atomic.Bool
	streamBody func(w http.ResponseWriter)
}

func bedrockEvent(eventType string, payload any) eventstream.Message {
	body, _ := json.Marshal(payload)
	return eventstream.Message{Headers: eventstream.Headers{
		{Name: ":message-type", Value: eventstream.StringValue("event")},
		{Name: ":event-type", Value: eventstream.StringValue(eventType)},
		{Name: ":content-type", Value: eventstream.StringValue("application/json")},
	}, Payload: body}
}

func newBedrockFixture(t *testing.T) *bedrockFixture {
	f := &bedrockFixture{}
	f.streamBody = func(w http.ResponseWriter) {
		encoder := eventstream.NewEncoder()
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		for _, message := range []eventstream.Message{
			bedrockEvent("messageStart", map[string]any{"role": "assistant"}),
			bedrockEvent("contentBlockDelta", map[string]any{"delta": map[string]any{"text": "OK"}, "contentBlockIndex": 0}),
			bedrockEvent("messageStop", map[string]any{"stopReason": "end_turn"}),
			bedrockEvent("metadata", map[string]any{"usage": map[string]any{"inputTokens": 4, "outputTokens": 6}, "metrics": map[string]any{"latencyMs": 1}}),
		} {
			if err := encoder.Encode(w, message); err != nil {
				return
			}
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		f.lastAuth.Store(r.Header.Get("Authorization"))
		f.lastPath.Store(r.URL.Path)
		if r.Method == "GET" {
			writeJSON(w, map[string]any{"modelSummaries": []any{map[string]string{"modelId": "anthropic.claude-3-haiku-20240307-v1:0", "modelName": "Claude"}}})
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/converse"):
			if f.big.Load() {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"output":{"message":{"role":"assistant","content":[{"text":%q}]}},"stopReason":"end_turn","usage":{"inputTokens":4,"outputTokens":6,"totalTokens":10}}`, strings.Repeat("x", (1<<20)+64))
				return
			}
			writeJSON(w, map[string]any{"output": map[string]any{"message": map[string]any{"role": "assistant", "content": []any{map[string]any{"text": "OK"}}}}, "stopReason": "end_turn", "usage": map[string]any{"inputTokens": 4, "outputTokens": 6, "totalTokens": 10}})
		case strings.HasSuffix(r.URL.Path, "/converse-stream"):
			f.streamBody(w)
		default:
			http.Error(w, "unexpected "+r.URL.Path, 404)
		}
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func provisionBedrock(t *testing.T, h *accessHarness, endpoint, model string, capabilities []any, operations []string) (*browser, map[string]any, string, string) {
	return provisionBedrockContract(t, h, endpoint, model, capabilities, operations, false)
}

func provisionBedrockContract(t *testing.T, h *accessHarness, endpoint, model string, capabilities []any, operations []string, strict bool) (*browser, map[string]any, string, string) {
	t.Helper()
	owner := h.owner()
	create := map[string]any{"name": "Bedrock fixture", "configuration": map[string]any{"kind": "bedrock", "auth_mode": "static", "endpoint": endpoint, "cloud_region": "us-east-1"}, "model": model, "credential": `{"access_key_id":"BEDROCKKEY1234567890","secret_access_key":"bedrock-secret-123456789","session_token":"bedrock-session-token"}`}
	if strict {
		config := create["configuration"].(map[string]any)
		config["profile_id"], config["profile_revision"] = "bedrock-converse", "1"
	}
	detail := h.want(owner, "POST", "/api/v3/providers", create, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	path := "/api/v3/providers/" + detail["id"].(string)
	probe := h.want(owner, "POST", path+"/probe", nil, etagHeader(detail), 200)
	if probe["succeeded"] != true {
		t.Fatalf("provider probe: %v", probe)
	}
	models := h.want(owner, "GET", path+"/models", nil, nil, 200)
	modelID := models["items"].([]any)[0].(map[string]any)["id"].(string)
	detail = h.want(owner, "PATCH", path+"/models/"+modelID, map[string]any{"enabled": true, "capabilities": capabilities}, etagHeader(detail), 200)
	certified := h.want(owner, "POST", path+"/models/"+modelID+"/certify", nil, etagHeader(detail), 200)
	if certified["status"] != "certified" {
		t.Fatalf("capability proof: %v", certified)
	}
	detail = h.want(owner, "GET", path, nil, nil, 200)
	h.want(owner, "POST", path+"/activate", nil, withMatch(detail, map[string]string{"Idempotency-Key": uuid.NewString()}), 200)
	slug := "bedrock-" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	routeInput := map[string]any{"slug": slug, "operations": operations, "overall_timeout_ms": 10000, "max_attempts": 1, "targets": []any{map[string]any{"provider_id": detail["id"], "provider_model": model, "priority": 0, "weight": 1, "timeout_ms": 5000}}}
	if strict {
		routeInput["fidelity"] = map[string]any{}
	}
	draft := h.want(owner, "POST", "/api/v3/route-drafts", routeInput, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	h.want(owner, "POST", "/api/v3/route-drafts/"+draft["id"].(string)+"/activate", nil, withMatch(draft, map[string]string{"Idempotency-Key": uuid.NewString()}), 200)
	key := h.want(owner, "POST", "/api/v3/api-keys", map[string]any{"name": "bedrock key", "scopes": []string{"inference"}, "allowed_routes": []string{slug}}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	h.refresh()
	return owner, detail, slug, key["secret"].(string)
}

func bedrockSDK(h *accessHarness, secret string) *bedrockruntime.Client {
	cfg := aws.Config{Region: "us-east-1", Credentials: credentials.NewStaticCredentialsProvider("sdk-akid-not-olp", "sdk-secret-not-olp", "")}
	return bedrockruntime.NewFromConfig(cfg, func(o *bedrockruntime.Options) {
		o.BaseEndpoint = aws.String(h.HTTP.URL + "/bedrock")
		o.APIOptions = append(o.APIOptions, func(stack *middleware.Stack) error {
			return stack.Build.Add(middleware.BuildMiddlewareFunc("OLPGatewayKey", func(ctx context.Context, in middleware.BuildInput, next middleware.BuildHandler) (middleware.BuildOutput, middleware.Metadata, error) {
				if req, ok := in.Request.(*smithyhttp.Request); ok {
					req.Header.Set("X-OLP-API-Key", secret)
				}
				return next.HandleBuild(ctx, in)
			}), middleware.After)
		})
	})
}

func TestBedrockIngress(t *testing.T) {
	testBedrockIngress(t, false)
}

func TestStrictBedrockIngress(t *testing.T) {
	testBedrockIngress(t, true)
}

func testBedrockIngress(t *testing.T, strict bool) {
	fixture := newBedrockFixture(t)
	if strict {
		fixture.streamBody = func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
			for _, message := range []eventstream.Message{
				bedrockEvent("messageStart", map[string]any{"role": "assistant"}),
				bedrockEvent("contentBlockDelta", map[string]any{"delta": map[string]any{"text": "OK"}, "contentBlockIndex": 0}),
				bedrockEvent("contentBlockStop", map[string]any{"contentBlockIndex": 0}),
				bedrockEvent("messageStop", map[string]any{"stopReason": "end_turn"}),
				bedrockEvent("metadata", map[string]any{"usage": map[string]any{"inputTokens": 4, "outputTokens": 6, "totalTokens": 10}, "metrics": map[string]any{"latencyMs": 1}}),
			} {
				if err := eventstream.NewEncoder().Encode(w, message); err != nil {
					t.Error(err)
					return
				}
			}
		}
	}
	h := newAccessHarness(t)
	owner, detail, slug, secret := provisionBedrockContract(t, h, fixture.URL, "anthropic.claude-3-haiku-20240307-v1:0",
		[]any{
			map[string]any{"operation": "generation", "surface": "bedrock", "mode": "unary"},
			map[string]any{"operation": "generation", "surface": "bedrock", "mode": "streaming"},
		}, []string{"generation"}, strict)

	client := bedrockSDK(h, secret)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	out, err := client.Converse(ctx, &bedrockruntime.ConverseInput{
		ModelId:  aws.String(slug),
		Messages: []btypes.Message{{Role: btypes.ConversationRoleUser, Content: []btypes.ContentBlock{&btypes.ContentBlockMemberText{Value: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("converse: %v", err)
	}
	if path, _ := fixture.lastPath.Load().(string); path != "/model/anthropic.claude-3-haiku-20240307-v1:0/converse" {
		t.Fatalf("route slug did not map to the upstream model: %s", path)
	}
	auth, _ := fixture.lastAuth.Load().(string)
	if !strings.Contains(auth, "Credential=BEDROCKKEY1234567890/") || strings.Contains(auth, "sdk-akid-not-olp") {
		t.Fatalf("upstream did not receive OLP's fresh signature: %s", auth)
	}
	message, ok := out.Output.(*btypes.ConverseOutputMemberMessage)
	if !ok || len(message.Value.Content) == 0 {
		t.Fatalf("converse output: %+v", out.Output)
	}

	stream, err := client.ConverseStream(ctx, &bedrockruntime.ConverseStreamInput{
		ModelId:  aws.String(slug),
		Messages: []btypes.Message{{Role: btypes.ConversationRoleUser, Content: []btypes.ContentBlock{&btypes.ContentBlockMemberText{Value: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("converse-stream: %v", err)
	}
	var text string
	for event := range stream.GetStream().Events() {
		if delta, ok := event.(*btypes.ConverseStreamOutputMemberContentBlockDelta); ok {
			if block, ok := delta.Value.Delta.(*btypes.ContentBlockDeltaMemberText); ok {
				text += block.Value
			}
		}
	}
	if err := stream.GetStream().Close(); err != nil {
		t.Fatalf("converse-stream close: %v", err)
	}
	if text != "OK" {
		t.Fatalf("eventstream relay produced %q", text)
	}

	status, _, _ := h.gatewayRaw("POST", "/bedrock/model/"+slug+"/converse", "", strings.NewReader(`{"messages":[]}`), map[string]string{
		"Authorization":        "AWS4-HMAC-SHA256 Credential=sdk-akid-not-olp/20240101/us-east-1/bedrock/aws4_request, SignedHeaders=host, Signature=deadbeef",
		"X-Amz-Date":           "20240101T000000Z",
		"X-Amz-Security-Token": "session",
		"Content-Type":         "application/json",
	})
	if status != 401 {
		t.Fatalf("SigV4-only request: %d", status)
	}

	fixture.big.Store(true)
	status, raw, _ := h.gatewayRaw("POST", "/bedrock/model/"+slug+"/converse", "", strings.NewReader(`{"messages":[{"role":"user","content":[{"text":"hi"}]}]}`), map[string]string{
		"X-OLP-API-Key": secret,
		"Content-Type":  "application/json",
	})
	fixture.big.Store(false)
	if status != 502 || bytes.Contains(raw, []byte("stopReason")) {
		t.Fatalf("oversized unary reply relayed truncated bytes: %d %.200s", status, raw)
	}

	unsupported := h.want(owner, "POST", "/api/v3/providers", map[string]any{"name": "Bedrock unsupported", "configuration": map[string]any{"kind": "bedrock", "auth_mode": "static", "endpoint": fixture.URL, "cloud_region": "us-east-1"}, "model": "custom.unsupported-v1:0", "credential": `{"access_key_id":"BEDROCKKEY1234567890","secret_access_key":"bedrock-secret-123456789"}`}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	upPath := "/api/v3/providers/" + unsupported["id"].(string)
	h.want(owner, "POST", upPath+"/probe", nil, etagHeader(unsupported), 200)
	models := h.want(owner, "GET", upPath+"/models", nil, nil, 200)
	upModelID := models["items"].([]any)[0].(map[string]any)["id"].(string)
	unsupported = h.want(owner, "PATCH", upPath+"/models/"+upModelID, map[string]any{"enabled": true, "capabilities": []any{map[string]any{"operation": "bedrock_invoke", "surface": "bedrock", "mode": "unary"}}}, etagHeader(unsupported), 200)
	result := h.want(owner, "POST", upPath+"/models/"+upModelID+"/certify", nil, etagHeader(unsupported), 200)
	if result["status"] != "failed" {
		t.Fatalf("unsupported invoke model certified: %v", result)
	}
	results, _ := result["results"].([]any)
	if len(results) == 0 || results[0].(map[string]any)["error_code"] != "capability_unavailable" {
		t.Fatalf("unsupported invoke certification: %v", result)
	}

	invokeDraft := h.want(owner, "POST", "/api/v3/route-drafts", map[string]any{"slug": "invoke-" + strings.ReplaceAll(uuid.NewString()[:8], "-", ""), "operations": []string{"bedrock_invoke"}, "overall_timeout_ms": 10000, "max_attempts": 1, "targets": []any{map[string]any{"provider_id": detail["id"], "provider_model": "anthropic.claude-3-haiku-20240307-v1:0", "priority": 0, "weight": 1, "timeout_ms": 5000}}}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	status, body, _ := h.request(owner, "POST", "/api/v3/route-drafts/"+invokeDraft["id"].(string)+"/activate", nil, withMatch(invokeDraft, map[string]string{"Idempotency-Key": uuid.NewString()}))
	problem, _ := body["detail"].(string)
	if status != 422 || !strings.Contains(problem, "bedrock_invoke") {
		t.Fatalf("invoke route without certified tuple activated: %d %v", status, body)
	}
}

func TestHistoricalResourceModel(t *testing.T) {
	fixture := newOpenAIFixture(t, "")
	h := newAccessHarness(t)
	modelA, modelB := "fixture-model-a", "fixture-model-b"
	owner := h.owner()
	configuration := map[string]any{"kind": "azure_openai", "auth_mode": "api_key", "endpoint": fixture.URL, "deployment": modelA, "api_version": "2024-10-21", "options": map[string]any{"models": map[string]any{modelB: map[string]any{"deployment": modelB}}}}
	create := map[string]any{"name": "Two model fixture", "configuration": configuration, "model": modelA, "credential": vendorSecret}
	detail := h.want(owner, "POST", "/api/v3/providers", create, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	path := "/api/v3/providers/" + detail["id"].(string)
	probe := h.want(owner, "POST", path+"/probe", nil, etagHeader(detail), 200)
	if probe["succeeded"] != true {
		t.Fatalf("provider probe: %v", probe)
	}
	h.want(owner, "POST", path+"/discovery", map[string]any{"models": []any{map[string]any{"upstream_model": modelB, "display_name": modelB}}}, etagHeader(detail), 200)
	detail = h.want(owner, "GET", path, nil, nil, 200)
	models := h.want(owner, "GET", path+"/models", nil, nil, 200)
	ids := map[string]string{}
	for _, item := range models["items"].([]any) {
		m := item.(map[string]any)
		ids[m["upstream_model"].(string)] = m["id"].(string)
	}
	caps := []any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}}
	for _, model := range []string{modelA, modelB} {
		detail = h.want(owner, "PATCH", path+"/models/"+ids[model], map[string]any{"enabled": true, "capabilities": caps}, etagHeader(detail), 200)
		certified := h.want(owner, "POST", path+"/models/"+ids[model]+"/certify", nil, etagHeader(detail), 200)
		if certified["status"] != "certified" {
			t.Fatalf("%s capability proof: %v", model, certified)
		}
		detail = h.want(owner, "GET", path, nil, nil, 200)
	}
	h.want(owner, "POST", path+"/activate", nil, withMatch(detail, map[string]string{"Idempotency-Key": uuid.NewString()}), 200)
	slug := "twomodel-" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	targets := func(priorityA, priorityB int) []any {
		return []any{
			map[string]any{"provider_id": detail["id"], "provider_model": modelA, "priority": priorityA, "weight": 1, "timeout_ms": 5000},
			map[string]any{"provider_id": detail["id"], "provider_model": modelB, "priority": priorityB, "weight": 1, "timeout_ms": 5000},
		}
	}
	draft := h.want(owner, "POST", "/api/v3/route-drafts", map[string]any{"slug": slug, "operations": []string{"generation"}, "overall_timeout_ms": 10000, "max_attempts": 1, "targets": targets(1, 0)}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	h.want(owner, "POST", "/api/v3/route-drafts/"+draft["id"].(string)+"/activate", nil, withMatch(draft, map[string]string{"Idempotency-Key": uuid.NewString()}), 200)
	secret := stateKey(t, h, owner, slug, true)
	h.refresh()

	status, created, _ := h.gateway("POST", "/v1/responses", secret, map[string]any{"model": slug, "input": "hi", "store": true})
	if status != 200 {
		t.Fatalf("create response: %d %v", status, created)
	}
	if upstream, _ := fixture.lastPath.Load().(string); upstream != "/openai/deployments/"+modelB+"/responses" {
		t.Fatalf("create did not pin the second model: %s", upstream)
	}
	local, _ := created["id"].(string)
	if !strings.HasPrefix(local, "response_") {
		t.Fatalf("response identifier leaked upstream id: %v", created)
	}

	detail = h.want(owner, "GET", path, nil, nil, 200)
	h.want(owner, "PATCH", path, map[string]any{"name": "Two model fixture v2", "configuration": create["configuration"]}, etagHeader(detail), 200)
	detail = h.want(owner, "GET", path, nil, nil, 200)
	h.want(owner, "POST", path+"/activate", nil, withMatch(detail, map[string]string{"Idempotency-Key": uuid.NewString()}), 200)
	draft = h.want(owner, "POST", "/api/v3/route-drafts", map[string]any{"slug": slug, "operations": []string{"generation"}, "overall_timeout_ms": 10000, "max_attempts": 1, "targets": targets(0, 1)}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	h.want(owner, "POST", "/api/v3/route-drafts/"+draft["id"].(string)+"/activate", nil, withMatch(draft, map[string]string{"Idempotency-Key": uuid.NewString()}), 200)
	h.refresh()

	status, fetched, _ := h.gateway("GET", "/v1/responses/"+local, secret, nil)
	if status != 200 || fetched["id"] != local {
		t.Fatalf("get response after churn: %d %v", status, fetched)
	}
	if upstream, _ := fixture.lastPath.Load().(string); !strings.Contains(upstream, "/openai/deployments/"+modelB+"/") {
		t.Fatalf("historical pin resolved the wrong model: %s", upstream)
	}
}

func TestResponseRetrievalAccounting(t *testing.T) {
	for _, background := range []bool{false, true} {
		t.Run(fmt.Sprintf("background=%t", background), func(t *testing.T) {
			fixture := newOpenAIFixture(t, "")
			h := newAccessHarness(t)
			owner, _, slug, _ := provisionOpenAI(t, h, fixture.URL,
				[]any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}},
				[]string{"generation"})
			h.want(owner, "POST", "/api/v3/pricing/revisions", map[string]any{
				"effective_at": time.Now().UTC().Format(time.RFC3339Nano),
				"prices":       []any{repPrice("azure_openai", vendorModel, "generation")},
			}, idem("response-prices"), 201)
			key := stateKey(t, h, owner, slug, true)
			// Runtime pricing refreshes independently of publication.
			time.Sleep(time.Until(h.Runtime.RoutingInputs().RefreshedAt.Add(10*time.Second + 10*time.Millisecond)))
			h.refresh()
			if len(h.Runtime.RoutingInputs().Prices) == 0 {
				t.Fatal("pricing was not installed")
			}
			sink := &captureSink{}
			h.Gateway.Sink = sink
			if background {
				fixture.resps["resp-up-1"]["status"] = "in_progress"
				fixture.resps["resp-up-1"]["usage"] = nil
			}
			status, created, _ := h.gateway("POST", "/v1/responses", key, map[string]any{"model": slug, "input": "hi", "store": true, "background": background})
			if status != 200 {
				t.Fatalf("create: %d %v", status, created)
			}
			original := sink.last()
			local := created["id"].(string)
			if background {
				status, _, _ = h.gateway("GET", "/v1/responses/"+local, key, nil)
				if status != 200 {
					t.Fatal("pending poll failed", status)
				}
				fixture.resps["resp-up-1"]["status"] = "completed"
				fixture.resps["resp-up-1"]["usage"] = map[string]any{"input_tokens": 4, "output_tokens": 6, "total_tokens": 10}
			}
			var wg sync.WaitGroup
			for range 3 {
				wg.Go(func() {
					status, out, _ := h.gateway("GET", "/v1/responses/"+local, key, nil)
					if status != 200 {
						t.Errorf("poll: %d %v", status, out)
					}
				})
			}
			wg.Wait()
			status, out, _ := h.gateway("POST", "/v1/responses/"+local+"/cancel", key, nil)
			if status != 200 {
				t.Fatalf("cancel: %d %v", status, out)
			}
			for _, ev := range sink.all()[1:] {
				for _, attempt := range ev.Attempts {
					if attempt.UsageObserved || attempt.BillingUncertain || attempt.Usage != nil {
						t.Fatalf("resource operation carried generation usage: %+v", attempt)
					}
				}
			}
			if background {
				var count, input, output int64
				var costCorrect bool
				if err := h.Pool.QueryRow(t.Context(), "SELECT count(*), coalesce(sum(input_tokens),0), coalesce(sum(output_tokens),0), sum(estimated_cost)=0.000024 FROM olp_go.attempt_usage_facts WHERE request_id=$1", original.AccountingID).Scan(&count, &input, &output, &costCorrect); err != nil {
					t.Fatal(err)
				}
				if count != 1 || input != 4 || output != 6 || !costCorrect {
					t.Fatalf("background usage: count=%d input=%d output=%d", count, input, output)
				}
			} else if !original.Attempts[0].UsageObserved {
				t.Fatal("generation lost its usage")
			}
		})
	}
}

func TestRealtimeConcurrencyLeasesOutliveRouteTimeout(t *testing.T) {
	for _, scope := range []string{"provider", "slot"} {
		t.Run(scope, func(t *testing.T) {
			fixture := newRealtimeFixture(t)
			h := newAccessHarness(t)
			owner, detail, slug, secret := provisionOpenAI(t, h, fixture.URL,
				[]any{map[string]any{"operation": "realtime", "surface": "openai", "mode": "realtime"}}, []string{"realtime"})
			path := "/api/v3/providers/" + detail["id"].(string)
			detail = h.want(owner, "GET", path, nil, nil, 200)
			if scope == "provider" {
				cfg := detail["configuration"].(map[string]any)
				options, _ := cfg["options"].(map[string]any)
				if options == nil {
					options = map[string]any{}
					cfg["options"] = options
				}
				options["limits"] = map[string]any{"max_concurrency": 1}
				h.want(owner, "PATCH", path, map[string]any{"name": "Realtime limited", "configuration": cfg}, etagHeader(detail), 200)
			} else {
				slots := h.want(owner, "GET", path+"/credential-slots", nil, nil, 200)
				slot := slots["items"].([]any)[0].(map[string]any)
				h.want(owner, "PUT", path+"/credential-slots/"+slot["id"].(string), map[string]any{"slot": map[string]any{"name": "default", "max_concurrency": 1}}, withMatch(slots, idem("limit-slot")), 200)
			}
			detail = h.want(owner, "GET", path, nil, nil, 200)
			h.want(owner, "POST", path+"/activate", nil, withMatch(detail, idem("activate-limit")), 200)
			c := limClient(t)
			h.Gateway.Admission = gateway.NewAdmission(limLimiter(t, c, limNamespace(t, c, "realtime-"+scope)), func() limits.OutagePolicy { return limits.FailClosed }, slog.New(slog.DiscardHandler))
			h.refresh()
			ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
			defer cancel()
			endpoint := strings.Replace(h.HTTP.URL, "http://", "ws://", 1) + "/v1/realtime?model=" + slug
			opts := &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + secret}}}
			first, _, err := websocket.Dial(ctx, endpoint, opts)
			if err != nil {
				t.Fatal(err)
			}
			defer first.CloseNow()
			// The published route timeout is ten seconds; the socket stays active.
			timer := time.NewTimer(11 * time.Second)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if err = first.Write(ctx, websocket.MessageText, []byte("still alive")); err != nil {
				t.Fatal(err)
			}
			if _, _, err = first.Read(ctx); err != nil {
				t.Fatal(err)
			}
			second, resp, err := websocket.Dial(ctx, endpoint, opts)
			if err == nil {
				second.CloseNow()
				t.Fatal("second session exceeded concurrency limit")
			}
			if resp == nil || resp.StatusCode != 429 {
				t.Fatalf("second session: response=%v err=%v", resp, err)
			}
			first.Close(websocket.StatusNormalClosure, "")
			glEventually(t, "realtime lease release", func() bool {
				next, _, err := websocket.Dial(ctx, endpoint, opts)
				if err != nil {
					return false
				}
				next.Close(websocket.StatusNormalClosure, "")
				return true
			})
		})
	}
}

func TestBackgroundResponseStreamAccounting(t *testing.T) {
	fixture := newOpenAIFixture(t, "")
	h := newAccessHarness(t)
	owner, _, slug, _ := provisionOpenAI(t, h, fixture.URL,
		[]any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}, map[string]any{"operation": "generation", "surface": "openai", "mode": "streaming"}}, []string{"generation"})
	key := stateKey(t, h, owner, slug, true)
	h.refresh()
	sink := &captureSink{}
	h.Gateway.Sink = sink
	status, raw, _ := h.gatewayRaw("POST", "/v1/responses", key, strings.NewReader(`{"model":"`+slug+`","input":"hi","store":true,"background":true,"stream":true}`),
		map[string]string{"Content-Type": "application/json", "X-Request-Id": "client-request-id"})
	if status != 200 {
		t.Fatalf("stream: %d %s", status, raw)
	}
	var local string
	for line := range strings.SplitSeq(string(raw), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event struct {
			Response struct {
				ID string `json:"id"`
			} `json:"response"`
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event) == nil && event.Response.ID != "" {
			local = event.Response.ID
		}
	}
	if local == "" {
		t.Fatalf("no resource in stream: %s", raw)
	}
	original := sink.last()
	var count, input, output int64
	if err := h.Pool.QueryRow(t.Context(), "SELECT count(*),coalesce(sum(input_tokens),0),coalesce(sum(output_tokens),0) FROM olp_go.attempt_usage_facts WHERE request_id=$1", original.AccountingID).Scan(&count, &input, &output); err != nil {
		t.Fatal(err)
	}
	if count != 1 || input != 4 || output != 6 {
		t.Fatalf("stream usage: count=%d input=%d output=%d; %s", count, input, output, raw)
	}
	if original.Attempts[0].UsageObserved || !original.Attempts[0].ResponseUsageDeferred {
		t.Fatal("background stream emitted duplicate billing evidence")
	}
	for range 3 {
		status, out, _ := h.gateway("GET", "/v1/responses/"+local, key, nil)
		if status != 200 {
			t.Fatalf("poll: %d %v", status, out)
		}
	}
	if err := h.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp_go.attempt_usage_facts WHERE request_id=$1", original.AccountingID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("duplicate stream billing: count=%d err=%v", count, err)
	}
}
