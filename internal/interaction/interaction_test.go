package interaction

import (
	"errors"
	"net/http"
	"net/url"
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

func assertReason(t *testing.T, err error, reason string) {
	t.Helper()
	var failure *oif.Incompatibility
	if !errors.As(err, &failure) || failure.Code != reason {
		t.Fatalf("reason: got %v, want %s", err, reason)
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
