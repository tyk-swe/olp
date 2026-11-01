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

func TestCodePaidLimitsMatchServingPolicy(t *testing.T) {
	provider := &Limits{RequestsPerMinute: new(int64(10))}
	slot := Slot{RequestsPerMinute: new(int64(10)), TokensPerMinute: new(int64(100)), MaxConcurrency: new(int64(2)), Supply: Supply{DailyCostLimit: new("2.00")}}
	if !CodePaidLimitsMatch(provider, &Limits{RequestsPerMinute: new(int64(10))}, slot, slot) {
		t.Fatal("equivalent live limits refused frozen serving policy")
	}
	for name, live := range map[string]Slot{
		"requests":    {RequestsPerMinute: new(int64(20)), TokensPerMinute: slot.TokensPerMinute, MaxConcurrency: slot.MaxConcurrency, Supply: slot.Supply},
		"tokens":      {RequestsPerMinute: slot.RequestsPerMinute, TokensPerMinute: new(int64(200)), MaxConcurrency: slot.MaxConcurrency, Supply: slot.Supply},
		"concurrency": {RequestsPerMinute: slot.RequestsPerMinute, TokensPerMinute: slot.TokensPerMinute, MaxConcurrency: new(int64(3)), Supply: slot.Supply},
		"supply":      {RequestsPerMinute: slot.RequestsPerMinute, TokensPerMinute: slot.TokensPerMinute, MaxConcurrency: slot.MaxConcurrency, Supply: Supply{DailyCostLimit: new("3.00")}},
	} {
		t.Run(name, func(t *testing.T) {
			if CodePaidLimitsMatch(provider, provider, live, slot) {
				t.Fatal("changed live slot limits admitted frozen policy")
			}
		})
	}
	if CodePaidLimitsMatch(&Limits{RequestsPerMinute: new(int64(20))}, provider, slot, slot) {
		t.Fatal("changed live provider limits admitted frozen policy")
	}
	if CodePaidLimitsMatch(nil, provider, slot, slot) {
		t.Fatal("removing live provider limits admitted frozen policy")
	}
}
