package gateway

import (
	"github.com/tyk-swe/olp/internal/runtime"
	"testing"
)

func TestRetainedNewWorkUsesCurrentRestrictions(t *testing.T) {
	for _, change := range []string{"allowed", "provider disabled", "slot removed", "slot disabled", "key removed", "model removed", "route removed", "target removed", "operation removed"} {
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
			}
			snapshot := &runtime.Snapshot{Providers: map[string]runtime.Provider{"provider": live}, Routes: map[string]runtime.Route{"route": route}}
			err := restrictRetainedWork(snapshot, "key", &provider, &route, &slot, target, "generation")
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
	if e := restrictRetainedWork(snapshot, "key", &provider, &route, &slot, target, "generation"); e != nil {
		t.Fatal(e)
	}
	if *provider.Limits.TokensPerMinute != 10 || *slot.TokensPerMinute != 10 || *slot.MaxConcurrency != 10 || *slot.CredentialID != credential {
		t.Fatalf("wrong retained authority: %+v %+v", provider, slot)
	}
	if *snapshot.Providers["provider"].Limits.TokensPerMinute != 10 {
		t.Fatal("mutated installed release")
	}
}
