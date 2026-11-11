package pricebook

import (
	"testing"

	"github.com/tyk-swe/olp/internal/pricingpolicy"
	"github.com/tyk-swe/olp/internal/usage"
	"github.com/tyk-swe/olp/internal/usage/rates"
)

// priceOf normalizes one entry as CreateRevision would, failing the test on refusal.
func priceOf(t *testing.T, p rates.Price) rates.Price {
	t.Helper()
	entry, err := normalizePrice(p, Normalizer{})
	if err != nil {
		t.Fatalf("normalizePrice: %v", err)
	}
	return entry
}

func priceRefused(t *testing.T, p rates.Price) {
	t.Helper()
	if _, err := normalizePrice(p, Normalizer{}); err == nil {
		t.Fatalf("price %+v was accepted", p)
	}
}

func rate(text string) *string { return &text }

func basePrice() rates.Price {
	return rates.Price{ProviderKind: "openai", Model: "m", Operation: "generation",
		InputPerMillion: rate("1"), OutputPerMillion: rate("2"), Currency: "USD"}
}

func TestPriceRateAgreesWithDecimalNormalization(t *testing.T) {
	for _, value := range []string{"0", "1", "0.000000000001", "12.50", " 1 ", "1000000000000", "1.0000000000001", "-1", "1.2.3", "", "1e3", ".1"} {
		t.Run(value, func(t *testing.T) {
			normalized, amountErr := normalizePriceAmount(value)
			price := basePrice()
			price.InputPerMillion = &value
			actual, priceErr := normalizePrice(price, Normalizer{})
			if (priceErr == nil) != (amountErr == nil) {
				t.Fatalf("price error = %v, amount error = %v", priceErr, amountErr)
			}
			if priceErr == nil && *actual.InputPerMillion != normalized {
				t.Fatalf("price rate = %q, normalized amount = %q", *actual.InputPerMillion, normalized)
			}
		})
	}
}

func TestPriceValidationComponents(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		price func() rates.Price
	}{
		{"tool fees alone satisfy the rate requirement", func() rates.Price {
			p := basePrice()
			p.InputPerMillion, p.OutputPerMillion = nil, nil
			p.ToolCalls = map[string]string{"web_search": "0.01"}
			return p
		}},
		{"per-request alone satisfies the rate requirement", func() rates.Price {
			p := basePrice()
			p.InputPerMillion, p.OutputPerMillion = nil, nil
			p.PerRequest = rate("0.05")
			return p
		}},
		{"audio rates need no text rates", func() rates.Price {
			p := basePrice()
			p.InputPerMillion, p.OutputPerMillion = nil, nil
			p.AudioInputPerMillion, p.AudioOutputPerMillion = rate("4"), rate("8")
			return p
		}},
		{"tiers carry the entry's per-million set", func() rates.Price {
			p := basePrice()
			p.Tiers = []rates.PriceTier{{AboveInputTokens: 1000, InputPerMillion: rate("2"), OutputPerMillion: rate("4")}}
			return p
		}},
		{"windows normalize and sort the week", func() rates.Price {
			p := basePrice()
			p.Windows = &rates.PriceWindows{TimeZone: "America/New_York", Periods: []rates.PriceWindowPeriod{
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
		price func() rates.Price
	}{
		{"empty tool fee set", func() rates.Price {
			p := basePrice()
			p.ToolCalls = map[string]string{}
			return p
		}},
		{"tool fee names a malformed tool", func() rates.Price {
			p := basePrice()
			p.ToolCalls = map[string]string{"Web-Search": "0.01"}
			return p
		}},
		{"too many tool fees", func() rates.Price {
			p := basePrice()
			p.ToolCalls = map[string]string{}
			for index := range usage.MaxToolCalls + 1 {
				p.ToolCalls[string(rune('a'+index))+"_tool"] = "0.01"
			}
			return p
		}},
		{"a tier missing an entry rate", func() rates.Price {
			p := basePrice()
			p.Tiers = []rates.PriceTier{{AboveInputTokens: 1000, InputPerMillion: rate("2")}}
			return p
		}},
		{"a tier adding a rate the entry lacks", func() rates.Price {
			p := basePrice()
			p.Tiers = []rates.PriceTier{{AboveInputTokens: 1000, InputPerMillion: rate("2"),
				OutputPerMillion: rate("4"), AudioInputPerMillion: rate("9")}}
			return p
		}},
		{"unordered tiers", func() rates.Price {
			p := basePrice()
			p.Tiers = []rates.PriceTier{
				{AboveInputTokens: 2000, InputPerMillion: rate("2"), OutputPerMillion: rate("4")},
				{AboveInputTokens: 1000, InputPerMillion: rate("3"), OutputPerMillion: rate("5")}}
			return p
		}},
		{"a non-positive threshold", func() rates.Price {
			p := basePrice()
			p.Tiers = []rates.PriceTier{{AboveInputTokens: 0, InputPerMillion: rate("2"), OutputPerMillion: rate("4")}}
			return p
		}},
		{"too many tiers", func() rates.Price {
			p := basePrice()
			for index := range pricingpolicy.MaxTiers + 1 {
				p.Tiers = append(p.Tiers, rates.PriceTier{AboveInputTokens: int64(index + 1),
					InputPerMillion: rate("2"), OutputPerMillion: rate("4")})
			}
			return p
		}},
		{"an unknown window zone", func() rates.Price {
			p := basePrice()
			p.Windows = &rates.PriceWindows{TimeZone: "Not/AZone", Periods: []rates.PriceWindowPeriod{
				{Days: []int{1}, Start: "01:00", End: "02:00", Multiplier: "1"}}}
			return p
		}},
		{"overlapping periods on a shared day", func() rates.Price {
			p := basePrice()
			p.Windows = &rates.PriceWindows{TimeZone: "UTC", Periods: []rates.PriceWindowPeriod{
				{Days: []int{1, 2}, Start: "10:00", End: "12:00", Multiplier: "1"},
				{Days: []int{2, 3}, Start: "11:00", End: "13:00", Multiplier: "1"}}}
			return p
		}},
		{"adjacent periods do not overlap", func() rates.Price {
			p := basePrice()
			p.Windows = &rates.PriceWindows{TimeZone: "UTC", Periods: []rates.PriceWindowPeriod{
				{Days: []int{2}, Start: "13:00", End: "10:00", Multiplier: "1"}}}
			return p
		}},
		{"an end that is not after its start", func() rates.Price {
			p := basePrice()
			p.Windows = &rates.PriceWindows{TimeZone: "UTC", Periods: []rates.PriceWindowPeriod{
				{Days: []int{2}, Start: "13:00", End: "10:00", Multiplier: "1"}}}
			return p
		}},
		{"a repeated day", func() rates.Price {
			p := basePrice()
			p.Windows = &rates.PriceWindows{TimeZone: "UTC", Periods: []rates.PriceWindowPeriod{
				{Days: []int{1, 1}, Start: "10:00", End: "12:00", Multiplier: "1"}}}
			return p
		}},
		{"a day out of range", func() rates.Price {
			p := basePrice()
			p.Windows = &rates.PriceWindows{TimeZone: "UTC", Periods: []rates.PriceWindowPeriod{
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
