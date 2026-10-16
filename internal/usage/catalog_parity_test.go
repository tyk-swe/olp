package usage

// The catalog format (internal/catalog) applies the same component rules as a
// pricing revision; this test feeds equivalent cases to both validators.

import (
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
