//go:build integration

package integration_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
		} else if r.Header.Get("Authorization") != "Bearer "+vendorSecret {
			t.Error("inspector fixture provider authentication differs")
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/models":
			io.WriteString(w, `{"data":[{"id":"fixture-model","object":"model","type":"model","display_name":"Fixture","created_at":"2026-01-01T00:00:00Z"}],"has_more":false,"first_id":"fixture-model","last_id":"fixture-model"}`)
		case "POST /v1/messages":
			io.WriteString(w, `{"id":"msg-inspector","type":"message","role":"assistant","model":"fixture-model","content":[{"type":"text","text":"fixture answer"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":3,"output_tokens":2}}`)
		case "POST /v1/responses":
			io.WriteString(w, `{"id":"resp_inspector","object":"response","created_at":1,"status":"completed","model":"fixture-model","output":[{"id":"msg_inspector","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"fixture answer","annotations":[]}]}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}`)
		case "POST /v1/chat/completions":
			io.WriteString(w, `{"id":"chat-inspector","object":"chat.completion","created":1,"model":"fixture-model","choices":[{"index":0,"message":{"role":"assistant","content":"fixture answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)
		default:
			t.Errorf("unexpected fixture provider path: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(up.Close)
	created := h.want(owner, "POST", "/api/v3/providers", map[string]any{
		"name": "Inspector " + profile, "model": vendorModel, "credential": vendorSecret,
		"configuration": map[string]any{"kind": kind, "auth_mode": "api_key", "endpoint": up.URL + "/v1", "profile_id": profile, "profile_revision": "1", "options": options},
	}, idem("create-"+profile), 201)
	providerID := created["id"].(string)
	certifyProfileNetworkProvider(t, h, owner, providerID)
	return providerID, calls
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
	return h.want(owner, "POST", "/api/v3/route-drafts", body, idem("draft-"+slug), 201)
}

func inspectorSimulation(t *testing.T, h *accessHarness, owner *browser, draft map[string]any, published bool, request any, fields map[string]any) map[string]any {
	t.Helper()
	path := "/api/v3/route-drafts/" + draft["id"].(string) + "/simulate"
	input := map[string]any{"operation": "generation", "surface": "openai", "mode": "unary", "seed": "inspector"}
	if request != nil {
		input["request"] = request
	}
	for name, value := range fields {
		input[name] = value
	}
	if published {
		path = "/api/v3/routing/simulate"
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

func assertInspectorRejection(t *testing.T, decision map[string]any, code string) {
	t.Helper()
	if decision["eligible"] != false || decision["attempt"] != nil || decision["reason"] != code {
		t.Fatalf("inspection did not reject %s precisely: %v", code, decision)
	}
	diagnostic, ok := decision["incompatibility"].(map[string]any)
	if !ok || diagnostic["code"] != code || diagnostic["requirement"] == "" || diagnostic["message"] == "" {
		t.Fatal("inspection omitted its safe incompatibility contract")
	}
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
	h.want(owner, "POST", "/api/v3/route-drafts/"+draft["id"].(string)+"/activate", nil, withMatch(draft, idem("activate-inspector")), 200)
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
		tuple := inspectorSimulation(t, h, owner, draft, published, nil, nil)["interaction"].(map[string]any)
		if tuple["status"] != "not_inspected" || tuple["class"] != nil || len(tuple["evidence"].([]any)) != 0 {
			t.Fatal("tuple-only simulation fabricated request qualification")
		}
	}
	for _, value := range []string{"bad\x01value", "bad\x7fvalue"} {
		for _, path := range []string{"/api/v3/route-drafts/" + draft["id"].(string) + "/simulate", "/api/v3/routing/simulate"} {
			input := map[string]any{"operation": "generation", "surface": "openai", "mode": "unary", "seed": "headers", "request": request, "semantic_headers": map[string]any{"OpenAI-Beta": value}}
			if path == "/api/v3/routing/simulate" {
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

func TestStrictPlanInspectorQualifiesTextAndRejectsSemanticLoss(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	providerID, calls := inspectorProvider(t, h, owner, "anthropic", "anthropic-messages", map[string]any{})
	draft := inspectorDraft(t, h, owner, providerID, "translated-inspection", nil)
	before := calls.Load()
	h.want(owner, "POST", "/api/v3/route-drafts/"+draft["id"].(string)+"/activate", nil, withMatch(draft, idem("activate-text-inspection")), 200)
	for _, published := range []bool{false, true} {
		request := map[string]any{"model": "translated-inspection", "max_tokens": 64, "messages": []any{map[string]any{"role": "user", "content": "private-user-marker"}}}
		decision := inspectorSimulation(t, h, owner, draft, published, request, nil)
		inspection := decision["interaction"].(map[string]any)
		if decision["eligible"] != true || inspection["class"] != "qualified_interaction" || inspection["status"] != "admitted" || inspection["egress_dialect"] != "anthropic-messages" || inspection["return_dialect"] != "openai-chat" {
			t.Fatalf("qualified text interaction missing: %v", decision)
		}
		for _, test := range []struct {
			code  string
			field string
			value any
		}{
			{"instruction_scope", "messages", []any{map[string]any{"role": "system", "content": "private-system-marker"}, map[string]any{"role": "developer", "content": "private-developer-marker"}, map[string]any{"role": "user", "content": "private-user-marker"}}},
			{"reasoning_budget", "reasoning_effort", "high"},
			{"state_carrier", "tools", []any{map[string]any{"type": "function", "function": map[string]any{"name": "private_tool_marker", "parameters": map[string]any{"type": "object"}}}}},
			{"target_capability", "private-native-name", true},
		} {
			input := map[string]any{"model": "translated-inspection", "max_tokens": 64, "messages": request["messages"]}
			input[test.field] = test.value
			rejected := inspectorSimulation(t, h, owner, draft, published, input, nil)
			assertInspectorRejection(t, rejected, test.code)
			encoded, _ := json.Marshal(rejected)
			if strings.Contains(string(encoded), "private-") || strings.Contains(string(encoded), "private_") {
				t.Fatal("incompatibility diagnostics exposed rejected native data")
			}
		}
	}
	if calls.Load() != before {
		t.Fatal("qualified or incompatible plan inspection performed inference")
	}
}

func TestStrictPlanInspectorChecksProviderStatePolicyAndCurrentRevocation(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	providerID, calls := inspectorProvider(t, h, owner, "openai_compatible", "compatible-responses", map[string]any{})
	draft := inspectorDraft(t, h, owner, providerID, "state-inspection", nil)
	before := calls.Load()
	h.want(owner, "POST", "/api/v3/route-drafts/"+draft["id"].(string)+"/activate", nil, withMatch(draft, idem("activate-state-inspection")), 200)
	request := map[string]any{"model": "state-inspection", "input": "private-state-input-marker"}
	for _, allowed := range []bool{false, true} {
		key := h.want(owner, "POST", "/api/v3/api-keys", map[string]any{"name": "state inspector", "scopes": []string{"inference"}, "allowed_routes": []string{"state-inspection"}, "allow_provider_state": allowed}, idem(map[bool]string{false: "deny-state", true: "allow-state"}[allowed]), 201)
		for _, published := range []bool{false, true} {
			decision := inspectorSimulation(t, h, owner, draft, published, request, map[string]any{"dialect": "openai-responses", "api_key_id": key["id"]})
			if !allowed {
				assertInspectorRejection(t, decision, "policy_conflict")
			} else if decision["eligible"] != true || decision["interaction"].(map[string]any)["status"] != "admitted" {
				t.Fatal("authorized native provider state was not admitted")
			} else if inspectorFields(t, decision)["/store"] != nil {
				t.Fatal("inspector silently changed omitted native store semantics")
			}
		}
	}
	request["store"] = false
	if decision := inspectorSimulation(t, h, owner, draft, true, request, map[string]any{"dialect": "openai-responses"}); decision["eligible"] != true || inspectorFields(t, decision)["/store"]["value_json"] != "false" {
		t.Fatal("explicitly stateless native request was not inspected faithfully")
	}
	path := "/api/v3/providers/" + providerID
	credentials := h.want(owner, "GET", path+"/credentials", nil, nil, 200)["items"].([]any)
	detail := h.want(owner, "GET", path, nil, nil, 200)
	h.want(owner, "POST", path+"/credentials/"+credentials[0].(map[string]any)["id"].(string)+"/revoke", nil, withMatch(detail, idem("revoke-inspected-api")), 200)
	for _, published := range []bool{false, true} {
		decision := inspectorSimulation(t, h, owner, draft, published, request, map[string]any{"dialect": "openai-responses"})
		if decision["eligible"] != false || decision["reason"] != "no_eligible_credentials" || decision["interaction"].(map[string]any)["status"] != "not_evaluated" {
			t.Fatal("inspection ignored current API credential revocation")
		}
	}
	if calls.Load() != before {
		t.Fatal("provider-state or credential authority inspection made an upstream call")
	}
}

func TestStrictPlanInspectorChecksNetworkRevocationWithoutDialing(t *testing.T) {
	f := newProfileNetworkFixture(t, true)
	h := newAccessHarness(t)
	owner := h.owner()
	config := profileNetworkConfiguration(f)
	created := createProfileNetworkProvider(t, h, owner, "Inspector mTLS", config)
	providerID := created["id"].(string)
	path := "/api/v3/providers/" + providerID
	stored := h.want(owner, "POST", path+"/network-credentials", map[string]any{"credential": f.credential}, withMatch(created, idem("network-inspector")), 201)
	networkID := stored["credential_id"].(string)
	config["options"].(map[string]any)["network"].(map[string]any)["credential_id"] = networkID
	h.want(owner, "PATCH", path, map[string]any{"name": "Inspector mTLS", "configuration": config}, etagHeader(stored), 200)
	certifyProfileNetworkProvider(t, h, owner, providerID)
	draft := inspectorDraft(t, h, owner, providerID, "network-inspection", nil)
	before := len(f.captured())
	h.want(owner, "POST", "/api/v3/route-drafts/"+draft["id"].(string)+"/activate", nil, withMatch(draft, idem("activate-network-inspection")), 200)
	request := map[string]any{"model": "network-inspection", "messages": []any{map[string]any{"role": "user", "content": "private-network-input"}}}
	if decision := inspectorSimulation(t, h, owner, draft, true, request, nil); decision["eligible"] != true {
		t.Fatal("unrevoked network identity was not eligible")
	}
	detail := h.want(owner, "GET", path, nil, nil, 200)
	h.want(owner, "POST", path+"/network-credentials/"+networkID+"/revoke", nil, withMatch(detail, idem("revoke-inspected-network")), 200)
	for _, published := range []bool{false, true} {
		decision := inspectorSimulation(t, h, owner, draft, published, request, nil)
		if decision["eligible"] != false || decision["reason"] != "network_credential_revoked" || decision["interaction"].(map[string]any)["status"] != "not_evaluated" {
			t.Fatal("inspection ignored current network credential revocation")
		}
	}
	if len(f.captured()) != before {
		t.Fatal("network inspection dialed the configured TLS provider")
	}
}

func TestStrictPlanInspectorChecksDefaultedToolContentPolicy(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	providerID, calls := inspectorProvider(t, h, owner, "openai_compatible", "compatible-chat", map[string]any{
		"operation_defaults": map[string]any{"generation": map[string]any{"dialect": "openai-chat", "values": map[string]any{
			"tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "lookup", "description": "blocked-default-marker", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}}},
		}}},
	})
	draft := inspectorDraft(t, h, owner, providerID, "policy-inspection", map[string]any{"rules": []any{map[string]any{"id": "block-default", "phase": "input", "action": "block", "pattern": "blocked-default-marker"}}})
	before := calls.Load()
	h.want(owner, "POST", "/api/v3/route-drafts/"+draft["id"].(string)+"/activate", nil, withMatch(draft, idem("activate-policy-inspection")), 200)
	request := map[string]any{"model": "policy-inspection", "messages": []any{map[string]any{"role": "user", "content": "innocent input"}}}
	for _, published := range []bool{false, true} {
		decision := inspectorSimulation(t, h, owner, draft, published, request, nil)
		assertInspectorRejection(t, decision, "content_policy_blocked")
		if decision["interaction"].(map[string]any)["status"] != "blocked" {
			t.Fatal("local input policy outcome was mislabeled as an admitted interaction")
		}
		encoded, _ := json.Marshal(decision)
		if strings.Contains(string(encoded), "blocked-default-marker") {
			t.Fatal("policy inspection exposed a defaulted tool description")
		}
	}
	if calls.Load() != before {
		t.Fatal("effective-input policy inspection performed inference")
	}
}
