package rates

import (
	"math/big"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/pricingpolicy"
)

// rateSet is the per-million rates of a price, or of one of its tiers: a tier
// replaces the entry's whole rate set, so both parse into the same shape.
type rateSet struct {
	input            *big.Int
	cached           *big.Int
	cacheWrite       *big.Int
	cacheWrite5m     *big.Int
	cacheWrite1h     *big.Int
	output           *big.Int
	audioInput       *big.Int
	cachedAudioInput *big.Int
	audioOutput      *big.Int
	imageInput       *big.Int
	cachedImageInput *big.Int
	imageOutput      *big.Int
}

// inputHead reports whether the set has any rate an input share could bill at.
func (s *rateSet) inputHead() bool {
	return s.input != nil || s.audioInput != nil || s.imageInput != nil
}

// outputHead reports whether the set has any rate an output share could bill at.
func (s *rateSet) outputHead() bool {
	return s.output != nil || s.audioOutput != nil || s.imageOutput != nil
}

// firstRate returns the first rate present in a fallback chain, nil when the
// chain resolves to nothing, and the field name of the rate it took.
func (s *rateSet) firstRate(chain []rateField) (*big.Int, string) {
	for _, field := range chain {
		if rate := field.of(s); rate != nil {
			return rate, field.name
		}
	}
	return nil, ""
}

// rateField names one rate field so fallback chains and the components list
// can share one ordering.
type rateField struct {
	name string
	of   func(*rateSet) *big.Int
}

// parsedTier is a threshold and the rate set above it.
type parsedTier struct {
	above int64
	rates rateSet
}

// parsedWindow is one period of a price's windows, with its days as an
// ISO-weekday set and its times as minutes of day.
type parsedWindow struct {
	days       [8]bool
	start, end int
	multiplier *big.Int
}

// priceRates is a Price with its rates parsed into 1e-12 units. Rates are
// numeric(24,12) in PostgreSQL, so every one has twelve fractional digits there
// whatever its text says, which is what the division scale below depends on.
type priceRates struct {
	retained Price
	// valid is false when a rate is not a decimal PostgreSQL could have stored.
	// Such a price is treated as no price at all.
	valid bool
	rateSet
	unit       *big.Int
	perRequest *big.Int
	batch      *big.Int
	tools      map[string]*big.Int
	tiers      []parsedTier
	location   *time.Location
	windows    []parsedWindow
}

func parseRateSet(dst *rateSet, input, cached, cacheWrite, cacheWrite5m, cacheWrite1h, output,
	audioInput, cachedAudioInput, audioOutput, imageInput, cachedImageInput, imageOutput *string) bool {
	for _, field := range [...]struct {
		text *string
		into **big.Int
	}{
		{input, &dst.input},
		{cached, &dst.cached},
		{cacheWrite, &dst.cacheWrite},
		{cacheWrite5m, &dst.cacheWrite5m},
		{cacheWrite1h, &dst.cacheWrite1h},
		{output, &dst.output},
		{audioInput, &dst.audioInput},
		{cachedAudioInput, &dst.cachedAudioInput},
		{audioOutput, &dst.audioOutput},
		{imageInput, &dst.imageInput},
		{cachedImageInput, &dst.cachedImageInput},
		{imageOutput, &dst.imageOutput},
	} {
		if field.text == nil {
			continue
		}
		scaled, ok := scaledDecimal(*field.text, costDigits)
		if !ok {
			return false
		}
		*field.into = scaled
	}
	return true
}

func parseRates(p *Price) *priceRates {
	r := &priceRates{valid: true, retained: *p}
	fail := func() *priceRates { return &priceRates{} }
	for _, rate := range []*string{p.TrainingPerMillion, p.StoragePerGiBDay, p.CacheStoragePerMillionTokenHour} {
		if rate != nil {
			if _, ok := scaledDecimal(*rate, costDigits); !ok {
				return fail()
			}
		}
	}
	for tier, fee := range p.ContainerPerSession {
		if !pricingpolicy.ValidContainerTier(tier) {
			return fail()
		}
		if _, ok := scaledDecimal(fee, costDigits); !ok {
			return fail()
		}
	}
	if !parseRateSet(&r.rateSet, p.InputPerMillion, p.CachedInputPerMillion, p.CacheWriteInputPerMillion,
		p.CacheWrite5MInputPerMillion, p.CacheWrite1HInputPerMillion, p.OutputPerMillion,
		p.AudioInputPerMillion, p.CachedAudioInputPerMillion, p.AudioOutputPerMillion,
		p.ImageInputPerMillion, p.CachedImageInputPerMillion, p.ImageOutputPerMillion) {
		return fail()
	}
	for _, field := range [...]struct {
		text *string
		into **big.Int
	}{
		{p.UnitPrice, &r.unit},
		{p.PerRequest, &r.perRequest},
		{p.BatchMultiplier, &r.batch},
	} {
		if field.text == nil {
			continue
		}
		scaled, ok := scaledDecimal(*field.text, costDigits)
		if !ok {
			return fail()
		}
		*field.into = scaled
	}
	if len(p.ToolCalls) > 0 {
		r.tools = make(map[string]*big.Int, len(p.ToolCalls))
		for tool, fee := range p.ToolCalls {
			scaled, ok := scaledDecimal(fee, costDigits)
			if !ok {
				return fail()
			}
			r.tools[tool] = scaled
		}
	}
	for _, tier := range p.Tiers {
		parsed := parsedTier{above: tier.AboveInputTokens}
		if !parseRateSet(&parsed.rates, tier.InputPerMillion, tier.CachedInputPerMillion, tier.CacheWriteInputPerMillion,
			tier.CacheWrite5MInputPerMillion, tier.CacheWrite1HInputPerMillion, tier.OutputPerMillion,
			tier.AudioInputPerMillion, tier.CachedAudioInputPerMillion, tier.AudioOutputPerMillion,
			tier.ImageInputPerMillion, tier.CachedImageInputPerMillion, tier.ImageOutputPerMillion) {
			return fail()
		}
		r.tiers = append(r.tiers, parsed)
	}
	if p.Windows != nil {
		location, err := time.LoadLocation(p.Windows.TimeZone)
		if err != nil {
			return fail()
		}
		r.location = location
		for _, period := range p.Windows.Periods {
			start, validStart := pricingpolicy.WindowMinutes(period.Start, false)
			end, validEnd := pricingpolicy.WindowMinutes(period.End, true)
			multiplier, ok := scaledDecimal(period.Multiplier, costDigits)
			if !validStart || !validEnd || !ok {
				return fail()
			}
			window := parsedWindow{start: start, end: end, multiplier: multiplier}
			for _, day := range period.Days {
				window.days[day] = true
			}
			r.windows = append(r.windows, window)
		}
	}
	return r
}

// scaledDecimal parses a non-negative decimal into units of 10^-digits. A value
// with more fractional digits than the column holds is refused rather than
// rounded: no stored value has them.
func scaledDecimal(text string, digits int) (*big.Int, bool) {
	text = strings.TrimPrefix(text, "+")
	integer, fraction, hasPoint := strings.Cut(text, ".")
	if integer == "" || hasPoint && fraction == "" || len(fraction) > digits {
		return nil, false
	}
	whole := integer + fraction + strings.Repeat("0", digits-len(fraction))
	for index := 0; index < len(whole); index++ {
		if whole[index] < '0' || whole[index] > '9' {
			return nil, false
		}
	}
	scaled, ok := new(big.Int).SetString(whole, 10)
	return scaled, ok
}
