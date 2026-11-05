//go:build qualification

package qualification_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const (
	vendorSecret       = "vendor-secret"
	vendorModel        = "fixture-model"
	vendorAnswer       = "hello from the fixture vendor"
	vendorInputTokens  = 4
	vendorOutputTokens = 6
)

func newVendor(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	authorized := func(r *http.Request) bool {
		return r.Header.Get("Authorization") == "Bearer "+vendorSecret || r.Header.Get("X-Api-Key") == vendorSecret
	}
	refuse := func(w http.ResponseWriter) {
		http.Error(w, `{"error":{"message":"bad key","type":"invalid_request_error","code":"invalid_api_key"}}`, http.StatusUnauthorized)
	}
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r) {
			refuse(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"`+vendorModel+`","object":"model","type":"model","created_at":"2024-01-01T00:00:00Z","display_name":"Fixture"}]}`)
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r) {
			refuse(w)
			return
		}
		var request struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Model != vendorModel {
			http.Error(w, `{"error":{"message":"unknown model","type":"invalid_request_error","code":"model_not_found"}}`, http.StatusNotFound)
			return
		}
		usage := fmt.Sprintf(`{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}`, vendorInputTokens, vendorOutputTokens, vendorInputTokens+vendorOutputTokens)
		if !request.Stream {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":%s}`, vendorModel, vendorAnswer, usage)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%q},\"finish_reason\":null}]}\n\n", vendorModel, vendorAnswer)
		_, _ = fmt.Fprintf(w, "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":%s}\n\n", vendorModel, usage)
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})
	mux.HandleFunc("POST /v1/responses", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r) {
			refuse(w)
			return
		}
		var request struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Model != vendorModel {
			http.Error(w, `{"error":{"message":"unknown model","type":"invalid_request_error","code":"model_not_found"}}`, http.StatusNotFound)
			return
		}
		response := map[string]any{
			"id": "resp_fixture", "object": "response", "created_at": 1, "model": vendorModel, "status": "completed", "error": nil, "incomplete_details": nil,
			"output": []any{map[string]any{"id": "msg_fixture", "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": vendorAnswer, "annotations": []any{}}}}},
			"usage":  map[string]int{"input_tokens": vendorInputTokens, "output_tokens": vendorOutputTokens, "total_tokens": vendorInputTokens + vendorOutputTokens},
		}
		if !request.Stream {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(response)
			return
		}
		created, _ := json.Marshal(map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_fixture", "object": "response", "created_at": 1, "model": vendorModel, "status": "in_progress", "error": nil, "incomplete_details": nil, "output": []any{}, "usage": nil}})
		delta, _ := json.Marshal(map[string]any{"type": "response.output_text.delta", "item_id": "msg_fixture", "output_index": 0, "content_index": 0, "delta": vendorAnswer})
		terminal, _ := json.Marshal(map[string]any{"type": "response.completed", "response": response})
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "event: response.created\ndata: %s\n\nevent: response.output_text.delta\ndata: %s\n\nevent: response.completed\ndata: %s\n\n", created, delta, terminal)
	})
	mux.HandleFunc("POST /v1/messages", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r) {
			refuse(w)
			return
		}
		var request struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Model != vendorModel {
			http.Error(w, `{"type":"error","error":{"type":"not_found_error","message":"unknown model"}}`, http.StatusNotFound)
			return
		}
		if !request.Stream {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"id":"msg_fixture","type":"message","role":"assistant","model":%q,"content":[{"type":"text","text":%q}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":%d,"output_tokens":%d}}`, vendorModel, vendorAnswer, vendorInputTokens, vendorOutputTokens)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_fixture\",\"type\":\"message\",\"role\":\"assistant\",\"model\":%q,\"content\":[],\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":%d,\"output_tokens\":0}}}\n\n", vendorModel, vendorInputTokens)
		_, _ = io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		_, _ = fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":%q}}\n\n", vendorAnswer)
		_, _ = io.WriteString(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		_, _ = fmt.Fprintf(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":%d}}\n\n", vendorOutputTokens)
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func (s *service) activeProvider(owner *caller, name, endpoint, projectID, kind, profileID, surface string) map[string]any {
	s.t.Helper()
	configuration := map[string]any{"kind": kind, "auth_mode": "api_key", "endpoint": endpoint + "/v1"}
	if profileID != "" {
		configuration["profile_id"] = profileID
		configuration["profile_revision"] = "1"
	}
	provider := s.want(owner, "POST", "/api/v1/providers", map[string]any{
		"name":          name,
		"configuration": configuration,
		"credential":    vendorSecret,
		"model":         vendorModel,
		"project_id":    projectID,
	}, idempotent(), http.StatusCreated)
	path := "/api/v1/providers/" + provider["id"].(string)
	probe := s.want(owner, "POST", path+"/probe", nil, etag(provider), http.StatusOK)
	if probe["succeeded"] != true {
		s.t.Fatalf("probe failed: %v", probe)
	}
	models := s.want(owner, "GET", path+"/models", nil, nil, http.StatusOK)["items"].([]any)
	modelID := ""
	for _, item := range models {
		model := item.(map[string]any)
		if model["upstream_model"] == vendorModel {
			modelID = model["id"].(string)
		}
	}
	if modelID == "" {
		s.t.Fatalf("probe did not discover %s: %v", vendorModel, models)
	}
	detail := s.want(owner, "GET", path, nil, nil, http.StatusOK)
	detail = s.want(owner, "PATCH", path+"/models/"+modelID, map[string]any{
		"enabled": true,
		"capabilities": []any{
			map[string]any{"operation": "generation", "surface": surface, "mode": "unary"},
			map[string]any{"operation": "generation", "surface": surface, "mode": "streaming"},
		},
	}, etag(detail), http.StatusOK)
	certified := s.want(owner, "POST", path+"/models/"+modelID+"/certify", nil, etag(detail), http.StatusOK)
	if certified["status"] != "certified" {
		s.t.Fatalf("certification failed: %v", certified)
	}
	detail = s.want(owner, "GET", path, nil, nil, http.StatusOK)
	s.want(owner, "POST", path+"/activate", nil, idempotent(etag(detail)), http.StatusOK)
	return provider
}

func (s *service) providerSlot(owner *caller, provider map[string]any) string {
	s.t.Helper()
	slots := s.want(owner, "GET", "/api/v1/providers/"+provider["id"].(string)+"/credential-slots", nil, nil, http.StatusOK)["items"].([]any)
	for _, item := range slots {
		slot := item.(map[string]any)
		if slot["enabled"] == true && slot["credential_version_id"] != nil {
			return slot["id"].(string)
		}
	}
	s.t.Fatalf("provider has no enabled credential slot: %v", slots)
	return ""
}

func (s *service) priceModel(owner *caller, provider map[string]any, kind string) {
	s.t.Helper()
	s.want(owner, "POST", "/api/v1/pricing/revisions", map[string]any{
		"effective_at": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		"prices": []any{map[string]any{
			"provider_kind": kind, "provider_id": provider["id"], "model": vendorModel, "operation": "generation",
			"currency": "USD", "input_per_million": "1", "output_per_million": "1",
		}},
	}, idempotent(), http.StatusCreated)
}
