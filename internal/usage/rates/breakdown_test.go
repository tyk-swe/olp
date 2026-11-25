package rates

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/usage"
)

func TestEvaluatePriceProducesMatchingTotalsComponentsAndMergedLines(t *testing.T) {
	t.Parallel()
	price := Price{
		ProviderKind:          "openai",
		Model:                 "m",
		Operation:             "generation",
		Currency:              "USD",
		InputPerMillion:       rate("2"),
		CachedInputPerMillion: rate("0.5"),
		OutputPerMillion:      rate("8"),
		PerRequest:            rate("0.01"),
		BatchMultiplier:       rate("0.5"),
		ToolCalls:             map[string]string{"web_search": "0.02"},
		Tiers: []PriceTier{{
			AboveInputTokens:      100_000,
			InputPerMillion:       rate("4"),
			CachedInputPerMillion: rate("1"),
			OutputPerMillion:      rate("12"),
		}},
		Windows: &PriceWindows{
			TimeZone: "UTC",
			Periods: []PriceWindowPeriod{{
				Days:       []int{1, 2, 3, 4, 5, 6, 7},
				Start:      "00:00",
				End:        "24:00",
				Multiplier: "1.5",
			}},
		},
	}
	u := usage.AttemptUsage{
		Evidence: usage.EvidenceObservedComplete,
		UsageQuantities: usage.UsageQuantities{
			InputTokens:       tokens(200_000),
			CachedInputTokens: tokens(50_000),
			AudioInputTokens:  tokens(25_000),
			OutputTokens:      tokens(100_000),
			ToolCalls:         map[string]int64{"web_search": 2},
			Batch:             true,
		},
	}
	discount := rate("0.8")
	got := EvaluatePrice(&price, u, settledAt, true, discount)
	if !got.Complete {
		t.Fatal("expected complete evaluation")
	}

	// Audio input falls back to input_per_million (4) so text (125k) + audio (25k) merge into 150k at tier 100000.
	// Base: input 150k*4/1e6=0.6 + cached 50k*1/1e6=0.05 + output 100k*12/1e6=1.2 + per_request 0.01 + tools 2*0.02=0.04 => 1.90
	// Multiplier: window 1.5 * batch 0.5 = 0.75 => list = 1.425; discounted (*0.8) = 1.14.
	if got.ListCost.String() != "1.425" {
		t.Fatalf("ListCost = %s, want 1.425", got.ListCost.String())
	}
	if got.Cost.String() != "1.14" {
		t.Fatalf("Cost = %s, want 1.14", got.Cost.String())
	}
	rd, err := NewRoutingDiscount(RoutingDiscount{Multiplier: "0.8"})
	if err != nil {
		t.Fatal(err)
	}
	mirrorCost, ok := NewRoutingPrice(RoutingPrice{Price: price}).Cost(u, settledAt, true, rd)
	if !ok || mirrorCost.String() != got.Cost.String() {
		t.Fatalf("RoutingPrice.Cost = %s (%v), EvaluatePrice = %s", mirrorCost.String(), ok, got.Cost.String())
	}

	wantComponents := []string{
		"batch_multiplier", "cached_input_per_million", "discount",
		"input_per_million", "output_per_million", "per_request",
		"tiers", "tool_calls.web_search", "windows",
	}
	if !slices.Equal(got.Components, wantComponents) {
		t.Fatalf("Components = %v, want %v", got.Components, wantComponents)
	}
	if len(got.Lines) != 5 {
		t.Fatalf("len(Lines) = %d, want 5: %+v", len(got.Lines), got.Lines)
	}
	if got.Lines[1].Component != "input_per_million" || got.Lines[1].Quantity != "0.15" ||
		got.Lines[1].Tier == nil || *got.Lines[1].Tier != "100000" ||
		got.Lines[1].ListUnitPrice != "3" || got.Lines[1].ContractedUnitPrice != "2.4" {
		t.Fatalf("merged input line = %+v", got.Lines[1])
	}

	scaled, err := ScalePriceLines(got.Lines, "1.25")
	if err != nil {
		t.Fatalf("ScalePriceLines: %v", err)
	}
	if scaled[1].ListUnitPrice != "3" || scaled[1].ContractedUnitPrice != "3" {
		t.Fatalf("scaled input line = %+v, want unit prices 3", scaled[1])
	}
}

func TestApplyMarkupKeepsFullQuotedCharge(t *testing.T) {
	t.Parallel()
	charge, ok := ApplyMarkup(MaxCost(), "1.5")
	if !ok || charge.String() != "1499999999999.999999999999" {
		t.Fatalf("marked-up charge = %s, priced = %v", charge.String(), ok)
	}
}

func TestEvaluatePriceForValidMediaUnits(t *testing.T) {
	t.Parallel()
	price := Price{UnitPrice: rate("2")}
	discount, err := NewRoutingDiscount(RoutingDiscount{Multiplier: "0.75"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		mediaUnits       string
		list, contracted string
	}{
		{"-0.000000", "0", "0"},
		{"1000000000000", "2000000000000", "1500000000000"},
	} {
		t.Run(tc.mediaUnits, func(t *testing.T) {
			u := usage.AttemptUsage{
				Evidence:        usage.EvidenceObservedComplete,
				UsageQuantities: usage.UsageQuantities{MediaUnits: &tc.mediaUnits},
			}
			if _, err := usage.ValidateUsage(&u, 0); err != nil {
				t.Fatal(err)
			}
			got := EvaluatePrice(&price, u, settledAt, true, rate("0.75"))
			if !got.Complete || got.ListCost.String() != tc.list || got.Cost.String() != tc.contracted {
				t.Errorf("quote = %s list, %s contracted, complete = %v, want %s list and %s contracted",
					got.ListCost, got.Cost, got.Complete, tc.list, tc.contracted)
			}
			wantReserved := tc.contracted
			if tc.mediaUnits == "1000000000000" {
				wantReserved = MaxCost().String()
			}
			cost, ok := NewRoutingPrice(RoutingPrice{Price: price}).Cost(u, settledAt, true, discount)
			if !ok || cost.String() != wantReserved {
				t.Errorf("routing cost = %s, priced = %v, want %s", cost, ok, wantReserved)
			}
		})
	}
}

func TestEvaluatePriceRetainedBreakdownAndRowDecode(t *testing.T) {
	t.Parallel()
	raw := []byte(`{
		"provider_kind":"openai",
		"model":"ft",
		"operation":"fine_tuning",
		"currency":"USD ",
		"training_per_million":2.500000000000,
		"storage_per_gib_day":0.100000000000,
		"cache_storage_per_million_token_hour":0.200000000000,
		"container_per_session":{"4g":"0.12"}
	}`)
	price, err := DecodePriceRowJSON(raw)
	if err != nil {
		t.Fatalf("DecodePriceRowJSON: %v", err)
	}
	if price.Currency != "USD" {
		t.Fatalf("Currency = %q, want USD", price.Currency)
	}
	u := usage.AttemptUsage{
		Evidence: usage.EvidenceObservedIncomplete,
		Deferred: true,
		UsageQuantities: usage.UsageQuantities{
			TrainingTokens:     tokens(1_000_000),
			StorageByteSeconds: rate("92771293593600"),
			CacheTokenSeconds:  rate("3600000000"),
			ContainerSessions:  map[string]int64{"4g": 2},
		},
	}
	// EvaluatePrice checks dimension completeness independently of Evidence.
	got := EvaluatePrice(&price, u, time.Now().UTC(), true, nil)
	if !got.Complete || got.Cost.String() != "3.04" || got.ListCost.String() != "3.04" {
		t.Fatalf("retained EvaluatePrice = %+v", got)
	}
	encoded, err := json.Marshal(got.Lines)
	if err != nil {
		t.Fatalf("marshal lines: %v", err)
	}
	if len(got.Lines) != 4 || string(encoded) == "null" {
		t.Fatalf("retained lines = %s", encoded)
	}
}
