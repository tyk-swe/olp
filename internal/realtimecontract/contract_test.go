package realtimecontract_test

import (
	"net/http"
	"testing"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/realtimecontract"
)

func realtimeConfig(profile, kind, endpoint string) realtimecontract.Config {
	return realtimecontract.Config{Provider: connectors.Config{
		Kind: kind, ProfileID: profile, ProfileRevision: connectors.ProfileRevision,
		AuthMode: "api_key", Endpoint: endpoint,
	}, Model: "native-model"}
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
