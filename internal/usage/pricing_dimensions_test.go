package usage

import (
	"testing"
	"time"
)

// priceOf normalizes one entry as CreateRevision would, failing the test on refusal.
func priceOf(t *testing.T, p Price) Price {
	t.Helper()
	entry, err := normalizePrice(p, nil)
	if err != nil {
		t.Fatalf("normalizePrice: %v", err)
	}
	return entry
}

func priceRefused(t *testing.T, p Price) {
	t.Helper()
	if _, err := normalizePrice(p, nil); err == nil {
		t.Fatalf("price %+v was accepted", p)
	}
}

func basePrice() Price {
	return Price{ProviderKind: "openai", Model: "m", Operation: "generation",
		InputPerMillion: rate("1"), OutputPerMillion: rate("2"), Currency: "USD"}
}

func TestPriceValidationComponents(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		price func() Price
	}{
		{"tool fees alone satisfy the rate requirement", func() Price {
			p := basePrice()
			p.InputPerMillion, p.OutputPerMillion = nil, nil
			p.ToolCalls = map[string]string{"web_search": "0.01"}
			return p
		}},
		{"per-request alone satisfies the rate requirement", func() Price {
			p := basePrice()
			p.InputPerMillion, p.OutputPerMillion = nil, nil
			p.PerRequest = rate("0.05")
			return p
		}},
		{"audio rates need no text rates", func() Price {
			p := basePrice()
			p.InputPerMillion, p.OutputPerMillion = nil, nil
			p.AudioInputPerMillion, p.AudioOutputPerMillion = rate("4"), rate("8")
			return p
		}},
		{"tiers carry the entry's per-million set", func() Price {
			p := basePrice()
			p.Tiers = []PriceTier{{AboveInputTokens: 1000, InputPerMillion: rate("2"), OutputPerMillion: rate("4")}}
			return p
		}},
		{"windows normalize and sort the week", func() Price {
			p := basePrice()
			p.Windows = &PriceWindows{TimeZone: "America/New_York", Periods: []PriceWindowPeriod{
				{Days: []int{1, 2, 3, 4, 5, 6, 7}, Start: "20:00", End: "24:00", Multiplier: "0.5"},
				{Days: []int{6, 7}, Start: "00:00", End: "20:00", Multiplier: "0.25"}}}
			return p
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			priceOf(t, test.price())
		})
	}
	for _, test := range []struct {
		name  string
		price func() Price
	}{
		{"empty tool fee set", func() Price {
			p := basePrice()
			p.ToolCalls = map[string]string{}
			return p
		}},
		{"tool fee names a malformed tool", func() Price {
			p := basePrice()
			p.ToolCalls = map[string]string{"Web-Search": "0.01"}
			return p
		}},
		{"too many tool fees", func() Price {
			p := basePrice()
			p.ToolCalls = map[string]string{}
			for index := range maxToolCalls + 1 {
				p.ToolCalls[string(rune('a'+index))+"_tool"] = "0.01"
			}
			return p
		}},
		{"a tier missing an entry rate", func() Price {
			p := basePrice()
			p.Tiers = []PriceTier{{AboveInputTokens: 1000, InputPerMillion: rate("2")}}
			return p
		}},
		{"a tier adding a rate the entry lacks", func() Price {
			p := basePrice()
			p.Tiers = []PriceTier{{AboveInputTokens: 1000, InputPerMillion: rate("2"),
				OutputPerMillion: rate("4"), AudioInputPerMillion: rate("9")}}
			return p
		}},
		{"unordered tiers", func() Price {
			p := basePrice()
			p.Tiers = []PriceTier{
				{AboveInputTokens: 2000, InputPerMillion: rate("2"), OutputPerMillion: rate("4")},
				{AboveInputTokens: 1000, InputPerMillion: rate("3"), OutputPerMillion: rate("5")}}
			return p
		}},
		{"a non-positive threshold", func() Price {
			p := basePrice()
			p.Tiers = []PriceTier{{AboveInputTokens: 0, InputPerMillion: rate("2"), OutputPerMillion: rate("4")}}
			return p
		}},
		{"too many tiers", func() Price {
			p := basePrice()
			for index := range maxTiers + 1 {
				p.Tiers = append(p.Tiers, PriceTier{AboveInputTokens: int64(index + 1),
					InputPerMillion: rate("2"), OutputPerMillion: rate("4")})
			}
			return p
		}},
		{"an unknown window zone", func() Price {
			p := basePrice()
			p.Windows = &PriceWindows{TimeZone: "Not/AZone", Periods: []PriceWindowPeriod{
				{Days: []int{1}, Start: "01:00", End: "02:00", Multiplier: "1"}}}
			return p
		}},
		{"overlapping periods on a shared day", func() Price {
			p := basePrice()
			p.Windows = &PriceWindows{TimeZone: "UTC", Periods: []PriceWindowPeriod{
				{Days: []int{1, 2}, Start: "10:00", End: "12:00", Multiplier: "1"},
				{Days: []int{2, 3}, Start: "11:00", End: "13:00", Multiplier: "1"}}}
			return p
		}},
		{"adjacent periods do not overlap", func() Price {
			p := basePrice()
			p.Windows = &PriceWindows{TimeZone: "UTC", Periods: []PriceWindowPeriod{
				{Days: []int{2}, Start: "13:00", End: "10:00", Multiplier: "1"}}}
			return p
		}},
		{"an end that is not after its start", func() Price {
			p := basePrice()
			p.Windows = &PriceWindows{TimeZone: "UTC", Periods: []PriceWindowPeriod{
				{Days: []int{2}, Start: "13:00", End: "10:00", Multiplier: "1"}}}
			return p
		}},
		{"a repeated day", func() Price {
			p := basePrice()
			p.Windows = &PriceWindows{TimeZone: "UTC", Periods: []PriceWindowPeriod{
				{Days: []int{1, 1}, Start: "10:00", End: "12:00", Multiplier: "1"}}}
			return p
		}},
		{"a day out of range", func() Price {
			p := basePrice()
			p.Windows = &PriceWindows{TimeZone: "UTC", Periods: []PriceWindowPeriod{
				{Days: []int{8}, Start: "10:00", End: "12:00", Multiplier: "1"}}}
			return p
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			priceRefused(t, test.price())
		})
	}
}

// usageOf builds observed complete usage on input/output totals.
func usageOf(input, output int64) AttemptUsage {
	return AttemptUsage{Observed: true, Complete: true, InputTokens: tokens(input), OutputTokens: tokens(output)}
}

func TestCostTiers(t *testing.T) {
	t.Parallel()
	price := priceOf(t, Price{ProviderKind: "openai", Model: "m", Operation: "generation",
		InputPerMillion: rate("1"), OutputPerMillion: rate("2"), Currency: "USD",
		Tiers: []PriceTier{
			{AboveInputTokens: 1000, InputPerMillion: rate("3"), OutputPerMillion: rate("4")},
			{AboveInputTokens: 10000, InputPerMillion: rate("5"), OutputPerMillion: rate("6")}}})
	for _, test := range []struct {
		input int64
		want  string
	}{
		{1000, "0.001002"}, // at the threshold the base rate applies
		{1001, "0.003007"}, // the lowest exceeded tier replaces the whole set
		{10001, "0.050011"},
	} {
		got, ok := price.Cost(usageOf(test.input, 1), settledAt, true)
		if !ok || got.String() != test.want {
			t.Fatalf("input %d: Cost = %q, %v, want %q", test.input, got.String(), ok, test.want)
		}
	}
}

func TestCostModalityShares(t *testing.T) {
	t.Parallel()
	price := priceOf(t, Price{ProviderKind: "openai", Model: "m", Operation: "generation",
		InputPerMillion: rate("1"), CachedInputPerMillion: rate("0.25"),
		OutputPerMillion:     rate("2"),
		AudioInputPerMillion: rate("4"), CachedAudioInputPerMillion: rate("0.5"),
		AudioOutputPerMillion: rate("8"),
		ImageInputPerMillion:  rate("6"), ImageOutputPerMillion: rate("12"), Currency: "USD"})
	u := usageOf(1000, 300)
	u.CachedInputTokens, u.CachedAudioInputTokens = tokens(500), tokens(100)
	u.AudioInputTokens, u.ImageInputTokens = tokens(200), tokens(100)
	u.AudioOutputTokens, u.ImageOutputTokens = tokens(40), tokens(60)
	// Input: text 300@1 + cached-text 400@0.25 + cached-audio 100@0.5 +
	// audio 100@4 + image 100@6 = 300+100+50+400+600 = 1450 /1e6 = 0.00145.
	// Output: text 200@2 + audio 40@8 + image 60@12 = 400+320+720 = 1440 /1e6 = 0.00144.
	got, ok := price.Cost(u, settledAt, true)
	if !ok || got.String() != "0.00289" {
		t.Fatalf("Cost = %q, %v, want 0.00289", got.String(), ok)
	}

	// With only text rates every share bills at the input or output rate.
	textOnly := priceOf(t, basePrice())
	got, ok = textOnly.Cost(u, settledAt, true)
	if !ok || got.String() != "0.0016" {
		// 1000@1 + 300@2 = 1600/1e6.
		t.Fatalf("text fallback Cost = %q, %v, want 0.0016", got.String(), ok)
	}
}

func TestCostAudioOnlyPrice(t *testing.T) {
	t.Parallel()
	price := priceOf(t, Price{ProviderKind: "openai", Model: "m", Operation: "generation",
		AudioInputPerMillion: rate("4"), Currency: "USD"})
	// Text tokens cannot resolve against an audio-only rate set.
	if _, ok := price.Cost(usageOf(1000, 0), settledAt, true); ok {
		t.Fatal("text tokens priced against an audio-only price")
	}
	// Usage that is all audio resolves through the audio rate.
	u := usageOf(1000, 0)
	u.OutputTokens = nil
	u.AudioInputTokens = tokens(1000)
	got, ok := price.Cost(u, settledAt, true)
	if !ok || got.String() != "0.004" {
		t.Fatalf("Cost = %q, %v, want 0.004", got.String(), ok)
	}
}

func TestCostWindows(t *testing.T) {
	t.Parallel()
	price := priceOf(t, Price{ProviderKind: "openai", Model: "m", Operation: "generation",
		InputPerMillion: rate("1"), Currency: "USD",
		Windows: &PriceWindows{TimeZone: "America/New_York", Periods: []PriceWindowPeriod{
			{Days: []int{1, 2, 3, 4, 5}, Start: "22:00", End: "24:00", Multiplier: "0.5"},
			{Days: []int{6, 7}, Start: "00:00", End: "23:59", Multiplier: "0.25"}}}})
	// EST (standard time): Friday 23:00 local is Saturday 04:00 UTC.
	million := usageOf(1_000_000, 0)
	million.OutputTokens = nil
	at := time.Date(2025, 1, 4, 4, 0, 0, 0, time.UTC)
	got, ok := price.Cost(million, at, true)
	if !ok || got.String() != "0.5" {
		t.Fatalf("Cost = %q, %v, want 0.5", got.String(), ok)
	}
	// EDT (daylight time): Friday 23:00 local is Saturday 03:00 UTC.
	at = time.Date(2025, 6, 7, 3, 0, 0, 0, time.UTC)
	got, ok = price.Cost(million, at, true)
	if !ok || got.String() != "0.5" {
		t.Fatalf("DST Cost = %q, %v, want 0.5", got.String(), ok)
	}
	// Saturday noon local prices at the weekend multiplier.
	at = time.Date(2025, 6, 7, 16, 0, 0, 0, time.UTC)
	got, ok = price.Cost(million, at, true)
	if !ok || got.String() != "0.25" {
		t.Fatalf("weekend Cost = %q, %v, want 0.25", got.String(), ok)
	}
	// An attempt outside every period prices at list.
	at = time.Date(2025, 6, 4, 15, 0, 0, 0, time.UTC)
	got, ok = price.Cost(million, at, true)
	if !ok || got.String() != "1" {
		t.Fatalf("list Cost = %q, %v, want 1", got.String(), ok)
	}
}

func TestCostPerRequestToolsAndBatch(t *testing.T) {
	t.Parallel()
	price := priceOf(t, Price{ProviderKind: "openai", Model: "m", Operation: "generation",
		InputPerMillion: rate("1"), PerRequest: rate("0.05"), BatchMultiplier: rate("0.5"),
		ToolCalls: map[string]string{"web_search": "0.01"}, Currency: "USD"})
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
	plain := priceOf(t, basePrice())
	if _, ok := plain.Cost(u, settledAt, true); ok {
		t.Fatal("batch usage priced without a batch multiplier")
	}
}

func TestCostMultiplierRounding(t *testing.T) {
	t.Parallel()
	// 1000 tokens at 1 per million is 0.001; at a multiplier of
	// 1.0000005 the product is 0.0010000005, which rounds away from zero at the
	// twelfth decimal.
	price := priceOf(t, Price{ProviderKind: "openai", Model: "m", Operation: "generation",
		InputPerMillion: rate("1"), BatchMultiplier: rate("1.0000005"), Currency: "USD"})
	u := usageOf(1000, 0)
	u.OutputTokens = nil
	u.Batch = true
	got, ok := price.Cost(u, settledAt, true)
	if !ok || got.String() != "0.0010000005" {
		t.Fatalf("Cost = %q, %v, want 0.0010000005", got.String(), ok)
	}
	// Half a trillionth rounds away from zero at the twelfth digit.
	price = priceOf(t, Price{ProviderKind: "openai", Model: "m", Operation: "generation",
		InputPerMillion: rate("0.5"), BatchMultiplier: rate("0.000001"), Currency: "USD"})
	u.InputTokens = tokens(1)
	got, ok = price.Cost(u, settledAt, true)
	if !ok || got.String() != "0.000000000001" {
		t.Fatalf("Cost = %q, %v, want 0.000000000001", got.String(), ok)
	}
}

func TestCostBoundWithComponents(t *testing.T) {
	t.Parallel()
	price := &RoutingPrice{Price: Price{
		InputPerMillion: rate("2"), AudioInputPerMillion: rate("9"),
		OutputPerMillion: rate("3"), AudioOutputPerMillion: rate("7"),
		PerRequest: rate("0.01"),
		Windows: &PriceWindows{TimeZone: "UTC", Periods: []PriceWindowPeriod{
			{Days: []int{1, 2, 3, 4, 5, 6, 7}, Start: "00:00", End: "24:00", Multiplier: "2"}}},
		Tiers: []PriceTier{{AboveInputTokens: 10_000_000, InputPerMillion: rate("5"), AudioInputPerMillion: rate("20"),
			OutputPerMillion: rate("4"), AudioOutputPerMillion: rate("6")}}}}
	price.rates = parseRates(&price.Price)
	// Below the tier: input at max(2,9)=9 and output at max(3,7)=7, plus the
	// request fee, then the window multiplier of two, rounded up.
	got, ok := price.CostBound(1_000_000, 1_000_000, true, settledAt)
	if !ok || got.String() != "32.02" {
		// (9 + 7 + 0.01) * 2
		t.Fatalf("CostBound = %q, %v, want 32.02", got.String(), ok)
	}
	// Above the tier: input at max(5,20)=20, output at max(4,6)=6.
	got, ok = price.CostBound(20_000_000, 1_000_000, true, settledAt)
	if !ok || got.String() != "812.02" {
		// (400 + 6 + 0.01) * 2
		t.Fatalf("tiered CostBound = %q, %v, want 812.02", got.String(), ok)
	}
	// A window-free instant pays no multiplier.
	outside := time.Date(2025, 6, 2, 12, 0, 0, 0, time.UTC)
	price.Windows = nil
	price.rates = parseRates(&price.Price)
	got, ok = price.CostBound(1_000_000, 0, false, outside)
	if !ok || got.String() != "9.01" {
		t.Fatalf("windowless CostBound = %q, %v, want 9.01", got.String(), ok)
	}
}

func TestValidateUsageModalityInvariants(t *testing.T) {
	t.Parallel()
	valid := func(u AttemptUsage) AttemptUsage {
		event := metadataEvent()
		event.Attempts[0].Usage = &u
		if _, err := Validate(event); err != nil {
			t.Fatalf("usage %+v was refused: %v", u, err)
		}
		return u
	}
	_ = valid
	invalid := func(u AttemptUsage) {
		event := metadataEvent()
		event.Attempts[0].Usage = &u
		if _, err := Validate(event); err == nil {
			t.Fatalf("usage %+v was accepted", u)
		}
	}
	for _, test := range []struct {
		name  string
		usage func() AttemptUsage
	}{
		{"audio share without input", func() AttemptUsage {
			u := completeUsage()
			u.AudioInputTokens = tokens(1)
			return u
		}},
		{"cached audio exceeds audio", func() AttemptUsage {
			u := usageOf(10, 0)
			u.AudioInputTokens, u.CachedAudioInputTokens = tokens(5), tokens(6)
			return u
		}},
		{"cached modality exceeds cached", func() AttemptUsage {
			u := usageOf(10, 0)
			u.CachedInputTokens, u.CachedAudioInputTokens, u.CachedImageInputTokens = tokens(4), tokens(3), tokens(3)
			return u
		}},
		{"shares exceed input", func() AttemptUsage {
			u := usageOf(10, 0)
			u.CachedInputTokens, u.AudioInputTokens = tokens(5), tokens(8)
			return u
		}},
		{"output shares exceed output", func() AttemptUsage {
			u := usageOf(10, 5)
			u.AudioOutputTokens, u.ImageOutputTokens = tokens(4), tokens(4)
			return u
		}},
		{"a malformed tool name", func() AttemptUsage {
			u := usageOf(1, 0)
			u.ToolCalls = map[string]int64{"Bad-Tool": 1}
			return u
		}},
		{"a negative tool count", func() AttemptUsage {
			u := usageOf(1, 0)
			u.ToolCalls = map[string]int64{"web_search": -1}
			return u
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			invalid(test.usage())
		})
	}
	for _, test := range []struct {
		name  string
		usage func() AttemptUsage
	}{
		{"modality shares inside the totals", func() AttemptUsage {
			u := usageOf(10, 5)
			u.CachedInputTokens = tokens(2)
			u.AudioInputTokens, u.CachedAudioInputTokens = tokens(5), tokens(2)
			u.ImageInputTokens, u.AudioOutputTokens = tokens(3), tokens(4)
			return u
		}},
		{"tool calls", func() AttemptUsage {
			u := usageOf(1, 0)
			u.ToolCalls = map[string]int64{"web_search": 2}
			u.Batch = true
			return u
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			valid(test.usage())
		})
	}
}
