package pricebook

import (
	"encoding/json"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/pricingpolicy"
	"github.com/tyk-swe/olp/internal/toolidentity"
	"github.com/tyk-swe/olp/internal/usage"
	"github.com/tyk-swe/olp/internal/usage/rates"
)

// Normalizer validates price book input against the vendors this binary ships.
type Normalizer struct {
	vendorKind func(vendor string) (string, bool)
}

func NewNormalizer(vendorKind func(vendor string) (string, bool)) Normalizer {
	return Normalizer{vendorKind: vendorKind}
}

func validProviderKind(value string) bool { return contract.ProviderKind(value).Valid() }

// validatePrices checks one revision as a whole and returns the normalized
// rates plus the revision currency.
func validatePrices(prices []rates.Price, normalizer Normalizer) ([]rates.Price, string, error) {
	if len(prices) < 1 || len(prices) > MaxRevisionPrices {
		return nil, "", invalid("prices",
			"A pricing revision must contain 1–"+strconv.Itoa(MaxRevisionPrices)+" entries.")
	}
	normalized := make([]rates.Price, 0, len(prices))
	dimensions := make(map[string]bool, len(prices))
	currency := ""
	for _, price := range prices {
		entry, err := normalizePrice(price, normalizer)
		if err != nil {
			return nil, "", err
		}
		if currency == "" {
			currency = entry.Currency
		} else if currency != entry.Currency {
			return nil, "", invalid("prices", "A pricing revision cannot mix currencies.")
		}
		key := dimensionKey(entry)
		if dimensions[key] {
			return nil, "", invalid("prices",
				"A pricing revision cannot repeat the same scoped dimensions.")
		}
		dimensions[key] = true
		normalized = append(normalized, entry)
	}
	return normalized, currency, nil
}

// optional renders an absent scope distinctly from any value it could hold, so
// the duplicate check cannot confuse "any provider" with a provider named "".
func optional(value *string) string {
	if value == nil {
		return "\x01"
	}
	return *value
}

func normalizePrice(price rates.Price, normalizer Normalizer) (rates.Price, error) {
	entry := price
	entry.Model = strings.TrimSpace(price.Model)
	entry.Currency = strings.ToUpper(strings.TrimSpace(price.Currency))
	// Validation writes normalized values back into the entry, so clone the
	// structured components rather than writing through to the caller's.
	entry.ContainerPerSession = maps.Clone(price.ContainerPerSession)
	if err := normalizeContainerRates(entry.ContainerPerSession); err != nil {
		return rates.Price{}, err
	}
	if price.ToolCalls != nil {
		entry.ToolCalls = maps.Clone(price.ToolCalls)
	}
	if price.Tiers != nil {
		entry.Tiers = slices.Clone(price.Tiers)
		for index := range entry.Tiers {
			for _, pair := range perMillionRates(&rates.Price{}, &entry.Tiers[index]) {
				if *pair[1] != nil {
					value := **pair[1]
					*pair[1] = &value
				}
			}
		}
	}
	if price.Windows != nil {
		windows := *price.Windows
		windows.Periods = slices.Clone(price.Windows.Periods)
		entry.Windows = &windows
	}
	if !validProviderKind(entry.ProviderKind) {
		return rates.Price{}, invalid("prices", "Use a supported provider kind.")
	}
	if !usage.ValidOperation(entry.Operation) {
		return rates.Price{}, invalid("prices", "Use a supported pricing operation.")
	}
	if entry.VendorID != nil {
		vendor := strings.TrimSpace(*entry.VendorID)
		kind, known := normalizer.vendorKind(vendor)
		if !known || kind != entry.ProviderKind {
			return rates.Price{}, invalid("prices", "The pricing vendor must match the connector.")
		}
		entry.VendorID = &vendor
	}
	if entry.ProviderID != nil {
		id, err := access.ParseUUID(*entry.ProviderID)
		if err != nil {
			return rates.Price{}, invalid("prices", "Use a valid provider identifier.")
		}
		entry.ProviderID = &id
	} else if entry.ProviderKind == "plugin" {
		// No list price applies to plugin providers, which may front any
		// upstream on any terms; the operator prices each one.
		return rates.Price{}, invalid("prices", "Scope a plugin provider's price to that provider.")
	}
	if entry.Model == "" || !pricingpolicy.ValidCurrency(entry.Currency) {
		return rates.Price{}, invalid("prices",
			"Pricing entries require a model and a three-letter ISO currency.")
	}
	if entry.InputPerMillion == nil && entry.OutputPerMillion == nil && entry.UnitPrice == nil &&
		entry.PerRequest == nil && len(entry.ToolCalls) == 0 && entry.TrainingPerMillion == nil && entry.StoragePerGiBDay == nil && entry.CacheStoragePerMillionTokenHour == nil && len(entry.ContainerPerSession) == 0 &&
		entry.AudioInputPerMillion == nil && entry.AudioOutputPerMillion == nil &&
		entry.ImageInputPerMillion == nil && entry.ImageOutputPerMillion == nil {
		return rates.Price{}, invalid("prices", "Pricing entries require at least one rate.")
	}
	// A cached rate prices a share of the input count; without an input rate
	// there is nothing for it to discount.
	if entry.CachedInputPerMillion != nil && entry.InputPerMillion == nil {
		return rates.Price{}, invalid("prices",
			"A cached input rate requires an input rate to discount.")
	}
	if entry.InputPerMillion == nil && (entry.CacheWriteInputPerMillion != nil ||
		entry.CacheWrite5MInputPerMillion != nil || entry.CacheWrite1HInputPerMillion != nil) {
		return rates.Price{}, invalid("prices",
			"A cache write rate requires an input rate.")
	}
	for _, amount := range []*string{entry.InputPerMillion, entry.CachedInputPerMillion,
		entry.OutputPerMillion, entry.UnitPrice, entry.CacheWriteInputPerMillion,
		entry.CacheWrite5MInputPerMillion, entry.CacheWrite1HInputPerMillion,
		entry.AudioInputPerMillion, entry.CachedAudioInputPerMillion, entry.AudioOutputPerMillion,
		entry.ImageInputPerMillion, entry.CachedImageInputPerMillion, entry.ImageOutputPerMillion,
		entry.PerRequest, entry.BatchMultiplier, entry.TrainingPerMillion, entry.StoragePerGiBDay, entry.CacheStoragePerMillionTokenHour} {
		if amount == nil {
			continue
		}
		value, err := normalizePriceAmount(*amount)
		if err != nil {
			return rates.Price{}, err
		}
		*amount = value
	}
	if err := normalizeToolCallFees(entry.ToolCalls); err != nil {
		return rates.Price{}, err
	}
	if err := normalizeTiers(entry.Tiers, &entry); err != nil {
		return rates.Price{}, err
	}
	if err := normalizeWindows(entry.Windows); err != nil {
		return rates.Price{}, err
	}
	return entry, nil
}

// normalizeToolCallFees checks the tool fee map bounds and normalizes each fee.
func normalizeToolCallFees(calls map[string]string) error {
	if calls == nil {
		return nil
	}
	if len(calls) < 1 || len(calls) > usage.MaxToolCalls {
		return invalid("prices",
			"Tool call fees name 1–"+strconv.Itoa(usage.MaxToolCalls)+" tools.")
	}
	for tool, fee := range calls {
		if !toolidentity.ValidMeteredName(tool) {
			return invalid("prices", "A tool call fee name is malformed.")
		}
		value, err := normalizePriceAmount(fee)
		if err != nil {
			return err
		}
		calls[tool] = value
	}
	return nil
}

// perMillionRates pairs each per-million rate field of a price and a tier so
// the tier's key set can be checked against the entry's.
func perMillionRates(p *rates.Price, t *rates.PriceTier) [][2]**string {
	if t == nil {
		return [][2]**string{}
	}
	return [][2]**string{
		{&p.InputPerMillion, &t.InputPerMillion},
		{&p.CachedInputPerMillion, &t.CachedInputPerMillion},
		{&p.CacheWriteInputPerMillion, &t.CacheWriteInputPerMillion},
		{&p.CacheWrite5MInputPerMillion, &t.CacheWrite5MInputPerMillion},
		{&p.CacheWrite1HInputPerMillion, &t.CacheWrite1HInputPerMillion},
		{&p.OutputPerMillion, &t.OutputPerMillion},
		{&p.AudioInputPerMillion, &t.AudioInputPerMillion},
		{&p.CachedAudioInputPerMillion, &t.CachedAudioInputPerMillion},
		{&p.AudioOutputPerMillion, &t.AudioOutputPerMillion},
		{&p.ImageInputPerMillion, &t.ImageInputPerMillion},
		{&p.CachedImageInputPerMillion, &t.CachedImageInputPerMillion},
		{&p.ImageOutputPerMillion, &t.ImageOutputPerMillion},
	}
}

// normalizeTiers checks the thresholds are positive and strictly increasing,
// normalizes every rate, and holds each tier to the per-million rate set its
// entry carries: a tier replaces that set, so it must name exactly it.
func normalizeTiers(tiers []rates.PriceTier, entry *rates.Price) error {
	if tiers == nil {
		return nil
	}
	if len(tiers) < 1 || len(tiers) > pricingpolicy.MaxTiers {
		return invalid("prices",
			"Pricing tiers name 1–"+strconv.Itoa(pricingpolicy.MaxTiers)+" thresholds.")
	}
	previous := int64(0)
	for i := range tiers {
		tier := &tiers[i]
		if tier.AboveInputTokens <= previous {
			return invalid("prices",
				"Pricing tiers require strictly increasing positive input thresholds.")
		}
		previous = tier.AboveInputTokens
		for _, pair := range perMillionRates(entry, tier) {
			if (*pair[0] == nil) != (*pair[1] == nil) {
				return invalid("prices",
					"A pricing tier must carry the same per-million rates as its entry.")
			}
			if *pair[1] == nil {
				continue
			}
			value, err := normalizePriceAmount(**pair[1])
			if err != nil {
				return err
			}
			**pair[1] = value
		}
	}
	return nil
}

// normalizeWindows checks one window set: a zone the clock can resolve,
// well-formed periods, and no two periods covering the same time on a shared
// day.
func normalizeWindows(windows *rates.PriceWindows) error {
	if windows == nil {
		return nil
	}
	if _, err := time.LoadLocation(windows.TimeZone); err != nil {
		return invalid("prices", "The pricing window time zone is unknown.")
	}
	if len(windows.Periods) < 1 || len(windows.Periods) > pricingpolicy.MaxWindowPeriods {
		return invalid("prices",
			"Pricing windows name 1–"+strconv.Itoa(pricingpolicy.MaxWindowPeriods)+" periods.")
	}
	type span struct{ start, end int }
	days := map[int][]span{}
	for i := range windows.Periods {
		period := &windows.Periods[i]
		start, validStart := pricingpolicy.WindowMinutes(period.Start, false)
		if !validStart {
			return invalid("prices", "A pricing window start must be a time between 00:00 and 23:59.")
		}
		end, validEnd := pricingpolicy.WindowMinutes(period.End, true)
		if !validEnd {
			return invalid("prices", "A pricing window end must be a time between 00:01 and 24:00.")
		}
		if end <= start {
			return invalid("prices", "A pricing window end must fall after its start.")
		}
		multiplier, err := normalizePriceAmount(period.Multiplier)
		if err != nil {
			return err
		}
		period.Multiplier = multiplier
		if len(period.Days) < 1 || len(period.Days) > 7 {
			return invalid("prices", "A pricing window period names the days it applies on.")
		}
		seen := map[int]bool{}
		for _, day := range period.Days {
			if day < 1 || day > 7 || seen[day] {
				return invalid("prices", "A pricing window period names unique ISO weekdays.")
			}
			seen[day] = true
			for _, other := range days[day] {
				if start < other.end && other.start < end {
					return invalid("prices", "Pricing window periods must not overlap on a shared day.")
				}
			}
			days[day] = append(days[day], span{start, end})
		}
	}
	return nil
}

func normalizePriceAmount(value string) (string, error) {
	if normalized, ok := pricingpolicy.NormalizeAmount(value); ok {
		return normalized, nil
	}
	return "", invalid("prices",
		"Rates must be non-negative decimals with at most 12 integer and 12 fractional digits.")
}

func normalizeContainerRates(rates map[string]string) error {
	if len(rates) > pricingpolicy.MaxContainerTiers {
		return invalid("container_per_session", "At most sixteen memory tiers may be priced.")
	}
	for tier, fee := range rates {
		if !pricingpolicy.ValidContainerTier(tier) {
			return invalid("container_per_session", "Use a native memory tier such as 1g or 4g.")
		}
		normalized, err := normalizePriceAmount(fee)
		if err != nil {
			return err
		}
		rates[tier] = normalized
	}
	return nil
}

func OptionalJSON(value any) any {
	data, _ := json.Marshal(value)
	if string(data) == "null" {
		return nil
	}
	return string(data)
}

func dimensionKey(p rates.Price) string {
	return p.ProviderKind + "\x00" + optional(p.ProviderID) + "\x00" +
		optional(p.VendorID) + "\x00" + p.Model + "\x00" + p.Operation
}

func priceDimensions(p rates.Price) map[string]string {
	dimensions := map[string]string{
		"provider_kind": p.ProviderKind,
		"model":         p.Model,
		"operation":     p.Operation,
	}
	if p.ProviderID != nil {
		dimensions["provider_id"] = *p.ProviderID
	}
	if p.VendorID != nil {
		dimensions["vendor_id"] = *p.VendorID
	}
	return dimensions
}

func isRemoval(p rates.Price) bool {
	return p.InputPerMillion == nil && p.CachedInputPerMillion == nil &&
		p.OutputPerMillion == nil && p.CacheWriteInputPerMillion == nil &&
		p.CacheWrite5MInputPerMillion == nil && p.CacheWrite1HInputPerMillion == nil &&
		p.AudioInputPerMillion == nil && p.CachedAudioInputPerMillion == nil &&
		p.AudioOutputPerMillion == nil && p.ImageInputPerMillion == nil &&
		p.CachedImageInputPerMillion == nil && p.ImageOutputPerMillion == nil &&
		p.UnitPrice == nil && p.PerRequest == nil && p.BatchMultiplier == nil &&
		p.TrainingPerMillion == nil && p.StoragePerGiBDay == nil &&
		p.CacheStoragePerMillionTokenHour == nil && len(p.ContainerPerSession) == 0 &&
		len(p.ToolCalls) == 0 && len(p.Tiers) == 0 && p.Windows == nil
}
