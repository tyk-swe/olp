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

func TestNamedVendorAContracts(t *testing.T) {
	for _, tc := range []struct {
		kind, auth, pass, block, key string
		parameters                   json.RawMessage
	}{
		{"aporia", "X-APORIA-API-KEY", `{"action":"passthrough"}`, `{"action":"block"}`, "validation_target", nil},
		{"dynamoai", "Authorization", `{"finalAction":"NONE"}`, `{"finalAction":"BLOCK"}`, "policyIds", json.RawMessage(`{"policy_ids":["policy"],"model_id":"model"}`)},
		{"guardrails_ai", "Authorization", `{"validationPassed":true}`, `{"validationPassed":false}`, "llmOutput", nil},
		{"lakera_v1", "Authorization", `{"results":[{"flagged":false}]}`, `{"results":[{"flagged":true}]}`, "input", nil},
		{"hiddenlayer", "Authorization", `{"evaluation":{"action":"Allow"}}`, `{"evaluation":{"action":"Block"}}`, "input", nil},
		{"aim", "Authorization", `{"required_action":{},"analysis_result":{}}`, `{"required_action":{"action_type":"block_action"}}`, "messages", nil},
		{"cato_networks", "Authorization", `{"required_action":{},"analysis_result":{}}`, `{"required_action":{"action_type":"block_action"}}`, "messages", nil},
		{"pangea", "Authorization", `{"status":"Success","result":{"blocked":false}}`, `{"status":"Success","result":{"blocked":true}}`, "input", nil},
		{"crowdstrike_aidr", "Authorization", `{"result":{"blocked":false}}`, `{"result":{"blocked":true}}`, "guard_input", nil},
		{"lasso", "lasso-api-key", `{"violations_detected":false}`, `{"violations_detected":true,"findings":{"jailbreak":[{"action":"BLOCK"}]}}`, "sessionId", nil},
		{"pillar", "Authorization", `{"flagged":false}`, `{"flagged":true}`, "messages", nil},
		{"grayswan", "grayswan-api-key", `{"violation":0.2}`, `{"violation":0.9}`, "messages", nil},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			for _, result := range []struct {
				name, response string
				want           bool
			}{{"pass", tc.pass, false}, {"block", tc.block, true}, {"malformed", `{}`, true}} {
				t.Run(result.name, func(t *testing.T) {
					called := 0
					runner, endpoint := fakeRunner(t, func(w http.ResponseWriter, r *http.Request) {
						called++
						if r.Method != http.MethodPost || r.URL.Path != "/native-operation" {
							t.Errorf("request %s %s", r.Method, r.URL.Path)
						}
						if !strings.Contains(r.Header.Get(tc.auth), "opaque-api-key") {
							t.Errorf("missing %s auth", tc.auth)
						}
						var body map[string]json.RawMessage
						if json.NewDecoder(r.Body).Decode(&body) != nil || body[tc.key] == nil {
							t.Errorf("missing native field %s in %v", tc.key, body)
						}
						encoded, _ := json.Marshal(body)
						if !strings.Contains(string(encoded), "é private") {
							t.Error("missing inspected text")
						}
						io.WriteString(w, result.response)
					})
					config, _ := json.Marshal(ExternalConfig{Endpoint: endpoint + "/native-operation", CredentialRef: "credential", ContentEgress: true, Parameters: tc.parameters})
					d := Revision{ID: "revision", GuardrailID: "guard", ProjectID: "project", Type: tc.kind, Configuration: config, TimeoutMS: 1000, OnError: FailClosed}
					if err := ValidateDefinition(d); err != nil {
						t.Fatal(err)
					}
					got, err := runner.Evaluate(t.Context(), d, Evaluation{Phase: Input, Segments: []Segment{{ID: "text", Role: "user", Text: "é private"}}})
					if result.name == "malformed" {
						if err == nil {
							t.Fatalf("accepted missing verdict: %+v", got)
						}
					} else if err != nil || (got.Action == Block) != result.want {
						t.Fatalf("verdict=%+v error=%v", got, err)
					}
					if called != 1 {
						t.Fatalf("calls=%d", called)
					}
				})
			}
		})
	}
}

func TestNamedVendorATransformations(t *testing.T) {
	for _, tc := range []struct {
		kind, response string
		parameters     json.RawMessage
	}{
		{"guardrails_ai", `{"validationPassed":true,"validatedOutput":"safe"}`, nil},
		{"hiddenlayer", `{"evaluation":{"action":"Redact"},"modified_data":{"input":{"messages":[{"content":"safe"}]}}}`, nil},
		{"aim", `{"required_action":{"action_type":"anonymize_action"},"redacted_chat":{"all_redacted_messages":[{"content":"safe"}]}}`, nil},
		{"cato_networks", `{"required_action":{"action_type":"anonymize_action"},"redacted_chat":{"all_redacted_messages":[{"content":"safe"}]}}`, nil},
		{"pangea", `{"result":{"blocked":false,"transformed":true,"output":{"messages":[{"content":"safe"}]}}}`, nil},
		{"crowdstrike_aidr", `{"result":{"blocked":false,"transformed":true,"guard_output":{"messages":[{"content":"safe"}]}}}`, nil},
		{"lasso", `{"violations_detected":true,"findings":{"pii":[{"action":"AUTO_MASKING"}]},"messages":[{"content":"safe"}]}`, json.RawMessage(`{"mask":true}`)},
		{"pillar", `{"flagged":true,"masked_session_messages":[{"content":"safe"}]}`, nil},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			runner, endpoint := fakeRunner(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, tc.response) })
			config, _ := json.Marshal(ExternalConfig{Endpoint: endpoint, CredentialRef: "credential", ContentEgress: true, Parameters: tc.parameters})
			d := Revision{ID: "revision", GuardrailID: "guard", Type: tc.kind, Configuration: config, TimeoutMS: 1000, OnError: FailClosed}
			got, err := runner.Evaluate(t.Context(), d, Evaluation{Phase: Input, Segments: []Segment{{ID: "text", Text: "é private"}}})
			if err != nil || len(got.Redactions) != 1 || got.Redactions[0].End != len("é private") || got.Redactions[0].Replacement != "safe" {
				t.Fatalf("%+v %v", got, err)
			}
		})
	}
}

func TestNamedVendorAParameters(t *testing.T) {
	for kind, parameters := range map[string]string{
		"aporia":      `{"api_key":"inline-secret"}`,
		"dynamoai":    `{"policy_ids":[""]}`,
		"lakera_v1":   `{"category_thresholds":{"unknown":0.5}}`,
		"hiddenlayer": `{"project_id":"bad\r\nheader"}`,
		"grayswan":    `{"violation_threshold":1.1}`,
		"lasso":       `{"user_id":"bad\nheader"}`,
	} {
		t.Run(kind, func(t *testing.T) {
			if namedVendorsA[kind].validate(json.RawMessage(parameters)) == nil {
				t.Fatal("accepted invalid parameters")
			}
		})
	}
}

func TestNamedVendorAOutput(t *testing.T) {
	for _, tc := range []struct{ kind, response, key, want string }{
		{"aporia", `{"action":"passthrough"}`, "validation_target", "response"},
		{"hiddenlayer", `{"evaluation":{"action":"Allow"}}`, "output", `{"messages":[{"content":"output text","role":"assistant"}]}`},
		{"pangea", `{"result":{"blocked":false}}`, "event_type", "output"},
		{"crowdstrike_aidr", `{"result":{"blocked":false}}`, "event_type", "output"},
		{"lasso", `{"violations_detected":false}`, "messageType", "COMPLETION"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]json.RawMessage
				json.NewDecoder(r.Body).Decode(&body)
				var str string
				if json.Unmarshal(body[tc.key], &str) != nil {
					str = string(body[tc.key])
				}
				if str != tc.want {
					t.Errorf("%s=%s want %s", tc.key, str, tc.want)
				}
				io.WriteString(w, tc.response)
			}))
			defer server.Close()
			_, err := namedVendorsA[tc.kind].evaluate(t.Context(), server.Client(), ExternalConfig{Endpoint: server.URL}, externalCredential{APIKey: "key"}, Output, Segment{ID: "text", Text: "output text"})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestNamedVendorARejectsMalformedReplacement(t *testing.T) {
	for _, response := range []string{
		`{"required_action":{"action_type":"anonymize_action"}}`,
		`{"required_action":{"action_type":"anonymize_action"},"redacted_chat":{"all_redacted_messages":[{"content":"a"},{"content":"b"}]}}`,
		`{"required_action":{"action_type":"anonymize_action"},"redacted_chat":{"all_redacted_messages":[{}]}}`,
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, response) }))
		_, err := aAim(t.Context(), server.Client(), ExternalConfig{Endpoint: server.URL}, externalCredential{APIKey: "key"}, Input, Segment{ID: "text", Text: "original"})
		server.Close()
		if err == nil {
			t.Fatal("accepted malformed replacement")
		}
	}
}

func TestNamedVendorAHiddenlayerOAuth(t *testing.T) {
	tokenCalls, scanCalls := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth2/token" {
			tokenCalls++
			id, secret, ok := r.BasicAuth()
			if !ok || id != "client" || secret != "secret" || r.URL.Query().Get("grant_type") != "client_credentials" {
				t.Error("incorrect OAuth request")
			}
			io.WriteString(w, `{"access_token":"sealed-token"}`)
			return
		}
		scanCalls++
		if r.Header.Get("Authorization") != "Bearer sealed-token" {
			t.Error("missing access token")
		}
		io.WriteString(w, `{"evaluation":{"action":"Allow"}}`)
	}))
	defer server.Close()
	_, err := aHiddenlayer(context.Background(), server.Client(), ExternalConfig{Endpoint: server.URL + "/scan", AuthEndpoint: server.URL + "/oauth2/token"}, externalCredential{ClientID: "client", ClientSecret: "secret"}, Input, Segment{ID: "text", Text: "original"})
	if err != nil || tokenCalls != 1 || scanCalls != 1 {
		t.Fatalf("error %v tokens %d scans %d", err, tokenCalls, scanCalls)
	}
}
