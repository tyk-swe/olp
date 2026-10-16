package routing

import (
	"encoding/json"
	"strings"
	"testing"
)

// A default slot can deliberately carry no secret (for example workload identity).

func TestDecodeRevisionSerializationAndPublicationLimits(t *testing.T) {
	for _, raw := range []string{`null`, `[]`} {
		provider, err := DecodeProviderRevision(ProviderRevision{ID: "provider", RevisionID: "revision", State: "active", Configuration: []byte(`{"kind":"openai","options":{"limits":{}}}`), Models: []byte(raw), Slots: []byte(raw)})
		if err != nil {
			t.Fatal(err)
		}
		if provider.Limits == nil || provider.Capabilities == nil || provider.Slots != nil {
			t.Fatalf("stored presence changed: %+v", provider)
		}
		want := `{"id":"provider","name":"","kind":"openai","enabled":true,"active_credential":null,"capabilities":[],"revision_id":"revision","limits":{}}`
		assertRevisionJSON(t, provider, want)
		provider.Limits = provider.Limits.Configured()
		assertRevisionJSON(t, provider, strings.Replace(want, `,"limits":{}`, "", 1))
	}
	for _, tc := range []struct{ operations, want string }{{`null`, `null`}, {`[]`, `[]`}} {
		route, err := DecodeRouteRevision(RouteRevision{ID: "route", Slug: "chat", Operations: []byte(tc.operations), Targets: []byte(`[]`)})
		if err != nil {
			t.Fatal(err)
		}
		assertRevisionJSON(t, route, `{"id":"route","slug":"chat","operations":`+tc.want+`,"overall_timeout":0,"max_attempts":0,"targets":null,"routing_id":"route","published_at":"0001-01-01T00:00:00Z","fidelity":{"mode":"strict"}}`)
	}
}

func assertRevisionJSON(t *testing.T, value any, want string) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != want {
		t.Fatalf("serialized revision = %s, want %s", encoded, want)
	}
}
