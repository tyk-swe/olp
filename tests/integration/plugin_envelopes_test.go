//go:build integration

package integration_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/testutil"
)

// envelopedRequest is what the enveloped upstream accepts: the model and the
// Gemini generateContent request inside the upstream's own envelope.
type envelopedRequest struct {
	Model   string `json:"model"`
	Request struct {
		Contents          []json.RawMessage          `json:"contents"`
		SystemInstruction json.RawMessage            `json:"systemInstruction"`
		GenerationConfig  map[string]json.RawMessage `json:"generationConfig"`
	} `json:"request"`
}

// envelopedUpstream is the fictional upstream the reference plugin's
// reference-gemini profile places requests at: Gemini generateContent under
// /enveloped/v1beta, with each request wrapped as {model, request} and each
// response and stream event as {response}. It authenticates the way the
// reference plugin declares and records every envelope it accepts.
type envelopedUpstream struct {
	*httptest.Server
	mu       sync.Mutex
	requests []envelopedRequest
}

func newEnvelopedUpstream(t *testing.T) *envelopedUpstream {
	t.Helper()
	u := &envelopedUpstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Token "+pluginCredential || r.Header.Get("X-Reference-Client") != "olp" {
			w.WriteHeader(http.StatusUnauthorized)
			writeJSON(w, map[string]any{"error": map[string]any{"code": 401, "message": "Unknown token", "status": "UNAUTHENTICATED"}})
			return
		}
		model, operation, found := strings.Cut(strings.TrimPrefix(r.URL.Path, "/enveloped/v1beta/models/"), ":")
		stream := operation == "streamGenerateContent" && r.URL.Query().Get("alt") == "sse"
		var envelope envelopedRequest
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if r.Method != http.MethodPost || !found || operation != "generateContent" && !stream || decoder.Decode(&envelope) != nil || envelope.Model != model || len(envelope.Request.Contents) == 0 {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]any{"error": map[string]any{"code": 400, "message": "Not an enveloped generateContent request", "status": "INVALID_ARGUMENT"}})
			return
		}
		u.mu.Lock()
		u.requests = append(u.requests, envelope)
		u.mu.Unlock()
		candidate := func(text, finish string) map[string]any {
			c := map[string]any{"index": 0, "content": map[string]any{"role": "model", "parts": []any{map[string]any{"text": text}}}}
			if finish != "" {
				c["finishReason"] = finish
			}
			return map[string]any{"responseId": "enveloped-1", "modelVersion": model, "candidates": []any{c}}
		}
		usage := map[string]any{"promptTokenCount": 4, "candidatesTokenCount": 6, "totalTokenCount": 10}
		if !stream {
			response := candidate("Hello from the enveloped upstream", "STOP")
			response["usageMetadata"] = usage
			writeJSON(w, map[string]any{"response": response, "traceId": "trace-unary"})
			return
		}
		last := candidate("the enveloped upstream", "STOP")
		last["usageMetadata"] = usage
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []map[string]any{candidate("Hello from ", ""), last} {
			data, _ := json.Marshal(map[string]any{"response": event, "traceId": "trace-stream"})
			fmt.Fprintf(w, "data: %s\n\n", data)
		}
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *envelopedUpstream) received() []envelopedRequest {
	u.mu.Lock()
	defer u.mu.Unlock()
	return slices.Clone(u.requests)
}

// A plugin profile that envelopes and rewrites the dialect's bodies serves
// transformed routes only. OLP wraps each request after the declared
// rewrites, and callers and usage accounting see the unwrapped dialect
// responses and stream events.
func TestPluginProfileWithAnEnvelopeServesTransformedRoutes(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	sink := &captureSink{}
	h.Gateway.Sink = sink
	upstream := newEnvelopedUpstream(t)
	module := testutil.BuildPlugin(t, "./sdk/plugin/reference", "-X=main.upstream="+upstream.URL+"/v1")
	digest := digestOf(module)
	installed := h.want(owner, "POST", "/api/v1/plugins", module, wasm, 201)
	declared := installed["manifest"].(map[string]any)["profiles"].([]any)[1].(map[string]any)["hosting"].(map[string]any)
	if envelope := declared["envelope"].(map[string]any); envelope["request"] != "request" || envelope["response"] != "response" || len(declared["rewrites"].([]any)) != 3 {
		t.Fatalf("the owner reviews %v", declared)
	}
	h.want(owner, "POST", "/api/v1/plugins/"+digest+"/approve", map[string]any{"origins": installed["manifest"].(map[string]any)["origins"]}, etagHeader(installed), 200)
	for _, profile := range h.want(owner, "GET", "/api/v1/provider-profiles", nil, nil, 200)["items"].([]any) {
		if p := profile.(map[string]any); p["kind"] == "plugin" && (p["id"] == "reference-gemini") == (p["strict"] == true) {
			t.Fatalf("catalogued %v", p)
		}
	}

	create := map[string]any{"name": "Enveloped", "credential": pluginCredential, "model": vendorModel,
		"configuration": map[string]any{"kind": "plugin", "auth_mode": "static_credential", "profile_id": "reference-gemini", "profile_revision": digest}}
	created := h.want(owner, "POST", "/api/v1/providers", create, idem(uuid.NewString()), 201)
	// Certification probes unary and streaming chat through the envelope.
	certifyPluginProvider(t, h, owner, "/api/v1/providers/"+created["id"].(string))

	draft := fidelityDraft("enveloped-strict", created["id"])
	draft["fidelity"] = map[string]any{"mode": "strict"}
	strict := h.want(owner, "POST", "/api/v1/route-drafts", draft, idem(uuid.NewString()), 201)
	for _, action := range []string{"validate", "activate"} {
		problem := h.want(owner, "POST", "/api/v1/route-drafts/"+strict["id"].(string)+"/"+action, nil, withMatch(strict, idem(uuid.NewString())), 422)
		if problemCode(t, problem) != "target_capability" || !strings.Contains(problem["detail"].(string), "Declare the route transformed to use this profile") {
			t.Fatalf("strict %s of an enveloped profile: %v", action, problem)
		}
	}
	route := h.want(owner, "POST", "/api/v1/route-drafts", transformed(fidelityDraft("enveloped", created["id"])), idem(uuid.NewString()), 201)
	h.want(owner, "POST", "/api/v1/route-drafts/"+route["id"].(string)+"/activate", nil, withMatch(route, idem(uuid.NewString())), 200)
	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "Enveloped", "scopes": []string{"inference"}, "allowed_routes": []string{"enveloped"}}, idem(uuid.NewString()), 201)["secret"].(string)
	h.refresh()

	before := len(upstream.received())
	status, reply, _ := h.gateway("POST", "/v1/chat/completions", key, map[string]any{"model": "enveloped", "seed": 7, "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	usage, _ := reply["usage"].(map[string]any)
	if status != 200 || !strings.Contains(fmt.Sprint(reply["choices"]), "Hello from the enveloped upstream") || usage["prompt_tokens"] != float64(4) || usage["completion_tokens"] != float64(6) {
		t.Fatalf("unary through the envelope: %d %v", status, reply)
	}
	if event := sink.last(); event.Usage == nil || event.Usage.InputTokens != 4 || event.Usage.OutputTokens != 6 || len(event.Attempts) != 1 {
		t.Fatalf("accounted %+v", event)
	}
	status, reply, _ = h.gateway("POST", "/v1/chat/completions", key, map[string]any{"model": "enveloped", "messages": []any{
		map[string]any{"role": "system", "content": "Answer in French."}, map[string]any{"role": "user", "content": "hi"},
	}})
	if status != 200 {
		t.Fatalf("unary with a system message: %d %v", status, reply)
	}
	streamed, _ := json.Marshal(map[string]any{"model": "enveloped", "stream": true, "stream_options": map[string]any{"include_usage": true}, "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	code, events, _ := h.gatewayRaw("POST", "/v1/chat/completions", key, bytes.NewReader(streamed), map[string]string{"Content-Type": "application/json"})
	var text strings.Builder
	var streamedUsage map[string]any
	scanner := bufio.NewScanner(bytes.NewReader(events))
	for scanner.Scan() {
		payload, found := strings.CutPrefix(scanner.Text(), "data: ")
		if !found || payload == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Usage map[string]any `json:"usage"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("the caller received %s", payload)
		}
		for _, choice := range chunk.Choices {
			text.WriteString(choice.Delta.Content)
		}
		if chunk.Usage != nil {
			streamedUsage = chunk.Usage
		}
	}
	if code != 200 || text.String() != "Hello from the enveloped upstream" || streamedUsage["completion_tokens"] != float64(6) || !bytes.Contains(events, []byte("data: [DONE]")) {
		t.Fatalf("stream through the envelope: %d %s", code, events)
	}
	if event := sink.last(); event.Usage == nil || event.Usage.OutputTokens != 6 || event.Mode != "streaming" {
		t.Fatalf("accounted %+v", event)
	}

	// Each request reached the upstream in its envelope, with only the
	// declared rewrites made: one candidate, no seed, and the default system
	// instruction unless the caller gave one.
	served := upstream.received()[before:]
	if len(served) != 3 {
		t.Fatalf("the upstream received %d requests", len(served))
	}
	defaulted := `{"parts":[{"text":"You are the reference assistant."}]}`
	for i, request := range served {
		config := request.Request.GenerationConfig
		if request.Model != vendorModel || string(config["candidateCount"]) != "1" || config["seed"] != nil {
			t.Fatalf("request %d reached the upstream as %+v", i, request)
		}
		if system := string(request.Request.SystemInstruction); (i == 1) == (system == defaulted) || !strings.Contains(system, "text") {
			t.Fatalf("request %d has system instruction %s", i, system)
		}
	}
}
