package gateway

import (
	"context"
	"net/http"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage"
)

func (s *Server) admitRetainedRequest(ctx context.Context, x *call, a access.Authority, p *pin, route *routing.Route, deferred bool) *Error {
	if err := s.retainedAccountingPolicy(x, p, deferred); err != nil {
		return err
	}
	if err := s.reserveState(ctx, x, a, time.Duration(route.OverallTimeout)*time.Millisecond); err != nil {
		return err
	}
	if err := s.reserveRetainedCost(ctx, x, p, route); err != nil {
		return err
	}
	return s.gateRetainedSlot(ctx, x, p, route)
}

// Unlike agent control, retained operations may have effective prices. Pin
// those prices and discounts before admission, and settle observed usage with
// the normal request accounting path. Deferred work cannot safely be accepted
// until a durable settlement owner exists for that resource kind.
func (s *Server) retainedAccountingPolicy(x *call, p *pin, deferred bool) *Error {
	if p.provider.CredentialSource == "caller" {
		return policyUnavailable("unsupported_provider_credentials", "Retained resources require managed provider credentials.")
	}
	if deferred {
		return policyUnavailable("unsupported_metered_operation", "This operation requires durable deferred-usage settlement, which is not available for this resource family.")
	}
	if x.routingInputs == nil && s.Runtime != nil {
		x.routingInputs = s.Runtime.RoutingInputs()
	}
	if x.routingInputs == nil || x.request.startedAt.Sub(x.routingInputs.RefreshedAt) > usage.PerformanceMaxAge {
		return serverError(http.StatusServiceUnavailable, "pricing_unavailable", "Current resource pricing is unavailable; retry shortly.")
	}
	p.attempt.Price = x.routingInputs.Price(p.provider.Kind, p.provider.ID, p.provider.VendorID, p.model, x.operationName(), x.request.startedAt)
	p.attempt.Discount = x.routingInputs.Discount(p.provider.Kind, p.provider.ID, p.provider.VendorID, x.request.startedAt)
	x.retainedAccounting = true
	return nil
}

func (s *Server) retainedSettle(ctx context.Context, x *call, p *pin) {
	if p != nil {
		s.settlePinHold(ctx, x, p.hold, x.settledTokens())
	}
	s.settleAdmission(ctx, x)
}

func observeRetainedUsage(x *call, source oif.Document, generation bool) error {
	if x.exchange == nil {
		return nil
	}
	fact := &x.facts[x.exchange.index]
	if !generation {
		// A completed control/search request has no generated tokens. Its
		// per-request price still applies through observed request usage.
		fact.observeUsage(&wire.Usage{}, true)
		return nil
	}
	observation, err := openai.ObserveResponseDocumentUsage(source)
	if err != nil {
		return err
	}
	// Compaction is a terminal unary result and has no response status field.
	fact.observeUsage(observation.Usage, observation.Usage != nil)
	return nil
}

func (s *Server) reserveRetainedCost(ctx context.Context, x *call, p *pin, route *routing.Route) *Error {
	if s.Runtime != nil {
		if release := s.Runtime.Release(); release != nil {
			x.currentCommitments = release.Snapshot
		}
	}
	if x.routingInputs == nil && s.Runtime != nil {
		x.routingInputs = s.Runtime.RoutingInputs()
	}
	if x.lease == nil || !x.costBudgeted(x.authority) {
		return nil
	}
	bound := x.attemptCostBound(p.attempt)
	charge := x.attemptChargeBound(p.attempt, bound)
	if bound.IsZero() && charge.IsZero() {
		return nil
	}
	// GrowCost replaces the reservation with the supplied total, so a
	// collection page must reserve what its earlier reads already
	// dispatched (tracked in spentCost/spentCharge by spendCaps) plus
	// this read, not just this read alone.
	total := x.spentCost.Add(bound)
	totalCharge := x.spentCharge.Add(charge)
	return s.growCost(ctx, x.lease, total.String(), totalCharge.String(), x.request.accountingID(), time.Duration(route.OverallTimeout)*time.Millisecond)
}
