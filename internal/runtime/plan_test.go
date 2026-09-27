package runtime

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/usage"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

func ptr[T any](v T) *T { return &v }
func planningFixture() (Snapshot, string, []string) {
	ids := []string{uuid.NewString(), uuid.NewString(), uuid.NewString()}
	s := Snapshot{Providers: map[string]Provider{}, Routes: map[string]Route{}}
	route := Route{Slug: "team-model", RoutingID: uuid.NewString(), OverallTimeout: 3000, MaxAttempts: 3, Operations: []string{"generation"}}
	for i, id := range ids {
		target := uuid.NewString()
		s.Providers[id] = Provider{ID: id, Kind: "openai", VendorID: "openai", Enabled: true, RevisionID: uuid.NewString(), Capabilities: []Capability{{Model: "wire-model", Operation: "generation", Surface: "openai", Mode: "unary"}}}
		route.Targets = append(route.Targets, Target{ID: target, RoutingID: target, ProviderID: id, ProviderModel: "wire-model", Weight: int64(i + 1), Timeout: 1000})
	}
	s.Routes[route.Slug] = route
	return s, route.Slug, ids
}
func TestPoliciesIntersectUnknownFactsAndPublishedLimits(t *testing.T) {
	s, slug, ids := planningFixture()
	now := time.Now()
	metadata, _ := json.Marshal(ModelMetadata{Region: ptr("eu"), ZeroDataRetention: ptr(true), DataCollection: ptr(false), Source: ptr("contract"), ObservedAt: &now, SupportedParameters: &[]string{"temperature"}})
	p := s.Providers[ids[0]]
	p.Models = map[string]json.RawMessage{"wire-model": metadata}
	s.Providers[p.ID] = p
	s.InstallationPolicy = &Policy{Constraints: Preferences{Regions: []string{"eu"}, RequireZeroDataRetention: ptr(true), RequireParameters: ptr(true)}}
	plan, e := PlanRequest(&s, slug, "generation", "openai", "unary", []byte("seed"), SelectionOptions{Parameters: []string{"temperature"}, Now: now})
	if e != nil || len(plan.Attempts) != 1 || plan.Attempts[0].ProviderID != ids[0] {
		t.Fatalf("facts: %+v %v", plan, e)
	}
	plan, e = PlanRequest(&s, slug, "generation", "openai", "unary", []byte("seed"), SelectionOptions{Preferences: &Preferences{Only: []string{"provider:" + ids[1]}}, Now: now})
	if e != nil || len(plan.Attempts) != 0 {
		t.Fatalf("request expanded parent policy: %+v %v", plan, e)
	}
	if _, e = PlanRequest(&s, slug, "generation", "openai", "unary", []byte("seed"), SelectionOptions{Preferences: &Preferences{MaxAttempts: ptr(4)}}); e == nil {
		t.Fatal("expanded route attempt budget")
	}
	s.InstallationPolicy = &Policy{AllowedStrategies: []string{"weighted"}}
	if _, e = PlanRequest(&s, slug, "generation", "openai", "unary", []byte("seed"), SelectionOptions{Preferences: &Preferences{Strategy: ptr("price")}}); e == nil {
		t.Fatal("expanded strategy permission")
	}
	for _, payload := range []string{`{"deadline":9000}`, `{"only":["anything"]}`, `{"max_price":{"input_per_million":"1e9"}}`, `{"allowed_strategies":["price"]}`, `{"strategy":"price"} {}`, `null`} {
		if _, e = ParsePreferences([]byte(payload)); e == nil {
			t.Fatalf("accepted invalid controls %s", payload)
		}
	}
}
func TestPricePriorityFallbackAndPerformanceShareOneOrdering(t *testing.T) {
	s, slug, ids := planningFixture()
	now := time.Now()
	inputs := &usage.RoutingInputs{RefreshedAt: now, Performance: map[string]usage.Performance{}}
	for i, id := range ids[:2] {
		amount := []string{"0.000000000002", "0.000000000001"}[i]
		inputs.Prices = append(inputs.Prices, usage.RoutingPrice{Price: usage.Price{ProviderID: &id, ProviderKind: "openai", Model: "wire-model", Operation: "generation", InputPerMillion: &amount, OutputPerMillion: &amount, Currency: "USD"}, RevisionID: uuid.NewString(), Revision: 1, EffectiveAt: now.Add(-time.Hour)})
	}
	options := SelectionOptions{Inputs: inputs, Now: now, Preferences: &Preferences{Strategy: ptr("price")}}
	plan, e := PlanRequest(&s, slug, "generation", "openai", "unary", []byte("seed"), options)
	if e != nil || len(plan.Attempts) != 3 || plan.Attempts[0].ProviderID != ids[1] || plan.Attempts[2].ProviderID != ids[2] {
		t.Fatalf("exact prices: %+v %v", plan, e)
	}
	route := s.Routes[slug]
	route.Targets[0].Priority = -1
	s.Routes[slug] = route
	plan, e = PlanRequest(&s, slug, "generation", "openai", "unary", []byte("seed"), options)
	if e != nil || plan.Attempts[0].ProviderID != ids[0] {
		t.Fatal("strategy escaped priority tier")
	}
	options.Preferences = &Preferences{Order: []string{"provider:" + ids[2]}, AllowFallbacks: ptr(false)}
	plan, e = PlanRequest(&s, slug, "generation", "openai", "unary", []byte("seed"), options)
	if e != nil || len(plan.Attempts) != 1 || plan.Attempts[0].ProviderID != ids[2] {
		t.Fatal("explicit fallback boundary ignored")
	}
	route.Targets[0].Priority = 0
	s.Routes[slug] = route
	inputs.Performance[usage.PerformanceKey(ids[1], "wire-model", "generation", "unary")] = usage.Performance{SampleCount: 20, LatencyMS: 5, ObservedAt: now}
	options.Preferences = &Preferences{Strategy: ptr("latency")}
	plan, e = PlanRequest(&s, slug, "generation", "openai", "unary", []byte("seed"), options)
	if e != nil || plan.Attempts[0].ProviderID != ids[1] {
		t.Fatalf("performance: %+v %v", plan, e)
	}
	options.Now = now.Add(61 * time.Second)
	plan, e = PlanRequest(&s, slug, "generation", "openai", "unary", []byte("seed"), options)
	if e != nil {
		t.Fatal(e)
	}
	for _, decision := range plan.Decisions {
		if decision.Performance != nil {
			t.Fatal("stale metric used")
		}
	}
}
func TestMountedTransportPreservesPublishedOwnership(t *testing.T) {
	s, _, ids := planningFixture()
	id := ids[0]
	slotID, secretID := uuid.NewString(), uuid.NewString()
	p := s.Providers[id]
	p.AuthMode = "api_key"
	p.Endpoint = "https://published.example/v1"
	p.DefaultSlotID = slotID
	p.Slots = []Slot{{ID: slotID, Name: "default", Enabled: true, Weight: 1, CredentialID: &secretID, RequestsPerMinute: ptr(int64(3))}}
	s.Providers = map[string]Provider{id: p}
	mounted := map[string]MountedProvider{id: {Configuration: Configuration{Kind: "openai", AuthMode: "api_key", Endpoint: "https://mounted.example/v1"}, Credential: []byte("mounted-secret")}}
	secrets, e := installMounted(&s, mounted)
	if e != nil {
		t.Fatal(e)
	}
	got := s.Providers[id]
	if got.Endpoint != "https://mounted.example/v1" || *got.Slots[0].RequestsPerMinute != 3 || string(secrets[secretID]) != "mounted-secret" {
		t.Fatal("mount changed published authority")
	}
	missingIdentity := got
	missingIdentity.DefaultSlotID = ""
	s.Providers[id] = missingIdentity
	if _, err := installMounted(&s, mounted); err == nil {
		t.Fatal("a mutable slot name became authoritative default-slot identity")
	}
	got.Slots = append(got.Slots, Slot{ID: uuid.NewString(), Name: "named", Enabled: true})
	s.Providers[id] = got
	if _, e = installMounted(&s, mounted); e == nil {
		t.Fatal("mounted credential synthesized a named pool")
	}
}

func TestPreviewEnumeratesCredentialAttemptsWithinOneBudget(t *testing.T) {
	s, _, ids := planningFixture()
	r := s.Routes["team-model"]
	r.Targets = r.Targets[:1]
	r.MaxAttempts = 2
	s.Routes[r.Slug] = r
	p := s.Providers[ids[0]]
	p.AuthMode = "api_key"
	for i := 0; i < 3; i++ {
		id := uuid.NewString()
		p.Slots = append(p.Slots, Slot{ID: uuid.NewString(), Enabled: true, Priority: i, Weight: 1, CredentialID: &id})
	}
	s.Providers[p.ID] = p
	plan, err := PlanRequest(&s, r.Slug, "generation", "openai", "unary", []byte("seed"), SelectionOptions{CheckSlots: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Attempts) != 1 || len(plan.Decisions) != 3 {
		t.Fatalf("target and credential plans: %+v", plan)
	}
	for i, d := range plan.Decisions {
		if d.CredentialSlotID == nil || *d.CredentialSlotID != p.Slots[i].ID {
			t.Fatal("credential order differs")
		}
		if i < 2 && (d.Attempt == nil || *d.Attempt != i+1) {
			t.Fatal("real credential trial missing from budget")
		}
		if i == 2 && (d.Eligible || d.Attempt != nil) {
			t.Fatal("credential escaped route budget")
		}
	}
}

func TestPlanningExcludesIneligibleCredentialsWithTheirReason(t *testing.T) {
	s, slug, ids := planningFixture()
	route := s.Routes[slug]
	route.Targets = route.Targets[:2]
	s.Routes[slug] = route
	network := uuid.NewString()
	eligibility := map[string]Eligibility{}
	for i, id := range ids[:2] {
		p := s.Providers[id]
		p.AuthMode = "api_key"
		for range 2 {
			credential := uuid.NewString()
			p.Slots = append(p.Slots, Slot{ID: uuid.NewString(), Enabled: true, Weight: 1, CredentialID: &credential})
		}
		if i == 1 {
			p.Network = &egress.ConnectionOptions{CredentialID: network}
		}
		s.Providers[id] = p
	}
	plan := func() Plan {
		t.Helper()
		plan, err := PlanRequest(&s, slug, "generation", "openai", "unary", []byte("seed"), SelectionOptions{
			CheckSlots: true, CredentialEligibility: func(id string) Eligibility { return eligibility[id] },
		})
		if err != nil {
			t.Fatal(err)
		}
		return plan
	}
	reasons := func(plan Plan) map[string]string {
		out := map[string]string{}
		for _, decision := range plan.Decisions {
			if decision.Reason != nil {
				out[decision.ProviderID] = *decision.Reason
			}
		}
		return out
	}
	first := s.Providers[ids[0]]
	eligibility[*first.Slots[0].CredentialID] = Revoked
	if got := plan(); len(got.Decisions) != 3 || reasons(got)[ids[0]] != "" {
		t.Fatalf("an ineligible slot was planned or spent the budget: %+v", got.Decisions)
	}
	for _, reason := range []Eligibility{Revoked, StaleAuthority} {
		eligibility[network] = reason
		eligibility[*first.Slots[1].CredentialID] = reason
		got := reasons(plan())
		if got[ids[0]] != "no_eligible_credentials" || got[ids[1]] != "network_credential_"+string(reason) {
			t.Fatalf("%s reasons: %v", reason, got)
		}
	}
}

// Only a deployment that enables unconfined plugins serves targets of
// providers whose plugin is unconfined; a confined plugin's are served
// anywhere.
func TestPlanningRefusesUnconfinedPluginTargetsWithoutTheTier(t *testing.T) {
	s, slug, ids := planningFixture()
	manifest := abi.Manifest{Name: "acme", Version: "1.0.0", Origins: []string{"https://api.acme.example"}, Profiles: []abi.Profile{{
		ID: "acme-chat", Label: "Acme Chat", Dialect: "openai-chat",
		Hosting: abi.Hosting{Address: "https://api.acme.example/v1", Headers: map[string]string{"Authorization": "Token {credential}"}},
	}}}
	confined, err := connectors.NewPluginProfile(strings.Repeat("ab", 32), manifest, "acme-chat")
	if err != nil {
		t.Fatal(err)
	}
	unconfined, err := connectors.NewUnconfinedPluginProfile(strings.Repeat("cd", 32), manifest, "acme-chat")
	if err != nil {
		t.Fatal(err)
	}
	for id, plugin := range map[string]*connectors.PluginProfile{ids[0]: confined, ids[1]: unconfined} {
		p := s.Providers[id]
		p.Kind, p.Plugin = connectors.KindPlugin, plugin
		s.Providers[id] = p
	}
	for enabled, want := range map[bool]string{false: "plugin_unconfined_disabled", true: ""} {
		plan, err := PlanRequest(&s, slug, "generation", "openai", "unary", []byte("seed"), SelectionOptions{UnconfinedPlugins: enabled})
		if err != nil {
			t.Fatal(err)
		}
		reasons := map[string]string{}
		for _, decision := range plan.Decisions {
			if decision.Reason != nil {
				reasons[decision.ProviderID] = *decision.Reason
			}
		}
		if reasons[ids[0]] != "" || reasons[ids[1]] != want {
			t.Fatalf("with the tier enabled %v, reasons %v", enabled, reasons)
		}
	}
}
