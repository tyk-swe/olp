package rates

import (
	"math/big"
	"strings"
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
