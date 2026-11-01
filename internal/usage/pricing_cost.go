package usage

import (
	"math/big"
	"slices"
	"strings"
	"time"
)

// This file prices an attempt in Go exactly as priceAttemptSQL does in
// PostgreSQL. The gateway uses it to reserve a request's cost before it is
// dispatched and to replace the reservation with what the request actually
// cost, so a figure that differed from the one PostgreSQL later accrues would
// leave the budget counting spend that never happened or missing spend that
// did. TestIntegrationPricingParity holds the two to each other.

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

// Cost is an amount of money in the installation currency, held exactly as a
// count of 1e-12 units, the scale attempt costs are stored at. The zero value
// is no cost.
type Cost struct{ units *big.Int }

// MaxCost is the largest amount a cost column holds.
func MaxCost() Cost { return Cost{new(big.Int).Set(maxCostUnits)} }

func ParseCost(text string) (Cost, bool) {
	units, ok := scaledDecimal(text, costDigits)
	if !ok || units.Cmp(maxCostUnits) > 0 {
		return Cost{}, false
	}
	return Cost{units}, true
}

// IsZero reports whether the amount is nothing.
func (c Cost) IsZero() bool { return c.units == nil || c.units.Sign() == 0 }

// Add returns the sum of two amounts, saturating at MaxCost.
func (c Cost) Add(other Cost) Cost {
	switch {
	case other.IsZero():
		return c
	case c.IsZero():
		return other
	}
	return clampCost(new(big.Int).Add(c.units, other.units))
}

// Cmp compares two amounts, returning -1, 0 or 1.
func (c Cost) Cmp(other Cost) int {
	left, right := c.units, other.units
	if left == nil {
		left = new(big.Int)
	}
	if right == nil {
		right = new(big.Int)
	}
	return left.Cmp(right)
}

// String renders the amount as the canonical decimal text the limiter and the
// management API use: plain digits, no exponent, no trailing zeros.
func (c Cost) String() string {
	if c.IsZero() {
		return "0"
	}
	digits := c.units.String()
	if len(digits) <= costDigits {
		digits = strings.Repeat("0", costDigits+1-len(digits)) + digits
	}
	integer, fraction := digits[:len(digits)-costDigits], strings.TrimRight(digits[len(digits)-costDigits:], "0")
	if fraction == "" {
		return integer
	}
	return integer + "." + fraction
}

func clampCost(units *big.Int) Cost {
	if units.Cmp(maxCostUnits) > 0 {
		return MaxCost()
	}
	return Cost{units}
}

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
		if !memoryTierPattern.MatchString(tier) {
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
			start, errStart := windowMinutes(period.Start, false)
			end, errEnd := windowMinutes(period.End, true)
			multiplier, ok := scaledDecimal(period.Multiplier, costDigits)
			if errStart != nil || errEnd != nil || !ok {
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

// Cost prices one attempt's usage against this price, exactly as PostgreSQL
// does when the attempt is accounted: the amount, and false when PostgreSQL
// would record no cost because the usage is incomplete or a dimension it used
// has no rate. startedAt decides which pricing window, if any, the attempt fell
// in, and successful decides whether a per-request fee applies.
func (p Price) Cost(u AttemptUsage, startedAt time.Time, successful bool) (Cost, bool) {
	return parseRates(&p).cost(u, startedAt, successful, nil)
}

// Cost prices one attempt's usage against this price under the pinned
// discount (nil discounts nothing). The rates are parsed once when the
// routing inputs are loaded, so pricing a request does not parse them again.
func (p *RoutingPrice) Cost(u AttemptUsage, startedAt time.Time, successful bool, discount *RoutingDiscount) (Cost, bool) {
	if p == nil {
		return Cost{}, false
	}
	var multiplier *big.Int
	if discount != nil {
		multiplier = discount.parsedMultiplier()
	}
	return p.parsed().cost(u, startedAt, successful, multiplier)
}

// parsed returns the rates of this price. A price built outside LoadRoutingInputs
// has none cached; they are parsed per call rather than stored, because copies
// of one price are shared between requests.
func (p *RoutingPrice) parsed() *priceRates {
	if p.rates != nil {
		return p.rates
	}
	return parseRates(&p.Price)
}

// cost mirrors olp.price_usage. Each division by a million is rounded at the
// scale PostgreSQL gives its quotient, the terms are summed exactly, the window
// and batch multipliers apply to the sum, and the total is rounded half away
// from zero to the twelve places the column stores.
func (r *priceRates) cost(u AttemptUsage, startedAt time.Time, successful bool, discount *big.Int) (Cost, bool) {
	if !u.Complete || !r.valid {
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
	if retainedValues(u) {
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
func inputSharesAbsent(u AttemptUsage) bool {
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
func (s *rateSet) inputCharge(u AttemptUsage) (*big.Int, bool) {
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
func (s *rateSet) outputCharge(u AttemptUsage) (*big.Int, bool) {
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

// CostBound is the most a request of input tokens and, for a generation, output
// tokens can cost under this price: a ceiling for admission to reserve, never an
// estimate of what the request will cost. Input is charged at the highest of
// the input-side rates the price has, because nothing tells admission whether
// the provider will read or write its cache or share the input across
// modalities, and each division by a million is rounded up. The bound includes
// a per-request fee and the window multiplier in force at `at`; tool fees and
// batch terms are excluded. It is false when the price lacks a rate the request
// needs, which is a request nobody can price.
func (p *RoutingPrice) CostBound(input, output int64, generation bool, at time.Time, discount *RoutingDiscount) (Cost, bool) {
	if p == nil {
		return Cost{}, false
	}
	return p.costBound(input, output, generation, discount, p.parsed().windowAt(at))
}

func (p *RoutingPrice) MaximumWindowCostBound(input, output int64, generation bool, discount *RoutingDiscount) (Cost, bool) {
	if p == nil {
		return Cost{}, false
	}
	return p.costBound(input, output, generation, discount, p.parsed().maximumWindow())
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

func (p *RoutingPrice) costBound(input, output int64, generation bool, discount *RoutingDiscount, window *big.Int) (Cost, bool) {
	r := p.parsed()
	if !r.valid {
		return Cost{}, false
	}
	rates := r.selectRates(&input)
	if !rates.inputHead() || generation && !rates.outputHead() {
		return Cost{}, false
	}
	rate := maxRate(rates.input, rates.cached, rates.cacheWrite, rates.cacheWrite5m,
		rates.cacheWrite1h, rates.audioInput, rates.cachedAudioInput,
		rates.imageInput, rates.cachedImageInput)
	bound := ceilPerMillion(max(input, 0), rate)
	if generation {
		bound.Add(bound, ceilPerMillion(max(output, 0),
			maxRate(rates.output, rates.audioOutput, rates.imageOutput)))
	}
	if r.perRequest != nil {
		bound.Add(bound, r.perRequest)
	}
	if window != nil {
		// The product carries twelve places of the multiplier's scale; round
		// the bound up to a whole unit so it never under-covers the request.
		bound.Mul(bound, window)
		bound.Add(bound, new(big.Int).Sub(costUnit, big.NewInt(1)))
		bound.Quo(bound, costUnit)
	}
	if discount != nil {
		bound.Mul(bound, discount.parsedMultiplier())
		bound.Add(bound, new(big.Int).Sub(costUnit, big.NewInt(1)))
		bound.Quo(bound, costUnit)
	}
	return clampCost(bound), true
}

// CostFloor is the least one attempt could cost on this price at `at` under the
// pinned discount: the input estimate at the lowest input-side rate the
// selected rate set carries (the estimate itself picks the tier), no output,
// plus the per-request fee, times the window multiplier and the discount,
// rounded down. It is false when no input rate exists.
func (p *RoutingPrice) CostFloor(input int64, at time.Time, discount *RoutingDiscount) (Cost, bool) {
	if p == nil {
		return Cost{}, false
	}
	r := p.parsed()
	if !r.valid {
		return Cost{}, false
	}
	rates := r.selectRates(&input)
	if !rates.inputHead() {
		return Cost{}, false
	}
	rate := minRate(rates.input, rates.cached, rates.cacheWrite, rates.cacheWrite5m,
		rates.cacheWrite1h, rates.audioInput, rates.cachedAudioInput,
		rates.imageInput, rates.cachedImageInput)
	floor := new(big.Int).Quo(new(big.Int).Mul(big.NewInt(max(input, 0)), rate), perMillion)
	if r.perRequest != nil {
		floor.Add(floor, r.perRequest)
	}
	if window := r.windowAt(at); window != nil {
		floor.Mul(floor, window)
		floor.Quo(floor, costUnit)
	}
	if discount != nil {
		floor.Mul(floor, discount.parsedMultiplier())
		floor.Quo(floor, costUnit)
	}
	return clampCost(floor), true
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

// Charge resolves what one billable attempt's consumer pays: the card's
// explicit rate when one matches the attempt's dimensions, else the attempt's
// already-rounded cost times the card's markup, else the cost itself when no
// card applies. Discounts and commitments never apply to explicit rates, so a
// card's explicit price costs the usage itself. False is no charge — the
// cost it multiplies, or the usage an explicit rate cannot price.
func (r *RoutingCardRevision) Charge(u AttemptUsage, startedAt time.Time, successful bool,
	cost *Cost, kind, vendor, model, operation string) (Cost, bool) {
	if explicit := r.Explicit(kind, vendor, model, operation); explicit != nil {
		return explicit.parsed().cost(u, startedAt, successful, nil)
	}
	if cost == nil {
		return Cost{}, false
	}
	if cost.units == nil {
		// A zero cost (a committed attempt) charges zero under any markup.
		return Cost{units: new(big.Int)}, true
	}
	// Round the already-rounded cost by the markup in one half-up rounding.
	total := new(big.Int).Mul(cost.units, r.parsedMarkup())
	total.Add(total, new(big.Int).Quo(costUnit, big.NewInt(2)))
	total.Quo(total, costUnit)
	return clampCost(total), true
}

// ChargeBound is the reservation-equivalent of Charge: the explicit rate's
// own bound, else the cost bound times markup rounded up. False when nothing
// can bound what the consumer pays.
func (r *RoutingCardRevision) ChargeBound(input, output int64, generation bool, at time.Time,
	bound Cost, kind, vendor, model, operation string) (Cost, bool) {
	if explicit := r.Explicit(kind, vendor, model, operation); explicit != nil {
		return explicit.CostBound(input, output, generation, at, nil)
	}
	if bound.units == nil {
		// A zero bound charges zero under any markup.
		return Cost{units: new(big.Int)}, true
	}
	total := new(big.Int).Mul(bound.units, r.parsedMarkup())
	total.Add(total, new(big.Int).Sub(costUnit, big.NewInt(1)))
	total.Quo(total, costUnit)
	return clampCost(total), true
}

func (p *RoutingPrice) WorkflowModelCostBound(input, output int64, discount *RoutingDiscount) (Cost, bool) {
	if p == nil || input < 1 || output < 1 {
		return Cost{}, false
	}
	for _, fee := range p.ToolCalls {
		cost, ok := ParseCost(fee)
		if !ok || !cost.IsZero() {
			return Cost{}, false
		}
	}
	return workflowModelCostThroughTiers(p, discount, input, output, p.Tiers)
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
