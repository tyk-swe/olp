package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/retainedapi"
	"github.com/tyk-swe/olp/internal/retrieval"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/usage"
)

func TestRetainedPreviousOnlyServingAffinity(t *testing.T) {
	parsed, err := protocols.Parse(wire.Responses, []byte(`{"model":"route","input":"next","store":false,"previous_response_id":"resp-native"}`), "route")
	if err != nil {
		t.Fatal(err)
	}
	prior := &resources.Resource{Kind: resources.KindResponse, ID: "response-local", UpstreamID: "resp-native", SlotID: "slot"}
	p := &pin{provider: routing.Provider{ID: "provider", RevisionID: "revision", Kind: "openai"}, model: "model", slot: routing.Slot{ID: "slot"}}
	x := &call{pin: prior, generation: generationCall{parsed: parsed, resources: &generationResources{resolved: map[string]string{"/previous_response_id": "resp-native"}}}}
	if failure := x.bindGenerationServing(p, prior, "/previous_response_id"); failure != nil {
		t.Fatal(failure)
	}
	if x.servingSlot != "slot" || x.servingBinding != "model" {
		t.Fatal("lost previous response slot/model affinity")
	}
	if err = x.checkGenerationResourceDestination(&p.provider, p.model, wire.Responses, parsed.OIF().Document()); err != nil {
		t.Fatalf("transformed previous-only continuation rejected: %v", err)
	}
	other := *p
	other.model = "other"
	if failure := x.bindGenerationServing(&other, prior, "/previous_response_id"); failure == nil {
		t.Fatal("incompatible reference replaced previous response affinity")
	}
}

func TestRetainedGenerationResourcesHonorFidelityTransitions(t *testing.T) {
	strict := routing.RouteFidelity{Mode: routing.FidelityStrict}
	transformed := routing.RouteFidelity{Mode: routing.FidelityTransformed}
	for _, kind := range []string{"conversation", "vector_store", "context_cache"} {
		t.Run(kind, func(t *testing.T) {
			resource := &resources.Resource{Kind: kind}
			if failure := retainedResourceContract(resource, transformed, strict, retainedNewWork); failure == nil || failure.Code != "provider_resource_unavailable" {
				t.Fatalf("transformed resource started work under current strict route: %v", failure)
			}
			if failure := retainedResourceContract(resource, strict, transformed, retainedRetrieval); failure == nil || failure.Code != "provider_resource_unavailable" {
				t.Fatalf("strict resource content read under current transformed route: %v", failure)
			}
			for _, current := range []routing.RouteFidelity{strict, transformed} {
				if failure := retainedResourceContract(resource, current, current, retainedNewWork); failure != nil {
					t.Fatalf("unchanged fidelity refused work: %v", failure)
				}
				if failure := retainedResourceContract(resource, transformed, current, retainedHousekeeping); failure != nil {
					t.Fatalf("fidelity transition blocked cleanup: %v", failure)
				}
			}
		})
	}
}

func TestRetainedMixedPricePageAdmissionIgnoresLookaheadState(t *testing.T) {
	paid := retainedListEntry{freeControl: false}
	free := retainedListEntry{freeControl: true}
	for _, tc := range []struct {
		name    string
		entries []retainedListEntry
		limit   int
		want    bool
	}{
		{"paid then free lookahead", []retainedListEntry{paid, free}, 1, false},
		{"paid then free read", []retainedListEntry{paid, free}, 2, false},
		{"free then paid lookahead", []retainedListEntry{free, paid}, 1, true},
		{"free then paid read", []retainedListEntry{free, paid}, 2, false},
		{"all free", []retainedListEntry{free, free}, 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := retainedPageFreeControl(tc.entries, tc.limit); got != tc.want {
				t.Fatalf("page free=%v, want %v", got, tc.want)
			}
		})
	}
}

func TestRetainedFreePageRefillChecksCostsWithoutReservingRatesAgain(t *testing.T) {
	authority := access.Authority{ID: benchOwner, LookupID: "lookup_test", Policy: access.KeyPolicy{RequestsPerMinute: new(int64(10)), DailyCostLimit: new("1")}}
	s := &Server{Admission: newAdmission(t, newAllowing(10, 0, 0, false))}
	x := &call{family: wire.File}
	var err *Error
	x.lease, err = s.Admission.reserveKey(t.Context(), researchRateAuthority(authority, false), "openai", resourceEstimate, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer settleKey(t.Context(), x.lease, false, nil, quiet)
	checked := false
	s.Admission.PendingCostAccountingReady = func(context.Context, string) error { checked = true; return errors.New("unresolved paid work") }
	if failure := s.admitRetainedPageCost(t.Context(), x, authority, time.Second); failure == nil || failure.Code != "cost_accounting_incomplete" || !checked {
		t.Fatalf("paid refill bypassed caller cost readiness: %v", failure)
	}
	costOnly := retainedCostAuthority(authority)
	if costOnly.Policy.RequestsPerMinute != nil || costOnly.Policy.DailyCostLimit == nil || authority.Policy.RequestsPerMinute == nil {
		t.Fatal("cost upgrade repeated request rates or mutated original authority")
	}
}

func TestRetainedNativeFileRefreshKeepsItsOperation(t *testing.T) {
	for _, operation := range []string{"fine_tuning", "eval", "vector_store", "generation"} {
		t.Run(operation, func(t *testing.T) {
			res := &resources.Resource{Kind: resources.KindFile, Metadata: mustJSON(map[string]any{"upstream_model": "m", "file_operation": operation, "filename": "old"})}
			metadata, state, _ := refreshedFileMetadata(res, []byte(`{"id":"file-native","filename":"new","bytes":12,"status":"processed","file_operation":"batch"}`))
			res.Metadata = metadata
			var fields struct {
				Filename string `json:"filename"`
			}
			if err := json.Unmarshal(metadata, &fields); err != nil {
				t.Fatal(err)
			}
			if retainedFileOperation(res) != operation || state != "processed" || fields.Filename != "new" {
				t.Fatalf("file authority lost on refresh: %s", metadata)
			}
		})
	}
}

func TestRetainedNativeFileControlsSettleRequestFees(t *testing.T) {
	for _, operation := range []string{"fine_tuning", "eval", "vector_store", "generation"} {
		t.Run(operation, func(t *testing.T) {
			now := time.Now()
			x := &call{operationOverride: operation, retainedAccounting: true, facts: []AttemptFact{{StartedAt: now, Status: 200}}, exchange: &pinnedExchange{state: attemptState{accepted: true}, billable: true}}
			x.completeFileControl()
			x.finalizePinned(nil, now)
			evidence := attemptUsage(&x.facts[0], operation)
			if !evidence.Observed || !evidence.Complete || evidence.BillingUncertain || evidence.InputTokens != nil || evidence.OutputTokens != nil {
				t.Fatalf("successful file control not tokenless and complete: %+v", evidence)
			}
			price := usage.Price{PerRequest: new("0.01")}
			cost, priced := price.Cost(evidence, now, true)
			if !priced || cost.String() != "0.01" {
				t.Fatalf("file request fee not charged: %s %v", cost.String(), priced)
			}
			card := usage.RoutingCardRevision{Prices: []usage.RoutingPrice{{Price: usage.Price{ProviderKind: "openai", Model: "m", Operation: operation, PerRequest: new("0.02")}}}}
			charge, priced := card.Charge(evidence, now, true, &cost, "openai", "openai", "m", operation)
			if !priced || charge.String() != "0.02" {
				t.Fatalf("file rate-card charge not settled: %s %v", charge.String(), priced)
			}
		})
	}
}

func TestRetainedEvalAdmissionRejectsInvocationTiers(t *testing.T) {
	now := time.Now()
	for _, chargeTiered := range []bool{false, true} {
		price := usage.RoutingPrice{Price: usage.Price{ProviderKind: "openai", Model: "m", Operation: "generation", InputPerMillion: new("1"), OutputPerMillion: new("1")}, EffectiveAt: now.Add(-time.Hour)}
		tiered := price
		tiered.Tiers = []usage.PriceTier{{AboveInputTokens: 100_000, InputPerMillion: new("3"), OutputPerMillion: new("3")}}
		x := &call{request: request{startedAt: now}, cardPinned: true, routingInputs: &usage.RoutingInputs{RefreshedAt: now, Prices: []usage.RoutingPrice{price}}}
		if chargeTiered {
			x.cardRev = &usage.RoutingCardRevision{Prices: []usage.RoutingPrice{tiered}}
		} else {
			x.routingInputs.Prices[0] = tiered
		}
		p := &pin{model: "m", provider: routing.Provider{ID: "provider", Kind: "openai"}}
		route := &routing.Route{Slug: "route"}
		x.route = route
		r := &resources.Resource{}
		e := retainedapi.Endpoint{Family: retainedapi.Family{Operation: "eval", Surface: "openai", Metered: true}}
		failure := (&Server{now: func() time.Time { return now }}).prepareRetainedAccounting(x, p, route, r, e, generationReferenceDocument(t, `{}`))
		if failure == nil || failure.Code != "unsupported_metered_operation" || r.RetainedAccounting != nil {
			t.Fatalf("tiered eval reached submission: charge=%v failure=%v", chargeTiered, failure)
		}
	}
}

func TestRetainedDeterministicEvalHasNoPhantomModel(t *testing.T) {
	r := &resources.Resource{Metadata: json.RawMessage(`{"upstream_model":"route-model","authorized_models":[]}`)}
	e := retainedapi.Endpoint{Family: retainedapi.Family{Operation: "eval", Metered: true}}
	if failure := (&Server{}).prepareRetainedAccounting(&call{}, &pin{model: "route-model"}, &routing.Route{}, r, e, generationReferenceDocument(t, `{}`)); failure != nil {
		t.Fatal(failure)
	}
	if r.RetainedAccounting == nil || r.RetainedAccounting.Templates == nil || len(r.RetainedAccounting.Templates) != 0 || r.RetainedAccounting.TokenConsuming {
		t.Fatalf("deterministic graders acquired model liability: %+v", r.RetainedAccounting)
	}
}

func TestRetainedEvalWithModelConsumesTokens(t *testing.T) {
	now := time.Now()
	price := usage.RoutingPrice{Price: usage.Price{ProviderKind: "openai", Model: "m", Operation: "generation", InputPerMillion: new("1"), OutputPerMillion: new("2")}, EffectiveAt: now.Add(-time.Hour)}
	inputs := &usage.RoutingInputs{RefreshedAt: now, Prices: []usage.RoutingPrice{price}}
	provider := routing.Provider{ID: "provider", Kind: "openai"}
	route := &routing.Route{ID: "route", Slug: "route"}
	release := &runtime.Release{Snapshot: &routing.Snapshot{Generation: routing.Generation{ID: "gen"}, Providers: map[string]routing.Provider{provider.ID: provider}}}
	x := &call{request: request{startedAt: now, release: release}, keyID: "key", routingInputs: inputs}
	x.route = route
	p := &pin{model: "m", provider: provider, slot: routing.Slot{ID: "slot"}, attempt: routing.Attempt{ProviderID: provider.ID, UpstreamModel: "m"}}
	r := &resources.Resource{}
	e := retainedapi.Endpoint{Family: retainedapi.Family{Operation: "eval", Surface: "openai", Metered: true}}
	s := &Server{now: func() time.Time { return now }, Runtime: &fakeRuntime{inputs: inputs, release: release}}
	if failure := s.prepareRetainedAccounting(x, p, route, r, e, generationReferenceDocument(t, `{}`)); failure != nil {
		t.Fatal(failure)
	}
	if r.RetainedAccounting == nil || len(r.RetainedAccounting.Templates) != 1 || !r.RetainedAccounting.TokenConsuming {
		t.Fatalf("eval with model lost token liability: %+v", r.RetainedAccounting)
	}
}

func TestRetainedAccountingTokenConsumingPrecedence(t *testing.T) {
	for _, tc := range []struct {
		operation string
		want      bool
	}{
		{"fine_tuning", true},
		{"eval", false},
		{"vector_store", false},
	} {
		r := &resources.Resource{Metadata: json.RawMessage(`{"upstream_model":"m","authorized_models":[]}`)}
		e := retainedapi.Endpoint{Family: retainedapi.Family{Operation: tc.operation, Metered: true}}
		if failure := (&Server{}).prepareRetainedAccounting(&call{}, &pin{model: "m"}, &routing.Route{}, r, e, generationReferenceDocument(t, `{}`)); failure != nil {
			t.Fatalf("%s empty models failed: %v", tc.operation, failure)
		}
		if r.RetainedAccounting == nil || len(r.RetainedAccounting.Templates) != 0 || r.RetainedAccounting.TokenConsuming != tc.want {
			t.Fatalf("%s TokenConsuming=%v want %v: %+v", tc.operation, r.RetainedAccounting.TokenConsuming, tc.want, r.RetainedAccounting)
		}
	}
}

func TestRetainedGenerationFileAdmissionBoundsRequestFees(t *testing.T) {
	now := time.Now()
	provider := routing.Provider{ID: "provider", Kind: "openai"}
	price := usage.RoutingPrice{Price: usage.Price{ProviderKind: "openai", Model: "m", Operation: "generation", PerRequest: new("0.01")}, EffectiveAt: now.Add(-time.Hour)}
	x := &call{
		request:           request{startedAt: now, release: &runtime.Release{Snapshot: &routing.Snapshot{Providers: map[string]routing.Provider{provider.ID: provider}}}},
		operationOverride: "generation", retainedHousekeeping: true, retainedTokenless: true, cardPinned: true,
		routingInputs: &usage.RoutingInputs{RefreshedAt: now, Prices: []usage.RoutingPrice{price}},
		cardRev:       &usage.RoutingCardRevision{Prices: []usage.RoutingPrice{{Price: usage.Price{ProviderKind: "openai", Model: "m", Operation: "generation", PerRequest: new("0.02")}}}},
	}
	p := &pin{provider: provider, model: "m", attempt: routing.Attempt{ProviderID: provider.ID, UpstreamModel: "m"}}
	if failure := (&Server{}).retainedAccountingPolicy(x, p, false); failure != nil {
		t.Fatal(failure)
	}
	cost := x.attemptCostBound(p.attempt)
	charge := x.attemptChargeBound(p.attempt, cost)
	if x.retainedFreeControl || cost.String() != "0.01" || charge.String() != "0.02" {
		t.Fatalf("tokenless request fees bypassed admission: free=%v cost=%s charge=%s", x.retainedFreeControl, cost.String(), charge.String())
	}
	// Compaction uses the same generation operation but bills model tokens.
	x.retainedTokenless = false
	x.estimate = 1000
	p.attempt.Price = &usage.RoutingPrice{Price: usage.Price{InputPerMillion: new("1"), OutputPerMillion: new("2")}}
	x.cardRev = &usage.RoutingCardRevision{Prices: []usage.RoutingPrice{{Price: usage.Price{ProviderKind: "openai", Model: "m", Operation: "generation", InputPerMillion: new("3"), OutputPerMillion: new("4")}}}}
	cost = x.attemptCostBound(p.attempt)
	charge = x.attemptChargeBound(p.attempt, cost)
	if cost.IsZero() || charge.Cmp(cost) <= 0 {
		t.Fatalf("compaction lost token reservations: cost=%s charge=%s", cost.String(), charge.String())
	}
}

func TestRetainedRetrievalResponseFailureKeepsBillingUnresolved(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		valid      bool
	}{
		{"valid", `{"value":[{"@search.score":1,"key":"doc","content":"text"}]}`, true},
		{"invalid", `{"value":{}}`, false},
		{"oversized", strings.Repeat("x", 256), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, x, p := pinnedExchangeFixture(t)
			h.gateway.cfg.MaxResponseBytes = 128
			x.operationOverride, x.retainedMetered = "retrieval", true
			exchange := h.gateway.beginPinned(t.Context(), x, p, http.MethodPost)
			exchange.state.accepted = true
			x.facts[0].Status, x.facts[0].Class = 200, classProtocol
			binding := &retrieval.Binding{Input: retrieval.Input{Backend: retrieval.Azure, Fields: retrieval.Fields{Text: "content", ID: "key"}}}
			_, failure := h.gateway.retrievalResult(x, binding, &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(tc.body))})
			if tc.valid {
				if failure != nil {
					t.Fatal(failure)
				}
				x.completePinned()
			} else if failure == nil {
				t.Fatal("malformed retrieval result accepted")
			}
			x.finalizePinned(nil, time.Now())
			evidence := attemptUsage(&x.facts[0], "retrieval")
			if !tc.valid {
				if evidence.Observed || evidence.Complete || !evidence.BillingUncertain {
					t.Fatalf("accepted malformed query became known zero usage: %+v", evidence)
				}
			} else if cost, priced := (&usage.Price{PerRequest: new("0.01")}).Cost(evidence, time.Now(), attemptSuccessful(&x.facts[0])); !priced || cost.String() != "0.01" {
				t.Fatalf("valid query lost its request fee: %s %v", cost.String(), priced)
			}
		})
	}
}
