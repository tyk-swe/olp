package interaction

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/openai"
)

func configuration(t *testing.T, id string) Config {
	t.Helper()
	profile, err := connectors.LookupProfile(id, "1")
	if err != nil {
		t.Fatal(err)
	}
	provider := connectors.Config{Kind: profile.Kind, AuthMode: profile.Authentication[0], ProfileID: id, ProfileRevision: "1"}
	if profile.Hosting == "azure-deployment" || profile.Hosting == "azure-responses-legacy" {
		provider.APIVersion = "2025-03-01-preview"
	}
	return Config{Provider: provider, ProviderID: "provider-1", RevisionID: "revision-1", Model: "native-model"}
}
func template(t *testing.T, config Config) *Template {
	t.Helper()
	result, err := Compile(config)
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func request(t *testing.T, family openai.Family, body string) *openai.Request {
	t.Helper()
	if family == openai.FamilyBedrock {
		document, err := oif.ParseJSON([]byte(body), oif.Limits{})
		if err != nil {
			t.Fatal(err)
		}
		return openai.NewSourceEnvelope(family, "route", false, document)
	}
	result, err := protocols.Parse(family, []byte(body), "route")
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func bind(t *testing.T, template *Template, request *openai.Request, context Context) *Plan {
	t.Helper()
	plan, err := template.Bind(request, context)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}
func field(t *testing.T, body []byte, name string) json.RawMessage {
	t.Helper()
	document, err := oif.ParseJSON(body, oif.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	return document.Fields()[name]
}
func assertReason(t *testing.T, err error, reason string) {
	t.Helper()
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != reason {
		t.Fatalf("reason: got %v, want %s", err, reason)
	}
}

func TestNativeProfilesPreserveOriginalGenerationDocuments(t *testing.T) {
	for _, id := range []string{"openai-chat", "compatible-chat", "azure-legacy-chat", "azure-v1-chat", "openai-responses", "compatible-responses", "azure-legacy-responses", "azure-v1-responses", "anthropic-messages", "vertex-anthropic", "bedrock-anthropic-invoke", "gemini-generation", "vertex-gemini", "bedrock-converse"} {
		t.Run(id, func(t *testing.T) {
			config := configuration(t, id)
			profile, _ := config.Provider.Profile()
			var family openai.Family
			var raw string
			switch profile.Dialect {
			case "openai-chat":
				family = openai.FamilyChat
				raw = `{"model":"route","messages":[{"role":"system","content":"system scope"},{"role":"developer","content":"developer scope"},{"role":"user","content":"literal"}],"max_completion_tokens":64,"temperature":null,"future_native":{"integer":123456789012345678901234567890,"array":[false,0,"",null]}}`
			case "openai-responses":
				family = openai.FamilyResponses
				raw = `{"model":"route","input":"exact scalar","store":false,"reasoning":{"effort":"high"},"future_native":{"integer":123456789012345678901234567890,"array":[false,0,"",null]}}`
			case "anthropic-messages":
				family = openai.FamilyAnthropic
				raw = `{"model":"route","messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"opaque reasoning","signature":"exact-state"},{"type":"text","text":"before"},{"type":"tool_use","id":"call-1","name":"f","input":{}},{"type":"text","text":"after"}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-1","content":"result"}]}],"max_tokens":64,"future_native":{"integer":123456789012345678901234567890,"array":[false,0,"",null]}}`
			case "gemini-generate-content":
				family = openai.FamilyGemini
				raw = `{"contents":[{"role":"user","parts":[{"text":"exact"},{"functionCall":{"name":"f","args":{}},"thoughtSignature":"opaque-state"}]}],"generationConfig":{"maxOutputTokens":64},"future_native":{"integer":123456789012345678901234567890,"array":[false,0,"",null]}}`
			case "bedrock-converse":
				family = openai.FamilyBedrock
				raw = `{"messages":[{"role":"user","content":[{"text":"exact"}]}],"inferenceConfig":{"maxTokens":64},"future_native":{"integer":123456789012345678901234567890,"array":[false,0,"",null]}}`
			default:
				t.Fatal("missing generation fixture")
			}
			source := request(t, family, raw)
			before := source.OIF().Document().Bytes()
			plan := bind(t, template(t, config), source, Context{})
			if plan.Receipt().Class != NativeIdentity {
				t.Fatal("native profile lost identity class")
			}
			if !bytes.Equal(field(t, plan.Body(), "future_native"), field(t, before, "future_native")) {
				t.Fatalf("native precision/presence changed: %s", plan.Body())
			}
			original, _ := oif.ParseJSON(before, oif.Limits{})
			for name, raw := range original.Fields() {
				if name == "model" {
					continue
				}
				if !bytes.Equal(field(t, plan.Body(), name), raw) {
					t.Fatalf("native field %s changed: %s", name, plan.Body())
				}
			}
			if !bytes.Equal(before, source.OIF().Document().Bytes()) {
				t.Fatal("source mutated")
			}
			if family == openai.FamilyResponses && string(field(t, plan.Body(), "input")) != `"exact scalar"` {
				t.Fatal("scalar Responses input normalized")
			}
			if family == openai.FamilyChat && string(field(t, plan.Body(), "max_completion_tokens")) != "64" {
				t.Fatal("native token control renamed")
			}
			for _, entry := range plan.Prepared().Provenance() {
				if entry.Origin == oif.LegacyMapping || entry.Origin == oif.ExplicitTransform {
					t.Fatal("legacy transformation admitted as native identity")
				}
			}
		})
	}
}

func TestNativeResponsesRetainScalarAndArrayDistinction(t *testing.T) {
	compiled := template(t, configuration(t, "azure-v1-responses"))
	for _, input := range []string{`"literal"`, `[{"role":"user","content":"literal"}]`, `[{"role":"user","content":[{"type":"input_text","text":"literal"}]}]`} {
		source := request(t, openai.FamilyResponses, `{"model":"route","store":false,"input":`+input+`}`)
		plan := bind(t, compiled, source, Context{})
		if string(field(t, plan.Body(), "input")) != input {
			t.Fatalf("input shape changed: %s", plan.Body())
		}
	}
}

func TestNativeDefaultsRespectPresenceAndBindingAtoms(t *testing.T) {
	config := configuration(t, "openai-chat")
	config.Provider.OperationDefaults = map[string]connectors.DefaultSet{"generation": {Dialect: "openai-chat", Values: map[string]json.RawMessage{"temperature": json.RawMessage(`0.9`), "tools": json.RawMessage(`[{"type":"function","function":{"name":"provider","description":"provider description","parameters":{"type":"object"}}}]`)}, NativeOptions: map[string]json.RawMessage{"native_false": json.RawMessage(`true`), "native_zero": json.RawMessage(`4`), "native_empty": json.RawMessage(`"filled"`)}}}
	config.Provider.Bindings = map[string]connectors.Binding{"native-model": {Model: "bound-model", Defaults: map[string]connectors.DefaultSet{"generation": {Dialect: "openai-chat", Values: map[string]json.RawMessage{"tools": json.RawMessage(`[{"type":"function","function":{"name":"binding","description":"binding description","parameters":{"type":"object","additionalProperties":false}}}]`)}}}}}
	compiled := template(t, config)
	config.Provider.OperationDefaults["generation"].Values["temperature"][0] = '9'
	source := request(t, openai.FamilyChat, `{"model":"route","messages":[{"role":"user","content":"hello"}],"temperature":null,"native_false":false,"native_zero":0,"native_empty":""}`)
	plan := bind(t, compiled, source, Context{})
	for name, want := range map[string]string{"model": `"bound-model"`, "temperature": "null", "native_false": "false", "native_zero": "0", "native_empty": `""`} {
		if string(field(t, plan.Body(), name)) != want {
			t.Fatalf("%s changed: %s", name, plan.Body())
		}
	}
	if !bytes.Contains(field(t, plan.Body(), "tools"), []byte(`"binding"`)) || bytes.Contains(field(t, plan.Body(), "tools"), []byte(`"provider"`)) {
		t.Fatal("tool/schema definitions recursively merged")
	}
	receipt := plan.Receipt()
	receipt.Dispositions[0].Rule = "changed"
	body := plan.Body()
	body[0] = '['
	detached := plan.Config()
	detached.Bindings["native-model"] = connectors.Binding{Model: "changed"}
	if plan.Receipt().Dispositions[0].Rule == "changed" || plan.Body()[0] != '{' || plan.Config().Bindings["native-model"].Model != "bound-model" {
		t.Fatal("plan exposes mutable aliases")
	}
}

func TestQualifiedTextMatchesIndependentFrozenBenchmark(t *testing.T) {
	config := configuration(t, "anthropic-messages")
	config.Model = "model-a"
	source := request(t, openai.FamilyChat, `{"model":"team-chat","messages":[{"role":"user","content":"hello"}],"max_tokens":64,"stream":false}`)
	plan := bind(t, template(t, config), source, Context{})
	want := `{"model":"model-a","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}],"max_tokens":64,"stream":false}`
	var actual, expected any
	actualDecoder := json.NewDecoder(bytes.NewReader(plan.Body()))
	actualDecoder.UseNumber()
	expectedDecoder := json.NewDecoder(strings.NewReader(want))
	expectedDecoder.UseNumber()
	if err := actualDecoder.Decode(&actual); err != nil {
		t.Fatal(err)
	}
	if err := expectedDecoder.Decode(&expected); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("qualified invocation differs from independent native fixture:\n%s\n%s", plan.Body(), want)
	}
	if plan.Receipt().Class != QualifiedInteraction || plan.Prepared().Identity() {
		t.Fatal("qualified mapping advertised as native source identity")
	}
	valid := `{"id":"msg-bench","type":"message","role":"assistant","model":"model-a","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":2,"output_tokens":1}}`
	if err := plan.ValidateUnary([]byte(valid)); err != nil {
		t.Fatal(err)
	}
	result, err := protocols.DecodeRequest(plan.Wire(), openai.FamilyChat, []byte(valid), "team-chat", "", source)
	if err != nil || result.OutputText != "hello" || result.FinishReason != "stop" || result.Usage.InputTokens != 2 || result.Usage.OutputTokens != 1 {
		t.Fatalf("qualified client projection lost text/usage: %+v %v", result, err)
	}
}

func TestQualifiedMappingRejectsUndischargedRequirements(t *testing.T) {
	compiled := template(t, configuration(t, "anthropic-messages"))
	for _, test := range []struct{ name, body, reason string }{
		{"developer scope", `{"model":"route","messages":[{"role":"developer","content":"scope"},{"role":"user","content":"x"}],"max_tokens":64}`, "instruction_scope"},
		{"system scope", `{"model":"route","messages":[{"role":"system","content":"scope"},{"role":"user","content":"x"}],"max_tokens":64}`, "instruction_scope"},
		{"adjacent boundaries", `{"model":"route","messages":[{"role":"user","content":"one"},{"role":"user","content":"two"}],"max_tokens":64}`, "instruction_scope"},
		{"budget alias", `{"model":"route","messages":[{"role":"user","content":"x"}],"max_completion_tokens":64}`, "reasoning_budget"},
		{"thinking", `{"model":"route","messages":[{"role":"user","content":"x"}],"max_tokens":64,"reasoning_effort":"high"}`, "reasoning_budget"},
		{"structured metadata", `{"model":"route","messages":[{"role":"user","content":"x"}],"max_tokens":64,"response_format":{"type":"json_schema","json_schema":{"name":"named","description":"required","strict":true,"schema":{"type":"object"}}}}`, "target_capability"},
		{"unknown required", `{"model":"route","messages":[{"role":"user","content":"x"}],"max_tokens":64,"secret_extension_name":{"required":true}}`, "target_capability"},
		{"tool ordering", `{"model":"route","messages":[{"role":"assistant","content":"before","tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"{}"}}]},{"role":"user","content":"after"}],"max_tokens":64}`, "state_carrier"},
		{"streaming", `{"model":"route","messages":[{"role":"user","content":"x"}],"max_tokens":64,"stream":true}`, "state_carrier"},
		{"native null", `{"model":"route","messages":[{"role":"user","content":"x"}],"max_tokens":null}`, "reasoning_budget"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := compiled.Bind(request(t, openai.FamilyChat, test.body), Context{})
			assertReason(t, err, test.reason)
			if strings.Contains(err.Error(), "secret_extension_name") {
				t.Fatal("error leaked untrusted native field name")
			}
		})
	}
}

func TestNativeSemanticHeadersAndServingHintsNeverDisappear(t *testing.T) {
	source := request(t, openai.FamilyAnthropic, `{"model":"route","messages":[{"role":"user","content":"x"}],"max_tokens":64}`)
	config := configuration(t, "anthropic-messages")
	compiled := template(t, config)
	plan := bind(t, compiled, source, Context{Headers: http.Header{"Anthropic-Version": {"2023-06-01"}, "Anthropic-Beta": {"feature-2026-09-01"}, "Authorization": {"Bearer gateway-key"}}})
	if plan.Config().SemanticHeaders["Anthropic-Beta"] != "feature-2026-09-01" {
		t.Fatal("caller feature header dropped")
	}
	for _, headers := range []http.Header{{"Anthropic-Version": {"foreign"}}, {"Anthropic-Beta": {"first", "second"}}, {"anthropic-beta": {"one"}, "Anthropic-Beta": {"one"}}, {"Anthropic-Beta": {"bad\x01value"}}} {
		_, err := compiled.Bind(source, Context{Headers: headers})
		assertReason(t, err, "target_capability")
	}
	for _, name := range []string{"OpenAI-Organization", "OpenAI-Project", "X-Goog-User-Project", "X-Goog-Request-Params", "X-Ms-Region"} {
		_, err := compiled.Bind(source, Context{Headers: http.Header{name: {"caller-scope"}}})
		assertReason(t, err, "resource_affinity")
	}
	config.Provider.SemanticHeaders = map[string]string{"Anthropic-Beta": "published"}
	_, err := template(t, config).Bind(source, Context{Headers: http.Header{"Anthropic-Beta": {"caller"}}})
	assertReason(t, err, "target_capability")
	gemini := template(t, configuration(t, "gemini-generation"))
	googleRequest := request(t, openai.FamilyGemini, `{"contents":[{"parts":[{"text":"x"}]}]}`)
	googlePlan := bind(t, gemini, googleRequest, Context{Query: url.Values{"$xgafv": {"2"}}})
	if googlePlan.Config().QuerySettings["$xgafv"] != "2" {
		t.Fatal("caller semantic query dropped")
	}
	_, err = gemini.Bind(googleRequest, Context{Query: url.Values{"key": {"must-not-forward"}}})
	assertReason(t, err, "target_capability")
}

func TestNativeStateOmissionAndResourceAffinityAreExplicit(t *testing.T) {
	compiled := template(t, configuration(t, "openai-responses"))
	source := request(t, openai.FamilyResponses, `{"model":"route","input":"hello"}`)
	_, err := compiled.Bind(source, Context{})
	assertReason(t, err, "policy_conflict")
	_, err = compiled.Bind(source, Context{AllowProviderState: true})
	assertReason(t, err, "state_carrier")
	plan := bind(t, compiled, request(t, openai.FamilyResponses, `{"model":"route","input":"hello","store":false}`), Context{})
	prior := request(t, openai.FamilyResponses, `{"model":"route","input":"follow up","previous_response_id":"opaque-native-id","store":false}`)
	_, err = compiled.Bind(prior, Context{AllowProviderState: true})
	assertReason(t, err, "resource_affinity")
	identity := plan.Serving()
	_, err = compiled.Bind(prior, Context{AllowProviderState: true, RequiredServing: &identity})
	assertReason(t, err, "state_carrier")
	identity.PrincipalID = "different-principal"
	_, err = compiled.Bind(prior, Context{AllowProviderState: true, RequiredServing: &identity})
	assertReason(t, err, "resource_affinity")
}

func TestNativeCloudAnthropicRevisionHeaderBindsToBody(t *testing.T) {
	for _, id := range []string{"vertex-anthropic", "bedrock-anthropic-invoke"} {
		t.Run(id, func(t *testing.T) {
			compiled := template(t, configuration(t, id))
			source := request(t, openai.FamilyAnthropic, `{"model":"route","max_tokens":32,"messages":[{"role":"user","content":"native"}]}`)
			plan := bind(t, compiled, source, Context{Headers: http.Header{"Anthropic-Version": {"2023-06-01"}}})
			if len(field(t, plan.Body(), "anthropic_version")) == 0 || plan.Config().SemanticHeaders["Anthropic-Version"] != "" {
				t.Fatal("cloud API revision was not bound into the hosting body")
			}
			mapped := false
			for _, entry := range plan.Receipt().Dispositions {
				mapped = mapped || entry.Field == "/headers/Anthropic-Version" && entry.Rule == "hosting_api_revision" && entry.Disposition == "mapped"
			}
			if !mapped {
				t.Fatal("cloud API revision mapping missing from receipt")
			}
			_, err := compiled.Bind(source, Context{Headers: http.Header{"Anthropic-Version": {"2099-01-01"}}})
			assertReason(t, err, "target_capability")
		})
	}
}

func TestEffectivePolicyChecksDefaultsAndRejectsOpaqueCoverage(t *testing.T) {
	policy := &contentpolicy.Policy{Rules: []contentpolicy.Rule{{ID: "block_secret", Phase: contentpolicy.PhaseInput, Action: contentpolicy.ActionBlock, Pattern: "classified"}}}
	config := configuration(t, "openai-chat")
	config.Policy = policy
	config.Provider.OperationDefaults = map[string]connectors.DefaultSet{"generation": {Dialect: "openai-chat", Values: map[string]json.RawMessage{"tools": json.RawMessage(`[{"type":"function","function":{"name":"f","description":"classified default instruction","parameters":{"type":"object"}}}]`)}}}
	plan := bind(t, template(t, config), request(t, openai.FamilyChat, `{"model":"route","messages":[{"role":"user","content":"harmless"}]}`), Context{})
	decisions, err := plan.CheckInput()
	assertReason(t, err, "content_policy_blocked")
	if len(decisions) != 1 || decisions[0].Outcome != contentpolicy.OutcomeBlocked {
		t.Fatalf("unsafe policy decision: %+v", decisions)
	}
	encoded, _ := json.Marshal(plan.Receipt())
	if bytes.Contains(encoded, []byte("classified")) || bytes.Contains(encoded, []byte("harmless")) {
		t.Fatal("receipt leaked input values")
	}
	config.Provider.OperationDefaults = nil
	compiled := template(t, config)
	for _, body := range []string{`{"model":"route","messages":[{"role":"user","content":"x"}],"future_native":"opaque"}`, `{"model":"route","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`} {
		_, err := compiled.Bind(request(t, openai.FamilyChat, body), Context{})
		assertReason(t, err, "policy_conflict")
	}
	ordinary := bind(t, compiled, request(t, openai.FamilyChat, `{"model":"route","messages":[{"role":"user","content":"public"}]}`), Context{})
	if _, err := ordinary.CheckInput(); err != nil {
		t.Fatal(err)
	}
	config.Policy = &contentpolicy.Policy{Rules: []contentpolicy.Rule{{ID: "redact", Phase: "input", Action: "redact", Pattern: "secret"}}}
	_, err = Compile(config)
	assertReason(t, err, "policy_conflict")
}

func TestQualifiedOutputGuardRejectsLossBeforeProjection(t *testing.T) {
	plan := bind(t, template(t, configuration(t, "anthropic-messages")), request(t, openai.FamilyChat, `{"model":"route","messages":[{"role":"user","content":"x"}],"max_tokens":64}`), Context{})
	valid := map[string]any{"id": "message-1", "type": "message", "role": "assistant", "model": "native-model", "content": []any{map[string]any{"type": "text", "text": "visible"}}, "stop_reason": "end_turn", "stop_sequence": nil, "usage": map[string]any{"input_tokens": 2, "output_tokens": 1}}
	for _, mutate := range []func(map[string]any){
		func(v map[string]any) {
			v["content"] = []any{map[string]any{"type": "thinking", "thinking": "secret", "signature": "opaque"}}
		},
		func(v map[string]any) {
			v["content"] = []any{map[string]any{"type": "text", "text": "before"}, map[string]any{"type": "text", "text": "after"}}
		},
		func(v map[string]any) {
			v["content"] = []any{map[string]any{"type": "text", "text": "cited", "citations": []any{map[string]any{"document_index": 0}}}}
		},
		func(v map[string]any) { v["native_future"] = true },
		func(v map[string]any) { v["stop_reason"] = "pause_turn" },
		func(v map[string]any) {
			v["usage"] = map[string]any{"input_tokens": 2, "output_tokens": 1, "unknown_tokens": 0}
		},
	} {
		body, _ := json.Marshal(valid)
		var changed map[string]any
		json.Unmarshal(body, &changed)
		mutate(changed)
		body, _ = json.Marshal(changed)
		assertReason(t, plan.ValidateUnary(body), "fidelity_protocol_violation")
	}
}

func TestNativeGuardsKeepExtensionsAndRejectCorruption(t *testing.T) {
	compiled := template(t, configuration(t, "anthropic-messages"))
	source := request(t, openai.FamilyAnthropic, `{"model":"route","messages":[{"role":"user","content":"x"}],"max_tokens":64,"stream":true}`)
	plan := bind(t, compiled, source, Context{})
	event, err := openai.LiftEvent(openai.FamilyAnthropic, `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"opaque"},"future":{"exact":true}}`, "content_block_delta", 2, 8192)
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.ValidateEvent(event); err != nil {
		t.Fatal(err)
	}
	invalid, _ := openai.LiftEvent(openai.FamilyAnthropic, `{"type":"content_block_delta","index":"zero","delta":{}}`, "content_block_delta", 3, 8192)
	assertReason(t, plan.ValidateEvent(invalid), "fidelity_protocol_violation")
	assertReason(t, plan.ValidateUnary([]byte(`{"content":[]}`)), "fidelity_protocol_violation")
}

func TestSharedTemplateConcurrentBindingsRemainIndependent(t *testing.T) {
	compiled := template(t, configuration(t, "openai-chat"))
	source := request(t, openai.FamilyChat, `{"model":"route","messages":[{"role":"user","content":"unchanged"}]}`)
	var group sync.WaitGroup
	for range 20 {
		group.Go(func() {
			plan, err := compiled.Bind(source, Context{})
			if err != nil {
				t.Error(err)
				return
			}
			body := plan.Body()
			body[0] = '['
			receipt := plan.Receipt()
			receipt.Dispositions[0].Rule = "private"
		})
	}
	group.Wait()
	if string(source.Field("model")) != `"route"` {
		t.Fatal("shared source mutated")
	}
}

func TestNativeStreamingDefaultsPrecedeTransportOverlay(t *testing.T) {
	config := configuration(t, "openai-chat")
	config.Provider.OperationDefaults = map[string]connectors.DefaultSet{"generation": {Dialect: "openai-chat", Values: map[string]json.RawMessage{"stream_options": json.RawMessage(`{"include_usage":false,"include_obfuscation":false}`)}}}
	source := request(t, openai.FamilyChat, `{"model":"route","messages":[{"role":"user","content":"hello"}],"stream":true}`)
	plan := bind(t, template(t, config), source, Context{})
	var options map[string]bool
	if err := json.Unmarshal(field(t, plan.Body(), "stream_options"), &options); err != nil {
		t.Fatal(err)
	}
	if enabled, present := options["include_obfuscation"]; !present || enabled || !options["include_usage"] {
		t.Fatalf("transport overlay suppressed an absent-only default: %s", plan.Body())
	}
	if source.Field("stream_options") != nil {
		t.Fatal("source transport controls mutated")
	}
	if plan.Obligations().Delivery != "incremental" || plan.Obligations().Submission != "immediate" {
		t.Fatal("execution dimensions missing")
	}
}

func TestNativeExecutionEffectsAreScopedAndImmutable(t *testing.T) {
	compiled := template(t, configuration(t, "openai-responses"))
	source := request(t, openai.FamilyResponses, `{"model":"route","input":"hello","store":false,"tools":[{"type":"function","name":"local","parameters":{"type":"object"}}]}`)
	plan := bind(t, compiled, source, Context{AllowProviderState: true})
	effects := plan.Obligations().Effects
	if !reflect.DeepEqual(effects, []string{"inference", "client_tool_call"}) {
		t.Fatalf("wrong declared effects: %v", effects)
	}
	effects[0] = "mutated"
	receipt := plan.Receipt()
	receipt.Obligations.Effects[0] = "also-mutated"
	if plan.Obligations().Effects[0] != "inference" {
		t.Fatal("effect metadata exposes mutable aliases")
	}
	hosted := request(t, openai.FamilyResponses, `{"model":"route","input":"hello","tools":[{"type":"web_search"}]}`)
	_, err := compiled.Bind(hosted, Context{AllowProviderState: true})
	assertReason(t, err, "state_carrier")
}

func TestNestedUnknownControlsCannotBypassInputPolicyCoverage(t *testing.T) {
	config := configuration(t, "gemini-generation")
	config.Policy = &contentpolicy.Policy{Rules: []contentpolicy.Rule{{ID: "input", Phase: "input", Action: "block", Pattern: "secret"}}}
	source := request(t, openai.FamilyGemini, `{"contents":[{"parts":[{"text":"safe"}]}],"generationConfig":{"future_encoded":"c2VjcmV0"}}`)
	_, err := template(t, config).Bind(source, Context{})
	assertReason(t, err, "policy_conflict")
}

func TestNativeAdapterCannotOverrideImmutableSourceIdentity(t *testing.T) {
	compiled := template(t, configuration(t, "openai-chat"))
	source := request(t, openai.FamilyChat, `{"model":"route","messages":[{"role":"user","content":"safe"}]}`)
	source.Stream = true
	_, err := compiled.Bind(source, Context{})
	assertReason(t, err, "target_capability")
	source.Stream = false
	source.Family = openai.FamilyResponses
	_, err = compiled.Bind(source, Context{})
	assertReason(t, err, "target_capability")
}

func TestNativeChatDefaultCannotIntroduceConflictingBudgetScope(t *testing.T) {
	for _, test := range []struct{ caller, configured, value string }{
		{"max_tokens", "max_completion_tokens", "64"},
		{"max_completion_tokens", "max_tokens", "64"},
		{"max_tokens", "max_completion_tokens", "null"},
	} {
		t.Run(test.caller+"_"+test.value, func(t *testing.T) {
			config := configuration(t, "openai-chat")
			config.Provider.OperationDefaults = map[string]connectors.DefaultSet{"generation": {Dialect: "openai-chat", Values: map[string]json.RawMessage{test.configured: json.RawMessage(`128`)}}}
			source := request(t, openai.FamilyChat, `{"model":"route","messages":[{"role":"user","content":"hello"}],"`+test.caller+`":`+test.value+`}`)
			before := source.OIF().Document().Bytes()
			_, err := template(t, config).Bind(source, Context{})
			assertReason(t, err, "reasoning_budget")
			if !bytes.Equal(before, source.OIF().Document().Bytes()) {
				t.Fatal("budget collision repaired the caller source")
			}
		})
	}
}
