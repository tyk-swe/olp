package configuration

import (
	"github.com/tyk-swe/olp/internal/usage"
	"testing"
)

func TestRetainedRateCardChangesRequireNewRevision(t *testing.T) {
	for name, set := range map[string]func(*usage.Price, string){
		"training":      func(p *usage.Price, v string) { p.TrainingPerMillion = new(v) },
		"storage":       func(p *usage.Price, v string) { p.StoragePerGiBDay = new(v) },
		"cache_storage": func(p *usage.Price, v string) { p.CacheStoragePerMillionTokenHour = new(v) },
		"container":     func(p *usage.Price, v string) { p.ContainerPerSession = map[string]string{"1g": v} },
	} {
		t.Run(name, func(t *testing.T) {
			original := usage.Price{ProviderKind: "openai", Model: "m", Operation: "fine_tuning", Currency: "USD"}
			set(&original, "1")
			current := RateCardEntry{Markup: "1", Prices: []PriceEntry{priceEntryFromUsage(original)}}
			changed := original
			set(&changed, "2")
			desired := RateCardEntry{Markup: "1", Prices: []PriceEntry{priceEntryFromUsage(changed)}}
			if cardEntryEqual(desired, current, "USD") {
				t.Fatal("promotion silently retained an old retained rate")
			}
			set(&changed, "1.00")
			desired.Prices = []PriceEntry{priceEntryFromUsage(changed)}
			if !cardEntryEqual(desired, current, "USD") {
				t.Fatal("equivalent retained rates caused a revision")
			}
		})
	}
}
