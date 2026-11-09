package vendor

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
)

const (
	sample             = "sensitive value"
	secret             = "opaque-api-key"
	purviewApplication = `"parameters":{"application_id":"app","application_name":"OLP"}`
)

var (
	inputRequest  = Request{Role: User, Text: sample}
	outputRequest = Request{Output: true, Role: Assistant, Text: sample}
)

func configuration(endpoint, extra string) json.RawMessage {
	return json.RawMessage(`{"endpoint":` + strconv.Quote(endpoint) + `,"credential_ref":"credential","content_egress":true` + strings.TrimSuffix(","+extra, ",") + `}`)
}

func serve(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func reply(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) }
}

func evaluate(t *testing.T, server *httptest.Server, kind Type, raw json.RawMessage, r Request) (Decision, error) {
	t.Helper()
	spec, err := Parse(kind, raw)
	if err != nil {
		t.Fatalf("parse %s: %v", kind, err)
	}
	return spec.Call(t.Context(), server.Client(), Credential{APIKey: secret, ClientID: "client", ClientSecret: "secret", AccessKeyID: "ACCESS", SecretAccessKey: "SECRET"}, r)
}

func lookup(node any, path []string) any {
	if len(path) == 0 {
		return node
	}
	switch n := node.(type) {
	case map[string]any:
		return lookup(n[path[0]], path[1:])
	case []any:
		i, err := strconv.Atoi(path[0])
		if err != nil || i < 0 || i >= len(n) {
			return nil
		}
		return lookup(n[i], path[1:])
	}
	return nil
}

func rendered(node any) string {
	if s, ok := node.(string); ok {
		return s
	}
	encoded, _ := json.Marshal(node)
	return string(encoded)
}

const (
	userMessages      = `[{"role":"user","content":"sensitive value"}]`
	assistantMessages = `[{"role":"assistant","content":"sensitive value"}]`
	xecguardPolicies  = `"policy_names":["Default_Policy_SystemPromptEnforcement","Default_Policy_HarmfulContentProtection","Default_Policy_GeneralPromptAttackProtection"]`
)

var contracts = []struct {
	kind                   Type
	extra, header, auth    string
	allow, flag, malformed string
	field, input, output   string
}{
	{kind: "aporia", header: "X-APORIA-API-KEY", auth: secret, allow: `{"action":"passthrough"}`, flag: `{"action":"block"}`, malformed: `{}`, field: "validation_target"},
	{kind: "dynamoai", extra: `"parameters":{"policy_ids":["policy"],"model_id":"model"}`, header: "Authorization", auth: "Bearer " + secret, allow: `{"finalAction":"NONE"}`, flag: `{"finalAction":"BLOCK"}`, malformed: `{}`, field: "policyIds"},
	{kind: "guardrails_ai", header: "Authorization", auth: "Bearer " + secret, allow: `{"validationPassed":true}`, flag: `{"validationPassed":false}`, malformed: `{}`, field: "llmOutput"},
	{kind: "lakera_v1", header: "Authorization", auth: "Bearer " + secret, allow: `{"results":[{"flagged":false}]}`, flag: `{"results":[{"flagged":true}]}`, malformed: `{}`, field: "input"},
	{kind: "hiddenlayer", header: "Authorization", auth: "Bearer " + secret, allow: `{"evaluation":{"action":"Allow"}}`, flag: `{"evaluation":{"action":"Block"}}`, malformed: `{}`, field: "input"},
	{kind: "aim", header: "Authorization", auth: "Bearer " + secret, allow: `{"required_action":{},"analysis_result":{}}`, flag: `{"required_action":{"action_type":"block_action"}}`, malformed: `{}`, field: "messages"},
	{kind: "cato_networks", header: "Authorization", auth: "Bearer " + secret, allow: `{"required_action":{},"analysis_result":{}}`, flag: `{"required_action":{"action_type":"block_action"}}`, malformed: `{}`, field: "messages"},
	{kind: "pangea", header: "Authorization", auth: "Bearer " + secret, allow: `{"status":"Success","result":{"blocked":false}}`, flag: `{"status":"Success","result":{"blocked":true}}`, malformed: `{}`, field: "input"},
	{kind: "crowdstrike_aidr", header: "Authorization", auth: "Bearer " + secret, allow: `{"result":{"blocked":false}}`, flag: `{"result":{"blocked":true}}`, malformed: `{}`, field: "guard_input"},
	{kind: "lasso", header: "lasso-api-key", auth: secret, allow: `{"violations_detected":false}`, flag: `{"violations_detected":true,"findings":{"jailbreak":[{"action":"BLOCK"}]}}`, malformed: `{}`, field: "sessionId"},
	{kind: "pillar", header: "Authorization", auth: "Bearer " + secret, allow: `{"flagged":false}`, flag: `{"flagged":true}`, malformed: `{}`, field: "messages"},
	{kind: "grayswan", header: "grayswan-api-key", auth: secret, allow: `{"violation":0.2}`, flag: `{"violation":0.9}`, malformed: `{}`, field: "messages"},
	{kind: "panw_prisma_airs", header: "x-pan-token", auth: secret, allow: `{"action":"allow"}`, flag: `{"action":"block"}`, malformed: `{}`, input: `{"contents":[{"prompt":"sensitive value"}],"metadata":{"is_response":false}}`, output: `{"contents":[{"response":"sensitive value"}],"metadata":{"is_response":true}}`},
	{kind: "cisco_ai_defense", header: "X-Cisco-AI-Defense-API-Key", auth: secret, allow: `{"is_safe":true,"action":"allow"}`, flag: `{"is_safe":false,"action":"block"}`, malformed: `{}`, input: `{"messages":` + userMessages + `,"metadata":{},"config":{}}`, output: `{"messages":` + assistantMessages + `,"metadata":{},"config":{}}`},
	{kind: "noma", header: "Authorization", auth: "Bearer " + secret, allow: `{"aggregatedScanResult":false}`, flag: `{"aggregatedScanResult":true}`, malformed: `{}`, input: `{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"sensitive value"}]}],"x-noma-context":{"applicationId":""}}`, output: `{"input":[{"type":"message","role":"assistant","content":[{"type":"input_text","text":"sensitive value"}]}],"x-noma-context":{"applicationId":""}}`},
	{kind: "noma_v2", header: "Authorization", auth: "Bearer " + secret, allow: `{"action":"NONE"}`, flag: `{"action":"BLOCKED"}`, malformed: `{}`, input: `{"inputs":{"texts":["sensitive value"]},"request_data":{},"input_type":"request","monitor_mode":false}`, output: `{"inputs":{"texts":["sensitive value"]},"request_data":{},"input_type":"response","monitor_mode":false}`},
	{kind: "zscaler_ai_guard", header: "Authorization", auth: "Bearer " + secret, allow: `{"statusCode":200,"action":"ALLOW"}`, flag: `{"statusCode":200,"action":"BLOCK"}`, malformed: `{}`, input: `{"direction":"IN","content":"sensitive value"}`, output: `{"direction":"OUT","content":"sensitive value"}`},
	{kind: "javelin", header: "x-javelin-apikey", auth: secret, allow: `{"assessments":[{"trustsafety":{"request_reject":false}}]}`, flag: `{"assessments":[{"trustsafety":{"request_reject":true}}]}`, malformed: `{}`, input: `{"input":{"text":"sensitive value"},"metadata":{},"config":{}}`},
	{kind: "enkryptai", header: "apikey", auth: secret, allow: `{"summary":{"toxicity":[],"policy_violation":0}}`, flag: `{"summary":{"toxicity":[],"policy_violation":1}}`, malformed: `{}`, input: `{"text":"sensitive value"}`, output: `{"text":"sensitive value"}`},
	{kind: "ibm_guardrails", extra: `"parameters":{"detector_id":"pii"}`, header: "Authorization", auth: "Bearer " + secret, allow: `[[]]`, flag: `[[{"detection_type":"pii","detection":"email","score":0.9}]]`, malformed: `{}`, input: `{"contents":["sensitive value"],"detector_params":{}}`, output: `{"contents":["sensitive value"],"detector_params":{}}`},
	{kind: "onyx", allow: `{"allowed":true}`, flag: `{"allowed":false}`, malformed: `{}`, input: `{"payload":{"messages":` + userMessages + `},"input_type":"request","conversation_id":"segment"}`, output: `{"payload":{"object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"sensitive value"},"finish_reason":"stop"}]},"input_type":"response","conversation_id":"segment"}`},
	{kind: "promptguard", header: "X-API-Key", auth: secret, allow: `{"decision":"allow"}`, flag: `{"decision":"block"}`, malformed: `{}`, input: `{"messages":` + userMessages + `,"direction":"input"}`, output: `{"messages":` + assistantMessages + `,"direction":"output"}`},
	{kind: "xecguard", header: "Authorization", auth: "Bearer " + secret, allow: `{"decision":"SAFE"}`, flag: `{"decision":"UNSAFE"}`, malformed: `{}`, input: `{"model":"xecguard_v2","scan_type":"input","messages":` + userMessages + `,` + xecguardPolicies + `}`, output: `{"model":"xecguard_v2","scan_type":"response","messages":` + assistantMessages + `,` + xecguardPolicies + `}`},
	{kind: "prompt_security", header: "APP-ID", auth: secret, allow: `{"result":{"prompt":{"action":"allow"},"response":{"action":"allow"}}}`, flag: `{"result":{"prompt":{"action":"block"},"response":{"action":"block"}}}`, malformed: `{}`, input: `{"messages":` + userMessages + `,"user":"","system_prompt":""}`, output: `{"response":"sensitive value","user":"","system_prompt":""}`},
	{kind: "qualifire", extra: `"parameters":{"prompt_injections":true}`, header: "X-Qualifire-API-Key", auth: secret, allow: `{"evaluationResults":[{"results":[{"flagged":false}]}]}`, flag: `{"evaluationResults":[{"results":[{"flagged":true}]}]}`, malformed: `{"evaluationResults":[{"results":[{}]}]}`},
	{kind: "ovalix", extra: `"parameters":{"application_id":"app","checkpoint_id":"checkpoint","actor":"caller"}`, header: "Authorization", auth: "Bearer " + secret, allow: `{"action_type":"allow"}`, flag: `{"action_type":"block"}`, malformed: `{}`},
	{kind: "microsoft_purview", extra: purviewApplication, header: "Authorization", auth: "Bearer " + secret, allow: `{"policyActions":[]}`, flag: `{"policyActions":[{"@odata.type":"microsoft.graph.restrictAccessAction","restrictionAction":"block"}]}`, malformed: `{"policyActions":[{}]}`},
	{kind: "akto", extra: `"parameters":{"account_id":"app","vxlan_id":"network"}`, header: "Authorization", auth: secret, allow: `{"data":{"guardrailsResult":{"Allowed":true}}}`, flag: `{"data":{"guardrailsResult":{"Allowed":false}}}`, malformed: `{"data":{"guardrailsResult":{}}}`},
	{kind: "deepkeep", extra: `"parameters":{"firewall_id":"firewall"}`, header: "X-API-Key", auth: secret, allow: `{"action":"NONE"}`, flag: `{"action":"BLOCKED"}`, malformed: `{"action":"unknown"}`},
	{kind: "qostodian_nexus", extra: `"parameters":{"source":"OLP"}`, header: "X-API-Key", auth: secret, allow: `{"action":"NONE"}`, flag: `{"action":"BLOCKED"}`, malformed: `{"action":"GUARDRAIL_INTERVENED"}`},
	{kind: "rubrik", header: "Authorization", auth: "Bearer " + secret, allow: `{}`, flag: `{"choices":[{"message":{"content":"refused"}}]}`, malformed: `null`},
	{kind: "vigil_guard", header: "Authorization", auth: "Bearer " + secret, allow: `{"decision":"ALLOWED"}`, flag: `{"decision":"BLOCKED"}`, malformed: `{"decision":"SANITIZED"}`},
	{kind: "repelloai", extra: `"parameters":{"asset_id":"asset"}`, header: "X-API-Key", auth: secret, allow: `{"verdict":"passed"}`, flag: `{"verdict":"blocked"}`, malformed: `{"verdict":"nonsense"}`},
	{kind: "singulr", extra: `"parameters":{"application_id":"app","guardrail_id":"guard"}`, header: "X-Singulr-Gateway-Token", auth: secret, allow: `{"should_block":false}`, flag: `{"should_block":true}`, malformed: `{}`},
	{kind: "headroom", extra: `"parameters":{"model":"model"}`, header: "Authorization", auth: "Bearer " + secret, allow: `{"messages":[{"role":"user","content":"sensitive value"}]}`, flag: `{"messages":[{"role":"user","content":"safe"}]}`, malformed: `{"messages":[{"role":"assistant","content":"safe"}]}`},
	{kind: "compresr", extra: `"parameters":{"query":"what matters?"}`, header: "X-API-Key", auth: secret, allow: `{"data":{"compressed_context":"sensitive value"}}`, flag: `{"data":{"compressed_context":"safe"}}`, malformed: `{"data":{}}`},
	{kind: "straiker", header: "Authorization", auth: "Bearer " + secret, allow: `{"action":"NONE"}`, flag: `{"action":"BLOCKED"}`, malformed: `{"action":"GUARDRAIL_INTERVENED"}`},
	{kind: "alice", header: "af-api-key", auth: secret, allow: `{"verdict":"ALLOW"}`, flag: `{"verdict":"BLOCK"}`, malformed: `{"verdict":"MASK","replacements":[{"index":2,"text":"safe"}]}`},
	{kind: "openai_moderation", header: "Authorization", auth: "Bearer " + secret, allow: `{"results":[{"flagged":false}]}`, flag: `{"results":[{"flagged":true}]}`, malformed: `{"results":[{}]}`, field: "input"},
	{kind: "generic_guardrail_api", header: "x-api-key", auth: secret, allow: `{"action":"NONE"}`, flag: `{"action":"BLOCKED","texts":["replacement"]}`, malformed: `{}`, field: "texts"},
}

func TestContracts(t *testing.T) {
	for _, tc := range contracts {
		t.Run(string(tc.kind), func(t *testing.T) {
			for _, call := range []struct {
				request Request
				payload string
			}{{inputRequest, tc.input}, {outputRequest, tc.output}} {
				if call.request.Output && call.payload == "" {
					continue
				}
				for _, scenario := range []struct {
					name, response string
					status         int
					flagged, fails bool
				}{{"allow", tc.allow, http.StatusOK, false, false}, {"flag", tc.flag, http.StatusOK, true, false}, {"malformed", tc.malformed, http.StatusOK, false, true}, {"unavailable", tc.allow, http.StatusServiceUnavailable, false, true}} {
					t.Run(direction(call.request)+"/"+scenario.name, func(t *testing.T) {
						server := serve(t, func(w http.ResponseWriter, r *http.Request) {
							raw, err := io.ReadAll(r.Body)
							if err != nil {
								t.Error(err)
							}
							if r.Method != http.MethodPost || !strings.HasPrefix(r.Header.Get(tc.header), tc.auth) {
								t.Errorf("method %s, %s header %q", r.Method, tc.header, r.Header.Get(tc.header))
							}
							if !strings.Contains(string(raw), sample) || strings.Contains(string(raw), secret) {
								t.Errorf("content or credential boundary: %s", raw)
							}
							var body map[string]any
							if err := json.Unmarshal(raw, &body); err != nil {
								t.Errorf("request is not a JSON object: %s", raw)
							}
							if tc.field != "" && body[tc.field] == nil {
								t.Errorf("missing native field %s in %s", tc.field, raw)
							}
							if call.payload != "" {
								samePayload(t, tc.kind, body, call.payload)
							}
							checkRequest(t, tc.kind, r, body)
							w.WriteHeader(scenario.status)
							_, _ = io.WriteString(w, scenario.response)
						})
						decision, err := evaluate(t, server, tc.kind, configuration(server.URL+"/native-operation", tc.extra), call.request)
						if scenario.fails {
							if err == nil {
								t.Fatalf("accepted invalid upstream response: %+v", decision)
							}
							return
						}
						if err != nil || decision.Flagged() != scenario.flagged {
							t.Fatalf("decision=%+v error=%v, want flagged=%v", decision, err, scenario.flagged)
						}
					})
				}
			}
		})
	}
}

func samePayload(t *testing.T, kind Type, body map[string]any, payload string) {
	t.Helper()
	conversation, _ := body["conversation_id"].(string)
	if kind == "onyx" {
		if _, err := uuid.Parse(conversation); err != nil {
			t.Errorf("invalid conversation ID %q", conversation)
		}
	}
	var want map[string]any
	if err := json.Unmarshal([]byte(strings.Replace(payload, `"conversation_id":"segment"`, `"conversation_id":`+strconv.Quote(conversation), 1)), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(body, want) {
		got, _ := json.Marshal(body)
		t.Errorf("native payload: got %s, want %s", got, payload)
	}
}

func checkRequest(t *testing.T, kind Type, r *http.Request, body map[string]any) {
	t.Helper()
	if kind == "onyx" {
		if !strings.HasSuffix(r.URL.Path, "/guard/evaluate/v1/"+secret+"/litellm") {
			t.Errorf("native credential path %s", r.URL.Path)
		}
		return
	}
	if r.URL.Path != "/native-operation" {
		t.Errorf("path %s", r.URL.Path)
	}
	switch kind {
	case "ibm_guardrails":
		if r.Header.Get("detector-id") != "pii" {
			t.Error("missing detector-id")
		}
	case "akto":
		query := r.URL.Query()
		if query.Get("guardrails") != "true" || query.Get("ingest_data") != "" || query.Get("akto_connector") != "litellm" {
			t.Errorf("unexpected query %s", r.URL.RawQuery)
		}
		if _, ok := body["time"].(string); !ok {
			t.Error("Akto requires a timestamp string")
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
		if body["source"] != "user_input" {
			t.Error("wrong Vigil source")
		}
	case "straiker":
		if r.Header.Get("X-Straiker-Webhook-Format") != "litellm" {
			t.Error("missing Straiker format")
		}
	}
}

func TestReplacements(t *testing.T) {
	const text = "pii é"
	for _, tc := range []struct {
		kind                 Type
		extra, input, output string
	}{
		{kind: "guardrails_ai", input: `{"validationPassed":true,"validatedOutput":"safe"}`},
		{kind: "hiddenlayer", input: `{"evaluation":{"action":"Redact"},"modified_data":{"input":{"messages":[{"content":"safe"}]}}}`},
		{kind: "aim", input: `{"required_action":{"action_type":"anonymize_action"},"redacted_chat":{"all_redacted_messages":[{"content":"safe"}]}}`},
		{kind: "cato_networks", input: `{"required_action":{"action_type":"anonymize_action"},"redacted_chat":{"all_redacted_messages":[{"content":"safe"}]}}`},
		{kind: "pangea", input: `{"result":{"blocked":false,"transformed":true,"output":{"messages":[{"content":"safe"}]}}}`},
		{kind: "crowdstrike_aidr", input: `{"result":{"blocked":false,"transformed":true,"guard_output":{"messages":[{"content":"safe"}]}}}`},
		{kind: "lasso", extra: `"parameters":{"mask":true}`, input: `{"violations_detected":true,"findings":{"pii":[{"action":"AUTO_MASKING"}]},"messages":[{"content":"safe"}]}`},
		{kind: "pillar", input: `{"flagged":true,"masked_session_messages":[{"content":"safe"}]}`},
		{kind: "panw_prisma_airs", input: `{"action":"allow","prompt_masked_data":{"data":"safe"}}`, output: `{"action":"allow","response_masked_data":{"data":"safe"}}`},
		{kind: "cisco_ai_defense", input: `{"action":"redact","sanitized_text":"safe"}`, output: `{"action":"redact","sanitized_text":"safe"}`},
		{kind: "noma", input: `{"aggregatedScanResult":false,"scanResult":[{"role":"user","results":{"anonymizedContent":{"anonymized":"safe"}}}]}`, output: `{"aggregatedScanResult":false,"scanResult":[{"role":"assistant","results":{"anonymizedContent":{"anonymized":"safe"}}}]}`},
		{kind: "noma_v2", input: `{"action":"GUARDRAIL_INTERVENED","texts":["safe"]}`, output: `{"action":"GUARDRAIL_INTERVENED","texts":["safe"]}`},
		{kind: "promptguard", input: `{"decision":"redact","redacted_messages":[{"role":"user","content":"safe"}]}`, output: `{"decision":"redact","redacted_messages":[{"role":"assistant","content":"safe"}]}`},
		{kind: "prompt_security", input: `{"result":{"prompt":{"action":"modify","modified_messages":[{"role":"user","content":"safe"}]}}}`, output: `{"result":{"response":{"action":"modify","modified_text":"safe"}}}`},
		{kind: "ovalix", extra: `"parameters":{"application_id":"app","checkpoint_id":"checkpoint","actor":"caller"}`, input: `{"action_type":"anonymize","modified_data":{"content":"safe"}}`},
		{kind: "deepkeep", extra: `"parameters":{"firewall_id":"firewall"}`, input: `{"action":"GUARDRAIL_INTERVENED","texts":["safe"]}`},
		{kind: "qostodian_nexus", input: `{"action":"GUARDRAIL_INTERVENED","texts":["safe"]}`},
		{kind: "vigil_guard", input: `{"decision":"SANITIZED","sanitizedText":"safe"}`},
		{kind: "straiker", input: `{"action":"GUARDRAIL_INTERVENED","texts":["safe"]}`},
		{kind: "alice", input: `{"verdict":"MASK","replacements":[{"index":0,"text":"safe"}]}`},
		{kind: "headroom", extra: `"parameters":{"model":"model"}`, input: `{"messages":[{"role":"user","content":"safe"}]}`},
		{kind: "compresr", extra: `"parameters":{"query":"what matters?"}`, input: `{"data":{"compressed_context":"safe"}}`},
		{kind: "generic_guardrail_api", input: `{"action":"GUARDRAIL_INTERVENED","texts":["safe"]}`, output: `{"action":"GUARDRAIL_INTERVENED","texts":["safe"]}`},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			for _, call := range []struct {
				request  Request
				response string
			}{{Request{Role: User, Text: text}, tc.input}, {Request{Output: true, Role: Assistant, Text: text}, tc.output}} {
				if call.response == "" {
					continue
				}
				t.Run(direction(call.request), func(t *testing.T) {
					server := serve(t, reply(call.response))
					decision, err := evaluate(t, server, tc.kind, configuration(server.URL, tc.extra), call.request)
					if err != nil || !decision.Flagged() || !reflect.DeepEqual(decision.Spans(), []Span{{End: len(text), Replacement: "safe"}}) {
						t.Fatalf("decision=%+v spans=%+v error=%v", decision, decision.Spans(), err)
					}
				})
			}
		})
	}
}

func TestMalformedDecisions(t *testing.T) {
	for _, tc := range []struct {
		kind            Type
		extra, response string
	}{
		{kind: "cisco_ai_defense", response: `{"action":"mystery","is_safe":true}`},
		{kind: "noma", response: `{"aggregatedScanResult":"false"}`},
		{kind: "noma_v2", response: `{"action":"GUARDRAIL_INTERVENED","texts":[]}`},
		{kind: "noma_v2", response: `{"action":"GUARDRAIL_INTERVENED","texts":[null]}`},
		{kind: "zscaler_ai_guard", response: `{"statusCode":500,"action":"ALLOW"}`},
		{kind: "javelin", response: `{"assessments":[{"trustsafety":{}}]}`},
		{kind: "enkryptai", response: `{"summary":{"policy_violation":2}}`},
		{kind: "enkryptai", response: `{"summary":{"policy_violation":null}}`},
		{kind: "ibm_guardrails", extra: `"parameters":{"detector_id":"pii"}`, response: `[[{"score":0.99}]]`},
		{kind: "promptguard", response: `{"decision":"redact","redacted_messages":[{"role":"assistant","content":"safe"}]}`},
		{kind: "xecguard", response: `{"decision":"UNKNOWN"}`},
		{kind: "prompt_security", response: `{"result":{"prompt":{"action":"modify","modified_messages":[]}}}`},
		{kind: "aim", response: `{"required_action":{"action_type":"anonymize_action"}}`},
		{kind: "aim", response: `{"required_action":{"action_type":"anonymize_action"},"redacted_chat":{"all_redacted_messages":[{"content":"a"},{"content":"b"}]}}`},
		{kind: "aim", response: `{"required_action":{"action_type":"anonymize_action"},"redacted_chat":{"all_redacted_messages":[{}]}}`},
		{kind: "ovalix", extra: `"parameters":{"application_id":"app","checkpoint_id":"checkpoint","actor":"caller"}`, response: `{"action_type":"future-action"}`},
		{kind: "ovalix", extra: `"parameters":{"application_id":"app","checkpoint_id":"checkpoint","actor":"caller"}`, response: `{"action_type":"anonymize"}`},
		{kind: "ovalix", extra: `"parameters":{"application_id":"app","checkpoint_id":"checkpoint","actor":"caller"}`, response: `{"action_type":"anonymize","modified_data":{}}`},
		{kind: "deepkeep", extra: `"parameters":{"firewall_id":"firewall"}`, response: `{"action":"GUARDRAIL_INTERVENED","texts":[null]}`},
		{kind: "qostodian_nexus", response: `{"action":"GUARDRAIL_INTERVENED","texts":[null]}`},
		{kind: "straiker", response: `{"action":"GUARDRAIL_INTERVENED","texts":[null]}`},
		{kind: "generic_guardrail_api", response: `{"action":"GUARDRAIL_INTERVENED"}`},
		{kind: "generic_guardrail_api", response: `{"action":"GUARDRAIL_INTERVENED","texts":[null]}`},
		{kind: "generic_guardrail_api", response: `{"action":"NONE","texts":["a","b"]}`},
		{kind: "openai_moderation", response: `null`},
		{kind: "lakera_v1", extra: `"parameters":{"category_thresholds":{"prompt_injection":0.5}}`, response: `{"results":[{"flagged":true,"category_scores":{"prompt_injection":null}}]}`},
		{kind: "microsoft_purview", extra: purviewApplication, response: `{"policyActions":[{"action":"restrictAccess","restrictionAction":null}]}`},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			server := serve(t, reply(tc.response))
			decision, err := evaluate(t, server, tc.kind, configuration(server.URL, tc.extra), Request{Role: User, Text: "private"})
			if err == nil {
				t.Fatalf("accepted malformed decision: %+v", decision)
			}
			if strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), secret) {
				t.Fatalf("error leaked content or credential: %v", err)
			}
		})
	}
}

func TestParseRejectsInvalidConfiguration(t *testing.T) {
	const endpoint = "https://guardrail.example/scan"
	for _, tc := range []struct {
		kind  Type
		extra string
	}{
		{"aporia", `"parameters":{"api_key":"inline-secret"}`},
		{"dynamoai", `"parameters":{"policy_ids":[""]}`},
		{"lakera_v1", `"parameters":{"category_thresholds":{"unknown":0.5}}`},
		{"hiddenlayer", `"parameters":{"project_id":"bad\r\nheader"}`},
		{"grayswan", `"parameters":{"violation_threshold":1.1}`},
		{"lasso", `"parameters":{"user_id":"bad\nheader"}`},
		{"ibm_guardrails", `"parameters":{}`},
		{"ibm_guardrails", `"parameters":{"detector_id":"pii","score_threshold":2}`},
		{"ibm_guardrails", `"parameters":{"detector_id":"pii","detector_params":[]}`},
		{"cisco_ai_defense", `"parameters":{"enabled_rules":[{}]}`},
		{"zscaler_ai_guard", `"parameters":{"policy_id":-1}`},
		{"javelin", `"parameters":{"config":[]}`},
		{"xecguard", `"parameters":{"policy_names":[""]}`},
		{"enkryptai", `"parameters":{"policy_name":"policy\r\nX-Injected: true"}`},
		{"qualifire", ""},
		{"ovalix", `"parameters":{"application_id":"app"}`},
		{"microsoft_purview", ""},
		{"akto", ""},
		{"deepkeep", ""},
		{"repelloai", ""},
		{"compresr", `"parameters":{"query":"q","target_compression_ratio":1.5}`},
		{"singulr", `"parameters":{"guardrail_id":"bad\nheader"}`},
		{"conduct", `"parameters":{"workspace_id":"bad\rheader"}`},
		{"qostodian_nexus", `"parameters":{"source":"bad\nheader"}`},
		{"lakera", `"region":"us-east-1"`},
		{"aporia", `"auth_endpoint":"https://auth.example"`},
		{"presidio", `"threshold":2`},
		{"bedrock", `"region":"us-east-1","guardrail":"id"`},
		{"bedrock", `"region":"us-east-1","guardrail":"id","version":"1","template":"projects/p/locations/us/templates/t"`},
		{"azure.content_safety", `"threshold":8`},
		{"model_armor", `"template":"organizations/o"`},
		{"hiddenlayer", `"auth_endpoint":"ftp://auth.example"`},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			if _, err := Parse(tc.kind, configuration(endpoint, tc.extra)); err == nil {
				t.Fatalf("accepted %s", tc.extra)
			}
		})
	}
	for kind := range vendors {
		t.Run(string(kind)+"/unknown_parameter", func(t *testing.T) {
			if _, err := Parse(kind, configuration(endpoint, `"parameters":{"unexpected":true}`)); err == nil {
				t.Fatal("accepted an unknown parameter")
			}
		})
	}
	for _, raw := range []string{
		`{"endpoint":"https://guardrail.example","credential_ref":"credential"}`,
		`{"endpoint":"https://guardrail.example","content_egress":true}`,
		`{"endpoint":"https://user@guardrail.example","credential_ref":"credential","content_egress":true}`,
	} {
		if _, err := Parse("lakera", json.RawMessage(raw)); err == nil {
			t.Errorf("accepted connection %s", raw)
		}
	}
}

func TestOutputMapping(t *testing.T) {
	for _, tc := range []struct {
		kind              Type
		extra, path, want string
	}{
		{kind: "aporia", path: "validation_target", want: "response"},
		{kind: "hiddenlayer", path: "output", want: `{"messages":[{"content":"answer","role":"assistant"}]}`},
		{kind: "pangea", path: "event_type", want: "output"},
		{kind: "crowdstrike_aidr", path: "event_type", want: "output"},
		{kind: "lasso", path: "messageType", want: "COMPLETION"},
		{kind: "qualifire", extra: `"parameters":{"prompt_injections":true}`, path: "output", want: "answer"},
		{kind: "deepkeep", extra: `"parameters":{"firewall_id":"firewall"}`, path: "input_type", want: "response"},
		{kind: "qostodian_nexus", path: "input_type", want: "response"},
		{kind: "alice", path: "input_type", want: "response"},
		{kind: "generic_guardrail_api", path: "input_type", want: "response"},
		{kind: "microsoft_purview", extra: purviewApplication, path: "contentToProcess/activityMetadata/activity", want: "downloadText"},
		{kind: "vigil_guard", path: "source", want: "model_output"},
		{kind: "repelloai", extra: `"parameters":{"asset_id":"asset"}`, path: "scan_data/response", want: "answer"},
		{kind: "singulr", path: "guardrail_scope", want: "response"},
		{kind: "straiker", path: "event/type", want: "post_call"},
		{kind: "rubrik", path: "request", want: `{}`},
		{kind: "rubrik", path: "response/object", want: "chat.completion"},
		{kind: "rubrik", path: "response/choices/0/message", want: `{"content":"answer","role":"assistant"}`},
	} {
		t.Run(string(tc.kind)+"/"+tc.path, func(t *testing.T) {
			server := serve(t, func(w http.ResponseWriter, r *http.Request) {
				var body any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if got := rendered(lookup(body, strings.Split(tc.path, "/"))); got != tc.want {
					t.Errorf("%s = %s, want %s", tc.path, got, tc.want)
				}
				_, _ = io.WriteString(w, `{}`)
			})
			_, _ = evaluate(t, server, tc.kind, configuration(server.URL, tc.extra), Request{Output: true, Role: Assistant, Text: "answer"})
		})
	}
}
