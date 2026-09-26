package gateway

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/testutil"
	"github.com/tyk-swe/olp/tests/fixtures"
)

func setRoutePolicy(h *harness, rules ...contentpolicy.Rule) {
	h.t.Helper()
	snapshot := h.rt.release.Snapshot
	route := snapshot.Routes[routeSlug]
	route.ContentPolicy = &contentpolicy.Policy{Rules: rules}
	snapshot.Routes[routeSlug] = route
}

func policyDecisions(h *harness) []contentpolicy.Decision {
	h.t.Helper()
	return h.sink.last(h.t).PolicyDecisions
}

func providerByName(h *harness, name string) runtime.Provider {
	h.t.Helper()
	for _, p := range h.rt.release.Snapshot.Providers {
		if p.Name == name {
			return p
		}
	}
	h.t.Fatalf("provider %s missing", name)
	return runtime.Provider{}
}

func TestContentPolicyInputBlocksBeforeDispatch(t *testing.T) {
	h := newHarness(t, Config{})
	setRoutePolicy(h, contentpolicy.Rule{ID: "no-secrets", Phase: contentpolicy.PhaseInput, Pattern: "forbidden", Action: contentpolicy.ActionBlock})
	resp, body := h.chat(fullKey, nil, `,"messages":[{"role":"user","content":"a forbidden prompt"}]`)
	if resp.StatusCode != http.StatusBadRequest || errorCode(t, body) != "content_policy_blocked" {
		t.Fatalf("status=%d body=%v", resp.StatusCode, body)
	}
	if h.mock.count("a") != 0 || h.mock.count("b") != 0 {
		t.Fatalf("provider called: a=%d b=%d", h.mock.count("a"), h.mock.count("b"))
	}
	decisions := policyDecisions(h)
	if len(decisions) != 1 || decisions[0] != (contentpolicy.Decision{RuleID: "no-secrets", Phase: "input", Action: "block", Outcome: "blocked"}) {
		t.Fatalf("decisions %+v", decisions)
	}
}

func TestContentPolicyInputRedactDispatchesTransformed(t *testing.T) {
	h := newHarness(t, Config{})
	setRoutePolicy(h, contentpolicy.Rule{ID: "mask-secret", Phase: contentpolicy.PhaseInput, Pattern: "s3cr3t", Action: contentpolicy.ActionRedact, Replacement: "[MASK]"})
	seen := make(chan string, 1)
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		seen <- string(data)
		completion(modelA, answerText)(w, r)
	})
	resp, body := h.chat(fullKey, nil, `,"messages":[{"role":"user","content":"the s3cr3t plan"}]`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%v", resp.StatusCode, body)
	}
	upstream := <-seen
	if !strings.Contains(upstream, "the [MASK] plan") || strings.Contains(upstream, "s3cr3t") {
		t.Fatalf("upstream body=%s", upstream)
	}
	decisions := policyDecisions(h)
	if len(decisions) != 1 || decisions[0].Outcome != "redacted" || decisions[0].RuleID != "mask-secret" {
		t.Fatalf("decisions %+v", decisions)
	}
}

func TestContentPolicyInputRulesApplyInOrder(t *testing.T) {
	h := newHarness(t, Config{})
	setRoutePolicy(h,
		contentpolicy.Rule{ID: "redact-first", Phase: contentpolicy.PhaseInput, Pattern: "s3cr3t", Action: contentpolicy.ActionRedact, Replacement: "safe"},
		contentpolicy.Rule{ID: "block-second", Phase: contentpolicy.PhaseInput, Pattern: "s3cr3t", Action: contentpolicy.ActionBlock},
	)
	resp, body := h.chat(fullKey, nil, `,"messages":[{"role":"user","content":"a s3cr3t here"}]`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("redact ran before block: status=%d body=%v", resp.StatusCode, body)
	}
}

func TestContentPolicyOutputBlocksWithAccounting(t *testing.T) {
	h := newHarness(t, Config{})
	setRoutePolicy(h, contentpolicy.Rule{ID: "no-leak", Phase: contentpolicy.PhaseOutput, Pattern: "upstream", Action: contentpolicy.ActionBlock})
	resp, body := h.chat(fullKey, nil)
	if resp.StatusCode != http.StatusBadRequest || errorCode(t, body) != "content_policy_blocked" {
		t.Fatalf("status=%d body=%v", resp.StatusCode, body)
	}
	if h.mock.count("a") != 1 {
		t.Fatalf("provider calls=%d", h.mock.count("a"))
	}
	env := h.sink.last(t)
	if env.Usage == nil || env.Usage.TotalTokens != 5 {
		t.Fatalf("usage not retained: %+v", env.Usage)
	}
	decisions := env.PolicyDecisions
	if len(decisions) != 1 || decisions[0] != (contentpolicy.Decision{RuleID: "no-leak", Phase: "output", Action: "block", Outcome: "blocked"}) {
		t.Fatalf("decisions %+v", decisions)
	}
}

func TestContentPolicyOutputRedactRewritesBody(t *testing.T) {
	h := newHarness(t, Config{})
	setRoutePolicy(h, contentpolicy.Rule{ID: "mask-hello", Phase: contentpolicy.PhaseOutput, Pattern: "hello", Action: contentpolicy.ActionRedact, Replacement: "[REDACTED]"})
	resp, body := h.chat(fullKey, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%v", resp.StatusCode, body)
	}
	choices, _ := body["choices"].([]any)
	message, _ := choices[0].(map[string]any)["message"].(map[string]any)
	if message["content"] != "[REDACTED] from upstream" {
		t.Fatalf("content=%v", message["content"])
	}
	env := h.sink.last(t)
	if env.Usage == nil || env.Usage.TotalTokens != 5 || env.Outcome != "success" {
		t.Fatalf("envelope %+v", env)
	}
	if len(env.PolicyDecisions) != 1 || env.PolicyDecisions[0].Outcome != "redacted" {
		t.Fatalf("decisions %+v", env.PolicyDecisions)
	}
}

func TestContentPolicyReplacementIsLiteral(t *testing.T) {
	h := newHarness(t, Config{})
	setRoutePolicy(h, contentpolicy.Rule{ID: "literal", Phase: contentpolicy.PhaseOutput, Pattern: "h(el)lo", Action: contentpolicy.ActionRedact, Replacement: "$1"})
	resp, body := h.chat(fullKey, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%v", resp.StatusCode, body)
	}
	choices, _ := body["choices"].([]any)
	message, _ := choices[0].(map[string]any)["message"].(map[string]any)
	if message["content"] != "$1 from upstream" {
		t.Fatalf("replacement expanded: %v", message["content"])
	}
}

func TestContentPolicyStreamingRequiresUnary(t *testing.T) {
	h := newHarness(t, Config{})
	setRoutePolicy(h, contentpolicy.Rule{ID: "out", Phase: contentpolicy.PhaseOutput, Pattern: "x", Action: contentpolicy.ActionBlock})
	resp, body := h.chat(fullKey, nil, `,"stream":true`)
	if resp.StatusCode != http.StatusUnprocessableEntity || errorCode(t, body) != "content_policy_streaming_requires_unary" {
		t.Fatalf("status=%d body=%v", resp.StatusCode, body)
	}
	if h.mock.count("a") != 0 {
		t.Fatal("provider called on streaming preflight rejection")
	}
}

func TestContentPolicyInputOnlyPolicyStillStreams(t *testing.T) {
	h := newHarness(t, Config{})
	setRoutePolicy(h, contentpolicy.Rule{ID: "in", Phase: contentpolicy.PhaseInput, Pattern: "zzz-never-matches", Action: contentpolicy.ActionBlock})
	data, err := fixtures.Files.ReadFile("streams/openai-chat.sse")
	if err != nil {
		t.Fatal(err)
	}
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		if err := testutil.Stream(w, r, data, 1, 0); err != nil {
			t.Error(err)
		}
	})
	resp := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", fullKey,
		[]byte(`{"model":"`+routeSlug+`","messages":[{"role":"user","content":"hi"}],"stream":true}`), nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status=%d type=%s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	scanner := bufio.NewScanner(resp.Body)
	done := false
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) == "data: [DONE]" {
			done = true
		}
	}
	if !done {
		t.Fatal("stream missing [DONE]")
	}
}

func TestContentPolicyResponsesSurface(t *testing.T) {
	h := newHarness(t, Config{})
	setRoutePolicy(h,
		contentpolicy.Rule{ID: "mask-in", Phase: contentpolicy.PhaseInput, Pattern: "s3cr3t", Action: contentpolicy.ActionRedact, Replacement: "[REDACTED]"},
		contentpolicy.Rule{ID: "mask-out", Phase: contentpolicy.PhaseOutput, Pattern: "upstream", Action: contentpolicy.ActionRedact, Replacement: "provider"},
	)
	seen := make(chan string, 1)
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		seen <- string(data)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"resp_1","object":"response","model":"model-a","status":"completed","output":[{"type":"message","id":"m","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hello from upstream","annotations":[]}]}],"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}`)
	})
	resp := h.do(t.Context(), http.MethodPost, "/v1/responses", fullKey,
		[]byte(`{"model":"`+routeSlug+`","input":"hide s3cr3t please"}`), nil)
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, data)
	}
	if upstream := <-seen; strings.Contains(upstream, "s3cr3t") || !strings.Contains(upstream, "[REDACTED]") {
		t.Fatalf("upstream body=%s", upstream)
	}
	if !strings.Contains(string(data), "provider") || strings.Contains(string(data), "hello from upstream") {
		t.Fatalf("output not rewritten: %s", data)
	}
	if env := h.sink.last(t); len(env.PolicyDecisions) != 2 {
		t.Fatalf("decisions %+v", env.PolicyDecisions)
	}
}

func TestContentPolicyAnthropicInputAndOutput(t *testing.T) {
	h := newHarness(t, Config{})
	snapshot := h.rt.release.Snapshot
	provider := providerByName(h, "a")
	provider.Kind = "openai"
	provider.Capabilities = append(provider.Capabilities,
		runtime.Capability{Model: modelA, Operation: "generation", Surface: "anthropic", Mode: "unary"})
	snapshot.Providers[provider.ID] = provider
	setRoutePolicy(h,
		contentpolicy.Rule{ID: "mask-in", Phase: contentpolicy.PhaseInput, Pattern: "s3cr3t", Action: contentpolicy.ActionRedact, Replacement: "[REDACTED]"},
		contentpolicy.Rule{ID: "block-out", Phase: contentpolicy.PhaseOutput, Pattern: "upstream", Action: contentpolicy.ActionBlock},
	)
	seen := make(chan string, 1)
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		seen <- string(data)
		completion(modelA, answerText)(w, r)
	})
	resp := h.do(t.Context(), http.MethodPost, "/anthropic/v1/messages", fullKey,
		[]byte(`{"model":"`+routeSlug+`","max_tokens":16,"messages":[{"role":"user","content":"the s3cr3t plan"}]}`), nil)
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(data), "content policy") {
		t.Fatalf("status=%d body=%s", resp.StatusCode, data)
	}
	if upstream := <-seen; strings.Contains(upstream, "s3cr3t") || !strings.Contains(upstream, "[REDACTED]") {
		t.Fatalf("upstream body=%s", upstream)
	}
}

func TestContentPolicyGeminiInputRedact(t *testing.T) {
	h := newHarness(t, Config{})
	snapshot := h.rt.release.Snapshot
	provider := providerByName(h, "a")
	provider.Kind = "openai"
	provider.Capabilities = append(provider.Capabilities,
		runtime.Capability{Model: modelA, Operation: "generation", Surface: "gemini", Mode: "unary"})
	snapshot.Providers[provider.ID] = provider
	setRoutePolicy(h, contentpolicy.Rule{ID: "mask-in", Phase: contentpolicy.PhaseInput, Pattern: "s3cr3t", Action: contentpolicy.ActionRedact, Replacement: "[REDACTED]"})
	seen := make(chan string, 1)
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		seen <- string(data)
		completion(modelA, answerText)(w, r)
	})
	resp := h.do(t.Context(), http.MethodPost, "/gemini/v1/models/"+routeSlug+":generateContent", fullKey,
		[]byte(`{"contents":[{"role":"user","parts":[{"text":"the s3cr3t plan"}]}]}`), nil)
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, data)
	}
	if upstream := <-seen; strings.Contains(upstream, "s3cr3t") || !strings.Contains(upstream, "[REDACTED]") {
		t.Fatalf("upstream body=%s", upstream)
	}
}

func TestContentPolicyEmbeddingsInput(t *testing.T) {
	h := newHarness(t, Config{})
	snapshot := h.rt.release.Snapshot
	for id, p := range snapshot.Providers {
		p.Capabilities = append(p.Capabilities, runtime.Capability{Model: p.Capabilities[0].Model, Operation: "embeddings", Surface: "openai", Mode: "unary"})
		snapshot.Providers[id] = p
	}
	route := snapshot.Routes[routeSlug]
	route.Operations = append(route.Operations, "embeddings")
	snapshot.Routes[routeSlug] = route
	setRoutePolicy(h, contentpolicy.Rule{ID: "no-embed-secret", Phase: contentpolicy.PhaseInput, Pattern: "s3cr3t", Action: contentpolicy.ActionBlock})
	resp := h.do(t.Context(), http.MethodPost, "/v1/embeddings", fullKey,
		[]byte(`{"model":"`+routeSlug+`","input":"embed s3cr3t"}`), nil)
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(data), "content_policy_blocked") {
		t.Fatalf("status=%d body=%s", resp.StatusCode, data)
	}
	if h.mock.count("a") != 0 {
		t.Fatal("provider called for blocked embedding")
	}
}

func TestContentPolicyMediaInput(t *testing.T) {
	h := newMediaHarness(t)
	setRoutePolicy(h, contentpolicy.Rule{ID: "no-photo-secret", Phase: contentpolicy.PhaseInput, Pattern: "s3cr3t", Action: contentpolicy.ActionBlock})
	resp := h.do(t.Context(), http.MethodPost, "/v1/images/generations", fullKey,
		[]byte(`{"model":"`+routeSlug+`","prompt":"a s3cr3t photo"}`), nil)
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(data), "content_policy_blocked") {
		t.Fatalf("status=%d body=%s", resp.StatusCode, data)
	}
	if h.mock.count("a") != 0 {
		t.Fatal("provider called for blocked media prompt")
	}
}

func TestContentPolicyVideoInputBlock(t *testing.T) {
	h := newMediaHarness(t)
	setRoutePolicy(h, contentpolicy.Rule{ID: "no-prompt-secret", Phase: contentpolicy.PhaseInput, Pattern: "s3cr3t", Action: contentpolicy.ActionBlock})
	body := "--policy-boundary\r\n" +
		"Content-Disposition: form-data; name=\"model\"\r\n\r\n" +
		routeSlug + "\r\n" +
		"--policy-boundary\r\n" +
		"Content-Disposition: form-data; name=\"prompt\"\r\n\r\n" +
		"film the s3cr3t\r\n" +
		"--policy-boundary--\r\n"
	resp := h.do(t.Context(), http.MethodPost, "/v1/videos", fullKey,
		[]byte(body), map[string]string{"Content-Type": "multipart/form-data; boundary=policy-boundary"})
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(data), "content_policy_blocked") {
		t.Fatalf("status=%d body=%s", resp.StatusCode, data)
	}
	if h.mock.count("a") != 0 {
		t.Fatal("provider called for blocked video prompt")
	}
}

func TestContentPolicyDecisionDedupMultiSlotInput(t *testing.T) {
	h := newHarness(t, Config{})
	setRoutePolicy(h, contentpolicy.Rule{ID: "mask-secret", Phase: contentpolicy.PhaseInput, Pattern: "s3cr3t", Action: contentpolicy.ActionRedact, Replacement: "[MASK]"})
	resp, body := h.chat(fullKey, nil, `,"messages":[{"role":"system","content":"s3cr3t setup"},{"role":"user","content":"tell me the s3cr3t"}]`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%v", resp.StatusCode, body)
	}
	env := h.sink.last(t)
	if len(env.PolicyDecisions) != 1 || env.PolicyDecisions[0] != (contentpolicy.Decision{RuleID: "mask-secret", Phase: "input", Action: "redact", Outcome: "redacted"}) {
		t.Fatalf("decisions %+v", env.PolicyDecisions)
	}
	if env.Usage == nil || env.Usage.TotalTokens != 5 {
		t.Fatalf("accounting event invalid: %+v", env.Usage)
	}
}

func TestContentPolicyDecisionDedupMediaSlots(t *testing.T) {
	h := newMediaHarness(t)
	setRoutePolicy(h, contentpolicy.Rule{ID: "mask-secret", Phase: contentpolicy.PhaseInput, Pattern: "s3cr3t", Action: contentpolicy.ActionRedact, Replacement: "[MASK]"})
	seen := make(chan string, 1)
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		seen <- string(data)
		w.Header().Set("Content-Type", "audio/mpeg")
		io.WriteString(w, "audio-payload")
	})
	resp := h.do(t.Context(), http.MethodPost, "/v1/audio/speech", fullKey,
		[]byte(`{"model":"`+routeSlug+`","input":"say the s3cr3t","instructions":"a s3cr3t voice","voice":"alloy"}`), nil)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	upstream := <-seen
	if strings.Contains(upstream, "s3cr3t") || strings.Count(upstream, "[MASK]") != 2 {
		t.Fatalf("upstream body=%s", upstream)
	}
	if decisions := policyDecisions(h); len(decisions) != 1 {
		t.Fatalf("decisions %+v", decisions)
	}
}

func TestContentPolicyDecisionDedupOutputSlots(t *testing.T) {
	h := newHarness(t, Config{})
	setRoutePolicy(h, contentpolicy.Rule{ID: "mask-hello", Phase: contentpolicy.PhaseOutput, Pattern: "hello", Action: contentpolicy.ActionRedact, Replacement: "[X]"})
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":"hello text","refusal":"hello refusal"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`, modelA)
	})
	resp, body := h.chat(fullKey, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%v", resp.StatusCode, body)
	}
	if decisions := policyDecisions(h); len(decisions) != 1 {
		t.Fatalf("decisions %+v", decisions)
	}
}

func TestContentPolicyDecisionDedupCompletion(t *testing.T) {
	h := newHarness(t, Config{})
	s := h.gateway
	x := &execution{route: &runtime.Route{Fidelity: runtime.RouteFidelity{Mode: runtime.FidelityTransformed}, ContentPolicy: &contentpolicy.Policy{Rules: []contentpolicy.Rule{
		{ID: "mask-out", Phase: contentpolicy.PhaseOutput, Pattern: "secret", Action: contentpolicy.ActionRedact, Replacement: "[X]"},
	}}}}
	completion := &openai.Completion{OutputText: "a secret", Refusal: "another secret"}
	if e := s.enforceCompletionOutput(x, completion); e != nil {
		t.Fatalf("enforce: %+v", e)
	}
	if len(x.policyDecisions) != 1 {
		t.Fatalf("decisions %+v", x.policyDecisions)
	}
	if completion.OutputText != "a [X]" || completion.Refusal != "another [X]" {
		t.Fatalf("completion %+v", completion)
	}
}

func TestPolicySurfaceGate(t *testing.T) {
	if e := policySurfaceGate(nil); e != nil {
		t.Fatalf("nil route: %v", e)
	}
	route := &runtime.Route{Slug: "r"}
	if e := policySurfaceGate(route); e != nil {
		t.Fatalf("no policy: %v", e)
	}
	route.ContentPolicy = &contentpolicy.Policy{Rules: []contentpolicy.Rule{
		{ID: "r1", Phase: contentpolicy.PhaseInput, Pattern: "x", Action: contentpolicy.ActionBlock},
	}}
	e := policySurfaceGate(route)
	if e == nil || e.Status != http.StatusUnprocessableEntity || e.Code != "content_policy_surface_unavailable" {
		t.Fatalf("gate=%+v", e)
	}
}
