package quota

import (
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/limits/admission"
)

// CostReservation is what a request asks to hold against the cost budgets of its
// applicable caller and aggregate boundaries while it is in flight. The zero value holds nothing:
// the request is judged on the spend accrued so far, which is how a request that
// nobody can price is treated.
type CostReservation struct {
	// Amount is the cost estimate as canonical decimal text, empty for none;
	// charge is the estimate a charge-basis budget enforces, which equals the
	// cost estimate when the consumer holds no rate card.
	Amount, Charge string
	// RequestID is the identifier accounting will record the request's spend
	// under, which is how that spend later replaces the reservation.
	RequestID string
}

// amountFor is the estimate one owner's basis enforces.
func (r CostReservation) amountFor(basis limits.Basis) string {
	return basis.Pick(r.Amount, r.Charge)
}

func (r CostReservation) On(request admission.Request) admission.Request {
	if request.Cost == nil {
		return request
	}
	return admission.Request{
		Lookup: request.Lookup, LeaseTTL: request.LeaseTTL, Rate: request.Rate,
		Cost: request.Cost.Holding(r.amountFor(request.Cost.Owner.Basis), r.RequestID),
	}
}

func reporting(rate admission.RateLimits, report bool) admission.RateLimits {
	return admission.RateLimits{
		RequestsPerMinute: rate.RequestsPerMinute, TokensPerMinute: rate.TokensPerMinute, MaxConcurrency: rate.MaxConcurrency,
		TrackConcurrency: rate.TrackConcurrency, Tokens: rate.Tokens, Report: report, Share: rate.Share,
	}
}

// The quotas that can reject an attempt before it is dispatched.
const (
	Connection = "connection"
	Slot       = "slot"
	// quotaRoute is a route's own spend cap, which every target shares.
	Route = "route"
)
