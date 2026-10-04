package usage

import (
	"math/big"
	"math/rand/v2"
	"testing"
)

func rate(text string) *string { return &text }

func tokens(value int64) *int64 { return &value }

func completeUsage() AttemptUsage { return AttemptUsage{Observed: true, Complete: true} }

func TestPriceCostMatchesHandComputedBills(t *testing.T) {
	t.Parallel()
	base := Price{InputPerMillion: rate("3.00"), OutputPerMillion: rate("15.00")}
	cached := Price{InputPerMillion: rate("2"), CachedInputPerMillion: rate("0.5")}
	writes := Price{
		InputPerMillion: rate("3"), CacheWriteInputPerMillion: rate("3.75"),
		CacheWrite1HInputPerMillion: rate("6"),
	}
	for _, test := range []struct {
		name  string
		price Price
		usage func() AttemptUsage
		want  string
	}{
		{"input and output", base, func() AttemptUsage {
			u := completeUsage()
			u.InputTokens, u.OutputTokens = tokens(1000), tokens(500)
			return u
		}, "0.0105"},
		{"input alone prices an embedding", base, func() AttemptUsage {
			u := completeUsage()
			u.InputTokens = tokens(2_000_000)
			return u
		}, "6"},
		{"cached tokens bill at the cached rate", cached, func() AttemptUsage {
			u := completeUsage()
			u.InputTokens, u.CachedInputTokens = tokens(10_000), tokens(4_000)
			return u
		}, "0.014"},
		{"cached tokens bill at the input rate without a cached tier", base, func() AttemptUsage {
			u := completeUsage()
			u.InputTokens, u.CachedInputTokens = tokens(1000), tokens(900)
			return u
		}, "0.003"},
		{"cache writes fall back from their duration to the write rate", writes, func() AttemptUsage {
			u := completeUsage()
			u.InputTokens, u.CacheWriteInputTokens = tokens(1000), tokens(600)
			u.CacheWrite5MInputTokens, u.CacheWrite1HInputTokens = tokens(200), tokens(300)
			return u
		}, "0.004125"},
		{"media units bill at the unit price", Price{UnitPrice: rate("0.04")}, func() AttemptUsage {
			u := completeUsage()
			u.MediaUnits = rate("2.5")
			return u
		}, "0.1"},
		{"usage without any dimension costs nothing", base, completeUsage, "0"},
		{"a half unit rounds away from zero", Price{InputPerMillion: rate("0.000000000001")}, func() AttemptUsage {
			u := completeUsage()
			u.InputTokens = tokens(500_000)
			return u
		}, "0.000000000001"},
		{"below half a unit rounds down", Price{InputPerMillion: rate("0.000000000001")}, func() AttemptUsage {
			u := completeUsage()
			u.InputTokens = tokens(499_999)
			return u
		}, "0"},
		// PostgreSQL divides at the scale it selects for the quotient, which here
		// is sixteen places: the rounding at the sixteenth carries into the twelfth
		// and bills one unit more than the exact quotient would.
		{"the quotient is rounded at the scale PostgreSQL gives it", Price{InputPerMillion: rate("12345678.000000499999")}, func() AttemptUsage {
			u := completeUsage()
			u.InputTokens = tokens(1)
			return u
		}, "12.345678000001"},
		{"an amount past the column saturates", Price{InputPerMillion: rate("100000000000")}, func() AttemptUsage {
			u := completeUsage()
			u.InputTokens = tokens(2_000_000_000)
			return u
		}, "999999999999.999999999999"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, ok := test.price.Cost(test.usage())
			if !ok || got.String() != test.want {
				t.Fatalf("Cost = %q, %v, want %q", got.String(), ok, test.want)
			}
			routing := &RoutingPrice{Price: test.price}
			if again, ok := routing.Cost(test.usage()); !ok || again.Cmp(got) != 0 {
				t.Fatalf("a routing price without cached rates costs %q, want %q", again.String(), got.String())
			}
			routing.rates = parseRates(&routing.Price)
			if again, ok := routing.Cost(test.usage()); !ok || again.Cmp(got) != 0 {
				t.Fatalf("a routing price with cached rates costs %q, want %q", again.String(), got.String())
			}
		})
	}
}

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
			if got, ok := test.price.Cost(test.usage()); ok {
				t.Fatalf("Cost = %q, true, want no cost", got.String())
			}
		})
	}
	var missing *RoutingPrice
	if _, ok := missing.Cost(completeUsage()); ok {
		t.Fatal("a missing price costs something")
	}
	if _, ok := missing.CostBound(10, 10, true); ok {
		t.Fatal("a missing price bounds something")
	}
}

func TestCostBoundIsACeilingOnEveryTierMix(t *testing.T) {
	t.Parallel()
	price := &RoutingPrice{Price: Price{
		InputPerMillion: rate("3"), CachedInputPerMillion: rate("0.3"), CacheWriteInputPerMillion: rate("3.75"),
		CacheWrite1HInputPerMillion: rate("6"), OutputPerMillion: rate("15"),
	}}
	got, ok := price.CostBound(1_000_000, 100_000, true)
	// A million input tokens at the dearest input rate, 6, plus the reply, 1.5.
	if !ok || got.String() != "7.5" {
		t.Fatalf("CostBound = %q, %v, want 7.5", got.String(), ok)
	}
	// An embedding has no reply to bound, so a price without an output rate
	// still prices it.
	embedding := &RoutingPrice{Price: Price{InputPerMillion: rate("0.02")}}
	if got, ok := embedding.CostBound(1000, 4096, false); !ok || got.String() != "0.00002" {
		t.Fatalf("embedding CostBound = %q, %v", got.String(), ok)
	}
	if _, ok := embedding.CostBound(1000, 4096, true); ok {
		t.Fatal("a generation without an output rate has a bound")
	}
	if _, ok := (&RoutingPrice{Price: Price{OutputPerMillion: rate("1")}}).CostBound(1, 1, true); ok {
		t.Fatal("a price without an input rate has a bound")
	}
	// Each division rounds up, so a single token never bounds at nothing.
	if got, ok := price.CostBound(1, 0, true); !ok || got.String() != "0.000006" {
		t.Fatalf("one token CostBound = %q, %v", got.String(), ok)
	}
	tiny := &RoutingPrice{Price: Price{InputPerMillion: rate("0.000000000001"), OutputPerMillion: rate("0.000000000001")}}
	if got, ok := tiny.CostBound(1, 1, true); !ok || got.String() != "0.000000000002" {
		t.Fatalf("rounded-up CostBound = %q, %v", got.String(), ok)
	}

	random := rand.New(rand.NewPCG(7, 11))
	randomRate := func() *string {
		whole, fraction := random.Int64N(1000), random.Int64N(1_000_000_000_000)
		text := big.NewRat(whole, 1)
		text.Add(text, big.NewRat(fraction, 1_000_000_000_000))
		value := text.FloatString(12)
		return &value
	}
	for range 2000 {
		p := &RoutingPrice{Price: Price{InputPerMillion: randomRate(), OutputPerMillion: randomRate()}}
		if random.IntN(2) == 0 {
			p.CachedInputPerMillion = randomRate()
		}
		if random.IntN(2) == 0 {
			p.CacheWriteInputPerMillion = randomRate()
		}
		if random.IntN(3) == 0 {
			p.CacheWrite5MInputPerMillion = randomRate()
		}
		if random.IntN(3) == 0 {
			p.CacheWrite1HInputPerMillion = randomRate()
		}
		input, output := random.Int64N(5_000_000), random.Int64N(500_000)
		u := completeUsage()
		u.InputTokens, u.OutputTokens = &input, &output
		var cached, write, write1h, write5m int64
		if p.CachedInputPerMillion != nil || random.IntN(2) == 0 {
			cached = random.Int64N(input + 1)
			u.CachedInputTokens = &cached
		}
		if random.IntN(2) == 0 {
			write = random.Int64N(input - cached + 1)
			u.CacheWriteInputTokens = &write
			write1h = random.Int64N(write + 1)
			u.CacheWrite1HInputTokens = &write1h
			// The two timed tiers are parts of the write total, so together they
			// never exceed it.
			write5m = random.Int64N(write - write1h + 1)
			u.CacheWrite5MInputTokens = &write5m
		}
		actual, ok := p.Cost(u)
		bound, boundOK := p.CostBound(input, output, true)
		if !ok || !boundOK || bound.Cmp(actual) < 0 {
			t.Fatalf("bound %q < cost %q for %+v / %+v", bound.String(), actual.String(), *p, u)
		}
	}
}

func TestCostArithmetic(t *testing.T) {
	t.Parallel()
	var zero Cost
	if !zero.IsZero() || zero.String() != "0" || zero.Cmp(Cost{}) != 0 {
		t.Fatal("the zero Cost is not nothing")
	}
	one, _ := (Price{UnitPrice: rate("1")}).Cost(AttemptUsage{Complete: true, MediaUnits: rate("1")})
	if one.String() != "1" || one.IsZero() || one.Cmp(zero) <= 0 || zero.Cmp(one) >= 0 {
		t.Fatalf("one = %q", one.String())
	}
	if sum := one.Add(one).Add(zero); sum.String() != "2" || zero.Add(one).String() != "1" {
		t.Fatalf("sum = %q", sum.String())
	}
	if got := MaxCost().Add(one); got.Cmp(MaxCost()) != 0 || got.String() != "999999999999.999999999999" {
		t.Fatalf("the sum of the largest cost and one = %q", got.String())
	}
	small, _ := (Price{UnitPrice: rate("0.000000000123")}).Cost(AttemptUsage{Complete: true, MediaUnits: rate("1")})
	if small.String() != "0.000000000123" {
		t.Fatalf("small = %q", small.String())
	}
}

// TestQuotientScaleFollowsNumericDiv pins select_div_scale against values worked
// out from PostgreSQL's base-10000 representation: the quotient keeps sixteen
// significant digits estimated from the weight and leading digit of the numerator
// and of the divisor's single digit 100 at weight 1, and never fewer places than
// the numerator's own twelve.
func TestQuotientScaleFollowsNumericDiv(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		numerator string
		want      int
	}{
		// 1.5: weight 0, leading digit 1 <= 100.
		{"1500000000000", 24},
		// 20000: weight 1, leading digit 2 <= 100.
		{"20000000000000000", 20},
		// 12345678: weight 1, leading digit 1234 > 100.
		{"12345678000000000000", 16},
		// 0.000001: the fraction group 0001 at weight -2.
		{"1000000", 32},
		// 100000000000: weight 2, leading digit 1000, so sixteen significant digits
		// need fewer than twelve places and the numerator's own twelve hold.
		{"100000000000000000000000", 12},
		// 1234567890123: thirteen digits are weight 3 with the leading digit 1.
		{"1234567890123000000000000", 12},
	} {
		numerator, _ := new(big.Int).SetString(test.numerator, 10)
		if got := quotientScale(numerator); got != test.want {
			t.Errorf("quotientScale(%s x 1e-12) = %d, want %d", test.numerator, got, test.want)
		}
	}
}

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
		if _, ok := price.CostBound(50_000, 4096, true); !ok {
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
		if _, ok := price.Cost(usage); !ok {
			b.Fatal("unpriced")
		}
	}
}
