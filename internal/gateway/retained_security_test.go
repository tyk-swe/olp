package gateway

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/tyk-swe/olp/internal/routing"

	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/resources"
)

func TestRetainedNativeContentHonorsCurrentGuardrails(t *testing.T) {
	h := newHarness(t, Config{})
	installTestGuardrail(t, h, guardrails.Output, guardrails.Block, "builtin.keywords", `{"words":["secret"]}`)
	route := h.rt.release.Snapshot.Routes[routeSlug]
	for _, item := range []struct {
		kind    string
		family  wire.Format
		adapter string
	}{
		{resources.KindAnthropicFile, wire.AnthropicFile, ""},
		{resources.KindAnthropicBatch, wire.AnthropicBatch, ""},
		{resources.KindGeminiFile, wire.GeminiFile, ""},
		{resources.KindGeminiBatch, wire.GeminiBatch, ""},
		{resources.KindFile, wire.File, "together"},
		{resources.KindFile, wire.File, "mistral"},
		{resources.KindFile, wire.File, "gemini_managed"},
		{resources.KindBatch, wire.Batch, "together"},
		{resources.KindBatch, wire.Batch, "mistral"},
		{resources.KindBatch, wire.Batch, "gemini_managed"},
	} {
		x := &call{request: request{release: h.rt.release}, keyID: h.keyID, family: item.family}
		metadata, _ := json.Marshal(map[string]string{"adapter": item.adapter})
		resource := &resources.Resource{Kind: item.kind, Metadata: metadata}
		if err := x.retainedContentGate(resource, &route, retainedRetrieval); err == nil || err.Code != "guardrail_surface_unavailable" {
			t.Fatalf("retained %s content bypassed enforcing output policy: %v", item.kind, err)
		}
		if err := x.retainedContentGate(resource, &route, retainedHousekeeping); err != nil {
			t.Fatalf("retained %s cleanup was blocked by output policy: %v", item.kind, err)
		}
	}
}

func TestRetainedNewWorkUsesCurrentPoliciesAndSupply(t *testing.T) {
	credential := "historical-secret"
	defaultValue := json.RawMessage(`32`)
	before := int64(2048)
	now := int64(1024)
	slot := routing.Slot{ID: "slot", Enabled: true, CredentialID: &credential, Supply: routing.Supply{DailyCostLimit: new("100"), MonthlyCostLimit: new("50")}}
	provider := routing.Provider{ID: "provider", Enabled: true, Endpoint: "https://old.example/v1", ParameterDefaults: map[string]json.RawMessage{"max_tokens": defaultValue}}
	target := routing.Target{ProviderID: provider.ID, ProviderModel: "model"}
	route := routing.Route{ID: "route-id", Slug: "route", Operations: []string{"generation"}, Targets: []routing.Target{target}, OverallTimeout: 2000, MaxAttempts: 2,
		Behavior: routing.Behavior{MaxBodyBytes: &before, CallerCostExempt: true, Budget: &routing.CostLimits{DailyCostLimit: new("100"), MonthlyCostLimit: new("5")}}}
	policy := &routing.Policy{Constraints: routing.Preferences{Only: []string{provider.ID}}}
	content := &contentpolicy.Policy{Rules: []contentpolicy.Rule{{ID: "block", Phase: contentpolicy.PhaseInput, Action: contentpolicy.ActionBlock, Pattern: "secret"}}}
	liveRoute := route
	liveRoute.Policy, liveRoute.ContentPolicy = policy, content
	liveRoute.CallerCostExempt = false
	liveRoute.Budget = &routing.CostLimits{DailyCostLimit: new("10"), MonthlyCostLimit: new("50")}
	liveRoute.MaxBodyBytes, liveRoute.OverallTimeout, liveRoute.MaxAttempts = &now, 1000, 1
	supply := routing.Supply{DailyCostLimit: new("10"), MonthlyCostLimit: new("100"), SaturationPercent: new(int64(90)), PriorityShares: &routing.Shares{Critical: 40, High: 30, Normal: 20, Low: 10}}
	live := routing.Provider{ID: provider.ID, Enabled: true, Endpoint: "https://new.example/v1", ParameterDefaults: map[string]json.RawMessage{"max_tokens": json.RawMessage(`999`)}, Limits: &routing.Limits{Supply: supply}, Slots: []routing.Slot{{ID: slot.ID, Enabled: true, CredentialID: new("new-secret"), Supply: supply}}}
	snapshot := &routing.Snapshot{Providers: map[string]routing.Provider{provider.ID: live}, Routes: map[string]routing.Route{route.Slug: liveRoute}}
	beforeSnapshot, _ := json.Marshal(snapshot)
	if e := restrictRetainedWork(snapshot, "key", &provider, &route, &slot, &target, "generation"); e != nil {
		t.Fatal(e)
	}
	if provider.Endpoint != "https://old.example/v1" || string(provider.ParameterDefaults["max_tokens"]) != "32" || *slot.CredentialID != credential {
		t.Fatal("current authority changed historical connection or defaults")
	}
	if route.Policy != policy || route.ContentPolicy != content || route.CallerCostExempt || *route.MaxBodyBytes != now || route.OverallTimeout != 1000 || route.MaxAttempts != 1 {
		t.Fatalf("current route policy was not applied: %+v", route)
	}
	if *route.Budget.DailyCostLimit != "10" || *route.Budget.MonthlyCostLimit != "5" || *provider.Limits.DailyCostLimit != "10" || *slot.DailyCostLimit != "10" || *slot.MonthlyCostLimit != "50" || !reflect.DeepEqual(slot.PriorityShares, supply.PriorityShares) {
		t.Fatalf("retained work bypassed cost or priority controls: route=%+v provider=%+v slot=%+v", route.Budget, provider.Limits, slot)
	}
	afterSnapshot, _ := json.Marshal(snapshot)
	if string(beforeSnapshot) != string(afterSnapshot) {
		t.Fatal("retained restriction mutated the installed snapshot")
	}
}

// A relaxed current timeout cannot loosen the pinned contract.
