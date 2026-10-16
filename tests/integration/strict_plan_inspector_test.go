//go:build integration

package integration_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func inspectorProvider(t *testing.T, h *accessHarness, owner *browser, kind, profile string, options map[string]any) (string, *atomic.Int64) {
	t.Helper()
	calls := &atomic.Int64{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if kind == "anthropic" {
			if r.Header.Get("X-Api-Key") != vendorSecret || r.Header.Get("Anthropic-Version") != "2023-06-01" {
				t.Error("inspector fixture provider authentication differs")
				w.WriteHeader(401)
				return
			}
		} else if kind == "gemini" {
			if r.Header.Get("X-Goog-Api-Key") != vendorSecret {
				t.Error("inspector fixture Gemini credential differs")
				w.WriteHeader(401)
				return
			}
		} else if r.Header.Get("Authorization") != "Bearer "+vendorSecret {
			t.Error("inspector fixture provider authentication differs")
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/models":
			if kind == "gemini" {
				io.WriteString(w, `{"models":[{"name":"models/fixture-model","displayName":"Fixture","supportedGenerationMethods":["generateContent"]}]}`)
				return
			}
			io.WriteString(w, `{"data":[{"id":"fixture-model","object":"model","type":"model","display_name":"Fixture","created_at":"2026-01-01T00:00:00Z"}],"has_more":false,"first_id":"fixture-model","last_id":"fixture-model"}`)
		case "POST /v1/messages":
			io.WriteString(w, `{"id":"msg-inspector","type":"message","role":"assistant","model":"fixture-model","content":[{"type":"text","text":"fixture answer"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":3,"output_tokens":2}}`)
		case "POST /v1/responses":
			io.WriteString(w, `{"id":"resp_inspector","object":"response","created_at":1,"status":"completed","model":"fixture-model","output":[{"id":"msg_inspector","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"fixture answer","annotations":[]}]}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}`)
		case "POST /v1/chat/completions":
			io.WriteString(w, `{"id":"chat-inspector","object":"chat.completion","created":1,"model":"fixture-model","choices":[{"index":0,"message":{"role":"assistant","content":"fixture answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)
		case "POST /v1/models/fixture-model:generateContent":
			io.WriteString(w, `{"modelVersion":"fixture-model","candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"fixture answer"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5}}`)
		default:
			t.Errorf("unexpected fixture provider path: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(up.Close)
	created := h.want(owner, "POST", "/api/v1/providers", map[string]any{
		"name": "Inspector " + profile, "model": vendorModel, "credential": vendorSecret,
		"configuration": map[string]any{"kind": kind, "auth_mode": "api_key", "endpoint": up.URL + "/v1", "profile_id": profile, "profile_revision": "1", "options": options},
	}, idem("create-"+profile), 201)
	providerID := created["id"].(string)
	certifyProfileNetworkProvider(t, h, owner, providerID)
	return providerID, calls
}

func TestStrictPlanInspectorAcceptsNativeQueryWithoutInventingBodyFields(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	providerID, calls := inspectorProvider(t, h, owner, "gemini", "gemini-generation", map[string]any{})
	path := "/api/v1/providers/" + providerID
	models := h.want(owner, "GET", path+"/models", nil, nil, 200)["items"].([]any)
	modelID := models[0].(map[string]any)["id"].(string)
	detail := h.want(owner, "GET", path, nil, nil, 200)
	detail = h.want(owner, "PATCH", path+"/models/"+modelID, map[string]any{"enabled": true, "capabilities": []any{map[string]any{"operation": "generation", "surface": "gemini", "mode": "unary"}}}, etagHeader(detail), 200)
	certified := h.want(owner, "POST", path+"/models/"+modelID+"/certify", nil, etagHeader(detail), 200)
	if certified["status"] != "certified" {
		t.Fatal("native Gemini fixture certification failed")
	}
	detail = h.want(owner, "GET", path, nil, nil, 200)
	h.want(owner, "POST", path+"/activate", nil, withMatch(detail, idem("activate-native-gemini-inspector")), 200)
	draft := inspectorDraft(t, h, owner, providerID, "query-inspection", nil)
	before := calls.Load()
	h.want(owner, "POST", "/api/v1/route-drafts/"+draft["id"].(string)+"/activate", nil, withMatch(draft, idem("activate-query-inspection")), 200)
	request := map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "private-query-input"}}}}}
	var draftDecision map[string]any
	for _, published := range []bool{false, true} {
		input := map[string]any{"operation": "generation", "surface": "gemini", "mode": "unary", "seed": "query", "dialect": "gemini-generate-content", "request": request, "query_settings": map[string]any{"$xgafv": "2"}}
		var decision map[string]any
		if published {
			delete(input, "request")
			input["operation"] = map[string]any{"operation": "generation", "route": "query-inspection", "request": request}
			decision = h.list(owner, "POST", "/api/v1/routing/simulate", input, nil, 200)[0].(map[string]any)
			if !reflect.DeepEqual(draftDecision, decision) {
				t.Fatalf("native query inspection differs: draft=%v published=%v", draftDecision, decision)
			}
		} else {
			response := h.want(owner, "POST", "/api/v1/route-drafts/"+draft["id"].(string)+"/simulate", input, nil, 200)
			decision = response["targets"].([]any)[0].(map[string]any)["decision"].(map[string]any)
			draftDecision = decision
		}
		inspection := decision["interaction"].(map[string]any)
		if decision["eligible"] != true || inspection["status"] != "admitted" || inspectorFields(t, decision)["/model"] != nil {
			t.Fatalf("native URL-bound request was changed by inspection: %v", decision)
		}
		context := inspection["semantic_context"].([]any)
		if len(context) != 1 || context[0].(map[string]any)["field"] != "/query/$xgafv" || context[0].(map[string]any)["origin"] != "caller" || context[0].(map[string]any)["redacted"] != true || context[0].(map[string]any)["value_json"] != nil {
			t.Fatal("semantic query was omitted, invented or exposed")
		}
	}
	if calls.Load() != before {
		t.Fatal("native query inspection contacted the provider")
	}
}

func inspectorDraft(t *testing.T, h *accessHarness, owner *browser, providerID, slug string, policy any) map[string]any {
	t.Helper()
	body := map[string]any{
		"slug": slug, "operations": []string{"generation"}, "overall_timeout_ms": 10000, "max_attempts": 1,
		"fidelity": map[string]any{"mode": "strict"},
		"targets":  []any{map[string]any{"provider_id": providerID, "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 5000}},
	}
	if policy != nil {
		body["content_policy"] = policy
	}
	return h.want(owner, "POST", "/api/v1/route-drafts", body, idem("draft-"+slug), 201)
}

func inspectorSimulation(t *testing.T, h *accessHarness, owner *browser, draft map[string]any, published bool, request any, fields map[string]any) map[string]any {
	t.Helper()
	path := "/api/v1/route-drafts/" + draft["id"].(string) + "/simulate"
	input := map[string]any{"operation": "generation", "surface": "openai", "mode": "unary", "seed": "inspector"}
	if request != nil {
		input["request"] = request
	}
	for name, value := range fields {
		input[name] = value
	}
	if published {
		path = "/api/v1/routing/simulate"
		if request == nil {
			request = map[string]any{"route": draft["slug"]}
		}
		delete(input, "request")
		input["operation"] = map[string]any{"operation": "generation", "request": request}
		decisions := h.list(owner, "POST", path, input, nil, 200)
		if len(decisions) != 1 {
			t.Fatal("published inspector changed its decision array shape")
		}
		return decisions[0].(map[string]any)
	}
	response := h.want(owner, "POST", path, input, nil, 200)
	targets := response["targets"].([]any)
	if len(targets) != 1 {
		t.Fatal("draft inspector omitted its target")
	}
	return targets[0].(map[string]any)["decision"].(map[string]any)
}

func inspectorFields(t *testing.T, decision map[string]any) map[string]map[string]any {
	t.Helper()
	inspection := decision["interaction"].(map[string]any)
	result := map[string]map[string]any{}
	for _, raw := range inspection["effective_request"].(map[string]any)["fields"].([]any) {
		field := raw.(map[string]any)
		result[field["field"].(string)] = field
	}
	return result
}

func TestStrictPlanInspectorPreservesNativeSettingsAndRedactsContent(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	providerID, calls := inspectorProvider(t, h, owner, "openai_compatible", "compatible-chat", map[string]any{
		"semantic_headers":   map[string]any{"OpenAI-Beta": "private-header-marker"},
		"operation_defaults": map[string]any{"generation": map[string]any{"dialect": "openai-chat", "values": map[string]any{"top_p": 0.8, "temperature": 0.5}}},
	})
	draft := inspectorDraft(t, h, owner, providerID, "native-inspection", nil)
	before := calls.Load()
	h.want(owner, "POST", "/api/v1/route-drafts/"+draft["id"].(string)+"/activate", nil, withMatch(draft, idem("activate-inspector")), 200)
	request := map[string]any{
		"model": "native-inspection", "messages": []any{map[string]any{"role": "user", "content": "private-prompt-marker"}},
		"temperature": 0, "seed": json.Number("9007199254740993"), "parallel_tool_calls": false, "stop": []any{},
		"tools":               []any{map[string]any{"type": "function", "function": map[string]any{"name": "private_tool_marker", "description": "private-description-marker", "parameters": map[string]any{"type": "object", "properties": map[string]any{"private_schema_marker": map[string]any{"type": "string"}}}}}},
		"private-native-name": map[string]any{"signature": "private-signature-marker"},
	}
	for _, published := range []bool{false, true} {
		decision := inspectorSimulation(t, h, owner, draft, published, request, map[string]any{"dialect": "openai-chat", "semantic_headers": map[string]any{"OpenAI-Beta": "private-header-marker"}})
		inspection := decision["interaction"].(map[string]any)
		if decision["eligible"] != true || inspection["status"] != "admitted" || inspection["class"] != "native_identity" || inspection["fidelity"] != "strict" {
			t.Fatalf("native request was not admitted: %v", decision)
		}
		if inspection["ingress_dialect"] != "openai-chat" || inspection["egress_dialect"] != "openai-chat" || inspection["return_dialect"] != "openai-chat" || len(inspection["evidence"].([]any)) == 0 {
			t.Fatal("native inspection omitted its path or scoped evidence")
		}
		encoded, _ := json.Marshal(decision)
		for _, forbidden := range []string{"private-", "private_", vendorSecret, "BEGIN PRIVATE KEY"} {
			if strings.Contains(string(encoded), forbidden) {
				t.Fatal("inspector exposed prompt, tool/schema names, native state, header values or credentials")
			}
		}
		settings := inspectorFields(t, decision)
		for field, exact := range map[string]string{"/temperature": "0", "/seed": "9007199254740993", "/top_p": "0.8", "/parallel_tool_calls": "false"} {
			if settings[field]["value_json"] != exact {
				t.Fatalf("full management JSON response lost exact scalar %s", field)
			}
			if _, present := settings[field]["value"]; present {
				t.Fatal("inspector exposed a number that ordinary JSON clients can round")
			}
		}
		if settings["/top_p"]["origin"] != "provider_default" || settings["/temperature"]["origin"] != "caller" || settings["/stop"]["empty"] != true || settings["/messages"]["redacted"] != true {
			t.Fatal("inspector lost default provenance, caller precedence or empty presence")
		}
		if inspection["obligations"].(map[string]any)["continuation"] != "client_native_history" {
			t.Fatal("ordinary native inspection invented a state store or client helper")
		}
		obligations := inspection["obligations"].(map[string]any)
		effects := obligations["effects"].([]any)
		if obligations["submission"] != "immediate" || len(effects) != 2 || effects[0] != "inference" || effects[1] != "client_tool_call" {
			t.Fatal("native tool inspection omitted its execution dimensions")
		}
		tuple := inspectorSimulation(t, h, owner, draft, published, nil, nil)["interaction"].(map[string]any)
		if tuple["status"] != "not_inspected" || tuple["class"] != nil || len(tuple["evidence"].([]any)) != 0 {
			t.Fatal("tuple-only simulation fabricated request qualification")
		}
		conflicting := map[string]any{}
		for name, value := range request {
			conflicting[name] = value
		}
		conflicting["max_tokens"], conflicting["max_completion_tokens"] = 32, 32
		invalidInput := map[string]any{"operation": "generation", "surface": "openai", "mode": "unary", "seed": "invalid-native", "request": conflicting}
		invalidPath := "/api/v1/route-drafts/" + draft["id"].(string) + "/simulate"
		if published {
			invalidInput["operation"] = map[string]any{"operation": "generation", "request": conflicting}
			delete(invalidInput, "request")
			invalidPath = "/api/v1/routing/simulate"
		}
		h.want(owner, "POST", invalidPath, invalidInput, nil, 422)
	}
	for _, headers := range []map[string]any{
		{"OpenAI-Beta": "bad\x01value"}, {"OpenAI-Beta": "bad\x7fvalue"},
		{"Authorization": "private-credential-marker"}, {"OpenAI-Beta": "", "openai-beta": "duplicate"},
	} {
		for _, path := range []string{"/api/v1/route-drafts/" + draft["id"].(string) + "/simulate", "/api/v1/routing/simulate"} {
			input := map[string]any{"operation": "generation", "surface": "openai", "mode": "unary", "seed": "headers", "request": request, "semantic_headers": headers}
			if path == "/api/v1/routing/simulate" {
				input["operation"] = map[string]any{"operation": "generation", "request": request}
				delete(input, "request")
			}
			h.want(owner, "POST", path, input, nil, 422)
		}
	}
	if calls.Load() != before {
		t.Fatal("strict activation or no-inference inspection dispatched to the provider")
	}
}
