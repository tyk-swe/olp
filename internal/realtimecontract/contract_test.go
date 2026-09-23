package realtimecontract_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/realtimecontract"
)

func realtimeConfig(profile, kind, endpoint string) realtimecontract.Config {
	return realtimecontract.Config{Provider: connectors.Config{
		Kind: kind, ProfileID: profile, ProfileRevision: connectors.ProfileRevision,
		AuthMode: "api_key", Endpoint: endpoint,
	}, Model: "native-model"}
}

func TestStrictRealtimeCompilesOnlyVersionedNativeGAHosting(t *testing.T) {
	for _, tc := range []struct{ profile, kind, endpoint string }{
		{"openai-responses", "openai", "https://api.openai.com/v1"},
		{"azure-v1-responses", "azure_openai", "https://resource.example"},
	} {
		c := realtimeConfig(tc.profile, tc.kind, tc.endpoint)
		if _, err := realtimecontract.Compile(c); err != nil {
			t.Fatalf("%s: %v", tc.profile, err)
		}
	}
	for _, tc := range []struct {
		name string
		edit func(*realtimecontract.Config)
	}{
		{"unversioned", func(c *realtimecontract.Config) { c.Provider.ProfileID, c.Provider.ProfileRevision = "", "" }},
		{"azure-preview", func(c *realtimecontract.Config) {
			c.Provider.ProfileID, c.Provider.APIVersion, c.Provider.Deployment = "azure-legacy-responses", "2025-04-01-preview", "native-model"
		}},
		{"semantic-header", func(c *realtimecontract.Config) {
			c.Provider.SemanticHeaders = map[string]string{"Openai-Beta": "realtime=v1"}
		}},
		{"blocking-policy", func(c *realtimecontract.Config) {
			c.Policy = &contentpolicy.Policy{Rules: []contentpolicy.Rule{{ID: "block", Phase: contentpolicy.PhaseInput, Action: contentpolicy.ActionBlock, Pattern: "secret"}}}
		}},
		{"realtime-default", func(c *realtimecontract.Config) {
			c.Provider.OperationDefaults = map[string]connectors.DefaultSet{"realtime": {Dialect: "openai-realtime", NativeOptions: map[string]json.RawMessage{"voice": json.RawMessage(`"alloy"`)}}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := realtimeConfig("azure-v1-responses", "azure_openai", "https://resource.example")
			tc.edit(&c)
			if _, err := realtimecontract.Compile(c); err == nil {
				t.Fatal("unqualified realtime target compiled")
			}
		})
	}
}

func TestStrictRealtimeHandshakeRefusesControlsThatCannotBeRelayed(t *testing.T) {
	template, err := realtimecontract.Compile(realtimeConfig("azure-v1-responses", "azure_openai", "https://resource.example"))
	if err != nil {
		t.Fatal(err)
	}
	headers := http.Header{"Authorization": {"Bearer local-key"}, "Connection": {"Upgrade"}, "Upgrade": {"websocket"}, "Sec-Websocket-Version": {"13"}, "Sec-Websocket-Key": {"fixture"}}
	if err := template.AdmitHandshake("model=published-route", headers, "published-route"); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		"model=published-route&model=other", "model=published-route&voice=alloy",
		"model=published-route&api-version=2025-04-01-preview", "model=published-route%zz",
	} {
		if err := template.AdmitHandshake(raw, headers, "published-route"); err == nil {
			t.Fatalf("admitted query %q", raw)
		}
	}
	for _, name := range []string{"Openai-Beta", "Openai-Safety-Identifier", "Sec-Websocket-Protocol", "X-Olp-Routing"} {
		copy := headers.Clone()
		copy.Set(name, "unqualified")
		if err := template.AdmitHandshake("model=published-route", copy, "published-route"); err == nil {
			t.Fatalf("admitted unforwarded header %s", name)
		}
	}
}
