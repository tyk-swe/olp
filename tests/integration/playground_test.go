//go:build integration

package integration_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func sseEvents(raw string) [][2]string {
	events := [][2]string{}
	for _, block := range strings.Split(raw, "\n\n") {
		event, data := "", ""
		for _, line := range strings.Split(block, "\n") {
			if rest, ok := strings.CutPrefix(line, "event: "); ok {
				event = rest
			}
			if rest, ok := strings.CutPrefix(line, "data: "); ok {
				data = rest
			}
		}
		if event != "" {
			events = append(events, [2]string{event, data})
		}
	}
	return events
}

func provisionPlaygroundRoute(t *testing.T, h *accessHarness, b *browser, cfg map[string]any, capabilities []any, operations []string, projectID any) string {
	t.Helper()
	create := map[string]any{"name": "PG " + cfg["kind"].(string) + " " + uuid.NewString()[:8], "configuration": cfg, "model": vendorModel, "credential": vendorSecret}
	if projectID != nil {
		create["project_id"] = projectID
	}
	detail := h.want(b, "POST", "/api/v1/providers", create, idem("pg-provider-"+uuid.NewString()[:8]), 201)
	path := "/api/v1/providers/" + detail["id"].(string)
	probe := h.want(b, "POST", path+"/probe", nil, etagHeader(detail), 200)
	if probe["succeeded"] != true {
		t.Fatalf("provider probe: %v", probe)
	}
	models := h.want(b, "GET", path+"/models", nil, nil, 200)
	var modelID string
	for _, item := range models["items"].([]any) {
		record := item.(map[string]any)
		if record["upstream_model"] == vendorModel {
			modelID = record["id"].(string)
		}
	}
	if modelID == "" {
		t.Fatalf("provider model %s not discovered: %v", vendorModel, models)
	}
	detail = h.want(b, "PATCH", path+"/models/"+modelID, map[string]any{"enabled": true, "capabilities": capabilities}, etagHeader(detail), 200)
	certified := h.want(b, "POST", path+"/models/"+modelID+"/certify", nil, etagHeader(detail), 200)
	if certified["status"] != "certified" {
		t.Fatalf("capability proof: %v", certified)
	}
	detail = h.want(b, "GET", path, nil, nil, 200)
	h.want(b, "POST", path+"/activate", nil, withMatch(detail, idem("pg-activate-"+detail["id"].(string))), 200)
	slug := "pg-" + uuid.NewString()[:8]
	draft := map[string]any{"slug": slug, "operations": operations, "overall_timeout_ms": 10000, "max_attempts": 1, "fidelity": map[string]any{"mode": "transformed"}, "targets": []any{map[string]any{"provider_id": detail["id"], "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 5000}}}
	if projectID != nil {
		draft["project_id"] = projectID
	}
	created := h.want(b, "POST", "/api/v1/route-drafts", draft, idem("pg-draft-"+slug), 201)
	h.want(b, "POST", "/api/v1/route-drafts/"+created["id"].(string)+"/activate", nil, withMatch(created, idem("pg-draft-activate-"+slug)), 200)
	return slug
}

func TestPlayground(t *testing.T) {
	fixture := newVendor(t)
	h := newAccessHarness(t)
	sink := &captureSink{}
	h.Gateway.Sink = sink
	owner := h.owner()

	capabilities := []any{
		map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"},
		map[string]any{"operation": "generation", "surface": "openai", "mode": "streaming"},
		map[string]any{"operation": "generation", "surface": "anthropic", "mode": "unary"},
		map[string]any{"operation": "generation", "surface": "gemini", "mode": "unary"},
		map[string]any{"operation": "embeddings", "surface": "openai", "mode": "unary"},
		map[string]any{"operation": "moderation", "surface": "openai", "mode": "unary"},
		map[string]any{"operation": "token_count", "surface": "openai", "mode": "unary"},
	}
	slug := provisionPlaygroundRoute(t, h, owner,
		map[string]any{"kind": "openai", "auth_mode": "api_key", "endpoint": fixture.URL + "/v1"},
		capabilities, []string{"generation", "embeddings", "moderation", "token_count"}, nil)
	rerankSlug := provisionPlaygroundRoute(t, h, owner,
		map[string]any{"kind": "openai_compatible", "auth_mode": "api_key", "endpoint": fixture.URL + "/v1", "options": map[string]any{"vendor_id": "voyage"}},
		[]any{map[string]any{"operation": "rerank", "surface": "openai", "mode": "unary"}},
		[]string{"rerank"}, nil)
	genOnlySlug := provisionPlaygroundRoute(t, h, owner,
		map[string]any{"kind": "openai_compatible", "auth_mode": "api_key", "endpoint": fixture.URL + "/v1"},
		[]any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}},
		[]string{"generation"}, nil)
	h.refresh()

	reply := h.want(owner, "POST", "/api/v1/playground",
		map[string]any{"model": slug, "input": "hi"}, nil, 200)
	if reply["output_text"] != vendorAnswer || reply["model"] != slug {
		t.Fatalf("basic playground: %v", reply)
	}
	if reply["usage"] == nil || reply["routing"] == nil {
		t.Fatalf("usage and routing must accompany the reply: %v", reply)
	}

	reply = h.want(owner, "POST", "/api/v1/playground", map[string]any{
		"model": slug,
		"request": map[string]any{
			"model": "injected-provider-model",
			"messages": []any{
				map[string]any{"role": "system", "content": "be brief"},
				map[string]any{"role": "user", "content": "first turn"},
				map[string]any{"role": "assistant", "content": "earlier reply"},
				map[string]any{"role": "user", "content": "second turn"},
			},
		},
	}, nil, 200)
	if reply["output_text"] != vendorAnswer {
		t.Fatalf("raw multi-turn: %v", reply)
	}

	reply = h.want(owner, "POST", "/api/v1/playground", map[string]any{
		"model": slug,
		"request": map[string]any{
			"model": "injected",
			"messages": []any{
				map[string]any{"role": "user", "content": "weather?"},
				map[string]any{"role": "assistant", "tool_calls": []any{
					map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "get_weather", "arguments": `{"city":"Berlin"}`}},
				}},
				map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "sunny"},
			},
			"tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "get_weather", "parameters": map[string]any{"type": "object"}}}},
		},
	}, nil, 200)
	if reply["output_text"] != vendorAnswer {
		t.Fatalf("tool turns: %v", reply)
	}
	reply = h.want(owner, "POST", "/api/v1/playground", map[string]any{
		"model": slug,
		"request": map[string]any{
			"messages": []any{map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "what is this"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,iVBORw0KGgo="}},
			}}},
		},
	}, nil, 200)
	if reply["output_text"] != vendorAnswer {
		t.Fatalf("multimodal: %v", reply)
	}

	reply = h.want(owner, "POST", "/api/v1/playground", map[string]any{
		"model": slug, "surface": "anthropic",
		"request": map[string]any{
			"model": "claude-injected", "max_tokens": 64,
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		},
	}, nil, 200)
	if reply["output_text"] != vendorAnswer {
		t.Fatalf("anthropic surface: %v", reply)
	}
	reply = h.want(owner, "POST", "/api/v1/playground", map[string]any{
		"model": slug, "surface": "gemini",
		"request": map[string]any{
			"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hi"}}}},
		},
	}, nil, 200)
	if reply["output_text"] != vendorAnswer {
		t.Fatalf("gemini surface: %v", reply)
	}

	reply = h.want(owner, "POST", "/api/v1/playground", map[string]any{
		"model": slug, "operation": "embeddings",
		"request": map[string]any{"model": "injected", "input": []any{"a", "b"}},
	}, nil, 200)
	if doc, ok := reply["response"].(map[string]any); !ok || doc["data"] == nil {
		t.Fatalf("embeddings response: %v", reply)
	}
	if reply["usage"] == nil {
		t.Fatalf("embeddings usage: %v", reply)
	}
	reply = h.want(owner, "POST", "/api/v1/playground", map[string]any{
		"model": slug, "operation": "moderation",
		"request": map[string]any{"input": "check this text"},
	}, nil, 200)
	if doc, ok := reply["response"].(map[string]any); !ok || doc["results"] == nil {
		t.Fatalf("moderation response: %v", reply)
	}
	reply = h.want(owner, "POST", "/api/v1/playground", map[string]any{
		"model": rerankSlug, "operation": "rerank",
		"request": map[string]any{"query": "q", "documents": []any{"a", "b"}, "top_n": 2},
	}, nil, 200)
	if doc, ok := reply["response"].(map[string]any); !ok || doc["results"] == nil {
		t.Fatalf("rerank response: %v", reply)
	}
	reply = h.want(owner, "POST", "/api/v1/playground", map[string]any{
		"model": slug, "operation": "token_count",
		"request": map[string]any{"model": "injected", "input": "hi"},
	}, nil, 200)
	if doc, ok := reply["response"].(map[string]any); !ok || doc["input_tokens"] != float64(42) {
		t.Fatalf("token_count response: %v", reply)
	}

	for name, extra := range map[string]any{
		"input":             "conflict",
		"temperature":       0.5,
		"max_output_tokens": 8,
		"tools":             []any{map[string]any{"name": "t", "input_schema": map[string]any{"type": "object"}}},
		"response_format":   map[string]any{"type": "text"},
	} {
		body := map[string]any{"model": slug, "request": map[string]any{"messages": []any{map[string]any{"role": "user", "content": "x"}}}}
		body[name] = extra
		if status, out, _ := h.request(owner, "POST", "/api/v1/playground", body, nil); status != 422 {
			t.Fatalf("raw+%s conflict: %d %v", name, status, out)
		}
	}

	if status, out, _ := h.request(owner, "POST", "/api/v1/playground", map[string]any{
		"model": genOnlySlug, "operation": "embeddings",
		"request": map[string]any{"input": "a"},
	}, nil); status == 200 {
		t.Fatalf("operation must be gated by the route: %d %v", status, out)
	}
	if status, out, _ := h.request(owner, "POST", "/api/v1/playground", map[string]any{
		"model": slug, "operation": "rerank",
		"request": map[string]any{"query": "q", "documents": []any{"a"}},
	}, nil); status == 200 {
		t.Fatalf("rerank must be gated by the route: %d %v", status, out)
	}

	project := createProject(h, owner, "Playground scope")
	scopedSlug := provisionPlaygroundRoute(t, h, owner,
		map[string]any{"kind": "openai_compatible", "auth_mode": "api_key", "endpoint": fixture.URL + "/v1"},
		[]any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}},
		[]string{"generation"}, project)
	h.refresh()
	outsider := h.invite(owner, "pg-outsider@example.com", "developer")
	users := h.want(owner, "GET", "/api/v1/users", nil, nil, 200)
	var outsiderID, outsiderEtag string
	for _, item := range users["items"].([]any) {
		record := item.(map[string]any)
		if record["email"] == "pg-outsider@example.com" {
			outsiderID = record["id"].(string)
			outsiderEtag = record["etag"].(string)
		}
	}
	h.want(owner, "PATCH", "/api/v1/users/"+outsiderID, map[string]any{"access_scope": "assigned"}, etagHeader(map[string]any{"etag": outsiderEtag}), 200)
	outsider = login(h, "pg-outsider@example.com")
	if status, out, _ := h.request(outsider, "POST", "/api/v1/playground", map[string]any{
		"model": scopedSlug, "input": "hi",
	}, nil); status != 403 && status != 404 {
		t.Fatalf("out-of-scope playground: %d %v", status, out)
	}
	h.want(owner, "POST", "/api/v1/playground", map[string]any{"model": scopedSlug, "input": "hi"}, nil, 200)

	if status, out, _ := h.request(owner, "POST", "/api/v1/playground/stream", map[string]any{
		"model": slug, "input": "hi",
	}, nil); status != 422 {
		t.Fatalf("stream without stream:true: %d %v", status, out)
	}
	if status, out, _ := h.request(owner, "POST", "/api/v1/playground/stream", map[string]any{
		"model": slug, "stream": true, "operation": "embeddings",
		"request": map[string]any{"input": "a"},
	}, nil); status != 422 {
		t.Fatalf("non-generation stream: %d %v", status, out)
	}
	if status, out, _ := h.request(owner, "POST", "/api/v1/playground", map[string]any{
		"model": slug, "input": "hi", "stream": true,
	}, nil); status != 422 {
		t.Fatalf("unary endpoint with stream:true: %d %v", status, out)
	}

	response, raw := h.do(owner, "POST", "/api/v1/playground/stream", map[string]any{
		"model": slug, "input": "hi", "stream": true,
	}, nil)
	if response.StatusCode != 200 {
		t.Fatalf("stream: %d %s", response.StatusCode, raw)
	}
	if ct := response.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("stream content type %q", ct)
	}
	events := sseEvents(string(raw))
	frames, done := 0, map[string]any{}
	var lastErr string
	for _, pair := range events {
		switch pair[0] {
		case "frame":
			frames++
			var payload map[string]any
			if err := json.Unmarshal([]byte(pair[1]), &payload); err != nil {
				t.Fatalf("frame payload: %v", pair[1])
			}
			if _, ok := payload["data"].(string); !ok {
				t.Fatalf("frame must wrap the provider frame text: %v", payload)
			}
		case "done":
			if err := json.Unmarshal([]byte(pair[1]), &done); err != nil {
				t.Fatalf("done payload: %v", pair[1])
			}
		case "error":
			lastErr = pair[1]
		}
	}
	if frames < 2 {
		t.Fatalf("expected incremental frames, got %d events: %s", frames, raw)
	}
	if lastErr != "" {
		t.Fatalf("stream error event: %s", lastErr)
	}
	if done["model"] != slug || done["usage"] == nil || done["routing"] == nil {
		t.Fatalf("done metadata: %v", done)
	}
	if !strings.Contains(string(raw), "chat.completion.chunk") {
		t.Fatal("frames must carry the provider's client-family frame text")
	}

	response, raw = h.do(owner, "POST", "/api/v1/playground/stream", map[string]any{
		"model": slug, "stream": true,
		"request": map[string]any{"model": "injected", "stream": false, "messages": []any{map[string]any{"role": "user", "content": "hi"}}},
	}, nil)
	if response.StatusCode != 200 || !strings.Contains(string(raw), "event: done") {
		t.Fatalf("raw stream: %d %s", response.StatusCode, raw)
	}

	fixture.fail.Store(true)
	response, raw = h.do(owner, "POST", "/api/v1/playground/stream", map[string]any{
		"model": slug, "input": "hi", "stream": true,
	}, nil)
	fixture.fail.Store(false)
	if response.StatusCode != 502 {
		t.Fatalf("failed upstream before stream: %d %s", response.StatusCode, raw)
	}

	fixture.truncate.Store(true)
	response, raw = h.do(owner, "POST", "/api/v1/playground/stream", map[string]any{
		"model": slug, "input": "hi", "stream": true,
	}, nil)
	fixture.truncate.Store(false)
	if response.StatusCode != 200 {
		t.Fatalf("failing stream: %d %s", response.StatusCode, raw)
	}
	failed := false
	for _, pair := range sseEvents(string(raw)) {
		if pair[0] == "error" {
			failed = true
		}
	}
	if !failed {
		t.Fatalf("expected an error event: %s", raw)
	}

	marker := "pg-marker-8"
	reply = h.want(owner, "POST", "/api/v1/playground", map[string]any{
		"model": slug, "input": "say " + marker,
	}, nil, 200)
	envelope, _ := json.Marshal(sink.last())
	if strings.Contains(string(envelope), marker) || strings.Contains(string(envelope), vendorAnswer) {
		t.Fatalf("accounting envelope holds content: %s", envelope)
	}
	var stored int
	if err := h.Pool.QueryRow(t.Context(),
		"SELECT count(*) FROM olp.requests r WHERE row_to_json(r)::text LIKE '%'||$1||'%'", marker).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 0 {
		t.Fatal("playground prompts must not persist")
	}
	if err := h.Pool.QueryRow(t.Context(),
		"SELECT count(*) FROM olp.attempt_usage_facts f WHERE row_to_json(f)::text LIKE '%'||$1||'%'", vendorAnswer).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 0 {
		t.Fatal("playground outputs must not persist")
	}
}
