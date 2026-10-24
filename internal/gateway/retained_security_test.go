package gateway

import (
	"encoding/json"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/upstream"
	"github.com/tyk-swe/olp/internal/usage"
	"reflect"
	"testing"
	"time"

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
		{resources.KindFile, wire.File, ""},
		{resources.KindFile, wire.File, "together"},
		{resources.KindFile, wire.File, "mistral"},
		{resources.KindFile, wire.File, "gemini_managed"},
		{resources.KindBatch, wire.Batch, "together"},
		{resources.KindBatch, wire.Batch, "mistral"},
		{resources.KindBatch, wire.Batch, "gemini_managed"},
	} {
		x := &call{request: request{release: h.rt.release}, keyID: h.keyID, family: item.family}
		fields := map[string]string{"adapter": item.adapter}
		if item.kind == resources.KindFile && item.adapter == "" {
			fields["file_operation"] = "generation"
		}
		metadata, _ := json.Marshal(fields)
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

func TestRetainedFreeControlKeepsCleanupAvailableUnderLateCaps(t *testing.T) {
	now := time.Now()
	s := &Server{}
	x := &call{retainedHousekeeping: true, operationOverride: "vector_store", request: request{startedAt: now}, routingInputs: &usage.RoutingInputs{RefreshedAt: now}}
	p := &pin{provider: routing.Provider{Kind: "openai"}, model: "m"}
	if err := s.retainedAccountingPolicy(x, p, false); err != nil || !x.retainedFreeControl {
		t.Fatalf("free cleanup refused: %v", err)
	}
	authority := access.Authority{Policy: access.KeyPolicy{TokensPerMinute: new(int64(100)), RequestsPerMinute: new(int64(5)), DailyCostLimit: new("1")}}
	admitted := x.admissionAuthority(authority)
	if admitted.Policy.DailyCostLimit != nil || admitted.Policy.TokensPerMinute != nil || admitted.Policy.RequestsPerMinute == nil || authority.Policy.TokensPerMinute == nil {
		t.Fatal("cleanup did not preserve request controls and original authority")
	}
	x.routingInputs.Prices = []usage.RoutingPrice{{Price: usage.Price{ProviderKind: "openai", Model: "m", Operation: "vector_store", PerRequest: new("1")}, EffectiveAt: now.Add(-time.Hour)}}
	if err := s.retainedAccountingPolicy(x, p, false); err != nil || x.retainedFreeControl {
		t.Fatalf("paid control lost cost admission: %v", err)
	}
}

func TestRetainedRejectionsNeverClearAmbiguousAcceptance(t *testing.T) {
	for _, status := range []int{408, 409, 500, 502, 503} {
		if retainedRejectedWithoutWork(&attemptFailure{status: status, acceptance: upstream.Terminal}) {
			t.Fatalf("ambiguous status %d cleared liability", status)
		}
	}
	if !retainedRejectedWithoutWork(&attemptFailure{status: 400, acceptance: upstream.Terminal}) || !retainedRejectedWithoutWork(&attemptFailure{acceptance: upstream.NotSent}) {
		t.Fatal("definitive rejection retained paid liability")
	}
	if retainedRejectedWithoutWork(&attemptFailure{status: 400, acceptance: upstream.Terminal, accepted: true}) {
		t.Fatal("accepted work cleared by rejection")
	}
}
