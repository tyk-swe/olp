package usage

import (
	"bytes"
	"testing"
	"time"
)

func TestRetainedSettlementOriginRoundTripRequiresParent(t *testing.T) {
	event := metadataEvent()
	event.Origin = OriginRetainedSettlement
	event.ParentRequestID = new(event.RequestID)
	encoded, err := Encode(event)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(encoded)
	if err != nil {
		t.Fatalf("retained accounting template cannot settle: %v", err)
	}
	if decoded.Origin != OriginRetainedSettlement || decoded.ParentRequestID == nil || *decoded.ParentRequestID != event.RequestID {
		t.Fatal("lost retained submission parent")
	}
	event.ParentRequestID = nil
	encoded, err = Encode(event)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Decode(encoded); err == nil {
		t.Fatal("retained child work accepted without its submission parent")
	}
}

func retainedRateChanges() map[string]func(*Price, string) {
	return map[string]func(*Price, string){
		"training":      func(p *Price, rate string) { p.TrainingPerMillion = new(rate) },
		"storage":       func(p *Price, rate string) { p.StoragePerGiBDay = new(rate) },
		"cache_storage": func(p *Price, rate string) { p.CacheStoragePerMillionTokenHour = new(rate) },
		"container":     func(p *Price, rate string) { p.ContainerPerSession = map[string]string{"4g": rate} },
	}
}

func TestRetainedRatesParticipateInPriceEquality(t *testing.T) {
	for name, set := range retainedRateChanges() {
		t.Run(name, func(t *testing.T) {
			base := Price{Currency: "USD"}
			set(&base, "1")
			changed := base
			set(&changed, "2")
			if SamePrice(base, changed) {
				t.Fatal("retained-only rate change was ignored")
			}
			equal := base
			set(&equal, "1.00")
			if !SamePrice(base, equal) {
				t.Fatal("equivalent retained decimals caused a false pricing change")
			}
			zero := Price{Currency: "USD"}
			set(&zero, "0")
			if SamePrice(zero, Price{Currency: "USD"}) {
				t.Fatal("explicit zero rate compared equal to missing pricing")
			}
		})
	}
	base := Price{Currency: "USD", ContainerPerSession: map[string]string{"4g": "1"}}
	changed := Price{Currency: "USD", ContainerPerSession: map[string]string{"16g": "1"}}
	if SamePrice(base, changed) {
		t.Fatal("container memory-tier change was ignored")
	}
}

func TestRetainedOnlySourceOverridesAreNotRemovals(t *testing.T) {
	for name, set := range retainedRateChanges() {
		t.Run(name, func(t *testing.T) {
			base := Price{ProviderKind: "openai", Model: "m", Operation: "fine_tuning", Currency: "USD"}
			set(&base, "1")
			for _, rate := range []string{"2", "0"} {
				override := base
				set(&override, rate)
				merged, err := mergeSourceOverrides([]Price{base}, []Price{override}, nil)
				if err != nil || len(merged) != 1 || !SamePrice(merged[0], override) {
					t.Fatalf("retained-only override removed its price: %+v %v", merged, err)
				}
			}
			removed, err := mergeSourceOverrides([]Price{base}, []Price{{ProviderKind: "openai", Model: "m", Operation: "fine_tuning"}}, nil)
			if err != nil || len(removed) != 0 {
				t.Fatalf("dimension-only deletion stopped working: %+v %v", removed, err)
			}
		})
	}
}

func TestRetainedEvalAggregateUsageCannotChooseInvocationTiers(t *testing.T) {
	price := Price{ProviderKind: "openai", Model: "m", Operation: "generation", InputPerMillion: new("1"), OutputPerMillion: new("0"), Tiers: []PriceTier{{AboveInputTokens: 100_000, InputPerMillion: new("3"), OutputPerMillion: new("0")}}}
	evidence := AttemptUsage{Observed: true, Complete: true, Deferred: true, UsageQuantities: UsageQuantities{InvocationCount: new(int64(2)), InputTokens: new(int64(120_000)), OutputTokens: new(int64(0))}}
	if cost, priced := price.Cost(evidence, time.Now(), true); priced {
		t.Fatalf("two 60k invocations charged aggregate tier: %s", cost.String())
	}
	card := RoutingCardRevision{Prices: []RoutingPrice{{Price: price}}}
	if charge, priced := card.Charge(evidence, time.Now(), true, nil, "openai", "openai", "m", "generation"); priced {
		t.Fatalf("explicit rate card charged aggregate tier: %s", charge.String())
	}
	evidence.InvocationCount = new(int64(1))
	evidence.InputTokens = new(int64(60_000))
	if cost, priced := price.Cost(evidence, time.Now(), true); !priced || cost.String() != "0.06" {
		t.Fatalf("single invocation pricing changed: %s %v", cost.String(), priced)
	}
}

func TestPendingGenerationPreservesWholeUsageSnapshots(t *testing.T) {
	initial := AttemptUsage{Observed: true, BillingUncertain: true,
		UsageQuantities: UsageQuantities{InputTokens: new(int64(100)), OutputTokens: new(int64(20)), CachedInputTokens: new(int64(20))}}
	payload, err := Encode(metadataEvent().WithFinalUsage(initial))
	if err != nil {
		t.Fatal(err)
	}
	pending, err := DecodePendingGeneration(payload)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name                              string
		input, output, cached             int64
		state                             GenerationState
		wantInput, wantOutput, wantCached int64
		invalid                           bool
	}{
		{name: "a partial observation cannot combine dimensions", input: 120, output: 10, cached: 30,
			state: GenerationPending, wantInput: 100, wantOutput: 20, wantCached: 20},
		{name: "a terminal provider snapshot replaces provisional evidence", input: 90, output: 10, cached: 10,
			state: GenerationTerminal, wantInput: 90, wantOutput: 10, wantCached: 10},
		{name: "terminal zero is evidence", state: GenerationTerminal},
		{name: "a stale invalid observation is still rejected", input: -1, state: GenerationPending, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			evidence := AttemptUsage{Observed: true, Complete: true, UsageQuantities: UsageQuantities{
				InputTokens: &tc.input, OutputTokens: &tc.output, CachedInputTokens: &tc.cached, ToolCalls: map[string]int64{"search": 2}, ContainerSessions: map[string]int64{"1g": 1}}}
			observed, err := pending.Observe(evidence, tc.state)
			if tc.invalid {
				if err == nil {
					t.Fatal("invalid usage was accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			_, err = observed.AccountingEvent(time.Now().UTC(), tc.state)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := observed.Encode()
			if err != nil {
				t.Fatal(err)
			}
			*evidence.InputTokens = 999
			evidence.ToolCalls["search"] = 999
			evidence.ContainerSessions["1g"] = 999
			unchanged, err := observed.Encode()
			if err != nil || !bytes.Equal(encoded, unchanged) {
				t.Fatalf("caller changed retained evidence: %v", err)
			}
			event, err := Decode(encoded)
			if err != nil {
				t.Fatal(err)
			}
			got := event.Attempts[len(event.Attempts)-1].Usage
			if got == nil || got.InputTokens == nil || got.OutputTokens == nil || got.CachedInputTokens == nil ||
				*got.InputTokens != tc.wantInput || *got.OutputTokens != tc.wantOutput || *got.CachedInputTokens != tc.wantCached ||
				event.InputTokens == nil || *event.InputTokens != *got.InputTokens || got.Complete != (tc.state == GenerationTerminal) {
				t.Fatalf("usage snapshot was mixed or lost: %+v", got)
			}
		})
	}
}
