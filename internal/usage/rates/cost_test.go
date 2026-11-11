package rates

import (
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/usage"
)

func rate(text string) *string { return &text }

// settledAt is a fixed attempt start time for tests that do not exercise windows.
var settledAt = time.Date(2025, 6, 2, 12, 0, 0, 0, time.UTC)

func tokens(value int64) *int64 { return &value }

func completeUsage() usage.AttemptUsage {
	return usage.AttemptUsage{Evidence: usage.EvidenceObservedComplete}
}

func TestPriceCostIsUnpricedWherePostgreSQLRecordsNoCost(t *testing.T) {
	t.Parallel()
	full := Price{
		InputPerMillion: rate("1"), OutputPerMillion: rate("2"), UnitPrice: rate("3"),
	}
	for _, test := range []struct {
		name  string
		price Price
		usage func() usage.AttemptUsage
	}{
		{"incomplete usage", full, func() usage.AttemptUsage {
			u := completeUsage()
			u.Evidence, u.InputTokens = usage.EvidenceObservedIncomplete, tokens(10)
			return u
		}},
		{"cached tokens without the input they came out of", full, func() usage.AttemptUsage {
			u := completeUsage()
			u.CachedInputTokens = tokens(10)
			return u
		}},
		{"input without an input rate", Price{OutputPerMillion: rate("2")}, func() usage.AttemptUsage {
			u := completeUsage()
			u.InputTokens = tokens(10)
			return u
		}},
		{"output without an output rate", Price{InputPerMillion: rate("1")}, func() usage.AttemptUsage {
			u := completeUsage()
			u.InputTokens, u.OutputTokens = tokens(10), tokens(10)
			return u
		}},
		{"media without a unit price", Price{InputPerMillion: rate("1")}, func() usage.AttemptUsage {
			u := completeUsage()
			u.MediaUnits = rate("1")
			return u
		}},
		{"a rate PostgreSQL could not have stored", Price{InputPerMillion: rate("1.0000000000001")}, func() usage.AttemptUsage {
			u := completeUsage()
			u.InputTokens = tokens(10)
			return u
		}},
		{"a rate that is not a number", Price{InputPerMillion: rate("one")}, func() usage.AttemptUsage {
			u := completeUsage()
			u.InputTokens = tokens(10)
			return u
		}},
		{"cache tiers that exceed their total", Price{InputPerMillion: rate("5"), CachedInputPerMillion: rate("0.1")}, func() usage.AttemptUsage {
			u := completeUsage()
			u.InputTokens, u.CachedInputTokens = tokens(10), tokens(20)
			return u
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got, ok := test.price.Cost(test.usage(), settledAt, true); ok {
				t.Fatalf("Cost = %q, true, want no cost", got.String())
			}
		})
	}
	var missing *RoutingPrice
	if _, ok := missing.Cost(completeUsage(), settledAt, true, nil); ok {
		t.Fatal("a missing price costs something")
	}
	if _, ok := missing.CostBound(10, 10, true, settledAt, nil); ok {
		t.Fatal("a missing price bounds something")
	}
}

// The estimate is made for every request of a key with a cost budget, and the
// settlement once more when it ends, so both are measured.
func BenchmarkCostBound(b *testing.B) {
	price := NewRoutingPrice(RoutingPrice{Price: Price{
		InputPerMillion: rate("3"), CachedInputPerMillion: rate("0.3"), CacheWriteInputPerMillion: rate("3.75"),
		OutputPerMillion: rate("15"),
	}})
	price = NewRoutingPrice(*price)
	b.ReportAllocs()
	for b.Loop() {
		if _, ok := price.CostBound(50_000, 4096, true, settledAt, nil); !ok {
			b.Fatal("unpriced")
		}
	}
}

func BenchmarkPriceCost(b *testing.B) {
	price := NewRoutingPrice(RoutingPrice{Price: Price{
		InputPerMillion: rate("3"), CachedInputPerMillion: rate("0.3"), CacheWriteInputPerMillion: rate("3.75"),
		OutputPerMillion: rate("15"),
	}})
	price = NewRoutingPrice(*price)
	usage := completeUsage()
	usage.InputTokens, usage.CachedInputTokens, usage.CacheWriteInputTokens, usage.OutputTokens =
		tokens(50_000), tokens(20_000), tokens(5_000), tokens(300)
	b.ReportAllocs()
	for b.Loop() {
		if _, ok := price.Cost(usage, settledAt, true, nil); !ok {
			b.Fatal("unpriced")
		}
	}
}

// usageOf builds observed complete usage on input/output totals.
func usageOf(input, output int64) usage.AttemptUsage {
	return usage.AttemptUsage{Evidence: usage.EvidenceObservedComplete, UsageQuantities: usage.UsageQuantities{InputTokens: tokens(input), OutputTokens: tokens(output)}}
}

func TestCostPerRequestToolsAndBatch(t *testing.T) {
	t.Parallel()
	price := Price{ProviderKind: "openai", Model: "m", Operation: "generation",
		InputPerMillion: rate("1"), PerRequest: rate("0.05"), BatchMultiplier: rate("0.5"),
		ToolCalls: map[string]string{"web_search": "0.01"}, Currency: "USD"}
	u := usageOf(1_000_000, 0)
	u.OutputTokens = nil
	u.ToolCalls = map[string]int64{"web_search": 3, "file_search": 0}
	got, ok := price.Cost(u, settledAt, true)
	if !ok || got.String() != "1.08" {
		// 1 + 0.05 + 3*0.01; the zero-count tool needs no fee.
		t.Fatalf("Cost = %q, %v, want 1.08", got.String(), ok)
	}
	// A failed attempt pays no per-request fee.
	got, ok = price.Cost(u, settledAt, false)
	if !ok || got.String() != "1.03" {
		t.Fatalf("unsuccessful Cost = %q, %v, want 1.03", got.String(), ok)
	}
	// A tool with a nonzero count and no fee leaves the attempt unpriced.
	u.ToolCalls = map[string]int64{"file_search": 1}
	if _, ok := price.Cost(u, settledAt, true); ok {
		t.Fatal("a tool without a fee priced")
	}
	// Batch usage multiplies the whole charge.
	u.ToolCalls = nil
	u.Batch = true
	got, ok = price.Cost(u, settledAt, true)
	if !ok || got.String() != "0.525" {
		t.Fatalf("batch Cost = %q, %v, want 0.525", got.String(), ok)
	}
	// And is unpriced when the price names no batch multiplier.
	plain := Price{ProviderKind: "openai", Model: "m", Operation: "generation",
		InputPerMillion: rate("1"), OutputPerMillion: rate("2"), Currency: "USD"}
	if _, ok := plain.Cost(u, settledAt, true); ok {
		t.Fatal("batch usage priced without a batch multiplier")
	}
}

func TestCostBoundWithComponents(t *testing.T) {
	t.Parallel()
	price := NewRoutingPrice(RoutingPrice{Price: Price{
		InputPerMillion: rate("2"), AudioInputPerMillion: rate("9"),
		OutputPerMillion: rate("3"), AudioOutputPerMillion: rate("7"),
		PerRequest: rate("0.01"),
		Windows: &PriceWindows{TimeZone: "UTC", Periods: []PriceWindowPeriod{
			{Days: []int{1, 2, 3, 4, 5, 6, 7}, Start: "00:00", End: "24:00", Multiplier: "2"}}},
		Tiers: []PriceTier{{AboveInputTokens: 10_000_000, InputPerMillion: rate("5"), AudioInputPerMillion: rate("20"),
			OutputPerMillion: rate("4"), AudioOutputPerMillion: rate("6")}}}})
	// Below the tier: input at max(2,9)=9 and output at max(3,7)=7, plus the
	// request fee, then the window multiplier of two, rounded up.
	got, ok := price.CostBound(1_000_000, 1_000_000, true, settledAt, nil)
	if !ok || got.String() != "32.02" {
		// (9 + 7 + 0.01) * 2
		t.Fatalf("CostBound = %q, %v, want 32.02", got.String(), ok)
	}
	// Above the tier: input at max(5,20)=20, output at max(4,6)=6.
	got, ok = price.CostBound(20_000_000, 1_000_000, true, settledAt, nil)
	if !ok || got.String() != "812.02" {
		// (400 + 6 + 0.01) * 2
		t.Fatalf("tiered CostBound = %q, %v, want 812.02", got.String(), ok)
	}
	// A window-free instant pays no multiplier.
	outside := time.Date(2025, 6, 2, 12, 0, 0, 0, time.UTC)
	price.Windows = nil
	price = NewRoutingPrice(*price)
	got, ok = price.CostBound(1_000_000, 0, false, outside, nil)
	if !ok || got.String() != "9.01" {
		t.Fatalf("windowless CostBound = %q, %v, want 9.01", got.String(), ok)
	}
}

func TestRetainedEvalAggregateUsageCannotChooseInvocationTiers(t *testing.T) {
	price := Price{ProviderKind: "openai", Model: "m", Operation: "generation", InputPerMillion: new("1"), OutputPerMillion: new("0"), Tiers: []PriceTier{{AboveInputTokens: 100_000, InputPerMillion: new("3"), OutputPerMillion: new("0")}}}
	evidence := usage.AttemptUsage{Evidence: usage.EvidenceObservedComplete, Deferred: true, UsageQuantities: usage.UsageQuantities{InvocationCount: new(int64(2)), InputTokens: new(int64(120_000)), OutputTokens: new(int64(0))}}
	if cost, priced := price.Cost(evidence, time.Now(), true); priced {
		t.Fatalf("two 60k invocations charged aggregate tier: %s", cost.String())
	}
	card := RoutingCardRevision{Prices: []RoutingPrice{*NewRoutingPrice(RoutingPrice{Price: price})}}
	if charge, priced := card.Charge(evidence, time.Now(), true, nil, "openai", "openai", "m", "generation"); priced {
		t.Fatalf("explicit rate card charged aggregate tier: %s", charge.String())
	}
	evidence.InvocationCount = new(int64(1))
	evidence.InputTokens = new(int64(60_000))
	if cost, priced := price.Cost(evidence, time.Now(), true); !priced || cost.String() != "0.06" {
		t.Fatalf("single invocation pricing changed: %s %v", cost.String(), priced)
	}
}
