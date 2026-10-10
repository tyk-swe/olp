package gateway

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/runtime"
)

func TestRetainedNewWorkUsesCurrentRestrictions(t *testing.T) {
	for _, change := range []string{"allowed", "provider disabled", "slot removed", "slot disabled", "key removed", "model removed", "route removed", "target removed", "operation removed", "route replaced", "target shadowed", "historical slot disabled"} {
		t.Run(change, func(t *testing.T) {
			slot := runtime.Slot{ID: "slot", Enabled: true}
			provider := runtime.Provider{ID: "provider", Enabled: true, Slots: []runtime.Slot{slot}}
			target := runtime.Target{ProviderID: "provider", ProviderModel: "model"}
			route := runtime.Route{Slug: "route", Operations: []string{"generation"}, Targets: []runtime.Target{target}}
			live := provider
			live.Slots = []runtime.Slot{slot}
			switch change {
			case "provider disabled":
				live.Enabled = false
			case "slot removed":
				live.Slots = nil
			case "slot disabled":
				live.Slots[0].Enabled = false
			case "key removed":
				live.Slots[0].AllowedAPIKeys = []string{"other"}
			case "model removed":
				live.Slots[0].AllowedModels = []string{"other"}
			case "route removed":
				live.Slots[0].AllowedRoutes = []string{"other"}
			case "target removed":
				route.Targets = nil
			case "operation removed":
				route.Operations = nil
			case "target shadowed":
				route.Targets[0].Shadow = &runtime.Shadow{SampleRate: 1}
			case "historical slot disabled":
				slot.Enabled = false
			}
			snapshot := &runtime.Snapshot{Providers: map[string]runtime.Provider{"provider": live}, Routes: map[string]runtime.Route{"route": route}}
			if change == "route replaced" {
				replaced := route
				replaced.ID = "replacement"
				snapshot.Routes[route.Slug] = replaced
			}
			err := restrictRetainedWork(snapshot, "key", &provider, &route, &slot, &target, "generation")
			if (err == nil) != (change == "allowed") {
				t.Fatalf("restriction %s: %v", change, err)
			}
		})
	}
}

func TestRetainedNewWorkUsesTighterLimitsWithoutChangingCredential(t *testing.T) {
	old, current := int64(100), int64(10)
	credential := "historical-secret"
	slot := runtime.Slot{ID: "slot", Enabled: true, CredentialID: &credential, TokensPerMinute: &old, MaxConcurrency: &current}
	provider := runtime.Provider{ID: "provider", Enabled: true, Limits: &runtime.Limits{TokensPerMinute: &old}}
	target := runtime.Target{ProviderID: "provider", ProviderModel: "model"}
	route := runtime.Route{Slug: "route", Operations: []string{"generation"}, Targets: []runtime.Target{target}}
	live := provider
	live.Limits = &runtime.Limits{TokensPerMinute: &current}
	live.Slots = []runtime.Slot{{ID: "slot", Enabled: true, TokensPerMinute: &current, MaxConcurrency: &old}}
	snapshot := &runtime.Snapshot{Providers: map[string]runtime.Provider{"provider": live}, Routes: map[string]runtime.Route{"route": route}}
	if e := restrictRetainedWork(snapshot, "key", &provider, &route, &slot, &target, "generation"); e != nil {
		t.Fatal(e)
	}
	if *provider.Limits.TokensPerMinute != 10 || *slot.TokensPerMinute != 10 || *slot.MaxConcurrency != 10 || *slot.CredentialID != credential {
		t.Fatalf("wrong retained authority: %+v %+v", provider, slot)
	}
	if *snapshot.Providers["provider"].Limits.TokensPerMinute != 10 {
		t.Fatal("mutated installed release")
	}
}

func TestRetainedNewWorkUsesCurrentPoliciesAndSupply(t *testing.T) {
	credential := "historical-secret"
	defaultValue := json.RawMessage(`32`)
	before := int64(2048)
	now := int64(1024)
	slot := runtime.Slot{ID: "slot", Enabled: true, CredentialID: &credential, Supply: runtime.Supply{DailyCostLimit: new("100"), MonthlyCostLimit: new("50")}}
	provider := runtime.Provider{ID: "provider", Enabled: true, Endpoint: "https://old.example/v1", ParameterDefaults: map[string]json.RawMessage{"max_tokens": defaultValue}}
	target := runtime.Target{ProviderID: provider.ID, ProviderModel: "model"}
	route := runtime.Route{ID: "route-id", Slug: "route", Operations: []string{"generation"}, Targets: []runtime.Target{target}, OverallTimeout: 2000, MaxAttempts: 2,
		Behavior: runtime.Behavior{MaxBodyBytes: &before, CallerCostExempt: true, Budget: &runtime.CostLimits{DailyCostLimit: new("100"), MonthlyCostLimit: new("5")}}}
	policy := &runtime.Policy{Constraints: runtime.Preferences{Only: []string{provider.ID}}}
	content := &contentpolicy.Policy{Rules: []contentpolicy.Rule{{ID: "block", Phase: contentpolicy.PhaseInput, Action: contentpolicy.ActionBlock, Pattern: "secret"}}}
	liveRoute := route
	liveRoute.Policy, liveRoute.ContentPolicy = policy, content
	liveRoute.CallerCostExempt = false
	liveRoute.Budget = &runtime.CostLimits{DailyCostLimit: new("10"), MonthlyCostLimit: new("50")}
	liveRoute.MaxBodyBytes, liveRoute.OverallTimeout, liveRoute.MaxAttempts = &now, 1000, 1
	supply := runtime.Supply{DailyCostLimit: new("10"), MonthlyCostLimit: new("100"), SaturationPercent: new(int64(90)), PriorityShares: &runtime.Shares{Critical: 40, High: 30, Normal: 20, Low: 10}}
	live := runtime.Provider{ID: provider.ID, Enabled: true, Endpoint: "https://new.example/v1", ParameterDefaults: map[string]json.RawMessage{"max_tokens": json.RawMessage(`999`)}, Limits: &runtime.Limits{Supply: supply}, Slots: []runtime.Slot{{ID: slot.ID, Enabled: true, CredentialID: new("new-secret"), Supply: supply}}}
	snapshot := &runtime.Snapshot{Providers: map[string]runtime.Provider{provider.ID: live}, Routes: map[string]runtime.Route{route.Slug: liveRoute}}
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

func TestRetainedNewWorkClampsAttemptTimeoutToCurrentTarget(t *testing.T) {
	target := runtime.Target{ProviderID: "provider", ProviderModel: "model", Timeout: 5000}
	route := runtime.Route{ID: "route-id", Slug: "route", Operations: []string{"generation"}, Targets: []runtime.Target{target}}
	liveRoute := route
	liveRoute.Targets = []runtime.Target{{ProviderID: "provider", ProviderModel: "model", Timeout: 1000}}
	provider := runtime.Provider{ID: "provider", Enabled: true, Slots: []runtime.Slot{{ID: "slot", Enabled: true}}}
	slot := provider.Slots[0]
	snapshot := &runtime.Snapshot{Providers: map[string]runtime.Provider{"provider": provider}, Routes: map[string]runtime.Route{"route": liveRoute}}
	if e := restrictRetainedWork(snapshot, "key", &provider, &route, &slot, &target, "generation"); e != nil {
		t.Fatal(e)
	}
	if target.Timeout != 1000 {
		t.Fatalf("retained timeout not clamped to current target: %d", target.Timeout)
	}
	// A relaxed current timeout cannot loosen the pinned contract.
	target.Timeout = 5000
	liveRoute.Targets[0].Timeout = 9000
	snapshot.Routes["route"] = liveRoute
	if e := restrictRetainedWork(snapshot, "key", &provider, &route, &slot, &target, "generation"); e != nil {
		t.Fatal(e)
	}
	if target.Timeout != 5000 {
		t.Fatalf("current timeout loosened the pinned contract: %d", target.Timeout)
	}
}
