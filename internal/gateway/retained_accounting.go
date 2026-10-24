package gateway

import (
	"context"
	"maps"
	"net/http"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage"
)

type retainedListEntry struct {
	resource    *resources.Resource
	pin         *pin
	route       *routing.Route
	freeControl bool
}

// A continuation candidate has no effect on the caller's page admission until
// it is actually read. Refills can acquire cost admission before a paid read.
func retainedPageFreeControl(entries []retainedListEntry, limit int) bool {
	for _, entry := range entries[:min(len(entries), limit)] {
		if !entry.freeControl {
			return false
		}
	}
	return true
}

// An initially free page may need paid reads after provider tombstones are
// removed. Add cost leases without reserving its request rates a second time.
func (s *Server) admitRetainedPageCost(ctx context.Context, x *call, authority access.Authority, ttl time.Duration) *Error {
	authority = retainedCostAuthority(authority)
	authority.Attribution = x.attribution
	authority.Policy.RouteLimits = nil // each route has its own page lease
	lease, failure := s.Admission.reserveKey(ctx, authority, x.clientSurface(), resourceEstimate, ttl)
	if failure != nil {
		return failure
	}
	if x.lease == nil {
		x.lease = lease
	} else {
		x.lease.Attach(lease)
	}
	return nil
}

func retainedCostAuthority(authority access.Authority) access.Authority {
	authority.Policy.RequestsPerMinute, authority.Policy.TokensPerMinute, authority.Policy.MaxConcurrency = nil, nil, nil
	authority.BudgetGroupRPM, authority.BudgetGroupTPM, authority.BudgetGroupConcurrency = nil, nil, nil
	authority.Policy.RouteLimits = maps.Clone(authority.Policy.RouteLimits)
	for slug, policy := range authority.Policy.RouteLimits {
		policy.RequestsPerMinute, policy.TokensPerMinute, policy.MaxConcurrency = nil, nil, nil
		authority.Policy.RouteLimits[slug] = policy
	}
	copyEndUser := func(policy *access.EndUserPolicy) *access.EndUserPolicy {
		if policy == nil {
			return nil
		}
		copy := *policy
		copy.Defaults.RequestsPerMinute, copy.Defaults.TokensPerMinute, copy.Defaults.MaxConcurrency = nil, nil, nil
		copy.Overrides = maps.Clone(policy.Overrides)
		for digest, limits := range copy.Overrides {
			limits.RequestsPerMinute, limits.TokensPerMinute, limits.MaxConcurrency = nil, nil, nil
			copy.Overrides[digest] = limits
		}
		return &copy
	}
	authority.Policy.EndUserPolicy = copyEndUser(authority.Policy.EndUserPolicy)
	authority.ProjectEndUserPolicy = copyEndUser(authority.ProjectEndUserPolicy)
	return authority
}

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
		if err := s.retainedUnboundedLimits(x, p); err != nil {
			return err
		}
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
	x.retainedFreeControl = false
	if x.retainedHousekeeping {
		bound := x.attemptCostBound(p.attempt)
		charge := x.attemptChargeBound(p.attempt, bound)
		x.retainedFreeControl = bound.IsZero() && charge.IsZero()
	}
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
		fact.TokenlessUsage = true
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
