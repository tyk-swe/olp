package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/retainedapi"
	"github.com/tyk-swe/olp/internal/routing"
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
