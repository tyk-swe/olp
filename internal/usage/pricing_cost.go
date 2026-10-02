package usage

import (
	"math/big"
	"strings"
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

// priceRates is a Price with its rates parsed into 1e-12 units. Rates are
// numeric(24,12) in PostgreSQL, so every one has twelve fractional digits there
// whatever its text says, which is what the division scale below depends on.
type priceRates struct {
	// valid is false when a rate is not a decimal PostgreSQL could have stored.
	// Such a price is treated as no price at all.
	valid        bool
	input        *big.Int
	cached       *big.Int
	cacheWrite   *big.Int
	cacheWrite5m *big.Int
	cacheWrite1h *big.Int
	output       *big.Int
	unit         *big.Int
}

func parseRates(p *Price) *priceRates {
	r := &priceRates{valid: true}
	for _, field := range [...]struct {
		text *string
		into **big.Int
	}{
		{p.InputPerMillion, &r.input},
		{p.CachedInputPerMillion, &r.cached},
		{p.CacheWriteInputPerMillion, &r.cacheWrite},
		{p.CacheWrite5MInputPerMillion, &r.cacheWrite5m},
		{p.CacheWrite1HInputPerMillion, &r.cacheWrite1h},
		{p.OutputPerMillion, &r.output},
		{p.UnitPrice, &r.unit},
	} {
		if field.text == nil {
			continue
		}
		scaled, ok := scaledDecimal(*field.text, costDigits)
		if !ok {
			return &priceRates{}
		}
		*field.into = scaled
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
// has no rate.
func (p Price) Cost(u AttemptUsage) (Cost, bool) { return parseRates(&p).cost(u) }

// Cost prices one attempt's usage against this price. The rates are parsed once
// when the routing inputs are loaded, so pricing a request does not parse them
// again.
func (p *RoutingPrice) Cost(u AttemptUsage) (Cost, bool) {
	if p == nil {
		return Cost{}, false
	}
	return p.parsed().cost(u)
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

// cost mirrors priceAttemptSQL. Each division by a million is rounded at the
// scale PostgreSQL gives its quotient, the terms are summed exactly, and the sum
// is rounded half away from zero to the twelve places the column stores.
func (r *priceRates) cost(u AttemptUsage) (Cost, bool) {
	if !u.Complete || !r.rated(u) {
		return Cost{}, false
	}
	total := new(big.Int)
	if u.InputTokens != nil {
		charge := r.inputCharge(u)
		if charge.Sign() < 0 {
			// Counts that exceed their own total are refused when an event is
			// validated; PostgreSQL would reject the row.
			return Cost{}, false
		}
		total.Add(total, quotient(charge))
	}
	if u.OutputTokens != nil {
		total.Add(total, quotient(new(big.Int).Mul(big.NewInt(*u.OutputTokens), r.output)))
	}
	if u.MediaUnits != nil {
		units, ok := scaledDecimal(*u.MediaUnits, mediaDigits)
		if !ok {
			return Cost{}, false
		}
		total.Add(total, units.Mul(units, r.unit))
	}
	// total is in units of 10^-18: round half up, which is half away from zero
	// for an amount that cannot be negative.
	half := big.NewInt(500_000)
	total.Add(total, half)
	total.Quo(total, perMillion)
	return clampCost(total), true
}

// rated reports whether the price has a rate for every dimension the usage
// reports, which is the completeness test priceAttemptSQL applies. Cached tokens
// without the input they came out of cannot be priced at all.
func (r *priceRates) rated(u AttemptUsage) bool {
	inputRated := u.InputTokens == nil && u.CachedInputTokens == nil || u.InputTokens != nil && r.input != nil
	return r.valid && inputRated && (u.OutputTokens == nil || r.output != nil) &&
		(u.MediaUnits == nil || r.unit != nil)
}

// inputCharge is the input side of the bill before it is divided by a million:
// the input tokens split into the tiers the usage reports, each at its own
// rate and falling back to the next one the price has.
func (r *priceRates) inputCharge(u AttemptUsage) *big.Int {
	count := func(tokens *int64) int64 {
		if tokens == nil {
			return 0
		}
		return *tokens
	}
	first := func(rates ...*big.Int) *big.Int {
		for _, rate := range rates {
			if rate != nil {
				return rate
			}
		}
		return nil
	}
	cached, write, write5m, write1h := count(u.CachedInputTokens), count(u.CacheWriteInputTokens),
		count(u.CacheWrite5MInputTokens), count(u.CacheWrite1HInputTokens)
	charge := new(big.Int)
	for _, tier := range [...]struct {
		tokens int64
		rate   *big.Int
	}{
		{*u.InputTokens - cached - write, r.input},
		{cached, first(r.cached, r.input)},
		{write - write5m - write1h, first(r.cacheWrite, r.input)},
		{write5m, first(r.cacheWrite5m, r.cacheWrite, r.input)},
		{write1h, first(r.cacheWrite1h, r.cacheWrite, r.input)},
	} {
		charge.Add(charge, new(big.Int).Mul(big.NewInt(tier.tokens), tier.rate))
	}
	return charge
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
// estimate of what the request will cost. Input is charged at the highest of the
// input, cached and cache write rates the price has, because nothing tells
// admission whether the provider will read or write its cache, and each
// division by a million is rounded up. It is false when the price lacks a rate
// the request needs, which is a request nobody can price.
func (p *RoutingPrice) CostBound(input, output int64, generation bool) (Cost, bool) {
	if p == nil {
		return Cost{}, false
	}
	r := p.parsed()
	if !r.valid || r.input == nil || generation && r.output == nil {
		return Cost{}, false
	}
	rate := r.input
	for _, other := range [...]*big.Int{r.cached, r.cacheWrite, r.cacheWrite5m, r.cacheWrite1h} {
		if other != nil && other.Cmp(rate) > 0 {
			rate = other
		}
	}
	bound := ceilPerMillion(max(input, 0), rate)
	if generation {
		bound.Add(bound, ceilPerMillion(max(output, 0), r.output))
	}
	return clampCost(bound), true
}

// ceilPerMillion is tokens x rate / a million, rounded up to a whole unit.
func ceilPerMillion(tokens int64, rate *big.Int) *big.Int {
	charge := new(big.Int).Mul(big.NewInt(tokens), rate)
	charge.Add(charge, big.NewInt(999_999))
	return charge.Quo(charge, perMillion)
}
