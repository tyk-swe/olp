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
	slot := Slot{
		RequestsPerMinute: new(int64(10)), TokensPerMinute: new(int64(100)), InputTokensPerMinute: new(int64(80)), OutputTokensPerMinute: new(int64(20)),
		MaxConcurrency: new(int64(2)), Supply: Supply{DailyCostLimit: new("2.00")},
	}
	changed := func(change func(*Slot)) Slot {
		live := slot
		change(&live)
		return live
	}
	if !CodePaidLimitsMatch(provider, &Limits{RequestsPerMinute: new(int64(10))}, slot, slot) {
		t.Fatal("equivalent live limits refused frozen serving policy")
	}
	for name, live := range map[string]Slot{
		"requests":      changed(func(s *Slot) { s.RequestsPerMinute = new(int64(20)) }),
		"tokens":        changed(func(s *Slot) { s.TokensPerMinute = new(int64(200)) }),
		"input tokens":  changed(func(s *Slot) { s.InputTokensPerMinute = new(int64(90)) }),
		"output tokens": changed(func(s *Slot) { s.OutputTokensPerMinute = nil }),
		"concurrency":   changed(func(s *Slot) { s.MaxConcurrency = new(int64(3)) }),
		"supply":        changed(func(s *Slot) { s.Supply = Supply{DailyCostLimit: new("3.00")} }),
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

func TestSplitTokenLimitsCountAsLimitsOnTheirOwn(t *testing.T) {
	for name, limits := range map[string]Limits{
		"input":  {InputTokensPerMinute: new(int64(80))},
		"output": {OutputTokensPerMinute: new(int64(20))},
	} {
		t.Run(name, func(t *testing.T) {
			if limits.Configured() == nil {
				t.Fatal("publication dropped a connection limited on one token dimension only")
			}
			if !UnboundedWorkBudgeted(&Provider{Limits: &limits}, nil, nil, true) {
				t.Fatal("unbounded work passed a connection limited on one token dimension only")
			}
			slot := Slot{InputTokensPerMinute: limits.InputTokensPerMinute, OutputTokensPerMinute: limits.OutputTokensPerMinute}
			if !UnboundedWorkBudgeted(&Provider{}, &slot, nil, true) {
				t.Fatal("unbounded work passed a slot limited on one token dimension only")
			}
		})
	}
}

func TestCloneKeepsSplitTokenLimitsApart(t *testing.T) {
	limits := Limits{InputTokensPerMinute: new(int64(80)), OutputTokensPerMinute: new(int64(20))}
	source := &Snapshot{Providers: map[string]Provider{"provider": {
		Limits: &limits,
		Slots:  []Slot{{InputTokensPerMinute: new(int64(8)), OutputTokensPerMinute: new(int64(2))}},
	}}}
	clone := source.Clone().Providers["provider"]
	*clone.Limits.InputTokensPerMinute, *clone.Limits.OutputTokensPerMinute = 1, 1
	*clone.Slots[0].InputTokensPerMinute, *clone.Slots[0].OutputTokensPerMinute = 1, 1
	original := source.Providers["provider"]
	if *original.Limits.InputTokensPerMinute != 80 || *original.Limits.OutputTokensPerMinute != 20 ||
		*original.Slots[0].InputTokensPerMinute != 8 || *original.Slots[0].OutputTokensPerMinute != 2 {
		t.Fatalf("editing a clone changed the source limits: %+v, slot %+v", original.Limits, original.Slots[0])
	}
}
