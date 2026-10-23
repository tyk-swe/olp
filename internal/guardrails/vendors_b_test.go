package guardrails

import (
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// Fixtures encode the named vendors' native API fields, rather than the OLP
// webhook envelope. Every profile must reject a missing decision.
func TestNamedVendorBContracts(t *testing.T) {
	const text = "sensitive value"
	const messages = `[{"role":"user","content":"sensitive value"}]`
	const responseMessages = `[{"role":"assistant","content":"sensitive value"}]`
	cases := []struct{ kind, header, auth, parameters, input, output, allow, block string }{
		{"panw_prisma_airs", "x-pan-token", "opaque-api-key", `{}`, `{"contents":[{"prompt":"sensitive value"}],"metadata":{"is_response":false}}`, `{"contents":[{"response":"sensitive value"}],"metadata":{"is_response":true}}`, `{"action":"allow"}`, `{"action":"block"}`},
		{"cisco_ai_defense", "X-Cisco-AI-Defense-API-Key", "opaque-api-key", `{}`, `{"messages":` + messages + `,"metadata":{},"config":{}}`, `{"messages":` + responseMessages + `,"metadata":{},"config":{}}`, `{"is_safe":true,"action":"allow"}`, `{"is_safe":false,"action":"block"}`},
		{"noma", "Authorization", "Bearer opaque-api-key", `{}`, `{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"sensitive value"}]}],"x-noma-context":{"applicationId":""}}`, `{"input":[{"type":"message","role":"assistant","content":[{"type":"input_text","text":"sensitive value"}]}],"x-noma-context":{"applicationId":""}}`, `{"aggregatedScanResult":false}`, `{"aggregatedScanResult":true}`},
		{"noma_v2", "Authorization", "Bearer opaque-api-key", `{}`, `{"inputs":{"texts":["sensitive value"]},"request_data":{},"input_type":"request","monitor_mode":false}`, `{"inputs":{"texts":["sensitive value"]},"request_data":{},"input_type":"response","monitor_mode":false}`, `{"action":"NONE"}`, `{"action":"BLOCKED"}`},
		{"zscaler_ai_guard", "Authorization", "Bearer opaque-api-key", `{}`, `{"direction":"IN","content":"sensitive value"}`, `{"direction":"OUT","content":"sensitive value"}`, `{"statusCode":200,"action":"ALLOW"}`, `{"statusCode":200,"action":"BLOCK"}`},
		{"javelin", "x-javelin-apikey", "opaque-api-key", `{}`, `{"input":{"text":"sensitive value"},"metadata":{},"config":{}}`, "", `{"assessments":[{"trustsafety":{"request_reject":false}}]}`, `{"assessments":[{"trustsafety":{"request_reject":true}}]}`},
		{"enkryptai", "apikey", "opaque-api-key", `{}`, `{"text":"sensitive value"}`, `{"text":"sensitive value"}`, `{"summary":{"toxicity":[],"policy_violation":0}}`, `{"summary":{"toxicity":[],"policy_violation":1}}`},
		{"ibm_guardrails", "Authorization", "Bearer opaque-api-key", `{"detector_id":"pii"}`, `{"contents":["sensitive value"],"detector_params":{}}`, `{"contents":["sensitive value"],"detector_params":{}}`, `[[]]`, `[[{"detection_type":"pii","detection":"email","score":0.9}]]`},
		{"onyx", "", "", `{}`, `{"payload":{"messages":` + messages + `},"input_type":"request","conversation_id":"segment"}`, `{"payload":{"object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"sensitive value"},"finish_reason":"stop"}]},"input_type":"response","conversation_id":"segment"}`, `{"allowed":true}`, `{"allowed":false}`},
		{"promptguard", "X-API-Key", "opaque-api-key", `{}`, `{"messages":` + messages + `,"direction":"input"}`, `{"messages":` + responseMessages + `,"direction":"output"}`, `{"decision":"allow"}`, `{"decision":"block"}`},
		{"xecguard", "Authorization", "Bearer opaque-api-key", `{}`, `{"model":"xecguard_v2","scan_type":"input","messages":` + messages + `,"policy_names":["Default_Policy_SystemPromptEnforcement","Default_Policy_HarmfulContentProtection","Default_Policy_GeneralPromptAttackProtection"]}`, `{"model":"xecguard_v2","scan_type":"response","messages":` + responseMessages + `,"policy_names":["Default_Policy_SystemPromptEnforcement","Default_Policy_HarmfulContentProtection","Default_Policy_GeneralPromptAttackProtection"]}`, `{"decision":"SAFE"}`, `{"decision":"UNSAFE"}`},
		{"prompt_security", "APP-ID", "opaque-api-key", `{}`, `{"messages":` + messages + `,"user":"","system_prompt":""}`, `{"response":"sensitive value","user":"","system_prompt":""}`, `{"result":{"prompt":{"action":"allow"},"response":{"action":"allow"}}}`, `{"result":{"prompt":{"action":"block"},"response":{"action":"block"}}}`},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			for _, phase := range []string{Input, Output} {
				if phase == Output && tc.output == "" {
					continue
				}
				t.Run(phase, func(t *testing.T) {
					for _, decision := range []struct {
						name, response, want string
						status               int
					}{{"allow", tc.allow, "pass", 200}, {"block", tc.block, Block, 200}, {"malformed", `{}`, "", 200}, {"unavailable", tc.allow, "", 503}} {
						t.Run(decision.name, func(t *testing.T) {
							runner, endpoint := fakeRunner(t, func(w http.ResponseWriter, r *http.Request) {
								if r.Method != http.MethodPost {
									t.Errorf("method = %s", r.Method)
								}
								if tc.header != "" && r.Header.Get(tc.header) != tc.auth {
									t.Errorf("missing native authentication header %s", tc.header)
								}
								if tc.kind == "onyx" && r.URL.Path != "/guard/evaluate/v1/opaque-api-key/litellm" {
									t.Errorf("native credential path = %s", r.URL.Path)
								}
								if tc.kind == "ibm_guardrails" && r.Header.Get("detector-id") != "pii" {
									t.Error("missing detector-id")
								}
								got, err := io.ReadAll(r.Body)
								if err != nil {
									t.Fatal(err)
								}
								want := tc.input
								if phase == Output {
									want = tc.output
								}
								var gotJSON, wantJSON any
								if json.Unmarshal(got, &gotJSON) != nil || json.Unmarshal([]byte(want), &wantJSON) != nil || !reflect.DeepEqual(gotJSON, wantJSON) {
									t.Errorf("native payload: got %s; want %s", got, want)
								}
								w.WriteHeader(decision.status)
								io.WriteString(w, decision.response)
							})
							config, _ := json.Marshal(ExternalConfig{Endpoint: endpoint, ContentEgress: true, CredentialRef: "secret", Parameters: json.RawMessage(tc.parameters)})
							verdict, err := runner.Evaluate(t.Context(), Revision{ID: "revision", GuardrailID: "guard", Type: tc.kind, Configuration: config, TimeoutMS: 1000, OnError: FailClosed}, Evaluation{Phase: phase, Segments: []Segment{{ID: "segment", Text: text}}})
							if decision.want == "" {
								if err == nil {
									t.Fatalf("invalid upstream response accepted: %+v", verdict)
								}
								return
							}
							if err != nil || verdict.Action != decision.want {
								t.Fatalf("verdict = %+v, error = %v; want %s", verdict, err, decision.want)
							}
						})
					}
				})
			}
		})
	}
}

func TestNamedVendorBReplacementContracts(t *testing.T) {
	for _, tc := range []struct{ kind, input, output string }{
		{"panw_prisma_airs", `{"action":"allow","prompt_masked_data":{"data":"[redacted]"}}`, `{"action":"allow","response_masked_data":{"data":"[redacted]"}}`},
		{"cisco_ai_defense", `{"action":"redact","sanitized_text":"[redacted]"}`, `{"action":"redact","sanitized_text":"[redacted]"}`},
		{"noma", `{"aggregatedScanResult":false,"scanResult":[{"role":"user","results":{"anonymizedContent":{"anonymized":"[redacted]"}}}]}`, `{"aggregatedScanResult":false,"scanResult":[{"role":"assistant","results":{"anonymizedContent":{"anonymized":"[redacted]"}}}]}`},
		{"noma_v2", `{"action":"GUARDRAIL_INTERVENED","texts":["[redacted]"]}`, `{"action":"GUARDRAIL_INTERVENED","texts":["[redacted]"]}`},
		{"promptguard", `{"decision":"redact","redacted_messages":[{"role":"user","content":"[redacted]"}]}`, `{"decision":"redact","redacted_messages":[{"role":"assistant","content":"[redacted]"}]}`},
		{"prompt_security", `{"result":{"prompt":{"action":"modify","modified_messages":[{"role":"user","content":"[redacted]"}]}}}`, `{"result":{"response":{"action":"modify","modified_text":"[redacted]"}}}`},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			for _, phase := range []string{Input, Output} {
				t.Run(phase, func(t *testing.T) {
					runner, endpoint := fakeRunner(t, func(w http.ResponseWriter, r *http.Request) {
						response := tc.input
						if phase == Output {
							response = tc.output
						}
						io.WriteString(w, response)
					})
					config, _ := json.Marshal(ExternalConfig{Endpoint: endpoint, ContentEgress: true, CredentialRef: "secret"})
					verdict, err := runner.Evaluate(t.Context(), Revision{ID: "revision", GuardrailID: "guard", Type: tc.kind, Configuration: config, TimeoutMS: 1000, OnError: FailClosed}, Evaluation{Phase: phase, Segments: []Segment{{ID: "segment", Text: "pii é"}}})
					if err != nil || len(verdict.Redactions) != 1 {
						t.Fatalf("replacement = %+v, error = %v", verdict, err)
					}
					redaction := verdict.Redactions[0]
					if redaction.Segment != "segment" || redaction.Start != 0 || redaction.End != len("pii é") || redaction.Replacement != "[redacted]" {
						t.Fatalf("replacement escaped its source segment: %+v", redaction)
					}
				})
			}
		})
	}
}

func TestNamedVendorBInvalidDecisions(t *testing.T) {
	for _, tc := range []struct{ kind, parameters, response string }{
		{"cisco_ai_defense", `{}`, `{"action":"mystery","is_safe":true}`},
		{"noma", `{}`, `{"aggregatedScanResult":"false"}`},
		{"noma_v2", `{}`, `{"action":"GUARDRAIL_INTERVENED","texts":[]}`},
		{"noma_v2", `{}`, `{"action":"GUARDRAIL_INTERVENED","texts":[null]}`},
		{"zscaler_ai_guard", `{}`, `{"statusCode":500,"action":"ALLOW"}`},
		{"javelin", `{}`, `{"assessments":[{"trustsafety":{}}]}`},
		{"enkryptai", `{}`, `{"summary":{"policy_violation":2}}`},
		{"ibm_guardrails", `{"detector_id":"pii"}`, `[[{"score":0.99}]]`},
		{"promptguard", `{}`, `{"decision":"redact","redacted_messages":[{"role":"assistant","content":"safe"}]}`},
		{"xecguard", `{}`, `{"decision":"UNKNOWN"}`},
		{"prompt_security", `{}`, `{"result":{"prompt":{"action":"modify","modified_messages":[]}}}`},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			runner, endpoint := fakeRunner(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, tc.response) })
			config, _ := json.Marshal(ExternalConfig{Endpoint: endpoint, ContentEgress: true, CredentialRef: "secret", Parameters: json.RawMessage(tc.parameters)})
			_, err := runner.Evaluate(t.Context(), Revision{ID: "revision", GuardrailID: "guard", Type: tc.kind, Configuration: config, TimeoutMS: 1000, OnError: FailClosed}, Evaluation{Phase: Input, Segments: []Segment{{ID: "segment", Text: "private"}}})
			if err == nil {
				t.Fatal("malformed native decision accepted")
			}
			if strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "secret") {
				t.Fatal("upstream error leaked content or credential")
			}
		})
	}
}

func TestNamedVendorBParameters(t *testing.T) {
	for kind, vendor := range namedVendorsB {
		if err := vendor.validate(json.RawMessage(`{"api_key":"plaintext"}`)); err == nil {
			t.Errorf("%s accepted plaintext or unknown parameter", kind)
		}
	}
	for _, tc := range []struct{ kind, parameters string }{
		{"ibm_guardrails", `{}`}, {"ibm_guardrails", `{"detector_id":"pii","score_threshold":2}`}, {"ibm_guardrails", `{"detector_id":"pii","detector_params":[]}`},
		{"cisco_ai_defense", `{"enabled_rules":[{}]}`}, {"zscaler_ai_guard", `{"policy_id":-1}`}, {"javelin", `{"config":[]}`}, {"xecguard", `{"policy_names":[""]}`}, {"enkryptai", `{"policy_name":"policy\r\nX-Injected: true"}`},
	} {
		if err := namedVendorsB[tc.kind].validate(json.RawMessage(tc.parameters)); err == nil {
			t.Errorf("%s accepted invalid parameters %s", tc.kind, tc.parameters)
		}
	}
}

func TestNamedVendorBNomaSensitiveReplacement(t *testing.T) {
	for _, tc := range []struct {
		name, response string
		replacement    bool
	}{
		{"sensitive_only", `{"aggregatedScanResult":true,"scanResult":[{"role":"user","results":{"anonymizedContent":{"anonymized":"safe"},"sensitiveData":{"pii":{"result":true}},"promptInjection":{"result":false}}}]}`, true},
		{"another_detector", `{"aggregatedScanResult":true,"scanResult":[{"role":"user","results":{"anonymizedContent":{"anonymized":"safe"},"sensitiveData":{"pii":{"result":true}},"promptInjection":{"result":true}}}]}`, false},
		{"another_message", `{"aggregatedScanResult":true,"scanResult":[{"role":"user","results":{"anonymizedContent":{"anonymized":"safe"},"sensitiveData":{"pii":{"result":true}}}},{"role":"assistant","results":{"promptInjection":{"result":true}}}]}`, false},
		{"duplicate_message", `{"aggregatedScanResult":true,"scanResult":[{"role":"user","results":{"anonymizedContent":{"anonymized":"safe"},"sensitiveData":{"pii":{"result":true}}}},{"role":"user","results":{"promptInjection":{"result":true}}}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner, endpoint := fakeRunner(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, tc.response) })
			config, _ := json.Marshal(ExternalConfig{Endpoint: endpoint, ContentEgress: true, CredentialRef: "secret"})
			verdict, err := runner.Evaluate(t.Context(), Revision{ID: "revision", GuardrailID: "guard", Type: "noma", Configuration: config, TimeoutMS: 1000, OnError: FailClosed}, Evaluation{Phase: Input, Segments: []Segment{{ID: "segment", Text: "private"}}})
			if err != nil || verdict.Action != Block || (len(verdict.Redactions) > 0) != tc.replacement {
				t.Fatalf("replacement escaped sensitive-only policy: %+v, %v", verdict, err)
			}
		})
	}
}

func TestNamedVendorBIBMOrchestrator(t *testing.T) {
	runner, endpoint := fakeRunner(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil || r.Header.Get("Authorization") != "Bearer opaque-api-key" || r.Header.Get("detector-id") != "" {
			t.Error("incorrect orchestrator authentication")
		}
		if body["content"] != "private" || !reflect.DeepEqual(body["detectors"], map[string]any{"pii": map[string]any{"language": "en"}}) {
			t.Errorf("incorrect orchestrator request: %+v", body)
		}
		io.WriteString(w, `{"detections":[{"detection_type":"pii","detection":"email","score":0.2}]}`)
	})
	config, _ := json.Marshal(ExternalConfig{Endpoint: endpoint, ContentEgress: true, CredentialRef: "secret", Parameters: json.RawMessage(`{"detector_id":"pii","orchestrator":true,"score_threshold":0.5,"detector_params":{"language":"en"}}`)})
	verdict, err := runner.Evaluate(t.Context(), Revision{ID: "revision", GuardrailID: "guard", Type: "ibm_guardrails", Configuration: config, TimeoutMS: 1000, OnError: FailClosed}, Evaluation{Phase: Input, Segments: []Segment{{ID: "segment", Text: "private"}}})
	if err != nil || verdict.Action != "pass" {
		t.Fatalf("threshold ignored: %+v, %v", verdict, err)
	}
}
