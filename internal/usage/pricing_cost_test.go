package usage

import (
	"testing"
	"time"
)

func rate(text string) *string { return &text }

// settledAt is a fixed attempt start time for tests that do not exercise windows.
var settledAt = time.Date(2025, 6, 2, 12, 0, 0, 0, time.UTC)

func tokens(value int64) *int64 { return &value }

func completeUsage() AttemptUsage { return AttemptUsage{Observed: true, Complete: true} }

// PostgreSQL divides at the scale it selects for the quotient, which here
// is sixteen places: the rounding at the sixteenth carries into the twelfth
// and bills one unit more than the exact quotient would.

func TestPriceCostIsUnpricedWherePostgreSQLRecordsNoCost(t *testing.T) {
	t.Parallel()
	full := Price{
		InputPerMillion: rate("1"), OutputPerMillion: rate("2"), UnitPrice: rate("3"),
	}
	for _, test := range []struct {
		name  string
		price Price
		usage func() AttemptUsage
	}{
		{"incomplete usage", full, func() AttemptUsage {
			u := completeUsage()
			u.Complete, u.InputTokens = false, tokens(10)
			return u
		}},
		{"cached tokens without the input they came out of", full, func() AttemptUsage {
			u := completeUsage()
			u.CachedInputTokens = tokens(10)
			return u
		}},
		{"input without an input rate", Price{OutputPerMillion: rate("2")}, func() AttemptUsage {
			u := completeUsage()
			u.InputTokens = tokens(10)
			return u
		}},
		{"output without an output rate", Price{InputPerMillion: rate("1")}, func() AttemptUsage {
			u := completeUsage()
			u.InputTokens, u.OutputTokens = tokens(10), tokens(10)
			return u
		}},
		{"media without a unit price", Price{InputPerMillion: rate("1")}, func() AttemptUsage {
			u := completeUsage()
			u.MediaUnits = rate("1")
			return u
		}},
		{"a rate PostgreSQL could not have stored", Price{InputPerMillion: rate("1.0000000000001")}, func() AttemptUsage {
			u := completeUsage()
			u.InputTokens = tokens(10)
			return u
		}},
		{"a rate that is not a number", Price{InputPerMillion: rate("one")}, func() AttemptUsage {
			u := completeUsage()
			u.InputTokens = tokens(10)
			return u
		}},
		{"cache tiers that exceed their total", Price{InputPerMillion: rate("5"), CachedInputPerMillion: rate("0.1")}, func() AttemptUsage {
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

// A million input tokens at the dearest input rate, 6, plus the reply, 1.5.

// An embedding has no reply to bound, so a price without an output rate
// still prices it.

// Each division rounds up, so a single token never bounds at nothing.

// The two timed tiers are parts of the write total, so together they
// never exceed it.

// 1.5: weight 0, leading digit 1 <= 100.

// 20000: weight 1, leading digit 2 <= 100.

// 12345678: weight 1, leading digit 1234 > 100.

// 0.000001: the fraction group 0001 at weight -2.

// 100000000000: weight 2, leading digit 1000, so sixteen significant digits
// need fewer than twelve places and the numerator's own twelve hold.

// 1234567890123: thirteen digits are weight 3 with the leading digit 1.

// The estimate is made for every request of a key with a cost budget, and the
// settlement once more when it ends, so both are measured.
func BenchmarkCostBound(b *testing.B) {
	price := &RoutingPrice{Price: Price{
		InputPerMillion: rate("3"), CachedInputPerMillion: rate("0.3"), CacheWriteInputPerMillion: rate("3.75"),
		OutputPerMillion: rate("15"),
	}}
	price.rates = parseRates(&price.Price)
	b.ReportAllocs()
	for b.Loop() {
		if _, ok := price.CostBound(50_000, 4096, true, settledAt, nil); !ok {
			b.Fatal("unpriced")
		}
	}
}

func BenchmarkPriceCost(b *testing.B) {
	price := &RoutingPrice{Price: Price{
		InputPerMillion: rate("3"), CachedInputPerMillion: rate("0.3"), CacheWriteInputPerMillion: rate("3.75"),
		OutputPerMillion: rate("15"),
	}}
	price.rates = parseRates(&price.Price)
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

// The explicit rate prices the usage itself, ignoring provider cost.

// A vendor-scoped entry wins over the unscoped one.

// Markup multiplies the already-rounded cost in one rounding.

// No explicit match and an unpriced cost is no charge.

// A zero cost — a committed attempt's — charges zero under any markup.

// The charge bound mirrors the same shapes.

// The explicit rate's bound is its own bound, markup-free: the vendor
// entry prices input only, so a generation request cannot bound.

// The installation card arriving first cannot shadow a better scope.

// A project card whose only revision is future falls through to the
// organization card — and without that, to the installation card.

// Stale inputs cannot answer at all.
