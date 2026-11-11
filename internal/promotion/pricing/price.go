package pricing

import (
	"cmp"
	"math/big"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/usage/pricebook"
	"github.com/tyk-swe/olp/internal/usage/rates"
)

type Price struct {
	Provider *string
	Rate     rates.Price
}

func parsePrice(entry contract.ConfigurationPriceEntry) Price {
	return Price{
		Provider: promotion.Trimmed(promotion.Pointer(entry.Provider)),
		Rate: rates.Price{
			TrainingPerMillion:              promotion.Pointer(entry.TrainingPerMillion),
			StoragePerGiBDay:                promotion.Pointer(entry.StoragePerGibDay),
			CacheStoragePerMillionTokenHour: promotion.Pointer(entry.CacheStoragePerMillionTokenHour),
			ContainerPerSession:             promotion.ValueOf(promotion.Pointer(entry.ContainerPerSession)),
			VendorID:                        promotion.Trimmed(promotion.Pointer(entry.VendorId)),
			ProviderKind:                    entry.ProviderKind,
			Model:                           strings.TrimSpace(entry.Model),
			Operation:                       entry.Operation,
			InputPerMillion:                 promotion.Pointer(entry.InputPerMillion),
			CachedInputPerMillion:           promotion.Pointer(entry.CachedInputPerMillion),
			CacheWriteInputPerMillion:       promotion.Pointer(entry.CacheWriteInputPerMillion),
			CacheWrite5MInputPerMillion:     promotion.Pointer(entry.CacheWrite5mInputPerMillion),
			CacheWrite1HInputPerMillion:     promotion.Pointer(entry.CacheWrite1hInputPerMillion),
			OutputPerMillion:                promotion.Pointer(entry.OutputPerMillion),
			AudioInputPerMillion:            promotion.Pointer(entry.AudioInputPerMillion),
			CachedAudioInputPerMillion:      promotion.Pointer(entry.CachedAudioInputPerMillion),
			AudioOutputPerMillion:           promotion.Pointer(entry.AudioOutputPerMillion),
			ImageInputPerMillion:            promotion.Pointer(entry.ImageInputPerMillion),
			CachedImageInputPerMillion:      promotion.Pointer(entry.CachedImageInputPerMillion),
			ImageOutputPerMillion:           promotion.Pointer(entry.ImageOutputPerMillion),
			UnitPrice:                       promotion.Pointer(entry.UnitPrice),
			PerRequest:                      promotion.Pointer(entry.PerRequest),
			BatchMultiplier:                 promotion.Pointer(entry.BatchMultiplier),
			ToolCalls:                       promotion.ValueOf(promotion.Pointer(entry.ToolCalls)),
			Tiers:                           parseTiers(promotion.ValueOf(promotion.Pointer(entry.Tiers))),
			Windows:                         parseWindows(promotion.Pointer(entry.Windows)),
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
		ContainerPerSession:             promotion.Nullable(promotion.PointerIf(rate.ContainerPerSession, rate.ContainerPerSession != nil)),
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
		ToolCalls:                       promotion.Nullable(promotion.PointerIf(contract.PriceToolCalls(rate.ToolCalls), rate.ToolCalls != nil)),
		Tiers:                           promotion.Nullable(promotion.PointerIf(promotion.Map(rate.Tiers, renderTier), rate.Tiers != nil)),
		Windows:                         promotion.Nullable(renderWindows(rate.Windows)),
		Currency:                        rate.Currency,
	}
}

func renderPrices(prices []Price) []contract.ConfigurationPriceEntry {
	return promotion.Map(slices.SortedFunc(slices.Values(prices), comparePrices), renderPrice)
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
		return slices.SortedFunc(slices.Values(promotion.Map(prices, func(price Price) Price {
			return Price{Provider: price.Provider, Rate: withCurrency(price.Rate, cmp.Or(price.Rate.Currency, currency))}
		})), comparePrices)
	}
	return slices.EqualFunc(sorted(want), sorted(have), func(a, b Price) bool {
		return comparePrices(a, b) == 0 && pricebook.SamePrice(a.Rate, b.Rate)
	})
}

func withCurrency(rate rates.Price, currency string) rates.Price {
	priced := rate
	priced.Currency = currency
	return priced
}

func scoped(rate rates.Price, providerID *string) rates.Price {
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

func folded(value *string) string {
	return strings.ToLower(promotion.ValueOf(value))
}

func parseTier(tier contract.PriceTier) rates.PriceTier {
	return rates.PriceTier{
		AboveInputTokens:            tier.AboveInputTokens,
		InputPerMillion:             promotion.Pointer(tier.InputPerMillion),
		CachedInputPerMillion:       promotion.Pointer(tier.CachedInputPerMillion),
		CacheWriteInputPerMillion:   promotion.Pointer(tier.CacheWriteInputPerMillion),
		CacheWrite5MInputPerMillion: promotion.Pointer(tier.CacheWrite5mInputPerMillion),
		CacheWrite1HInputPerMillion: promotion.Pointer(tier.CacheWrite1hInputPerMillion),
		OutputPerMillion:            promotion.Pointer(tier.OutputPerMillion),
		AudioInputPerMillion:        promotion.Pointer(tier.AudioInputPerMillion),
		CachedAudioInputPerMillion:  promotion.Pointer(tier.CachedAudioInputPerMillion),
		AudioOutputPerMillion:       promotion.Pointer(tier.AudioOutputPerMillion),
		ImageInputPerMillion:        promotion.Pointer(tier.ImageInputPerMillion),
		CachedImageInputPerMillion:  promotion.Pointer(tier.CachedImageInputPerMillion),
		ImageOutputPerMillion:       promotion.Pointer(tier.ImageOutputPerMillion),
	}
}

func renderTier(tier rates.PriceTier) contract.PriceTier {
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

func parseTiers(tiers []contract.PriceTier) []rates.PriceTier {
	if len(tiers) == 0 {
		return nil
	}
	return promotion.Map(tiers, parseTier)
}

func parseWindows(windows *contract.PriceWindows) *rates.PriceWindows {
	if windows == nil {
		return nil
	}
	return &rates.PriceWindows{
		TimeZone: windows.TimeZone,
		Periods: promotion.Map(windows.Periods, func(period contract.PriceWindowPeriod) rates.PriceWindowPeriod {
			return rates.PriceWindowPeriod{Days: period.Days, Start: period.Start, End: period.End, Multiplier: period.Multiplier}
		}),
	}
}

func renderWindows(windows *rates.PriceWindows) *contract.PriceWindows {
	if windows == nil {
		return nil
	}
	return &contract.PriceWindows{
		TimeZone: windows.TimeZone,
		Periods: promotion.Map(windows.Periods, func(period rates.PriceWindowPeriod) contract.PriceWindowPeriod {
			return contract.PriceWindowPeriod{Days: period.Days, Start: period.Start, End: period.End, Multiplier: period.Multiplier}
		}),
	}
}
