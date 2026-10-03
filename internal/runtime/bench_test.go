package runtime

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/usage"
)

// BenchmarkAuthenticate is the check every request starts with: the presented key
// is split, its record found by lookup ID among a thousand keys, and its digest
// computed and compared in constant time. The refusals are what a key that was
// rotated, mistyped or never issued costs.
//
//	go test ./internal/runtime -run '^$' -bench BenchmarkAuthenticate -benchmem
func BenchmarkAuthenticate(b *testing.B) {
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		b.Fatal(err)
	}
	auth := secrets.NewAuthKey(key, uuid.NewString())
	m := NewManager(nil, uuid.NewString(), auth, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	// An authority read in the future never goes stale however long the run is.
	state := authorityState{loaded: true, readAt: time.Now().Add(24 * time.Hour), keys: map[string]keyRecord{}}
	var presented string
	for i := range 1000 {
		lookup := fmt.Sprintf("lookup%04d", i)
		secret := "olp_" + lookup + "_" + uuid.NewString() + uuid.NewString()
		state.keys[lookup] = keyRecord{authority: access.Authority{ID: uuid.NewString(), LookupID: lookup, Policy: access.KeyPolicy{Scopes: []string{"inference"}}}, digest: auth.Digest(secrets.APIKeyDigest, secret)}
		if i == 500 {
			presented = secret
		}
	}
	m.authority = state
	for _, tc := range []struct {
		name   string
		secret string
		want   error
	}{
		{"valid", presented, nil},
		{"wrong-secret", presented + "x", ErrInvalidKey},
		{"unknown-lookup", "olp_nolookup0_" + uuid.NewString(), ErrInvalidKey},
		{"malformed", "sk-" + uuid.NewString(), ErrInvalidKey},
	} {
		b.Run(tc.name, func(b *testing.B) {
			if _, err := m.Authenticate(tc.secret); err != tc.want {
				b.Fatalf("Authenticate = %v, want %v", err, tc.want)
			}
			b.ReportAllocs()
			for b.Loop() {
				m.Authenticate(tc.secret)
			}
		})
	}
}

// BenchmarkEligibility is the check that a credential version may still serve,
// which planning makes for every slot of every target and the executor again
// before each attempt, and the read of its secret from the installed release.
func BenchmarkEligibility(b *testing.B) {
	credential := uuid.NewString()
	m := authorityManager(time.Now().Add(24*time.Hour), map[string]Eligibility{uuid.NewString(): Revoked})
	release := &Release{credentials: map[string][]byte{credential: []byte("sk-bench")}}
	b.Run("eligible", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if m.Eligibility(credential) != Eligible {
				b.Fatal("the credential cannot serve")
			}
		}
	})
	b.Run("secret", func(b *testing.B) {
		ctx := context.Background()
		b.ReportAllocs()
		for b.Loop() {
			if _, _, err := m.Secret(ctx, release, credential); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// benchRoute is a route of the given number of targets, each on a provider of
// its own with the given number of credential slots and the metadata that
// catalogued models carry.
func benchRoute(targets, slots int) *Snapshot {
	metadata, _ := json.Marshal(ModelMetadata{
		ContextLength: ptr(int64(128_000)), MaxOutputTokens: ptr(int64(16_384)),
		SupportedParameters: &[]string{"temperature", "top_p", "max_tokens", "tools", "tool_choice", "response_format", "seed", "stop"},
		Region:              ptr("us"), Source: ptr("reference"),
	})
	s := &Snapshot{Providers: map[string]Provider{}, Routes: map[string]Route{}}
	route := Route{Slug: "team-model", RoutingID: uuid.NewString(), OverallTimeout: 30_000, MaxAttempts: targets, Operations: []string{"generation"}}
	for i := range targets {
		credential, version := uuid.NewString(), 1
		provider := Provider{
			ID: uuid.NewString(), Kind: "openai", VendorID: "openai", Enabled: true, RevisionID: uuid.NewString(), AuthMode: "api_key",
			Capabilities: []Capability{
				{Model: "wire-model", Operation: "generation", Surface: "openai", Mode: "unary"},
				{Model: "wire-model", Operation: "generation", Surface: "openai", Mode: "streaming"},
			},
			Models: map[string]json.RawMessage{"wire-model": metadata},
		}
		for range slots {
			provider.Slots = append(provider.Slots, Slot{ID: uuid.NewString(), Name: "slot", Enabled: true, Weight: 1, CredentialID: &credential, CredentialVersion: &version})
		}
		s.Providers[provider.ID] = provider
		target := uuid.NewString()
		route.Targets = append(route.Targets, Target{ID: target, RoutingID: target, ProviderID: provider.ID, ProviderModel: "wire-model", Priority: i / 2, Weight: int64(i%3 + 1), Timeout: 30_000})
	}
	s.Routes[route.Slug] = route
	return s
}

// benchPrices is a price catalogue of n models, of which the last is the one the
// route's targets are served by.
func benchPrices(n int, now time.Time) *usage.RoutingInputs {
	rate := "1.25"
	inputs := &usage.RoutingInputs{RefreshedAt: now, Performance: map[string]usage.Performance{}}
	for i := range n {
		price := usage.RoutingPrice{EffectiveAt: now.Add(-time.Hour), Revision: 1}
		price.ProviderKind, price.Model, price.Operation = "openai", fmt.Sprintf("model-%d", i), "generation"
		if i == n-1 {
			price.Model = "wire-model"
		}
		price.InputPerMillion, price.OutputPerMillion = &rate, &rate
		inputs.Prices = append(inputs.Prices, price)
	}
	return inputs
}

// BenchmarkPlanRequest is the planning every request pays: each target of the
// route weighed against the request and the policies in force, its credential
// slots ranked and checked, and the attempts ordered. The priced case plans
// against a catalogue of a thousand prices, which every target is looked up in,
// and the no-metadata case plans models that the catalogue knows nothing of.
//
//	go test ./internal/runtime -run '^$' -bench BenchmarkPlanRequest -benchmem
func BenchmarkPlanRequest(b *testing.B) {
	manager := authorityManager(time.Now().Add(24*time.Hour), nil)
	affinity := []byte(uuid.NewString())
	for _, tc := range []struct {
		name            string
		targets, slots  int
		prices          int
		bare            bool // the models carry no metadata
		wantAttempts    int
		wantDecisionLen int
	}{
		{"2-targets", 2, 1, 0, false, 2, 2},
		{"8-targets", 8, 2, 0, false, 8, 16},
		{"8-targets-priced", 8, 2, 1000, false, 8, 16},
		{"8-targets-no-metadata", 8, 2, 0, true, 8, 16},
	} {
		b.Run(tc.name, func(b *testing.B) {
			snapshot := benchRoute(tc.targets, tc.slots)
			if tc.bare {
				for id, provider := range snapshot.Providers {
					provider.Models = nil
					snapshot.Providers[id] = provider
				}
			}
			now := time.Now()
			options := SelectionOptions{
				KeyID: uuid.NewString(), Now: now, CheckSlots: true, CredentialEligibility: manager.Eligibility,
				Parameters:  []string{"temperature", "max_tokens"},
				TokenDemand: &TokenDemand{EstimatedInputTokens: 4_200, MaxOutputTokens: ptr(int64(512))},
			}
			if tc.prices > 0 {
				options.Inputs = benchPrices(tc.prices, now)
			}
			// A plan that found no attempt is the error path, which is not what a
			// request pays.
			plan, err := PlanRequest(snapshot, "team-model", "generation", "openai", "unary", affinity, options)
			if err != nil || len(plan.Attempts) != tc.wantAttempts || len(plan.Decisions) != tc.wantDecisionLen {
				b.Fatalf("planned %d attempts and %d decisions, %v; want %d and %d", len(plan.Attempts), len(plan.Decisions), err, tc.wantAttempts, tc.wantDecisionLen)
			}
			// The cases differ in what the planner is given to look up, which a plan
			// that did not use it would not show: every attempt of the priced case
			// carries the price of its model and no other does, and every decision
			// carries the context length of its model's metadata unless it has none.
			for i, attempt := range plan.Attempts {
				if (attempt.Price != nil) != (tc.prices > 0) {
					b.Fatalf("attempt %d has price %v against a catalogue of %d prices: the case no longer measures what it names", i, attempt.Price, tc.prices)
				}
			}
			for i, decision := range plan.Decisions {
				if (decision.ContextLength != nil) == tc.bare {
					b.Fatalf("decision %d has context length %v, and the models have metadata: %t: the case no longer measures what it names", i, decision.ContextLength, !tc.bare)
				}
			}
			b.ReportAllocs()
			for b.Loop() {
				if _, err := PlanRequest(snapshot, "team-model", "generation", "openai", "unary", affinity, options); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkSelectSlots is the choice among the credential slots of one provider:
// their restrictions applied and the rest ranked by priority and by weighted
// rendezvous on the request's affinity.
func BenchmarkSelectSlots(b *testing.B) {
	affinity := []byte(uuid.NewString())
	for _, slots := range []int{1, 4} {
		b.Run(fmt.Sprintf("%d-slots", slots), func(b *testing.B) {
			snapshot := benchRoute(1, slots)
			route := snapshot.Routes["team-model"]
			provider := snapshot.Providers[route.Targets[0].ProviderID]
			if got := SelectSlots(provider, "wire-model", route, "key", "generation", "openai", "unary", affinity); len(got) != slots {
				b.Fatalf("selected %d slots, want %d", len(got), slots)
			}
			b.ReportAllocs()
			for b.Loop() {
				SelectSlots(provider, "wire-model", route, "key", "generation", "openai", "unary", affinity)
			}
		})
	}
}
