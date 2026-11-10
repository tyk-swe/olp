package limits

import (
	"errors"
	"math"
	"math/big"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/pricingpolicy"
)

const MaxCounter = int64(1)<<53 - 1

type Basis string

const (
	BasisCost   Basis = "cost"
	BasisCharge Basis = "charge"
)

func (b Basis) Normalized() Basis {
	if b == BasisCharge {
		return BasisCharge
	}
	return BasisCost
}

func (b Basis) Valid() bool {
	return b == "" || b == BasisCost || b == BasisCharge
}

func (b Basis) Pick(cost, charge string) string {
	if b.Normalized() == BasisCharge {
		return charge
	}
	return cost
}

type Dimension string

const (
	DimensionRequests    Dimension = "requests"
	DimensionTokens      Dimension = "tokens"
	DimensionConcurrency Dimension = "concurrency"
	DimensionDailyCost   Dimension = "daily_cost"
	DimensionWeeklyCost  Dimension = "weekly_cost"
	DimensionMonthlyCost Dimension = "monthly_cost"
)

var (
	ErrMalformedState     = errors.New("distributed limit state is malformed")
	ErrUnexpectedResponse = errors.New("distributed limit script returned an unexpected response")
	ErrUninitializedCost  = errors.New("cost state awaits PostgreSQL reconciliation for the current UTC window")
)

type ServiceError struct{ Err error }

func (e *ServiceError) Error() string { return "distributed limit service failed: " + e.Err.Error() }

func (e *ServiceError) Unwrap() error { return e.Err }

type ExceededError struct {
	Dimension  Dimension
	Basis      Basis
	RetryAfter time.Duration
	Estimate   bool
	Rate       RateState
}

func (e *ExceededError) Error() string {
	return string(e.Dimension) + " limit exceeded, retry after " + e.RetryAfter.String()
}

type InvalidRequestError struct{ Reason string }

func (e *InvalidRequestError) Error() string { return e.Reason }

type RateState struct {
	RequestLimit     int64
	TokenLimit       int64
	RequestRemaining int64
	TokenRemaining   int64
	ResetAfter       time.Duration
	ResetAt          time.Time
}

func (s RateState) Limited() bool { return s.RequestLimit > 0 || s.TokenLimit > 0 }

func (s RateState) LimitsRequests() bool { return s.RequestLimit > 0 }

func (s RateState) LimitsTokens() bool { return s.TokenLimit > 0 }

func ValidRequestLimit(value *int64) bool {
	return value == nil || *value > 0 && *value <= math.MaxInt32
}

func ValidTokenLimit(value *int64) bool {
	return value == nil || *value > 0 && *value <= MaxCounter
}

func ValidCostLimit(value string) bool {
	return pricingpolicy.ValidAmount(value) && strings.Trim(value, "0.") != ""
}

func ValidDecimal(value string) bool {
	if len(value) < 1 || len(value) > 96 {
		return false
	}
	integer, fraction, fractional := strings.Cut(value, ".")
	if len(integer) < 1 || fractional && (len(fraction) < 1 || len(fraction) > 12) {
		return false
	}
	return digits(integer) && digits(fraction)
}

func digits(text string) bool {
	return strings.Trim(text, "0123456789") == ""
}

func ValidLookup(lookup string) bool {
	if len(lookup) < 8 || len(lookup) > 40 {
		return false
	}
	return strings.Trim(lookup, "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ_") == ""
}

func WithinBound(bound, observed, amount string) bool {
	limit, ok := new(big.Rat).SetString(bound)
	if !ok {
		return false
	}
	total, ok := new(big.Rat).SetString(observed)
	if !ok {
		return false
	}
	added, ok := new(big.Rat).SetString(amount)
	if !ok {
		return false
	}
	return total.Add(total, added).Cmp(limit) <= 0
}

func AmountEqual(a, b string) bool {
	left, okLeft := new(big.Rat).SetString(a)
	right, okRight := new(big.Rat).SetString(b)
	return okLeft && okRight && left.Cmp(right) == 0
}

func ValidBudgetBoundary(boundary string) bool {
	switch boundary {
	case "api_key", "budget_group", "key_route", "key_end_user", "project_end_user", "attribution", "project", "organization", "installation":
		return true
	default:
		return false
	}
}
