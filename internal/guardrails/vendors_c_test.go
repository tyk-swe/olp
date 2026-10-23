package guardrails

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPurviewRestrictionDecisions(t *testing.T) {
	for _, tc := range []struct {
		name, actions      string
		blocked, malformed bool
	}{
		{"block", `[{"@odata.type":"#microsoft.graph.restrictAccessAction","restrictionAction":"block"}]`, true, false},
		{"warn", `[{"@odata.type":"#microsoft.graph.restrictAccessAction","restrictionAction":"warn"}]`, false, false},
		{"audit", `[{"action":"restrictAccess","restrictionAction":"audit"}]`, false, false},
		{"missing", `[{"@odata.type":"#microsoft.graph.restrictAccessAction"}]`, false, true},
		{"null", `[{"action":"restrictAccess","restrictionAction":null}]`, false, true},
		{"unknown", `[{"action":"restrictAccess","restrictionAction":"unknownFutureValue"}]`, false, true},
		{"malformed after block", `[{"action":"restrictAccess","restrictionAction":"block"},{"action":"restrictAccess"}]`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, `{"policyActions":`+tc.actions+`}`)
			}))
			defer server.Close()
			verdict, err := evaluatePurview(t.Context(), server.Client(), ExternalConfig{
				Endpoint: server.URL, Parameters: json.RawMessage(`{"application_id":"app","application_name":"OLP"}`),
			}, externalCredential{APIKey: "token"}, Input, Segment{ID: "s", Text: "private"})
			if (err != nil) != tc.malformed || err == nil && (verdict.Action == Block) != tc.blocked {
				t.Fatalf("verdict=%+v error=%v", verdict, err)
			}
		})
	}
}

func TestVendorCContracts(t *testing.T) {
	const sample = "inspect café"
	tests := []struct {
		kind, parameters, header, auth, allow, detect, invalid string
		transforms                                             bool
	}{
		{"qualifire", `{"prompt_injections":true}`, "X-Qualifire-API-Key", "token", `{"evaluationResults":[{"results":[{"flagged":false}]}]}`, `{"evaluationResults":[{"results":[{"flagged":true}]}]}`, `{"evaluationResults":[{"results":[{}]}]}`, false},
		{"ovalix", `{"application_id":"app","checkpoint_id":"checkpoint","actor":"caller"}`, "Authorization", "Bearer token", `{"action_type":"allow"}`, `{"action_type":"block"}`, `{}`, false},
		{"microsoft_purview", `{"application_id":"app","application_name":"OLP"}`, "Authorization", "Bearer token", `{"policyActions":[]}`, `{"policyActions":[{"@odata.type":"microsoft.graph.restrictAccessAction","restrictionAction":"block"}]}`, `{"policyActions":[{}]}`, false},
		{"akto", `{"account_id":"app","vxlan_id":"network"}`, "Authorization", "token", `{"data":{"guardrailsResult":{"Allowed":true}}}`, `{"data":{"guardrailsResult":{"Allowed":false}}}`, `{"data":{"guardrailsResult":{}}}`, false},
		{"deepkeep", `{"firewall_id":"firewall"}`, "X-API-Key", "token", `{"action":"NONE"}`, `{"action":"BLOCKED"}`, `{"action":"unknown"}`, false},
		{"qostodian_nexus", `{"source":"OLP"}`, "X-API-Key", "token", `{"action":"NONE"}`, `{"action":"BLOCKED"}`, `{"action":"GUARDRAIL_INTERVENED"}`, false},
		{"rubrik", `{}`, "Authorization", "Bearer token", `{}`, `{"choices":[{"message":{"content":"refused"}}]}`, `null`, false},
		{"vigil_guard", `{}`, "Authorization", "Bearer token", `{"decision":"ALLOWED"}`, `{"decision":"BLOCKED"}`, `{"decision":"SANITIZED"}`, false},
		{"repelloai", `{"asset_id":"asset"}`, "X-API-Key", "token", `{"verdict":"passed"}`, `{"verdict":"blocked"}`, `{"verdict":"nonsense"}`, false},
		{"singulr", `{"application_id":"app","guardrail_id":"guard"}`, "X-Singulr-Gateway-Token", "token", `{"should_block":false}`, `{"should_block":true}`, `{}`, false},
		{"headroom", `{"model":"model"}`, "Authorization", "Bearer token", `{"messages":[{"role":"user","content":"inspect café"}]}`, `{"messages":[{"role":"user","content":"safe"}]}`, `{"messages":[{"role":"assistant","content":"safe"}]}`, true},
		{"compresr", `{"query":"what matters?"}`, "X-API-Key", "token", `{"data":{"compressed_context":"inspect café"}}`, `{"data":{"compressed_context":"safe"}}`, `{"data":{}}`, true},
		{"straiker", `{}`, "Authorization", "Bearer token", `{"action":"NONE"}`, `{"action":"BLOCKED"}`, `{"action":"GUARDRAIL_INTERVENED"}`, false},
		{"alice", `{}`, "af-api-key", "token", `{"verdict":"ALLOW"}`, `{"verdict":"BLOCK"}`, `{"verdict":"MASK","replacements":[{"index":2,"text":"safe"}]}`, false},
		{"conduct", `{"workspace_id":"workspace"}`, "Authorization", "Bearer token", `{"text":"ok"}`, `{"text":"BLOCKED — policy"}`, `{"text":"unrecognized verdict"}`, false},
	}
	for _, tc := range tests {
		t.Run(tc.kind, func(t *testing.T) {
			adapter := namedVendorsC[tc.kind]
			if err := adapter.validate(json.RawMessage(tc.parameters)); err != nil {
				t.Fatalf("valid config: %v", err)
			}
			for _, scenario := range []struct {
				name, response           string
				wantError, wantDetection bool
			}{
				{"allow", tc.allow, false, false}, {"detect", tc.detect, false, true}, {"malformed", tc.invalid, true, false},
			} {
				t.Run(scenario.name, func(t *testing.T) {
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.Method != http.MethodPost || r.Header.Get(tc.header) != tc.auth {
							t.Errorf("method/auth: %s %s", r.Method, r.Header.Get(tc.header))
						}
						body, err := io.ReadAll(r.Body)
						if err != nil {
							t.Error(err)
						}
						if !strings.Contains(string(body), sample) || strings.Contains(string(body), "token") {
							t.Errorf("content or credential boundary: %s", body)
						}
						var request map[string]any
						if err := json.Unmarshal(body, &request); err != nil {
							t.Error(err)
						}
						switch tc.kind {
						case "akto":
							if r.URL.Query().Get("guardrails") != "true" || r.URL.Query().Get("ingest_data") != "" || r.URL.Query().Get("akto_connector") != "litellm" {
								t.Errorf("unexpected query: %s", r.URL.RawQuery)
							}
							if _, ok := request["time"].(string); !ok {
								t.Error("Akto requires timestamp string")
							}
						case "qostodian_nexus":
							if r.Header.Get("X-Qostodian-Nexus-Identifiers-Source") != "OLP" || r.Header.Get("X-Qostodian-Nexus-Identifiers-Trace") == "" {
								t.Error("missing Qostodian identifiers")
							}
						case "singulr":
							if r.Header.Get("X-Singulr-Guardrail-Id") != "guard" || r.Header.Get("X-Singulr-Enforcement-Entity-Id") != "app" {
								t.Error("missing Singulr policy headers")
							}
						case "vigil_guard":
							if request["source"] != "user_input" {
								t.Error("wrong Vigil source")
							}
						case "straiker":
							if r.Header.Get("X-Straiker-Webhook-Format") != "litellm" {
								t.Error("missing Straiker format")
							}
						case "conduct":
							if r.Header.Get("X-Workspace-Id") != "workspace" || request["method"] != "tools/call" {
								t.Error("wrong Conduct request")
							}
							var result map[string]string
							_ = json.Unmarshal([]byte(scenario.response), &result)
							_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request["id"], "result": map[string]any{"content": []any{map[string]string{"type": "text", "text": result["text"]}}}})
							return
						}
						_, _ = io.WriteString(w, scenario.response)
					}))
					defer server.Close()
					segment := Segment{ID: "message", Role: "user", Text: sample}
					verdict, err := adapter.evaluate(context.Background(), server.Client(), ExternalConfig{Endpoint: server.URL, Parameters: json.RawMessage(tc.parameters)}, externalCredential{APIKey: "token"}, Input, segment)
					if (err != nil) != scenario.wantError {
						t.Fatalf("error=%v, want error=%v", err, scenario.wantError)
					}
					if err != nil {
						return
					}
					if err := validateVerdict(verdict, []Segment{segment}); err != nil {
						t.Fatalf("invalid verdict: %v", err)
					}
					if scenario.wantDetection != (verdict.Action != "pass") {
						t.Fatalf("action=%s, want detection=%v", verdict.Action, scenario.wantDetection)
					}
					if tc.transforms && scenario.wantDetection && (len(verdict.Redactions) != 1 || verdict.Redactions[0].Replacement != "safe") {
						t.Fatalf("replacement=%+v", verdict.Redactions)
					}
				})
			}
		})
	}
}

func TestVendorCReplacements(t *testing.T) {
	for _, tc := range []struct{ kind, parameters, response string }{
		{"ovalix", `{"application_id":"app","checkpoint_id":"checkpoint","actor":"caller"}`, `{"action_type":"anonymize","modified_data":{"content":"safe"}}`},
		{"deepkeep", `{"firewall_id":"firewall"}`, `{"action":"GUARDRAIL_INTERVENED","texts":["safe"]}`},
		{"qostodian_nexus", `{}`, `{"action":"GUARDRAIL_INTERVENED","texts":["safe"]}`},
		{"vigil_guard", `{}`, `{"decision":"SANITIZED","sanitizedText":"safe"}`},
		{"straiker", `{}`, `{"action":"GUARDRAIL_INTERVENED","texts":["safe"]}`},
		{"alice", `{}`, `{"verdict":"MASK","replacements":[{"index":0,"text":"safe"}]}`},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, tc.response) }))
			defer server.Close()
			segment := Segment{ID: "message", Text: "sensitive café"}
			verdict, err := namedVendorsC[tc.kind].evaluate(context.Background(), server.Client(), ExternalConfig{Endpoint: server.URL, Parameters: json.RawMessage(tc.parameters)}, externalCredential{APIKey: "token"}, Input, segment)
			if err != nil {
				t.Fatal(err)
			}
			if len(verdict.Redactions) != 1 || verdict.Redactions[0].End != len(segment.Text) || verdict.Redactions[0].Replacement != "safe" {
				t.Fatalf("replacement=%+v", verdict.Redactions)
			}
		})
	}
}

func TestVendorCOutputMapping(t *testing.T) {
	for _, kind := range []string{"qualifire", "microsoft_purview", "akto", "deepkeep", "qostodian_nexus", "rubrik", "vigil_guard", "repelloai", "singulr", "straiker", "alice"} {
		t.Run(kind, func(t *testing.T) {
			parameters := map[string]string{"qualifire": `{"prompt_injections":true}`, "microsoft_purview": `{"application_id":"app","application_name":"OLP"}`, "akto": `{"account_id":"account","vxlan_id":"network"}`, "deepkeep": `{"firewall_id":"firewall"}`, "repelloai": `{"asset_id":"asset"}`}[kind]
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				switch kind {
				case "qualifire":
					if body["output"] != "answer" {
						t.Error("missing output text")
					}
				case "deepkeep", "qostodian_nexus", "alice":
					if body["input_type"] != "response" {
						t.Error("wrong input type")
					}
				case "microsoft_purview":
					content := body["contentToProcess"].(map[string]any)
					if content["activityMetadata"].(map[string]any)["activity"] != "downloadText" {
						t.Error("wrong activity")
					}
				case "vigil_guard":
					if body["source"] != "model_output" {
						t.Error("wrong source")
					}
				case "repelloai":
					if body["scan_data"].(map[string]any)["response"] != "answer" {
						t.Error("wrong response")
					}
				case "singulr":
					if body["guardrail_scope"] != "response" || body["response"] == nil {
						t.Error("wrong scope")
					}
				case "straiker":
					if body["event"].(map[string]any)["type"] != "post_call" || body["response"] == nil {
						t.Error("wrong event")
					}
				case "rubrik":
					request, requestOK := body["request"].(map[string]any)
					response, responseOK := body["response"].(map[string]any)
					if !requestOK || len(request) != 0 || !responseOK || response["object"] != "chat.completion" || response["choices"] == nil {
						t.Error("wrong envelope")
					} else {
						choice := response["choices"].([]any)[0].(map[string]any)
						message := choice["message"].(map[string]any)
						if message["role"] != "assistant" || message["content"] != "answer" {
							t.Error("wrong completion message")
						}
					}
				case "akto":
					if !strings.Contains(body["responsePayload"].(string), "answer") {
						t.Error("wrong response payload")
					}
				}
				_, _ = io.WriteString(w, `{}`)
			}))
			defer server.Close()
			_, _ = namedVendorsC[kind].evaluate(context.Background(), server.Client(), ExternalConfig{Endpoint: server.URL, Parameters: json.RawMessage(parameters)}, externalCredential{APIKey: "token"}, Output, Segment{ID: "out", Role: "assistant", Text: "answer"})
		})
	}
}

func TestVendorCRejectsConfiguration(t *testing.T) {
	for kind, parameters := range map[string]string{
		"qualifire": `{}`, "ovalix": `{"application_id":"app"}`, "microsoft_purview": `{}`, "akto": `{}`, "deepkeep": `{}`, "repelloai": `{}`, "compresr": `{"query":"q","target_compression_ratio":1.5}`, "singulr": `{"guardrail_id":"bad\nheader"}`, "conduct": `{"workspace_id":"bad\rheader"}`, "qostodian_nexus": `{"source":"bad\nheader"}`,
	} {
		t.Run(kind, func(t *testing.T) {
			if err := namedVendorsC[kind].validate(json.RawMessage(parameters)); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
	for kind, adapter := range namedVendorsC {
		t.Run(kind+"/unknown_field", func(t *testing.T) {
			if adapter.validate(json.RawMessage(`{"unexpected":true}`)) == nil {
				t.Fatal("unknown field accepted")
			}
		})
	}
}

func TestVendorCConductIdentityAndFraming(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		result    map[string]any
		text      string
		wantBlock bool
		wantError bool
	}{
		{name: "unauthorized", status: http.StatusUnauthorized, wantBlock: true},
		{name: "forbidden", status: http.StatusForbidden, wantBlock: true},
		{name: "unavailable", status: http.StatusServiceUnavailable, wantError: true},
		{name: "identity_marker", result: map[string]any{"structuredContent": map[string]bool{"identity_required": true}}, wantBlock: true},
		{name: "error_result", result: map[string]any{"isError": true}, wantError: true},
		{name: "workspace_prefix_block", text: "[ws:workspace] BLOCKED — policy", wantBlock: true},
		{name: "workspace_prefix_allow", text: "[ws:workspace] ok"},
		{name: "missing_verdict", text: "", wantError: true},
		{name: "unrecognized_verdict", text: "okay to proceed", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request map[string]any
				_ = json.NewDecoder(r.Body).Decode(&request)
				if tc.status != 0 {
					w.WriteHeader(tc.status)
					return
				}
				result := tc.result
				if result == nil {
					result = map[string]any{"content": []any{map[string]string{"type": "text", "text": tc.text}}}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request["id"], "result": result})
			}))
			defer server.Close()
			verdict, err := namedVendorsC["conduct"].evaluate(context.Background(), server.Client(), ExternalConfig{Endpoint: server.URL}, externalCredential{APIKey: "token"}, Input, Segment{ID: "in", Text: "content"})
			if (err != nil) != tc.wantError {
				t.Fatalf("error=%v, want error=%v", err, tc.wantError)
			}
			if err == nil && (verdict.Action == Block) != tc.wantBlock {
				t.Fatalf("action=%s, want block=%v", verdict.Action, tc.wantBlock)
			}
		})
	}
}

func TestVendorCOvalixRejectsUnknownAndIncompleteActions(t *testing.T) {
	for _, reply := range []string{`{"action_type":"future-action"}`, `{"action_type":"anonymize"}`, `{"action_type":"anonymize","modified_data":{}}`} {
		t.Run(reply, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, reply) }))
			defer server.Close()
			_, err := namedVendorsC["ovalix"].evaluate(context.Background(), server.Client(), ExternalConfig{Endpoint: server.URL}, externalCredential{APIKey: "token"}, Input, Segment{ID: "in", Text: "content"})
			if err == nil {
				t.Fatal("malformed action passed")
			}
		})
	}
}

func TestVendorCRejectsNullReplacement(t *testing.T) {
	for _, kind := range []string{"deepkeep", "qostodian_nexus", "straiker"} {
		t.Run(kind, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, `{"action":"GUARDRAIL_INTERVENED","texts":[null]}`)
			}))
			defer server.Close()
			_, err := namedVendorsC[kind].evaluate(context.Background(), server.Client(), ExternalConfig{Endpoint: server.URL}, externalCredential{APIKey: "token"}, Input, Segment{ID: "in", Text: "content"})
			if err == nil {
				t.Fatal("null replacement accepted")
			}
		})
	}
}
