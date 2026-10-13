package usage

// The catalog format (internal/catalog) applies the same component rules as a
// pricing revision; this test feeds equivalent cases to both validators.

import (
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/catalog"
)

// catalogPrice wraps a catalog price in the smallest valid catalog.
func catalogPrice(p catalog.Price) *catalog.Catalog {
	three := catalog.Decimal("1")
	provenance := catalog.Provenance{SourceURL: "https://example.test/pricing",
		ObservedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
	if p.Unrepresentable == nil {
		p.Unrepresentable = []catalog.Unrepresentable{}
	}
	if p.Provenance.SourceURL == "" {
		p.Provenance = provenance
	}
	if p.Operation == "" {
		p.Operation = "generation"
	}
	// An audio-only entry needs no text rate, but the model fixture does.
	for _, rate := range []*catalog.Decimal{p.InputPerMillion, p.CachedInputPerMillion, p.CacheWriteInputPerMillion, p.CacheWrite5MInputPerMillion, p.CacheWrite1HInputPerMillion, p.OutputPerMillion, p.AudioInputPerMillion, p.CachedAudioInputPerMillion, p.AudioOutputPerMillion, p.ImageInputPerMillion, p.CachedImageInputPerMillion, p.ImageOutputPerMillion, p.UnitPrice, p.PerRequest, p.BatchMultiplier} {
		_ = rate
	}
	if p.InputPerMillion == nil && p.CachedInputPerMillion == nil && p.CacheWriteInputPerMillion == nil &&
		p.CacheWrite5MInputPerMillion == nil && p.CacheWrite1HInputPerMillion == nil && p.OutputPerMillion == nil &&
		p.AudioInputPerMillion == nil && p.CachedAudioInputPerMillion == nil && p.AudioOutputPerMillion == nil &&
		p.ImageInputPerMillion == nil && p.CachedImageInputPerMillion == nil && p.ImageOutputPerMillion == nil &&
		p.UnitPrice == nil && p.PerRequest == nil && p.BatchMultiplier == nil && len(p.ToolCalls) == 0 {
		p.InputPerMillion = &three
	}
	return &catalog.Catalog{APIVersion: catalog.APIVersion,
		PublishedAt: time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC), Currency: "USD",
		Vendors: []catalog.Vendor{{ID: "anthropic", Name: "Anthropic", Models: []catalog.Model{{
			ID: "m", InputModalities: []string{"text"}, OutputModalities: []string{"text"},
			Prices: []catalog.Price{p}, Provenance: provenance}}}}}
}

func usageFromCatalog(p catalog.Price) Price {
	entry := Price{ProviderKind: "anthropic", Model: "m", Operation: p.Operation, Currency: "USD",
		InputPerMillion: catalogRate(p.InputPerMillion), CachedInputPerMillion: catalogRate(p.CachedInputPerMillion),
		CacheWriteInputPerMillion: catalogRate(p.CacheWriteInputPerMillion), CacheWrite5MInputPerMillion: catalogRate(p.CacheWrite5MInputPerMillion),
		CacheWrite1HInputPerMillion: catalogRate(p.CacheWrite1HInputPerMillion), OutputPerMillion: catalogRate(p.OutputPerMillion),
		AudioInputPerMillion: catalogRate(p.AudioInputPerMillion), CachedAudioInputPerMillion: catalogRate(p.CachedAudioInputPerMillion),
		AudioOutputPerMillion: catalogRate(p.AudioOutputPerMillion), ImageInputPerMillion: catalogRate(p.ImageInputPerMillion),
		CachedImageInputPerMillion: catalogRate(p.CachedImageInputPerMillion), ImageOutputPerMillion: catalogRate(p.ImageOutputPerMillion),
		UnitPrice: catalogRate(p.UnitPrice), PerRequest: catalogRate(p.PerRequest), BatchMultiplier: catalogRate(p.BatchMultiplier),
		ToolCalls: catalogToolCalls(p.ToolCalls), Tiers: catalogTiers(p.Tiers), Windows: catalogWindows(p.Windows)}
	return entry
}

func TestCatalogAndRevisionRulesAgree(t *testing.T) {
	decimal := func(s string) *catalog.Decimal { v := catalog.Decimal(s); return &v }
	tier := func(above int64, input, output bool) catalog.Tier {
		t := catalog.Tier{AboveInputTokens: above}
		if input {
			t.InputPerMillion = decimal("1")
		}
		if output {
			t.OutputPerMillion = decimal("2")
		}
		return t
	}
	window := func(days []int, start, end string) catalog.WindowPeriod {
		return catalog.WindowPeriod{Days: days, Start: start, End: end, Multiplier: catalog.Decimal("2")}
	}
	cases := []struct {
		name  string
		price catalog.Price
		valid bool
	}{
		{"audio only", catalog.Price{AudioInputPerMillion: decimal("1")}, true},
		{"tool fees only", catalog.Price{ToolCalls: map[string]catalog.Decimal{"web_search": "0"}}, true},
		{"cached without input", catalog.Price{CachedInputPerMillion: decimal("0.5"), OutputPerMillion: decimal("1")}, false},
		{"tier key set differs", catalog.Price{InputPerMillion: decimal("1"), OutputPerMillion: decimal("2"),
			Tiers: []catalog.Tier{tier(100, true, false)}}, false},
		{"tier key set matches", catalog.Price{InputPerMillion: decimal("1"), OutputPerMillion: decimal("2"),
			Tiers: []catalog.Tier{tier(100, true, true)}}, true},
		{"tier thresholds not increasing", catalog.Price{InputPerMillion: decimal("1"),
			Tiers: []catalog.Tier{tier(100, true, false), tier(100, true, false)}}, false},
		{"window overlap on shared day", catalog.Price{InputPerMillion: decimal("1"),
			Windows: &catalog.Windows{TimeZone: "UTC", Periods: []catalog.WindowPeriod{
				window([]int{1, 2}, "09:00", "17:00"), window([]int{2, 3}, "16:00", "24:00")}}}, false},
		{"window no shared day", catalog.Price{InputPerMillion: decimal("1"),
			Windows: &catalog.Windows{TimeZone: "UTC", Periods: []catalog.WindowPeriod{
				window([]int{1}, "09:00", "17:00"), window([]int{2}, "16:00", "24:00")}}}, true},
		{"window same day contiguous", catalog.Price{InputPerMillion: decimal("1"),
			Windows: &catalog.Windows{TimeZone: "UTC", Periods: []catalog.WindowPeriod{
				window([]int{1}, "09:00", "17:00"), window([]int{1}, "17:00", "24:00")}}}, true},
		{"bad tool name", catalog.Price{ToolCalls: map[string]catalog.Decimal{"Web Search": "1"}}, false},
		{"bad zone", catalog.Price{InputPerMillion: decimal("1"),
			Windows: &catalog.Windows{TimeZone: "Mars/Olympus", Periods: []catalog.WindowPeriod{
				window([]int{1}, "00:00", "24:00")}}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			price := tc.price
			price.Operation = "generation"
			catalogErr := catalogPrice(price).Validate()
			_, usageErr := normalizePrice(usageFromCatalog(price), nil)
			if (catalogErr == nil) != tc.valid || (usageErr == nil) != tc.valid {
				t.Fatalf("catalog %v, revision %v, want valid=%v", catalogErr, usageErr, tc.valid)
			}
		})
	}
}
