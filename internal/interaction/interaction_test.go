package interaction

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/wire"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols"
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
func request(t *testing.T, family wire.Format, body string) *wire.Request {
	t.Helper()
	if family == wire.Bedrock {
		document, err := oif.ParseJSON([]byte(body), oif.Limits{})
		if err != nil {
			t.Fatal(err)
		}
		request, err := wire.NewRequest(family, "route", false, document)
		if err != nil {
			t.Fatal(err)
		}
		return request
	}
	result, err := protocols.Parse(family, []byte(body), "route")
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func bind(t *testing.T, template *Template, request *wire.Request, context Context) *Plan {
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
	var failure *oif.Incompatibility
	if !errors.As(err, &failure) || failure.Code != reason {
		t.Fatalf("reason: got %v, want %s", err, reason)
	}
}

func TestQualifiedTextMatchesIndependentNativeFixture(t *testing.T) {
	config := configuration(t, "anthropic-messages")
	config.Model = "model-a"
	source := request(t, wire.Chat, `{"model":"team-chat","messages":[{"role":"user","content":"hello"}],"max_tokens":64,"stream":false}`)
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
	result, err := protocols.DecodeRequest(plan.Wire(), wire.Chat, []byte(valid), "team-chat", "", source)
	if err != nil || result.OutputText != "hello" || result.FinishReason != "stop" || result.Usage.InputTokens != 2 || result.Usage.OutputTokens != 1 {
		t.Fatalf("qualified client projection lost text/usage: %+v %v", result, err)
	}
	realistic := strings.Replace(valid, `"usage":{"input_tokens":2,"output_tokens":1}`, `"usage":{"input_tokens":2,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":0},"output_tokens":1,"service_tier":"standard"}`, 1)
	if err := plan.ValidateUnary([]byte(realistic)); err != nil {
		t.Fatalf("real Anthropic usage shape rejected: %v", err)
	}
	result, err = protocols.DecodeRequest(plan.Wire(), wire.Chat, []byte(realistic), "team-chat", "", source)
	if err != nil || result.Usage.InputTokens != 2 || result.Usage.OutputTokens != 1 {
		t.Fatalf("qualified client projection lost realistic usage: %+v %v", result, err)
	}
	for _, usage := range []string{
		`{"input_tokens":2,"output_tokens":1,"future_token_category":1}`,
		`{"input_tokens":2,"output_tokens":1,"cache_creation_input_tokens":-1}`,
		`{"input_tokens":2,"output_tokens":1,"cache_creation":{"ephemeral_2h_input_tokens":0}}`,
		`{"input_tokens":2,"output_tokens":1,"service_tier":7}`,
	} {
		invalid := strings.Replace(valid, `{"input_tokens":2,"output_tokens":1}`, usage, 1)
		assertReason(t, plan.ValidateUnary([]byte(invalid)), "fidelity_protocol_violation")
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
			_, err := compiled.Bind(request(t, wire.Chat, test.body), Context{})
			assertReason(t, err, test.reason)
			if strings.Contains(err.Error(), "secret_extension_name") {
				t.Fatal("error leaked untrusted native field name")
			}
		})
	}
}

func TestNativeSemanticHeadersAndServingHintsNeverDisappear(t *testing.T) {
	source := request(t, wire.Anthropic, `{"model":"route","messages":[{"role":"user","content":"x"}],"max_tokens":64}`)
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
	googleRequest := request(t, wire.Gemini, `{"contents":[{"parts":[{"text":"x"}]}]}`)
	googlePlan := bind(t, gemini, googleRequest, Context{Query: url.Values{"$xgafv": {"2"}}})
	if googlePlan.Config().QuerySettings["$xgafv"] != "2" {
		t.Fatal("caller semantic query dropped")
	}
	_, err = gemini.Bind(googleRequest, Context{Query: url.Values{"key": {"must-not-forward"}}})
	assertReason(t, err, "target_capability")
}

func TestNativeCloudAnthropicRevisionHeaderBindsToBody(t *testing.T) {
	for _, id := range []string{"vertex-anthropic", "bedrock-anthropic-invoke"} {
		t.Run(id, func(t *testing.T) {
			compiled := template(t, configuration(t, id))
			source := request(t, wire.Anthropic, `{"model":"route","max_tokens":32,"messages":[{"role":"user","content":"native"}]}`)
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
