//go:build integration

package integration_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/gateway"
	"github.com/tyk-swe/olp/internal/usage"
)

type strictProviderCall struct {
	body    []byte
	headers http.Header
	query   url.Values
}

type strictProviderFixture struct {
	*httptest.Server
	mu         sync.Mutex
	calls      []strictProviderCall
	profile    string
	providerID string
	failure    atomic.Int32
}

func newStrictProviderFixture(t *testing.T, profile string) *strictProviderFixture {
	t.Helper()
	f := &strictProviderFixture{profile: profile}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			if profile == "gemini-generation" {
				writeJSON(w, map[string]any{"models": []any{map[string]string{"name": "models/" + vendorModel}}})
			} else {
				writeJSON(w, map[string]any{"data": []any{map[string]string{"id": vendorModel}}})
			}
			return
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		f.mu.Lock()
		f.calls = append(f.calls, strictProviderCall{raw, r.Header.Clone(), r.URL.Query()})
		f.mu.Unlock()
		if failure := f.failure.Load(); failure != 0 {
			if failure == 2 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, `{"id":`)
				_ = http.NewResponseController(w).Flush()
			}
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close()
			return
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Error(err)
			return
		}
		stream := string(body["stream"]) == "true" || strings.HasSuffix(r.URL.Path, ":streamGenerateContent")
		switch profile {
		case "compatible-responses", "azure-v1-responses":
			writeResponsesFixture(w, vendorModel, vendorAnswer, stream)
		case "anthropic-messages":
			parityGeneration(w, "anthropic", stream)
		case "gemini-generation":
			parityGeneration(w, "gemini", stream)
		default:
			parityGeneration(w, "openai", stream)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *strictProviderFixture) captured() []strictProviderCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]strictProviderCall(nil), f.calls...)
}

func publishStrictProvider(t *testing.T, h *accessHarness, owner *browser, f *strictProviderFixture, options map[string]any, policy map[string]any, mode string) (string, string) {
	t.Helper()
	profile, err := connectors.LookupProfile(f.profile, "1")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := f.URL + "/v1"
	if f.profile == "gemini-generation" {
		endpoint = f.URL + "/v1beta"
	}
	if profile.Kind == "azure_openai" {
		endpoint = f.URL
	}
	config := map[string]any{"kind": profile.Kind, "profile_id": profile.ID, "profile_revision": profile.Revision, "endpoint": endpoint, "auth_mode": "api_key"}
	if profile.Kind == "azure_openai" {
		config["deployment"] = vendorModel
	}
	if options != nil {
		config["options"] = options
	}
	provider := h.want(owner, "POST", "/api/v3/providers", map[string]any{"name": "Strict " + f.profile + uuid.NewString(), "configuration": config, "model": vendorModel, "credential": vendorSecret}, idem(uuid.NewString()), 201)
	providerPath := "/api/v3/providers/" + provider["id"].(string)
	f.providerID = provider["id"].(string)
	modelID := h.want(owner, "GET", providerPath+"/models", nil, nil, 200)["items"].([]any)[0].(map[string]any)["id"].(string)
	surfaces := []string{"openai"}
	if profile.Dialect == "anthropic-messages" {
		surfaces = append(surfaces, "anthropic")
	}
	if profile.Dialect == "gemini-generate-content" {
		surfaces = append(surfaces, "gemini")
	}
	capabilities := []any{}
	for _, surface := range surfaces {
		for _, delivery := range []string{"unary", "streaming"} {
			capabilities = append(capabilities, map[string]any{"operation": "generation", "surface": surface, "mode": delivery})
		}
	}
	provider = h.want(owner, "PATCH", providerPath+"/models/"+modelID, map[string]any{"enabled": true, "capabilities": capabilities}, etagHeader(provider), 200)
	h.want(owner, "POST", providerPath+"/models/"+modelID+"/certify", nil, etagHeader(provider), 200)
	provider = h.want(owner, "GET", providerPath, nil, nil, 200)
	h.want(owner, "POST", providerPath+"/activate", nil, withMatch(provider, idem(uuid.NewString())), 200)
	slug := "strict-" + uuid.NewString()
	input := fidelityDraft(slug, provider["id"])
	input["fidelity"] = map[string]any{"mode": mode}
	if policy != nil {
		input["content_policy"] = policy
	}
	draft := h.want(owner, "POST", "/api/v3/route-drafts", input, idem(uuid.NewString()), 201)
	h.want(owner, "POST", "/api/v3/route-drafts/"+draft["id"].(string)+"/activate", nil, withMatch(draft, idem(uuid.NewString())), 200)
	key := h.want(owner, "POST", "/api/v3/api-keys", map[string]any{"name": "Strict fixture", "scopes": []string{"inference"}, "allowed_routes": []string{slug}}, idem(uuid.NewString()), 201)
	h.refresh()
	return slug, key["secret"].(string)
}

type strictOutcomeSink chan gateway.Envelope

func (s strictOutcomeSink) Terminal(e gateway.Envelope) { s <- e }

func TestStrictPublicAmbiguousWorkNeverReplaysOrSubstitutes(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	first := newStrictProviderFixture(t, "compatible-chat")
	second := newStrictProviderFixture(t, "compatible-chat")
	slug, key := publishStrictProvider(t, h, owner, first, nil, nil, "strict")
	publishStrictProvider(t, h, owner, second, nil, nil, "strict")
	input := fidelityDraft(slug, first.providerID)
	input["fidelity"] = map[string]any{}
	input["max_attempts"] = 2
	input["targets"] = []any{
		map[string]any{"provider_id": first.providerID, "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 5000},
		map[string]any{"provider_id": second.providerID, "provider_model": vendorModel, "priority": 1, "weight": 1, "timeout_ms": 5000},
	}
	draft := h.want(owner, "POST", "/api/v3/route-drafts", input, idem(uuid.NewString()), 201)
	h.want(owner, "POST", "/api/v3/route-drafts/"+draft["id"].(string)+"/activate", nil, withMatch(draft, idem(uuid.NewString())), 200)
	h.refresh()
	sink := make(strictOutcomeSink, 2)
	h.Gateway.Sink = sink
	for _, failure := range []int32{1, 2} {
		first.failure.Store(failure)
		beforeFirst, beforeSecond := len(first.captured()), len(second.captured())
		status, response, _ := h.gatewayRaw("POST", "/v1/chat/completions", key, strings.NewReader(fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"accepted work"}]}`, slug)), map[string]string{"Content-Type": "application/json"})
		if status != 502 || !bytes.Contains(response, []byte(`"code":"ambiguous_upstream_result"`)) || len(first.captured()) != beforeFirst+1 || len(second.captured()) != beforeSecond {
			t.Fatalf("ambiguous work replayed or substituted: %d %s", status, response)
		}
		select {
		case envelope := <-sink:
			if len(envelope.Attempts) != 1 {
				t.Fatalf("ambiguous attempt accounting: %+v", envelope.Attempts)
			}
			fact := envelope.Attempts[0]
			want := usage.UpstreamUnknown
			if failure == 2 {
				want = usage.UpstreamAccepted
			}
			if fact.Interaction == nil || fact.Interaction.UpstreamState != want || fact.Interaction.ClientState != usage.ClientUnobserved || fact.Committed || !fact.BillingUncertain {
				t.Fatalf("ambiguous evidence %+v / %+v", fact, fact.Interaction)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no terminal Attempt evidence")
		}
	}
}

func TestStrictPublicResponsesPreserveScalarAndArrayInputs(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	f := newStrictProviderFixture(t, "azure-v1-responses")
	slug, key := publishStrictProvider(t, h, owner, f, nil, nil, "strict")
	for _, input := range []string{`"exact scalar"`, `[{"role":"user","content":[{"type":"input_text","text":"exact array"}]}]`} {
		body := `{"model":"` + slug + `","input":` + input + `,"store":false,"max_output_tokens":32,"native_extension":{"counter":9007199254740993,"nil":null,"empty":""}}`
		status, response, _ := h.gatewayRaw("POST", "/v1/responses", key, strings.NewReader(body), map[string]string{"Content-Type": "application/json"})
		if status != 200 {
			t.Fatalf("native Responses: %d %s", status, response)
		}
		calls := f.captured()
		requireProfileNetworkJSON(t, strings.Replace(body, slug, vendorModel, 1), calls[len(calls)-1].body)
	}
	before := len(f.captured())
	status, response, _ := h.gatewayRaw("POST", "/v1/responses", key, strings.NewReader(`{"model":"`+slug+`","input":"native default retention"}`), map[string]string{"Content-Type": "application/json"})
	if status != 400 || !bytes.Contains(response, []byte(`"param":"store"`)) || !bytes.Contains(response, []byte(`"code":"policy_conflict"`)) || len(f.captured()) != before {
		t.Fatalf("native retention default bypassed policy: %d %s", status, response)
	}
}

func TestStrictPublicQualifiedTextAndPreciseRefusals(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	f := newStrictProviderFixture(t, "anthropic-messages")
	slug, key := publishStrictProvider(t, h, owner, f, nil, nil, "strict")
	beforePlayground := len(f.captured())
	problem := h.want(owner, "POST", "/api/v3/playground", map[string]any{"model": slug, "input": "hello"}, nil, 400)
	if problemCode(t, problem) != "state_carrier" || len(f.captured()) != beforePlayground {
		t.Fatal("unqualified playground projection dispatched strict work", problem)
	}
	body := `{"model":"` + slug + `","messages":[{"role":"user","content":"hello"}],"max_tokens":64,"stream":false}`
	status, response, _ := h.gatewayRaw("POST", "/v1/chat/completions", key, strings.NewReader(body), map[string]string{"Content-Type": "application/json"})
	if status != 200 || !bytes.Contains(response, []byte(vendorAnswer)) {
		t.Fatalf("qualified control: %d %s", status, response)
	}
	calls := f.captured()
	requireProfileNetworkJSON(t, `{"model":"`+vendorModel+`","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}],"max_tokens":64,"stream":false}`, calls[len(calls)-1].body)
	for _, fixture := range []struct{ field, value, code string }{
		{"messages", `[{"role":"system","content":"scope A"},{"role":"developer","content":"scope B"},{"role":"user","content":"hello"}]`, "instruction_scope"},
		{"messages", `[{"role":"assistant","content":"before","tool_calls":[{"id":"call-1","type":"function","function":{"name":"f","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call-1","content":"result"},{"role":"user","content":"after"}]`, "state_carrier"},
		{"response_format", `{"type":"json_schema","json_schema":{"name":"result","description":"required description","strict":true,"schema":{"type":"object"}}}`, "target_capability"},
		{"reasoning_effort", `"high"`, "reasoning_budget"},
		{"native_extension", `{"required":true}`, "unsupported_parameter"},
	} {
		t.Run(fixture.code, func(t *testing.T) {
			var request map[string]json.RawMessage
			if err := json.Unmarshal([]byte(body), &request); err != nil {
				t.Fatal(err)
			}
			request[fixture.field] = json.RawMessage(fixture.value)
			raw, _ := json.Marshal(request)
			before := len(f.captured())
			status, response, headers := h.gatewayRaw("POST", "/v1/chat/completions", key, bytes.NewReader(raw), map[string]string{"Content-Type": "application/json"})
			if status != 400 || !bytes.Contains(response, []byte(`"code":"`+fixture.code+`"`)) || len(f.captured()) != before || headers.Get("X-Request-Id") == "" {
				t.Fatalf("incompatible dispatch: %d %s", status, response)
			}
		})
	}
}

func TestStrictPublicGeminiQueryAuthenticationAndStreaming(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	f := newStrictProviderFixture(t, "gemini-generation")
	slug, key := publishStrictProvider(t, h, owner, f, nil, nil, "strict")
	body := `{"contents":[{"role":"user","parts":[{"text":"hello"}]}],"generationConfig":{"maxOutputTokens":32}}`
	path := "/gemini/v1beta/models/" + slug + ":streamGenerateContent?key=" + url.QueryEscape(key) + "&alt=sse"
	status, response, _ := h.gatewayRaw("POST", path, "", strings.NewReader(body), map[string]string{"Content-Type": "application/json"})
	if status != 200 || !bytes.Contains(response, []byte("data:")) {
		t.Fatalf("native query-key stream: %d %s", status, response)
	}
	calls := f.captured()
	actual := calls[len(calls)-1]
	requireProfileNetworkJSON(t, body, actual.body)
	if actual.query.Get("key") == key || bytes.Contains(response, []byte(key)) {
		t.Fatal("gateway key entered provider semantics or observation")
	}
	for _, suffix := range []string{"&key=" + url.QueryEscape(key), "&alt=json", "&bad=%zz"} {
		before := len(f.captured())
		status, response, _ := h.gatewayRaw("POST", path+suffix, "", strings.NewReader(body), map[string]string{"Content-Type": "application/json"})
		if status != 400 || len(f.captured()) != before {
			t.Fatalf("ambiguous query dispatched: %d %s", status, response)
		}
	}
}

func TestStrictPublicCallerSemanticHeadersArePreservedOrRejected(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	f := newStrictProviderFixture(t, "anthropic-messages")
	const beta = "interleaved-thinking-2025-05-14"
	slug, key := publishStrictProvider(t, h, owner, f, map[string]any{"semantic_headers": map[string]string{"Anthropic-Beta": beta}}, nil, "strict")
	body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":[{"type":"text","text":"native request"}]}],"max_tokens":32,"native_extension":{"value":null}}`, slug)
	for _, header := range []string{"", beta} {
		headers := map[string]string{"Content-Type": "application/json", "Anthropic-Version": "2023-06-01"}
		if header != "" {
			headers["Anthropic-Beta"] = header
		}
		status, response, _ := h.gatewayRaw("POST", "/anthropic/v1/messages", key, strings.NewReader(body), headers)
		if status != 200 {
			t.Fatalf("native semantic headers: %d %s", status, response)
		}
		calls := f.captured()
		actual := calls[len(calls)-1]
		if actual.headers.Get("Anthropic-Version") != "2023-06-01" || actual.headers.Get("Anthropic-Beta") != beta {
			t.Fatal("caller/config semantic headers changed")
		}
		requireProfileNetworkJSON(t, strings.Replace(body, slug, vendorModel, 1), actual.body)
	}
	before := len(f.captured())
	status, response, _ := h.gatewayRaw("POST", "/anthropic/v1/messages", key, strings.NewReader(body), map[string]string{"Content-Type": "application/json", "Anthropic-Beta": "conflicting-native-feature"})
	if status != 400 || !bytes.Contains(response, []byte(`"code":"target_capability"`)) || len(f.captured()) != before || bytes.Contains(response, []byte("conflicting-native-feature")) {
		t.Fatalf("conflicting semantic header dispatched or leaked: %d %s", status, response)
	}
}

func TestPublicEffectiveDefaultsAreInspectedBeforeDispatch(t *testing.T) {
	for _, mode := range []string{"strict", "transformed"} {
		t.Run(mode, func(t *testing.T) {
			h := newAccessHarness(t)
			owner := h.owner()
			f := newStrictProviderFixture(t, "compatible-chat")
			options := map[string]any{"operation_defaults": map[string]any{"generation": map[string]any{"dialect": "openai-chat", "values": map[string]any{"tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "lookup", "description": "private-marker", "parameters": map[string]any{"type": "object"}}}}}}}}
			action := "block"
			if mode == "transformed" {
				action = "redact"
			}
			slug, key := publishStrictProvider(t, h, owner, f, options, fidelityPolicy(action, "input"), mode)
			body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"safe caller"}]}`, slug)
			before := len(f.captured())
			status, response, _ := h.gatewayRaw("POST", "/v1/chat/completions", key, strings.NewReader(body), map[string]string{"Content-Type": "application/json"})
			if mode == "strict" {
				if status != 400 || !bytes.Contains(response, []byte(`"code":"content_policy_blocked"`)) || len(f.captured()) != before {
					t.Fatalf("default tool bypassed block: %d %s", status, response)
				}
				body = strings.TrimSuffix(body, "}") + `,"tools":[]}`
				status, response, _ = h.gatewayRaw("POST", "/v1/chat/completions", key, strings.NewReader(body), map[string]string{"Content-Type": "application/json"})
				if status != 200 {
					t.Fatalf("caller atomic empty override: %d %s", status, response)
				}
			} else {
				calls := f.captured()
				if status != 200 || bytes.Contains(calls[len(calls)-1].body, []byte("private-marker")) || !bytes.Contains(calls[len(calls)-1].body, []byte("[MASK]")) {
					t.Fatalf("default tool bypassed explicit transformation: %d %s", status, response)
				}
			}
		})
	}
}
