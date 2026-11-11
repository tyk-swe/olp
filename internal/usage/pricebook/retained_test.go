package pricebook

import (
	"testing"

	"github.com/tyk-swe/olp/internal/usage/rates"
)

func retainedRateChanges() map[string]func(*rates.Price, string) {
	return map[string]func(*rates.Price, string){
		"training":      func(p *rates.Price, rate string) { p.TrainingPerMillion = new(rate) },
		"storage":       func(p *rates.Price, rate string) { p.StoragePerGiBDay = new(rate) },
		"cache_storage": func(p *rates.Price, rate string) { p.CacheStoragePerMillionTokenHour = new(rate) },
		"container":     func(p *rates.Price, rate string) { p.ContainerPerSession = map[string]string{"4g": rate} },
	}
}

func TestRetainedRatesParticipateInPriceEquality(t *testing.T) {
	for name, set := range retainedRateChanges() {
		t.Run(name, func(t *testing.T) {
			base := rates.Price{Currency: "USD"}
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
			zero := rates.Price{Currency: "USD"}
			set(&zero, "0")
			if SamePrice(zero, rates.Price{Currency: "USD"}) {
				t.Fatal("explicit zero rate compared equal to missing pricing")
			}
		})
	}
	base := rates.Price{Currency: "USD", ContainerPerSession: map[string]string{"4g": "1"}}
	changed := rates.Price{Currency: "USD", ContainerPerSession: map[string]string{"16g": "1"}}
	if SamePrice(base, changed) {
		t.Fatal("container memory-tier change was ignored")
	}
}

func TestRetainedOnlySourceOverridesAreNotRemovals(t *testing.T) {
	for name, set := range retainedRateChanges() {
		t.Run(name, func(t *testing.T) {
			base := rates.Price{ProviderKind: "openai", Model: "m", Operation: "fine_tuning", Currency: "USD"}
			set(&base, "1")
			for _, rate := range []string{"2", "0"} {
				override := base
				set(&override, rate)
				merged, err := mergeSourceOverrides([]rates.Price{base}, []rates.Price{override}, Normalizer{})
				if err != nil || len(merged) != 1 || !SamePrice(merged[0], override) {
					t.Fatalf("retained-only override removed its price: %+v %v", merged, err)
				}
			}
			removed, err := mergeSourceOverrides([]rates.Price{base}, []rates.Price{{ProviderKind: "openai", Model: "m", Operation: "fine_tuning"}}, Normalizer{})
			if err != nil || len(removed) != 0 {
				t.Fatalf("dimension-only deletion stopped working: %+v %v", removed, err)
			}
		})
	}
}
