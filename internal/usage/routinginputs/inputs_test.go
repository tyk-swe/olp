package routinginputs

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/usage/rates"
)

func TestRoutingPriceSelectsApplicableRevision(t *testing.T) {
	now := time.Unix(1700000000, 0)
	price := func(id, kind, model, operation string, provider, vendor *string, at time.Time, revision int) rates.RoutingPrice {
		return *rates.NewRoutingPrice(rates.RoutingPrice{Price: rates.Price{ProviderKind: kind, Model: model, Operation: operation, ProviderID: provider, VendorID: vendor, InputPerMillion: new("1.25"), OutputPerMillion: new("2.5"), Currency: "USD"}, RevisionID: id, Revision: revision, EffectiveAt: at})
	}
	prices := []rates.RoutingPrice{
		price("kind", "openai", "model", "generation", nil, nil, now.Add(-time.Hour), 1),
		price("vendor", "openai", "model", "generation", nil, new("vendor"), now.Add(-time.Hour), 1),
		price("provider", "openai", "model", "generation", new("provider"), nil, now.Add(-2*time.Hour), 1),
		price("provider-newer", "openai", "model", "generation", new("provider"), nil, now.Add(-time.Hour), 1),
		price("provider-revised", "openai", "model", "generation", new("provider"), nil, now.Add(-time.Hour), 2),
		price("provider-tied", "openai", "model", "generation", new("provider"), nil, now.Add(-time.Hour), 2),
		price("scheduled", "openai", "model", "generation", new("provider"), nil, now.Add(time.Second), 3),
		price("both", "openai", "model", "generation", new("provider"), new("specific"), now.Add(-3*time.Hour), 1),
		price("other-kind", "anthropic", "model", "generation", nil, nil, now, 1),
		price("other-model", "openai", "other", "generation", nil, nil, now, 1),
		price("other-operation", "openai", "model", "embeddings", nil, nil, now, 1),
	}
	for _, indexed := range []bool{false, true} {
		inputs := &Inputs{Prices: prices, RefreshedAt: now}
		if indexed {
			inputs.priceIndex = indexRoutingPrices(prices)
		}
		for _, tc := range []struct {
			name, kind, model, operation, provider, vendor, want string
			at                                                   time.Time
			priority                                             int
		}{
			{"provider precedence and revision tie", "openai", "model", "generation", "provider", "vendor", "provider-revised", now, 2},
			{"vendor fallback", "openai", "model", "generation", "other", "vendor", "vendor", now, 1},
			{"kind fallback", "openai", "model", "generation", "other", "other", "kind", now, 0},
			{"both scope constraints", "openai", "model", "generation", "provider", "specific", "both", now, 3},
			{"scheduled boundary", "openai", "model", "generation", "provider", "vendor", "scheduled", now.Add(time.Second), 2},
			{"freshness boundary", "openai", "model", "generation", "other", "vendor", "vendor", now.Add(PerformanceMaxAge), 1},
			{"stale", "openai", "model", "generation", "provider", "vendor", "", now.Add(PerformanceMaxAge + time.Nanosecond), 0},
			{"missing bucket", "openai", "missing", "generation", "provider", "vendor", "", now, 0},
			{"kind isolation", "anthropic", "model", "generation", "provider", "vendor", "other-kind", now, 0},
			{"model isolation", "openai", "other", "generation", "provider", "vendor", "other-model", now, 0},
			{"operation isolation", "openai", "model", "embeddings", "provider", "vendor", "other-operation", now, 0},
		} {
			t.Run(tc.name+"/indexed="+fmt.Sprint(indexed), func(t *testing.T) {
				got := inputs.Price(tc.kind, tc.provider, tc.vendor, tc.model, tc.operation, tc.at)
				if tc.want == "" {
					if got != nil {
						t.Fatalf("unexpected price %+v", got)
					}
					return
				}
				if got == nil || got.RevisionID != tc.want || got.ScopePriority != tc.priority || got.ProviderID == nil || *got.ProviderID != tc.provider {
					t.Fatalf("price %+v, want revision %s for provider %s with priority %d", got, tc.want, tc.provider, tc.priority)
				}
				if got.InputPerMillion == nil || *got.InputPerMillion != *prices[0].InputPerMillion || got.OutputPerMillion == nil || *got.OutputPerMillion != *prices[0].OutputPerMillion || got.Currency != prices[0].Currency {
					t.Fatalf("price lost its rates or currency: %+v", got)
				}
				original := *got
				got.ProviderID = new("changed")
				got.Revision = -1
				if again := inputs.Price(tc.kind, tc.provider, tc.vendor, tc.model, tc.operation, tc.at); !reflect.DeepEqual(again, &original) {
					t.Fatal("modifying the returned price changed later requests")
				}
			})
		}
	}
}
