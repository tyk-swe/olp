package pricing

import (
	"cmp"
	"maps"
	"math/big"
	"slices"
	"strings"

	"github.com/oapi-codegen/nullable"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/usage"
)

type Price struct {
	Provider *string
	Rate     usage.Price
}

func parsePrice(entry contract.ConfigurationPriceEntry) Price {
	return Price{
		Provider: trimmed(optional(entry.Provider)),
		Rate: usage.Price{
			TrainingPerMillion:              optional(entry.TrainingPerMillion),
			StoragePerGiBDay:                optional(entry.StoragePerGibDay),
			CacheStoragePerMillionTokenHour: optional(entry.CacheStoragePerMillionTokenHour),
			ContainerPerSession:             valueOf(optional(entry.ContainerPerSession)),
			VendorID:                        trimmed(optional(entry.VendorId)),
			ProviderKind:                    entry.ProviderKind,
			Model:                           strings.TrimSpace(entry.Model),
			Operation:                       entry.Operation,
			InputPerMillion:                 optional(entry.InputPerMillion),
			CachedInputPerMillion:           optional(entry.CachedInputPerMillion),
			CacheWriteInputPerMillion:       optional(entry.CacheWriteInputPerMillion),
			CacheWrite5MInputPerMillion:     optional(entry.CacheWrite5mInputPerMillion),
			CacheWrite1HInputPerMillion:     optional(entry.CacheWrite1hInputPerMillion),
			OutputPerMillion:                optional(entry.OutputPerMillion),
			AudioInputPerMillion:            optional(entry.AudioInputPerMillion),
			CachedAudioInputPerMillion:      optional(entry.CachedAudioInputPerMillion),
			AudioOutputPerMillion:           optional(entry.AudioOutputPerMillion),
			ImageInputPerMillion:            optional(entry.ImageInputPerMillion),
			CachedImageInputPerMillion:      optional(entry.CachedImageInputPerMillion),
			ImageOutputPerMillion:           optional(entry.ImageOutputPerMillion),
			UnitPrice:                       optional(entry.UnitPrice),
			PerRequest:                      optional(entry.PerRequest),
			BatchMultiplier:                 optional(entry.BatchMultiplier),
			ToolCalls:                       valueOf(optional(entry.ToolCalls)),
			Tiers:                           slices.Collect(mapped(valueOf(optional(entry.Tiers)), parseTier)),
			Windows:                         parseWindows(optional(entry.Windows)),
			Currency:                        strings.ToUpper(strings.TrimSpace(entry.Currency)),
		},
	}
}

func renderPrice(price Price) contract.ConfigurationPriceEntry {
	rate := price.Rate
	return contract.ConfigurationPriceEntry{
		TrainingPerMillion:              promotion.Nullable(rate.TrainingPerMillion),
		StoragePerGibDay:                promotion.Nullable(rate.StoragePerGiBDay),
		CacheStoragePerMillionTokenHour: promotion.Nullable(rate.CacheStoragePerMillionTokenHour),
		ContainerPerSession:             promotion.Nullable(nonNil(rate.ContainerPerSession, rate.ContainerPerSession != nil)),
		VendorId:                        promotion.Nullable(rate.VendorID),
		ProviderKind:                    rate.ProviderKind,
		Provider:                        promotion.Nullable(price.Provider),
		Model:                           rate.Model,
		Operation:                       rate.Operation,
		InputPerMillion:                 promotion.Nullable(rate.InputPerMillion),
		CachedInputPerMillion:           promotion.Nullable(rate.CachedInputPerMillion),
		CacheWriteInputPerMillion:       promotion.Nullable(rate.CacheWriteInputPerMillion),
		CacheWrite5mInputPerMillion:     promotion.Nullable(rate.CacheWrite5MInputPerMillion),
		CacheWrite1hInputPerMillion:     promotion.Nullable(rate.CacheWrite1HInputPerMillion),
		OutputPerMillion:                promotion.Nullable(rate.OutputPerMillion),
		AudioInputPerMillion:            promotion.Nullable(rate.AudioInputPerMillion),
		CachedAudioInputPerMillion:      promotion.Nullable(rate.CachedAudioInputPerMillion),
		AudioOutputPerMillion:           promotion.Nullable(rate.AudioOutputPerMillion),
		ImageInputPerMillion:            promotion.Nullable(rate.ImageInputPerMillion),
		CachedImageInputPerMillion:      promotion.Nullable(rate.CachedImageInputPerMillion),
		ImageOutputPerMillion:           promotion.Nullable(rate.ImageOutputPerMillion),
		UnitPrice:                       promotion.Nullable(rate.UnitPrice),
		PerRequest:                      promotion.Nullable(rate.PerRequest),
		BatchMultiplier:                 promotion.Nullable(rate.BatchMultiplier),
		ToolCalls:                       promotion.Nullable(nonNil(contract.PriceToolCalls(rate.ToolCalls), rate.ToolCalls != nil)),
		Tiers:                           promotion.Nullable(nonNil(slices.Collect(mapped(rate.Tiers, renderTier)), rate.Tiers != nil)),
		Windows:                         promotion.Nullable(renderWindows(rate.Windows)),
		Currency:                        rate.Currency,
	}
}

func renderPrices(prices []Price) []contract.ConfigurationPriceEntry {
	return append([]contract.ConfigurationPriceEntry{}, slices.Collect(mapped(slices.SortedFunc(slices.Values(prices), comparePrices), renderPrice))...)
}

func comparePrices(a, b Price) int {
	return cmp.Or(
		strings.Compare(a.Rate.ProviderKind, b.Rate.ProviderKind),
		strings.Compare(folded(a.Provider), folded(b.Provider)),
		strings.Compare(folded(a.Rate.VendorID), folded(b.Rate.VendorID)),
		strings.Compare(a.Rate.Model, b.Rate.Model),
		strings.Compare(a.Rate.Operation, b.Rate.Operation),
	)
}

func samePrices(want, have []Price, currency string) bool {
	sorted := func(prices []Price) []Price {
		return slices.SortedFunc(mapped(prices, func(price Price) Price {
			return Price{Provider: price.Provider, Rate: withCurrency(price.Rate, cmp.Or(price.Rate.Currency, currency))}
		}), comparePrices)
	}
	return slices.EqualFunc(sorted(want), sorted(have), func(a, b Price) bool {
		return comparePrices(a, b) == 0 && usage.SamePrice(a.Rate, b.Rate)
	})
}

func withCurrency(rate usage.Price, currency string) usage.Price {
	priced := rate
	priced.Currency = currency
	return priced
}

func scoped(rate usage.Price, providerID *string) usage.Price {
	priced := rate
	priced.ProviderID = providerID
	return priced
}

func decimal(value string) (*big.Rat, bool) {
	return new(big.Rat).SetString(strings.TrimSpace(value))
}

func decimalEqual(a, b string) bool {
	x, okX := decimal(a)
	y, okY := decimal(b)
	return okX && okY && x.Cmp(y) == 0
}

func nonNegative(value string) bool {
	_, ok := decimal(value)
	return ok && !strings.HasPrefix(value, "-")
}

func optional[T any](value nullable.Nullable[T]) *T {
	present, ok := promotion.Present(value)
	if !ok {
		return nil
	}
	return &present
}

func nonNil[T any](value T, present bool) *T {
	if !present {
		return nil
	}
	return &value
}

func valueOf[T any](pointer *T) T {
	if pointer == nil {
		var zero T
		return zero
	}
	return *pointer
}

func trimmed(value *string) *string {
	if value == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	return &trimmed
}

func folded(value *string) string {
	return strings.ToLower(valueOf(value))
}

func mapped[T, U any](values []T, convert func(T) U) func(func(U) bool) {
	return func(yield func(U) bool) {
		for _, value := range values {
			if !yield(convert(value)) {
				return
			}
		}
	}
}

func firstIndex[T any, K comparable](values []T, key func(T) K) map[K]int {
	return maps.Collect(func(yield func(K, int) bool) {
		for i, value := range slices.Backward(values) {
			if !yield(key(value), i) {
				return
			}
		}
	})
}

func parseTier(tier contract.PriceTier) usage.PriceTier {
	return usage.PriceTier{
		AboveInputTokens:            tier.AboveInputTokens,
		InputPerMillion:             optional(tier.InputPerMillion),
		CachedInputPerMillion:       optional(tier.CachedInputPerMillion),
		CacheWriteInputPerMillion:   optional(tier.CacheWriteInputPerMillion),
		CacheWrite5MInputPerMillion: optional(tier.CacheWrite5mInputPerMillion),
		CacheWrite1HInputPerMillion: optional(tier.CacheWrite1hInputPerMillion),
		OutputPerMillion:            optional(tier.OutputPerMillion),
		AudioInputPerMillion:        optional(tier.AudioInputPerMillion),
		CachedAudioInputPerMillion:  optional(tier.CachedAudioInputPerMillion),
		AudioOutputPerMillion:       optional(tier.AudioOutputPerMillion),
		ImageInputPerMillion:        optional(tier.ImageInputPerMillion),
		CachedImageInputPerMillion:  optional(tier.CachedImageInputPerMillion),
		ImageOutputPerMillion:       optional(tier.ImageOutputPerMillion),
	}
}

func renderTier(tier usage.PriceTier) contract.PriceTier {
	return contract.PriceTier{
		AboveInputTokens:            tier.AboveInputTokens,
		InputPerMillion:             promotion.Nullable(tier.InputPerMillion),
		CachedInputPerMillion:       promotion.Nullable(tier.CachedInputPerMillion),
		CacheWriteInputPerMillion:   promotion.Nullable(tier.CacheWriteInputPerMillion),
		CacheWrite5mInputPerMillion: promotion.Nullable(tier.CacheWrite5MInputPerMillion),
		CacheWrite1hInputPerMillion: promotion.Nullable(tier.CacheWrite1HInputPerMillion),
		OutputPerMillion:            promotion.Nullable(tier.OutputPerMillion),
		AudioInputPerMillion:        promotion.Nullable(tier.AudioInputPerMillion),
		CachedAudioInputPerMillion:  promotion.Nullable(tier.CachedAudioInputPerMillion),
		AudioOutputPerMillion:       promotion.Nullable(tier.AudioOutputPerMillion),
		ImageInputPerMillion:        promotion.Nullable(tier.ImageInputPerMillion),
		CachedImageInputPerMillion:  promotion.Nullable(tier.CachedImageInputPerMillion),
		ImageOutputPerMillion:       promotion.Nullable(tier.ImageOutputPerMillion),
	}
}

func parseWindows(windows *contract.PriceWindows) *usage.PriceWindows {
	if windows == nil {
		return nil
	}
	return &usage.PriceWindows{
		TimeZone: windows.TimeZone,
		Periods: slices.Collect(mapped(windows.Periods, func(period contract.PriceWindowPeriod) usage.PriceWindowPeriod {
			return usage.PriceWindowPeriod{Days: period.Days, Start: period.Start, End: period.End, Multiplier: period.Multiplier}
		})),
	}
}

func renderWindows(windows *usage.PriceWindows) *contract.PriceWindows {
	if windows == nil {
		return nil
	}
	return &contract.PriceWindows{
		TimeZone: windows.TimeZone,
		Periods: slices.Collect(mapped(windows.Periods, func(period usage.PriceWindowPeriod) contract.PriceWindowPeriod {
			return contract.PriceWindowPeriod{Days: period.Days, Start: period.Start, End: period.End, Multiplier: period.Multiplier}
		})),
	}
}
