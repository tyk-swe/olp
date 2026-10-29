//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/gateway"
	"github.com/tyk-swe/olp/internal/resources"
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
	files           map[string]map[string]any
	batches         map[string]map[string]any
	resps           map[string]map[string]any
	mu              sync.Mutex
	lastReq         atomic.Value
	chatCalls       atomic.Int64
	responseCalls   atomic.Int64
	failNextChat    atomic.Bool
	lastBatchRaw    atomic.Value
	batchCreates    atomic.Int64
	fileCreates     atomic.Int64
	earlyFileReply  atomic.Bool
	fileEcho        atomic.Bool // reject uploads, echoing the applied credential
	batchCreateRaw  atomic.Value
	batchFetchRaw   atomic.Value
	fileCreateRaw   atomic.Value
	fileFetchRaw    atomic.Value
	contentByID     sync.Map
	lastPath        atomic.Value
	content         atomic.Value
	respID          atomic.Value
	respCreates     atomic.Int64
	respPostStream  atomic.Value
	respGetStream   atomic.Value
	respFetchRaw    atomic.Value
	respFetchStatus atomic.Int32
	respCancelRaw   atomic.Value
	lastRespQuery   atomic.Value
	holdCreated     atomic.Bool
	holdGet         atomic.Bool
	batchCreateHold chan struct{}
	batchCreateRead chan struct{}
	dials           atomic.Int64

	batchCreateStatus atomic.Int32
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
		f.chatCalls.Add(1)
		if f.failNextChat.Swap(false) {
			http.Error(w, `{"error":{"message":"temporary failure","type":"server_error"}}`, http.StatusInternalServerError)
			return
		}
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
		if f.fileEcho.Load() {
			key := r.Header.Get("Api-Key")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": "Invalid key " + key, "code": key, "type": key}})
			return
		}
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
		fileNumber := f.fileCreates.Add(1)
		fileID := fmt.Sprintf("file-up-%d", fileNumber)
		file := map[string]any{"id": fileID, "object": "file", "bytes": 128, "created_at": time.Now().Unix(), "filename": "input.jsonl", "purpose": "batch", "status": "processed"}
		f.mu.Lock()
		f.files[fileID] = file
		f.mu.Unlock()
		writeJSON(w, file)
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
		f.mu.Lock()
		items := make([]any, 0, len(f.batches))
		for _, batch := range f.batches {
			copy := make(map[string]any, len(batch))
			for key, value := range batch {
				copy[key] = value
			}
			items = append(items, copy)
		}
		f.mu.Unlock()
		writeJSON(w, map[string]any{"object": "list", "data": items, "has_more": false})
	})
	mux.HandleFunc("POST /openai/batches", func(w http.ResponseWriter, r *http.Request) {
		f.dials.Add(1)
		batchNumber := f.batchCreates.Add(1)
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
		batchID := fmt.Sprintf("batch-up-%d", batchNumber)
		// Seeded fields (such as a test's pending_usage) survive creation;
		// the canonical fields below win over the seeded template.
		f.mu.Lock()
		batch := make(map[string]any, len(f.batches[batchID])+7)
		for key, value := range f.batches[batchID] {
			batch[key] = value
		}
		f.mu.Unlock()
		batch["id"], batch["object"], batch["status"] = batchID, "batch", "validating"
		batch["endpoint"], batch["completion_window"] = body["endpoint"], body["completion_window"]
		batch["input_file_id"], batch["created_at"] = body["input_file_id"], time.Now().Unix()
		f.mu.Lock()
		f.batches[batchID] = batch
		hold, read := f.batchCreateHold, f.batchCreateRead
		f.mu.Unlock()
		if hold != nil {
			if read != nil {
				close(read)
			}
			<-hold
			return
		}
		if status := f.batchCreateStatus.Load(); status != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(int(status))
			writeJSON(w, map[string]any{"error": map[string]any{"type": "server_error", "message": "temporary failure"}})
			return
		}
		writeJSON(w, batch)
	})
	mux.HandleFunc("GET /openai/batches/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.dials.Add(1)
		f.mu.Lock()
		batch, ok := f.batches[r.PathValue("id")]
		copy := make(map[string]any, len(batch))
		for key, value := range batch {
			copy[key] = value
		}
		f.mu.Unlock()
		if !ok {
			http.Error(w, `{"error":{"message":"no such batch"}}`, http.StatusNotFound)
			return
		}
		if raw := f.batchFetchRaw.Load(); raw != nil {
			_, _ = io.WriteString(w, raw.(string))
			return
		}
		writeJSON(w, copy)
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
		f.responseCalls.Add(1)
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
		if previous, ok := body["previous_response_id"].(string); ok {
			out["previous_response_id"] = previous
		}
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
	// the deployment path above serves Automatic Azure providers.
	mux.HandleFunc("POST /openai/responses", func(w http.ResponseWriter, r *http.Request) {
		f.responseCalls.Add(1)
		f.dials.Add(1)
		f.respCreates.Add(1)
		body := decodeBody(t, r)
		f.lastReq.Store(body)
		out := map[string]any{}
		for k, v := range f.resps["resp-up-1"] {
			out[k] = v
		}
		out["id"] = f.respID.Load().(string)
		if previous, ok := body["previous_response_id"].(string); ok {
			out["previous_response_id"] = previous
		}
		if stream, _ := body["stream"].(bool); stream {
			w.Header().Set("Content-Type", "text/event-stream")
			if custom := f.respPostStream.Load(); custom != nil {
				wire := custom.(string)
				if f.holdCreated.Load() {
					end := strings.Index(wire, "\n\n")
					if end < 0 {
						t.Error("fixture stream has no first event")
						return
					}
					_, _ = io.WriteString(w, wire[:end+2])
					_ = http.NewResponseController(w).Flush()
					<-r.Context().Done()
					return
				}
				_, _ = io.WriteString(w, wire)
				return
			}
			pending := maps.Clone(out)
			pending["status"] = "in_progress"
			pending["output"] = []any{}
			delete(pending, "usage")
			created, _ := json.Marshal(map[string]any{"type": "response.created", "sequence_number": 0, "response": pending})
			completed, _ := json.Marshal(map[string]any{"type": "response.completed", "sequence_number": 1, "response": out})
			fmt.Fprintf(w, "event: response.created\ndata: %s\n\nevent: response.completed\ndata: %s\n\n", created, completed)
			return
		}
		writeJSON(w, out)
	})
	mux.HandleFunc("GET /openai/responses/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.dials.Add(1)
		f.lastRespQuery.Store(r.URL.RawQuery)
		res, ok := f.resps[r.PathValue("id")]
		if !ok {
			http.Error(w, `{"error":{"message":"no such response"}}`, http.StatusNotFound)
			return
		}
		if r.URL.Query().Get("stream") == "true" {
			w.Header().Set("Content-Type", "text/event-stream")
			if custom := f.respGetStream.Load(); custom != nil {
				wire := custom.(string)
				if f.holdGet.Load() {
					// Serve the first event, then hold the stream open
					// until the caller goes away.
					end := strings.Index(wire, "\n\n")
					_, _ = io.WriteString(w, wire[:end+2])
					_ = http.NewResponseController(w).Flush()
					<-r.Context().Done()
					return
				}
				_, _ = io.WriteString(w, wire)
				return
			}
			pending := maps.Clone(res)
			pending["status"] = "in_progress"
			pending["output"] = []any{}
			delete(pending, "usage")
			created, _ := json.Marshal(map[string]any{"type": "response.created", "sequence_number": 0, "response": pending})
			completed, _ := json.Marshal(map[string]any{"type": "response.completed", "sequence_number": 1, "response": res})
			fmt.Fprintf(w, "event: response.created\ndata: %s\n\nevent: response.completed\ndata: %s\n\n", created, completed)
			return
		}
		if raw := f.respFetchRaw.Load(); raw != nil {
			w.Header().Set("Content-Type", "application/json")
			if status := f.respFetchStatus.Load(); status != 0 {
				w.WriteHeader(int(status))
			}
			_, _ = io.WriteString(w, raw.(string))
			return
		}
		writeJSON(w, res)
	})
	mux.HandleFunc("DELETE /openai/responses/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.dials.Add(1)
		if _, ok := f.resps[r.PathValue("id")]; !ok {
			http.Error(w, `{"error":{"message":"no such response"}}`, http.StatusNotFound)
			return
		}
		writeJSON(w, map[string]any{"id": r.PathValue("id"), "object": "response.deleted", "deleted": true})
	})
	mux.HandleFunc("POST /openai/responses/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		f.dials.Add(1)
		res, ok := f.resps[r.PathValue("id")]
		if !ok {
			http.Error(w, `{"error":{"message":"no such response"}}`, http.StatusNotFound)
			return
		}
		if raw := f.respCancelRaw.Load(); raw != nil {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, raw.(string))
			return
		}
		out := map[string]any{}
		for name, value := range res {
			out[name] = value
		}
		out["status"] = "cancelled"
		writeJSON(w, out)
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

func nativeBatchInput(customID string) string {
	return fmt.Sprintf(`{"custom_id":%q,"method":"POST","url":"/v1/chat/completions","body":{"model":%q,"messages":[{"role":"user","content":"batch input"}]}}`+"\n", customID, vendorModel)
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
	return provisionOpenAIInProject(t, h, h.owner(), nil, endpoint, capabilities, operations, draftFields, providerFields...)
}
func provisionOpenAIInProject(t *testing.T, h *accessHarness, owner *browser, project *string, endpoint string, capabilities []any, operations []string, draftFields map[string]any, providerFields ...map[string]any) (*browser, map[string]any, string, string) {
	t.Helper()
	detail := activeAzureProviderInProject(t, h, owner, "Provider state fixture "+uuid.NewString()[:8], endpoint, project, capabilities, providerFields...)
	slug := "state-" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	// Without a provider profile the route must be transformed; callers that
	// configure a profile may declare a strict route instead.
	draftBody := map[string]any{"project_id": project, "slug": slug, "operations": operations, "overall_timeout_ms": 10000, "max_attempts": 1, "fidelity": map[string]any{"mode": "transformed"}, "targets": []any{map[string]any{"provider_id": detail["id"], "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 5000}}}
	for key, value := range draftFields {
		draftBody[key] = value
	}
	// A caller may shorten the overall timeout below the default target
	// timeout; the target must fit inside it.
	if overall, ok := draftBody["overall_timeout_ms"].(int); ok && overall < 5000 {
		if targets, ok := draftBody["targets"].([]any); ok && len(targets) == 1 {
			if target, ok := targets[0].(map[string]any); ok {
				target["timeout_ms"] = overall
			}
		}
	}
	draft := h.want(owner, "POST", "/api/v1/route-drafts", draftBody, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	h.want(owner, "POST", "/api/v1/route-drafts/"+draft["id"].(string)+"/activate", nil, withMatch(draft, map[string]string{"Idempotency-Key": uuid.NewString()}), 200)
	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"project_id": project, "name": "stateful inference", "scopes": []string{"inference"}, "allowed_routes": []string{slug}}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	h.refresh()
	return owner, detail, slug, key["secret"].(string)
}

func activeAzureProviderInProject(t *testing.T, h *accessHarness, owner *browser, name, endpoint string, projectID *string, capabilities []any, providerFields ...map[string]any) map[string]any {
	t.Helper()
	configuration := map[string]any{"kind": "azure_openai", "auth_mode": "api_key", "endpoint": endpoint, "deployment": vendorModel, "api_version": "2024-10-21"}
	if len(providerFields) > 0 {
		for field, value := range providerFields[0] {
			configuration[field] = value
		}
	}
	create := map[string]any{"name": name, "project_id": projectID, "configuration": configuration, "model": vendorModel, "credential": vendorSecret}
	detail := h.want(owner, "POST", "/api/v1/providers", create, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	path := "/api/v1/providers/" + detail["id"].(string)
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
	return detail
}

func stateKey(t *testing.T, h *accessHarness, owner *browser, slug string, allowState bool) string {
	t.Helper()
	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "state key", "scopes": []string{"inference"}, "allowed_routes": []string{slug}, "allow_provider_state": allowState}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
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
	other := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "other key", "scopes": []string{"inference"}, "allowed_routes": []string{slug}}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	otherSecret := other["secret"].(string)
	h.refresh()

	fixture.fileEcho.Store(true)
	status, rejected := h.uploadTestFile(slug, secret, nativeBatchInput("upload-content-marker-7"))
	fixture.fileEcho.Store(false)
	if encoded, _ := json.Marshal(rejected); status != 400 || bytes.Contains(encoded, []byte(vendorSecret)) {
		t.Fatalf("a provider echo of the applied credential reached the client: %d %s", status, encoded)
	}

	status, uploaded := h.uploadTestFile(slug, secret, nativeBatchInput("upload-content-marker-7"))
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
	if list["has_more"] != false || list["first_id"] != fileID || list["last_id"] != fileID {
		t.Fatalf("file list cursor: %v", list)
	}
	if fixture.dials.Load() != before {
		t.Fatal("a metadata-only list must not contact the provider")
	}
	var originalFileMetadata []byte
	if err := h.Pool.QueryRow(t.Context(), `SELECT metadata FROM olp.provider_resources WHERE kind='file' AND route_slug=$1`, slug).Scan(&originalFileMetadata); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Pool.Exec(t.Context(), `UPDATE olp.provider_resources SET metadata='[]'::jsonb WHERE kind='file' AND route_slug=$1`, slug); err != nil {
		t.Fatal(err)
	}
	status, raw, _ := h.gatewayRaw("GET", "/v1/files", secret, nil, nil)
	if status != http.StatusServiceUnavailable || !bytes.Contains(raw, []byte(`"code":"provider_resource_unavailable"`)) || fixture.dials.Load() != before {
		t.Fatalf("corrupt file was silently omitted from local list: status=%d body=%s", status, raw)
	}
	if _, err := h.Pool.Exec(t.Context(), `UPDATE olp.provider_resources SET metadata=$2::jsonb WHERE kind='file' AND route_slug=$1`, slug, string(originalFileMetadata)); err != nil {
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
	if overflowEnvelope.Outcome != "failure" || overflowEnvelope.Committed || overflow.Class != "protocol" || overflow.UsageObserved || !overflow.UsageComplete || overflow.BillingUncertain {
		t.Fatalf("non-generating download failure changed billing evidence: %+v %+v", overflowEnvelope, overflow)
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
	if batch["input_file_id"] != fileID {
		t.Fatalf("batch input file leaked upstream identifier: %v", batch)
	}
	// A row created before local_input_file_id existed must still never reveal
	// its provider's input identifier, both on refresh and on inventory reads.
	if _, err := h.Pool.Exec(t.Context(), `UPDATE olp.provider_resources SET metadata=(metadata-'local_input_file_id') || '{"input_file_id":"file-up-1"}'::jsonb WHERE upstream_id='batch-up-1'`); err != nil {
		t.Fatal(err)
	}
	fixture.mu.Lock()
	fixture.batches["batch-up-1"]["input_file_id"] = "file-up-1"
	fixture.mu.Unlock()
	status, legacy, _ := h.gateway("GET", "/v1/batches/"+batchID, secret, nil)
	if status != 200 || legacy["input_file_id"] != fileID {
		t.Fatalf("legacy batch leaked provider input ID: %d %v", status, legacy)
	}
	status, inventory, _ := h.gateway("GET", "/v1/batches", secret, nil)
	encodedInventory, _ := json.Marshal(inventory)
	if status != 200 || bytes.Contains(encodedInventory, []byte("file-up-1")) {
		t.Fatalf("legacy batch inventory leaked provider input ID: %d %s", status, encodedInventory)
	}
	if _, err := h.Pool.Exec(t.Context(), `UPDATE olp.provider_resources SET metadata=metadata || jsonb_build_object('local_input_file_id',$1::text) WHERE upstream_id='batch-up-1'`, fileID); err != nil {
		t.Fatal(err)
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
	fixture.batches["batch-up-1"]["input_file_id"] = "file-up-1"
	fixture.mu.Unlock()

	if _, err := h.Pool.Exec(t.Context(), `INSERT INTO olp.provider_resources
		(id,kind,api_key_id,route_slug,provider_id,provider_revision_id,route_revision_id,target_id,slot_id,credential_id,upstream_id,state,metadata)
		SELECT gen_random_uuid(),'file',$1,route_slug,provider_id,provider_revision_id,route_revision_id,target_id,slot_id,credential_id,'file-up-out','processed',jsonb_build_object('upstream_model',metadata->>'upstream_model')
		FROM olp.provider_resources WHERE kind='batch' AND upstream_id='batch-up-1'`, other["id"]); err != nil {
		t.Fatal(err)
	}
	var foreignFileID, batchTargetID string
	if err := h.Pool.QueryRow(t.Context(), `SELECT id::text FROM olp.provider_resources WHERE kind='file' AND api_key_id=$1 AND upstream_id='file-up-out'`, other["id"]).Scan(&foreignFileID); err != nil {
		t.Fatal(err)
	}
	foreignFileLocal := resources.LocalID(resources.KindFile, uuid.MustParse(foreignFileID))
	if err := h.Pool.QueryRow(t.Context(), `SELECT target_id::text FROM olp.provider_resources WHERE kind='batch' AND upstream_id='batch-up-1'`).Scan(&batchTargetID); err != nil {
		t.Fatal(err)
	}
	status, raw, _ = h.gatewayRaw("GET", "/v1/batches/"+batchID, secret, nil, nil)
	if status != 200 || bytes.Contains(raw, []byte("file-up-out")) || bytes.Contains(raw, []byte(foreignFileLocal)) {
		t.Fatalf("other key's native file identity was reused or exposed: %d %s", status, raw)
	}
	beforeList := fixture.dials.Load()
	status, raw, _ = h.gatewayRaw("GET", "/v1/batches", secret, nil, nil)
	if status != 200 || bytes.Contains(raw, []byte("file-up-out")) || bytes.Contains(raw, []byte(foreignFileLocal)) || fixture.dials.Load() != beforeList {
		t.Fatalf("owned batch inventory changed its source or dispatched: status=%d body=%s", status, raw)
	}
	status, _, _ = h.gatewayRaw("GET", "/v1/files/"+foreignFileLocal, secret, nil, nil)
	if status != 404 || fixture.dials.Load() != beforeList {
		t.Fatalf("other key's local file identity was usable: %d", status)
	}
	// Inventory must surface an unprovable source instead of silently omitting it.
	if _, err := h.Pool.Exec(t.Context(), `UPDATE olp.provider_resources SET target_id=NULL WHERE kind='batch' AND upstream_id='batch-up-1'`); err != nil {
		t.Fatal(err)
	}
	status, raw, _ = h.gatewayRaw("GET", "/v1/batches", secret, nil, nil)
	if status != http.StatusServiceUnavailable || !bytes.Contains(raw, []byte(`"code":"provider_resource_unavailable"`)) || fixture.dials.Load() != beforeList {
		t.Fatalf("unprojectable batch was silently omitted from local list: status=%d body=%s", status, raw)
	}
	if _, err := h.Pool.Exec(t.Context(), `UPDATE olp.provider_resources SET target_id=$1 WHERE kind='batch' AND upstream_id='batch-up-1'`, batchTargetID); err != nil {
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
	if batch["input_file_id"] != fileID {
		t.Fatalf("input_file_id leaked upstream identifier: %v", batch)
	}
	// A provider whose own file IDs use the gateway's file_ prefix is still
	// mapped, so its results stay reachable through the gateway.
	fixture.mu.Lock()
	fixture.batches["batch-up-1"]["output_file_id"] = "file_up_out"
	fixture.files["file_up_out"] = map[string]any{"id": "file_up_out", "object": "file", "bytes": 16, "created_at": 1, "filename": "output.jsonl", "purpose": "batch_output", "status": "processed"}
	fixture.mu.Unlock()
	status, batch, _ = h.gateway("GET", "/v1/batches/"+batchID, secret, nil)
	prefixed, _ := batch["output_file_id"].(string)
	if status != 200 || prefixed == "file_up_out" || !strings.HasPrefix(prefixed, "file_") {
		t.Fatalf("file_-prefixed upstream identifier was not mapped: %d %v", status, batch)
	}
	if status, fetched, _ := h.gateway("GET", "/v1/files/"+prefixed, secret, nil); status != 200 {
		t.Fatalf("mapped file_-prefixed output file: %d %v", status, fetched)
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
	if err := h.Pool.QueryRow(t.Context(), "SELECT coalesce(string_agg(metadata::text,''),'') FROM olp.provider_resources").Scan(&metadata); err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"upload-content-marker-7", "batch-download-marker-9"} {
		if strings.Contains(metadata, marker) {
			t.Fatalf("provider metadata retained content marker %q", marker)
		}
	}

	detail = h.want(owner, "GET", "/api/v1/providers/"+detail["id"].(string), nil, nil, 200)
	h.want(owner, "PATCH", "/api/v1/providers/"+detail["id"].(string), map[string]any{"name": "Provider state fixture v2", "configuration": map[string]any{"kind": "azure_openai", "auth_mode": "api_key", "endpoint": fixture.URL, "deployment": vendorModel, "api_version": "2024-10-21"}}, etagHeader(detail), 200)
	detail = h.want(owner, "GET", "/api/v1/providers/"+detail["id"].(string), nil, nil, 200)
	h.want(owner, "POST", "/api/v1/providers/"+detail["id"].(string)+"/activate", nil, withMatch(detail, map[string]string{"Idempotency-Key": uuid.NewString()}), 200)
	draft := h.want(owner, "POST", "/api/v1/route-drafts", map[string]any{"slug": slug, "operations": []string{"batch"}, "overall_timeout_ms": 10000, "max_attempts": 1, "fidelity": map[string]any{"mode": "transformed"}, "targets": []any{map[string]any{"provider_id": detail["id"], "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 5000}}}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	h.want(owner, "POST", "/api/v1/route-drafts/"+draft["id"].(string)+"/activate", nil, withMatch(draft, map[string]string{"Idempotency-Key": uuid.NewString()}), 200)
	h.refresh()
	status, fetched, _ = h.gateway("GET", "/v1/files/"+fileID, secret, nil)
	if status != 200 || fetched["id"] != fileID {
		t.Fatalf("get file after revision churn: %d %v", status, fetched)
	}
	routes := h.want(owner, "GET", "/api/v1/routes", nil, nil, 200)
	var routeID string
	for _, item := range routes["items"].([]any) {
		if item.(map[string]any)["slug"] == slug {
			routeID = item.(map[string]any)["id"].(string)
		}
	}
	route := h.want(owner, "GET", "/api/v1/routes/"+routeID, nil, nil, 200)
	h.want(owner, "POST", "/api/v1/routes/"+routeID+"/retire", nil, withMatch(route, map[string]string{"Idempotency-Key": uuid.NewString()}), 200)
	h.refresh()
	status, fetched, _ = h.gateway("GET", "/v1/files/"+fileID, secret, nil)
	if status != 200 || fetched["id"] != fileID {
		t.Fatalf("get file after retire: %d %v", status, fetched)
	}
	status, list, _ = h.gateway("GET", "/v1/files", secret, nil)
	if status != 200 {
		t.Fatalf("list files before delete: %d %v", status, list)
	}
	listedBefore := map[string]bool{}
	for _, item := range list["data"].([]any) {
		listedBefore[item.(map[string]any)["id"].(string)] = true
	}
	status, deleted, _ := h.gateway("DELETE", "/v1/files/"+fileID, secret, nil)
	if status != 200 || deleted["deleted"] != true {
		t.Fatalf("delete file after retire: %d %v", status, deleted)
	}
	// A batch keeps naming the file it was created from after the client
	// deletes that file, and reading it never resurrects the upstream file.
	status, batch, _ = h.gateway("GET", "/v1/batches/"+batchID, secret, nil)
	if status != 200 || batch["input_file_id"] != fileID {
		t.Fatalf("batch after input file delete: %d %v", status, batch)
	}
	status, list, _ = h.gateway("GET", "/v1/files", secret, nil)
	if status != 200 {
		t.Fatalf("list files after delete: %d %v", status, list)
	}
	for _, item := range list["data"].([]any) {
		if id := item.(map[string]any)["id"].(string); !listedBefore[id] {
			t.Fatalf("batch read resurrected a deleted input file as %s: %v", id, list)
		}
	}
	var inputMappings int
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.provider_resources WHERE kind='file' AND upstream_id='file-up-1'`).Scan(&inputMappings); err != nil {
		t.Fatal(err)
	}
	if inputMappings != 1 {
		t.Fatalf("batch read created %d mappings for the deleted input file, want 1", inputMappings)
	}
}

func TestResponseLifecycle(t *testing.T) {
	fixture := newOpenAIFixture(t, "")
	fixture.resps["resp-up-1"]["output"] = []any{map[string]any{"id": "msg-up-1", "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "response-output-marker-3", "annotations": []any{}}}}}
	h := newAccessHarness(t)
	owner, detail, slug, _ := provisionOpenAI(t, h, fixture.URL,
		[]any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}, map[string]any{"operation": "generation", "surface": "openai", "mode": "streaming"}},
		[]string{"generation"})
	plain := stateKey(t, h, owner, slug, false)
	opted := stateKey(t, h, owner, slug, true)
	otherKey := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "other state key", "scopes": []string{"inference"}, "allowed_routes": []string{slug}, "allow_provider_state": true}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
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
		if _, err := h.Pool.Exec(t.Context(), `INSERT INTO olp.provider_resources
			(id,kind,api_key_id,route_slug,provider_id,provider_revision_id,route_revision_id,target_id,slot_id,credential_id,upstream_id,state,metadata)
			SELECT gen_random_uuid(),'response',$1,route_slug,provider_id,provider_revision_id,route_revision_id,target_id,slot_id,credential_id,$2,'completed',jsonb_build_object('upstream_model',metadata->>'upstream_model')
			FROM olp.provider_resources WHERE kind='response' AND upstream_id='resp-up-1'`, otherKeyID, upstreamID); err != nil {
			t.Fatal(err)
		}
	}
	foreignAlias := func(upstreamID string) string {
		t.Helper()
		var id string
		if err := h.Pool.QueryRow(t.Context(), `SELECT id::text FROM olp.provider_resources WHERE kind='response' AND api_key_id=$1 AND upstream_id=$2`, otherKeyID, upstreamID).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return resources.LocalID(resources.KindResponse, uuid.MustParse(id))
	}
	fixture.respID.Store("resp-up-blocked")
	block("resp-up-blocked")
	status, raw, _ := h.gatewayRaw("POST", "/v1/responses", opted, strings.NewReader(`{"model":"`+slug+`","input":"hi","store":true}`), map[string]string{"Content-Type": "application/json"})
	if status != 200 || bytes.Contains(raw, []byte("resp-up-blocked")) || bytes.Contains(raw, []byte(foreignAlias("resp-up-blocked"))) {
		t.Fatalf("unary mapping reused another key's identity: %d %s", status, raw)
	}

	fixture.respID.Store("resp-up-stream")
	block("resp-up-stream")
	status, raw, header := h.gatewayRaw("POST", "/v1/responses", opted, strings.NewReader(`{"model":"`+slug+`","input":"hi","store":true,"stream":true}`), map[string]string{"Content-Type": "application/json"})
	if status != 200 || bytes.Contains(raw, []byte("resp-up-stream")) || bytes.Contains(raw, []byte(foreignAlias("resp-up-stream"))) || !strings.Contains(header.Get("Content-Type"), "event-stream") {
		t.Fatalf("streamed mapping reused another key's identity: %d %s %v", status, raw, header.Get("Content-Type"))
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
	if next["previous_response_id"] != local {
		t.Fatalf("chained response leaked the upstream previous_response_id: %v", next)
	}

	status, cancelled, _ := h.gateway("POST", "/v1/responses/"+local+"/cancel", opted, nil)
	if status != 200 || cancelled["status"] != "cancelled" {
		t.Fatalf("cancel response: %d %v", status, cancelled)
	}

	var metadata string
	if err := h.Pool.QueryRow(t.Context(), "SELECT coalesce(string_agg(metadata::text,''),'') FROM olp.provider_resources").Scan(&metadata); err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"prompt-marker-1", "response-output-marker-3"} {
		if strings.Contains(metadata, marker) {
			t.Fatalf("provider metadata retained content marker %q", marker)
		}
	}

	detail = h.want(owner, "GET", "/api/v1/providers/"+detail["id"].(string), nil, nil, 200)
	h.want(owner, "PATCH", "/api/v1/providers/"+detail["id"].(string), map[string]any{"name": "Responses fixture v2", "configuration": map[string]any{"kind": "azure_openai", "auth_mode": "api_key", "endpoint": fixture.URL, "deployment": vendorModel, "api_version": "2024-10-21"}}, etagHeader(detail), 200)
	detail = h.want(owner, "GET", "/api/v1/providers/"+detail["id"].(string), nil, nil, 200)
	h.want(owner, "POST", "/api/v1/providers/"+detail["id"].(string)+"/activate", nil, withMatch(detail, map[string]string{"Idempotency-Key": uuid.NewString()}), 200)
	h.refresh()
	status, fetched, _ = h.gateway("GET", "/v1/responses/"+local, opted, nil)
	if status != 200 || fetched["id"] != local {
		t.Fatalf("get response after new revision: %d %v", status, fetched)
	}

	slots := h.want(owner, "GET", "/api/v1/providers/"+detail["id"].(string)+"/credential-slots", nil, nil, 200)
	credentialID := slots["items"].([]any)[0].(map[string]any)["credential_version_id"].(string)
	detail = h.want(owner, "GET", "/api/v1/providers/"+detail["id"].(string), nil, nil, 200)
	h.want(owner, "POST", "/api/v1/providers/"+detail["id"].(string)+"/credentials/"+credentialID+"/revoke", nil, withMatch(detail, map[string]string{"Idempotency-Key": uuid.NewString()}), 200)
	h.refresh()
	status, fetched, _ = h.gateway("GET", "/v1/responses/"+local, opted, nil)
	if status != 409 || h.gatewayCode(status, fetched) != "provider_resource_credential_unavailable" {
		t.Fatalf("revoked credential: %d %v", status, fetched)
	}
}

type realtimeFixture struct {
	*httptest.Server
	dials atomic.Int64
	echo  atomic.Bool // reject the handshake, echoing the applied credential
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
		if f.echo.Load() {
			key := r.Header.Get("Api-Key")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": "Invalid key " + key, "code": key, "type": key}})
			return
		}
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

	// A rejected upstream handshake that echoes the applied credential must
	// not carry it into the recorded outcome, request history, or traces.
	fixture.echo.Store(true)
	echoed, _, err := websocket.Dial(t.Context(), wsURL, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + secret}}})
	if err == nil {
		_, _, err = echoed.Read(t.Context())
	}
	fixture.echo.Store(false)
	if err == nil {
		t.Fatal("a rejected upstream handshake opened a realtime session")
	}
	admissionReleased("rejected handshake")
	recorded, _ := json.Marshal(sink.last())
	if sink.last().Outcome != "failure" || bytes.Contains(recorded, []byte(vendorSecret)) {
		t.Fatalf("a provider echo of the applied credential was recorded: %s", recorded)
	}

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
	keys := h.want(owner, "GET", "/api/v1/api-keys", nil, nil, 200)
	var keyID string
	for _, item := range keys["items"].([]any) {
		keyID = item.(map[string]any)["id"].(string)
	}
	record := h.want(owner, "GET", "/api/v1/api-keys/"+keyID, nil, nil, 200)
	h.want(owner, "POST", "/api/v1/api-keys/"+keyID+"/revoke", nil, withMatch(record, map[string]string{"Idempotency-Key": uuid.NewString()}), 200)
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
