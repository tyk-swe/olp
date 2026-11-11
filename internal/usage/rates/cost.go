package rates

import (
	"math/big"
	"slices"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/usage"
)

// This file prices an attempt in Go exactly as priceAttemptSQL does in
// PostgreSQL. The gateway uses it to reserve a request's cost before it is
// dispatched and to replace the reservation with what the request actually
// cost, so a figure that differed from the one PostgreSQL later accrues would
// leave the budget counting spend that never happened or missing spend that
// did.

const (
	// costDigits is the scale of estimated_cost, numeric(24,12).
	costDigits = 12
	// mediaDigits is the scale media units are stored at, numeric(24,6).
	mediaDigits = 6
	// quotientDigits is the scale of a rate times a token count divided by a
	// million, which is where PostgreSQL's exact quotient ends.
	quotientDigits = costDigits + 6
)

var (
	bigTen   = big.NewInt(10)
	costUnit = new(big.Int).Exp(bigTen, big.NewInt(costDigits), nil)
	// perMillion is the divisor of every token rate, in the scale of a rate.
	perMillion = big.NewInt(1_000_000)
	// maxCostUnits is the largest value numeric(24,12) holds: PostgreSQL raises
	// an overflow beyond it, and a reservation saturates there instead.
	maxCostUnits = new(big.Int).Sub(new(big.Int).Exp(bigTen, big.NewInt(24), nil), big.NewInt(1))
)

var (
	fieldInput            = rateField{"input_per_million", func(s *rateSet) *big.Int { return s.input }}
	fieldCached           = rateField{"cached_input_per_million", func(s *rateSet) *big.Int { return s.cached }}
	fieldCacheWrite       = rateField{"cache_write_input_per_million", func(s *rateSet) *big.Int { return s.cacheWrite }}
	fieldCacheWrite5m     = rateField{"cache_write_5m_input_per_million", func(s *rateSet) *big.Int { return s.cacheWrite5m }}
	fieldCacheWrite1h     = rateField{"cache_write_1h_input_per_million", func(s *rateSet) *big.Int { return s.cacheWrite1h }}
	fieldOutput           = rateField{"output_per_million", func(s *rateSet) *big.Int { return s.output }}
	fieldAudioInput       = rateField{"audio_input_per_million", func(s *rateSet) *big.Int { return s.audioInput }}
	fieldCachedAudioInput = rateField{"cached_audio_input_per_million", func(s *rateSet) *big.Int { return s.cachedAudioInput }}
	fieldAudioOutput      = rateField{"audio_output_per_million", func(s *rateSet) *big.Int { return s.audioOutput }}
	fieldImageInput       = rateField{"image_input_per_million", func(s *rateSet) *big.Int { return s.imageInput }}
	fieldCachedImageInput = rateField{"cached_image_input_per_million", func(s *rateSet) *big.Int { return s.cachedImageInput }}
	fieldImageOutput      = rateField{"image_output_per_million", func(s *rateSet) *big.Int { return s.imageOutput }}
)

// The fallback chains every input share bills through: the share's own rate,
// then the rates a vendor that publishes only text rates charges it at.
var (
	chainText        = []rateField{fieldInput}
	chainCached      = []rateField{fieldCached, fieldInput}
	chainCachedAudio = []rateField{fieldCachedAudioInput, fieldCached, fieldInput}
	chainCachedImage = []rateField{fieldCachedImageInput, fieldCached, fieldInput}
	chainAudio       = []rateField{fieldAudioInput, fieldInput}
	chainImage       = []rateField{fieldImageInput, fieldInput}
	chainWrite       = []rateField{fieldCacheWrite, fieldInput}
	chainWrite5m     = []rateField{fieldCacheWrite5m, fieldCacheWrite, fieldInput}
	chainWrite1h     = []rateField{fieldCacheWrite1h, fieldCacheWrite, fieldInput}
	chainOutput      = []rateField{fieldOutput}
	chainAudioOut    = []rateField{fieldAudioOutput, fieldOutput}
	chainImageOut    = []rateField{fieldImageOutput, fieldOutput}
)

// cost mirrors olp.price_usage. Each division by a million is rounded at the
// scale PostgreSQL gives its quotient, the terms are summed exactly, the window
// and batch multipliers apply to the sum, and the total is rounded half away
// from zero to the twelve places the column stores.
func (r *priceRates) cost(u usage.AttemptUsage, startedAt time.Time, successful bool, discount *big.Int) (Cost, bool) {
	if !u.Evidence.Complete() || !r.valid {
		return Cost{}, false
	}
	// Per-model eval totals do not reveal the individual input sizes that
	// choose invocation tiers. Keep such costs and explicit charges unpriced.
	if u.Deferred && u.InvocationCount != nil && *u.InvocationCount > 1 && len(r.tiers) > 0 {
		return Cost{}, false
	}
	rates := r.selectRates(u.InputTokens)
	total := new(big.Int)
	if u.InputTokens == nil {
		if !inputSharesAbsent(u) {
			// Shares without the total they came out of cannot be priced.
			return Cost{}, false
		}
	} else {
		if !rates.inputHead() {
			return Cost{}, false
		}
		charge, ok := rates.inputCharge(u)
		if !ok {
			return Cost{}, false
		}
		total.Add(total, quotient(charge))
	}
	if u.OutputTokens == nil {
		if u.AudioOutputTokens != nil || u.ImageOutputTokens != nil {
			return Cost{}, false
		}
	} else {
		if !rates.outputHead() {
			return Cost{}, false
		}
		charge, ok := rates.outputCharge(u)
		if !ok {
			return Cost{}, false
		}
		total.Add(total, quotient(charge))
	}
	if u.MediaUnits != nil {
		if r.unit == nil {
			return Cost{}, false
		}
		units, ok := scaledDecimal(*u.MediaUnits, mediaDigits)
		if !ok {
			return Cost{}, false
		}
		total.Add(total, units.Mul(units, r.unit))
	}
	invocations := int64(1)
	if u.Deferred {
		invocations = 0
	}
	if u.InvocationCount != nil {
		invocations = *u.InvocationCount
	}
	if invocations < 0 {
		return Cost{}, false
	}
	if successful && invocations > 0 && r.perRequest != nil {
		total.Add(total, new(big.Int).Mul(new(big.Int).Mul(r.perRequest, perMillion), big.NewInt(invocations)))
	}
	for tool, calls := range u.ToolCalls {
		if calls <= 0 {
			continue
		}
		fee, priced := r.tools[tool]
		if !priced {
			return Cost{}, false
		}
		total.Add(total, new(big.Int).Mul(new(big.Int).Mul(big.NewInt(calls), fee), perMillion))
	}
	if usage.RetainedValues(u) {
		return r.retainedCost(total, u, startedAt, discount)
	}
	window := r.windowAt(startedAt)
	if u.Batch && r.batch == nil {
		return Cost{}, false
	}
	if window == nil && !u.Batch && discount == nil {
		// total is in units of 10^-18: round half up, which is half away from
		// zero for an amount that cannot be negative.
		total.Add(total, big.NewInt(500_000))
		total.Quo(total, perMillion)
		return clampCost(total), true
	}
	// Window and batch multipliers scale the whole usage charge. Each is a
	// rate-scale decimal, so the product's scale grows twelve places per
	// multiplier and the final rounding divides them back out.
	divisorDigits := 6
	if window != nil {
		total.Mul(total, window)
		divisorDigits += costDigits
	}
	if u.Batch {
		total.Mul(total, r.batch)
		divisorDigits += costDigits
	}
	if discount != nil {
		total.Mul(total, discount)
		divisorDigits += costDigits
	}
	divisor := new(big.Int).Exp(bigTen, big.NewInt(int64(divisorDigits)), nil)
	total.Add(total, new(big.Int).Quo(divisor, big.NewInt(2)))
	total.Quo(total, divisor)
	return clampCost(total), true
}

// selectRates is the rate set the attempt prices against: the highest tier its
// input tokens exceed, else the entry's own rates.
func (r *priceRates) selectRates(input *int64) *rateSet {
	if input == nil {
		return &r.rateSet
	}
	selected := &r.rateSet
	for index := range r.tiers {
		if r.tiers[index].above < *input {
			selected = &r.tiers[index].rates
		}
	}
	return selected
}

// windowAt is the multiplier of the pricing window that holds at this instant,
// nil when no period matches, which prices like a multiplier of one.
func (r *priceRates) windowAt(at time.Time) *big.Int {
	if r.location == nil {
		return nil
	}
	local := at.In(r.location)
	day := (int(local.Weekday())+6)%7 + 1
	minutes := local.Hour()*60 + local.Minute()
	for index := range r.windows {
		window := &r.windows[index]
		if window.days[day] && minutes >= window.start && minutes < window.end {
			return window.multiplier
		}
	}
	return nil
}

// inputSharesAbsent reports whether no input-side share is reported, which an
// absent input count requires of a validated event.
func inputSharesAbsent(u usage.AttemptUsage) bool {
	return u.CachedInputTokens == nil && u.CacheWriteInputTokens == nil &&
		u.CacheWrite5MInputTokens == nil && u.CacheWrite1HInputTokens == nil &&
		u.AudioInputTokens == nil && u.CachedAudioInputTokens == nil &&
		u.ImageInputTokens == nil && u.CachedImageInputTokens == nil
}

// inputCharge is the input side of the bill before it is divided by a million:
// the input tokens split into the shares the usage reports, each billed at the
// first rate its fallback chain carries. It is false when a share exceeds its
// total or a nonzero share finds no rate in its chain, either of which leaves
// the attempt unpriced.
func (s *rateSet) inputCharge(u usage.AttemptUsage) (*big.Int, bool) {
	count := func(tokens *int64) int64 {
		if tokens == nil {
			return 0
		}
		return *tokens
	}
	cached, write, write5m, write1h := count(u.CachedInputTokens), count(u.CacheWriteInputTokens),
		count(u.CacheWrite5MInputTokens), count(u.CacheWrite1HInputTokens)
	audio, cachedAudio := count(u.AudioInputTokens), count(u.CachedAudioInputTokens)
	image, cachedImage := count(u.ImageInputTokens), count(u.CachedImageInputTokens)
	var charge *big.Int
	for _, share := range [...]struct {
		tokens int64
		chain  []rateField
	}{
		{*u.InputTokens - cached - write - (audio - cachedAudio) - (image - cachedImage), chainText},
		{cached - cachedAudio - cachedImage, chainCached},
		{cachedAudio, chainCachedAudio},
		{cachedImage, chainCachedImage},
		{audio - cachedAudio, chainAudio},
		{image - cachedImage, chainImage},
		{write - write5m - write1h, chainWrite},
		{write5m, chainWrite5m},
		{write1h, chainWrite1h},
	} {
		if share.tokens < 0 {
			// Counts that exceed their own total are refused when an event is
			// validated; PostgreSQL would reject the row.
			return nil, false
		}
		if share.tokens == 0 {
			continue
		}
		rate, _ := s.firstRate(share.chain)
		if rate == nil {
			return nil, false
		}
		term := new(big.Int).Mul(big.NewInt(share.tokens), rate)
		if charge == nil {
			charge = term
		} else {
			charge.Add(charge, term)
		}
	}
	if charge == nil {
		return new(big.Int), true
	}
	return charge, true
}

// outputCharge is the output side of the bill before it is divided by a
// million, with the same share and fallback rules as inputCharge.
func (s *rateSet) outputCharge(u usage.AttemptUsage) (*big.Int, bool) {
	count := func(tokens *int64) int64 {
		if tokens == nil {
			return 0
		}
		return *tokens
	}
	audio, image := count(u.AudioOutputTokens), count(u.ImageOutputTokens)
	var charge *big.Int
	for _, share := range [...]struct {
		tokens int64
		chain  []rateField
	}{
		{*u.OutputTokens - audio - image, chainOutput},
		{audio, chainAudioOut},
		{image, chainImageOut},
	} {
		if share.tokens < 0 {
			return nil, false
		}
		if share.tokens == 0 {
			continue
		}
		rate, _ := s.firstRate(share.chain)
		if rate == nil {
			return nil, false
		}
		term := new(big.Int).Mul(big.NewInt(share.tokens), rate)
		if charge == nil {
			charge = term
		} else {
			charge.Add(charge, term)
		}
	}
	if charge == nil {
		return new(big.Int), true
	}
	return charge, true
}

// quotient divides a charge in units of 10^-12 by a million the way PostgreSQL's
// numeric division does, returning units of 10^-18. The result is exact when
// the quotient scale PostgreSQL selects reaches eighteen places, and rounded
// half up at that scale otherwise.
func quotient(charge *big.Int) *big.Int {
	if charge.Sign() == 0 {
		return charge
	}
	scale := quotientScale(charge)
	if scale >= quotientDigits {
		return charge
	}
	unit := new(big.Int).Exp(bigTen, big.NewInt(int64(quotientDigits-scale)), nil)
	rounded, remainder := new(big.Int).QuoRem(charge, unit, new(big.Int))
	if remainder.Lsh(remainder, 1).Cmp(unit) >= 0 {
		rounded.Add(rounded, big.NewInt(1))
	}
	return rounded.Mul(rounded, unit)
}

// quotientScale is numeric_div's select_div_scale for a numerator holding
// charge x 10^-12 and a divisor of one million. PostgreSQL stores numerics in
// base 10000: the quotient keeps sixteen significant digits, estimated from the
// base-10000 weight and leading digit of each operand, and never fewer digits
// than the numerator's own twelve.
func quotientScale(charge *big.Int) int {
	digits := charge.String()
	var weight, leading int
	if len(digits) > costDigits {
		// The integer part has len-12 digits, grouped by four from the point.
		integer := len(digits) - costDigits
		weight = (integer - 1) / 4
		leading = atoi(digits[:integer-4*weight])
	} else {
		fraction := strings.Repeat("0", costDigits-len(digits)) + digits
		group := 0
		for fraction[group*4:group*4+4] == "0000" {
			group++
		}
		weight = -(group + 1)
		leading = atoi(fraction[group*4 : group*4+4])
	}
	// A million is the base-10000 digit 100 at weight 1.
	quotientWeight := weight - 1
	if leading <= 100 {
		quotientWeight--
	}
	return min(max(16-quotientWeight*4, costDigits), 1000)
}

func atoi(digits string) int {
	value := 0
	for index := 0; index < len(digits); index++ {
		value = value*10 + int(digits[index]-'0')
	}
	return value
}

func (r *priceRates) maximumWindow() *big.Int {
	if !r.valid || len(r.windows) == 0 {
		return nil
	}
	window := slices.MaxFunc(r.windows, func(a, b parsedWindow) int { return a.multiplier.Cmp(b.multiplier) }).multiplier
	if window.Cmp(costUnit) <= 0 {
		return nil
	}
	return window
}

// minRate is the lowest of the rates present, nil when none is.
func minRate(rates ...*big.Int) *big.Int {
	var lowest *big.Int
	for _, rate := range rates {
		if rate != nil && (lowest == nil || rate.Cmp(lowest) < 0) {
			lowest = rate
		}
	}
	return lowest
}

// maxRate is the highest of the rates present, nil when none is.
func maxRate(rates ...*big.Int) *big.Int {
	var highest *big.Int
	for _, rate := range rates {
		if rate != nil && (highest == nil || rate.Cmp(highest) > 0) {
			highest = rate
		}
	}
	return highest
}

// ceilPerMillion is tokens x rate / a million, rounded up to a whole unit.
func ceilPerMillion(tokens int64, rate *big.Int) *big.Int {
	charge := new(big.Int).Mul(big.NewInt(tokens), rate)
	charge.Add(charge, big.NewInt(999_999))
	return charge.Quo(charge, perMillion)
}

func workflowModelCostThroughTiers(price *RoutingPrice, discount *RoutingDiscount, input, output int64, tiers []PriceTier) (Cost, bool) {
	if len(tiers) == 0 {
		return price.MaximumWindowCostBound(input, output, true, discount)
	}
	bound, ok := workflowModelCostThroughTiers(price, discount, input, output, tiers[1:])
	if !ok {
		return Cost{}, false
	}
	if tiers[0].AboveInputTokens >= input {
		return bound, true
	}
	prior, ok := price.MaximumWindowCostBound(tiers[0].AboveInputTokens, output, true, discount)
	if !ok {
		return Cost{}, false
	}
	if prior.Cmp(bound) > 0 {
		return prior, true
	}
	return bound, true
}
