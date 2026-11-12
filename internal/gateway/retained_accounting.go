package gateway

import (
	"context"
	"maps"
	"net/http"
	"time"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage/routinginputs"
)

type retainedUsage uint8

const (
	retainedTokenUsage retainedUsage = iota
	retainedTokenlessUsage
)

type retainedBilling uint8

const (
	retainedUnpriced retainedBilling = iota
	retainedMeteredWork
	retainedPriced
	retainedFree
)

type retainedOperation struct {
	use     retainedUse
	usage   retainedUsage
	billing retainedBilling
}

type retainedAdmissionPlan struct {
	attempt routing.Attempt
	billing retainedBilling
}

func retainedOperationForAction(action string) retainedOperation {
	switch action {
	case "list", "get", "delete", "cancel", "content":
		return retainedOperation{use: retainedHousekeeping}
	default:
		return retainedOperation{use: retainedNewWork}
	}
}

func (p retainedAdmissionPlan) authority(authority keyauthority.Authority, costExempt bool) keyauthority.Authority {
	if p.billing == retainedFree {
		return researchRateAuthority(authority, false)
	}
	if costExempt {
		return withoutCostBudgets(authority)
	}
	return authority
}

func (x *call) freeRetainedControl() bool {
	return x.retainedAdmission != nil && x.retainedAdmission.billing == retainedFree
}

func (x *call) unpricedRetainedControl() bool {
	return x.retainedAdmission == nil && x.retained.billing != retainedMeteredWork
}

type retainedListEntry struct {
	resource  *resources.Resource
	pin       *pin
	route     *routing.Route
	admission retainedAdmissionPlan
}

// A continuation candidate has no effect on the caller's page admission until
// it is actually read. Refills can acquire cost admission before a paid read.
func retainedPageFreeControl(entries []retainedListEntry, limit int) bool {
	for _, entry := range entries[:min(len(entries), limit)] {
		if entry.admission.billing != retainedFree {
			return false
		}
	}
	return true
}

// An initially free page may need paid reads after provider tombstones are
// removed. Add cost leases without reserving its request rates a second time.
func (s *Server) admitRetainedPageCost(ctx context.Context, x *call, authority keyauthority.Authority, ttl time.Duration) *fault.Error {
	authority = retainedCostAuthority(authority)
	authority.Attribution = x.attribution
	authority.Policy.RouteLimits = nil // each route has its own page lease
	lease, failure := s.deps.Admission.reserveKey(ctx, authority, x.clientSurface(), resourceEstimate, ttl)
	if failure != nil {
		return failure
	}
	if x.lease == nil {
		x.lease = lease
	} else {
		x.lease.Join(lease)
	}
	return nil
}

func retainedCostAuthority(authority keyauthority.Authority) keyauthority.Authority {
	authority.Policy.RequestsPerMinute, authority.Policy.TokensPerMinute, authority.Policy.MaxConcurrency = nil, nil, nil
	authority.BudgetGroupRPM, authority.BudgetGroupTPM, authority.BudgetGroupConcurrency = nil, nil, nil
	authority.Policy.RouteLimits = maps.Clone(authority.Policy.RouteLimits)
	for slug, policy := range authority.Policy.RouteLimits {
		policy.RequestsPerMinute, policy.TokensPerMinute, policy.MaxConcurrency = nil, nil, nil
		authority.Policy.RouteLimits[slug] = policy
	}
	copyEndUser := func(policy *keyauthority.EndUserPolicy) *keyauthority.EndUserPolicy {
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

func (s *Server) admitRetainedRequest(ctx context.Context, x *call, a keyauthority.Authority, p *pin, route *routing.Route, deferred bool) *fault.Error {
	admission, err := s.prepareRetainedAdmission(x, p, deferred)
	if err != nil {
		return err
	}
	x.retainedAdmission = &admission
	p.attempt = admission.attempt
	if err := s.reserveState(ctx, x, a, time.Duration(route.OverallTimeout)*time.Millisecond); err != nil {
		return err
	}
	if err := s.reserveRetainedCost(ctx, x, p, route); err != nil {
		return err
	}
	return s.gateRetainedSlot(ctx, x, p, route)
}

func (s *Server) reserveRetainedPage(ctx context.Context, x *call, authority keyauthority.Authority, entries []retainedListEntry, limit int, ttl time.Duration) *fault.Error {
	authority.Attribution = x.attribution
	if retainedPageFreeControl(entries, limit) {
		authority = researchRateAuthority(authority, false)
	}
	lease, failure := s.deps.Admission.reserveKey(ctx, authority, x.clientSurface(), max(resourceEstimate, x.estimate), ttl)
	x.lease = lease
	return failure
}

func (s *Server) prepareRetainedAdmission(x *call, p *pin, deferred bool) (retainedAdmissionPlan, *fault.Error) {
	if p.provider.CredentialSource == "caller" {
		return retainedAdmissionPlan{}, policyUnavailable("unsupported_provider_credentials", "Retained resources require managed provider credentials.")
	}
	if deferred {
		if err := s.retainedUnboundedLimits(x, p); err != nil {
			return retainedAdmissionPlan{}, err
		}
	}
	if x.routingInputs == nil && s.deps.Runtime != nil {
		x.routingInputs = s.deps.Runtime.RoutingInputs()
	}
	if x.routingInputs == nil || x.request.startedAt.Sub(x.routingInputs.RefreshedAt) > routinginputs.PerformanceMaxAge {
		return retainedAdmissionPlan{}, fault.ServerError(http.StatusServiceUnavailable, "pricing_unavailable", "Current resource pricing is unavailable; retry shortly.")
	}
	attempt := p.attempt
	attempt.Price = x.routingInputs.Price(p.provider.Kind, p.provider.ID, p.provider.VendorID, p.model, x.operationName(), x.request.startedAt)
	attempt.Discount = x.routingInputs.Discount(p.provider.Kind, p.provider.ID, p.provider.VendorID, x.request.startedAt)
	if x.retained.use == retainedHousekeeping {
		bound := x.attemptCostBound(attempt)
		charge := x.attemptChargeBound(attempt, bound)
		if bound.IsZero() && charge.IsZero() {
			return retainedAdmissionPlan{attempt: attempt, billing: retainedFree}, nil
		}
	}
	return retainedAdmissionPlan{attempt: attempt, billing: retainedPriced}, nil
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

func (s *Server) reserveRetainedCost(ctx context.Context, x *call, p *pin, route *routing.Route) *fault.Error {
	if s.deps.Runtime != nil {
		if release := s.deps.Runtime.Release(); release != nil {
			x.currentCommitments = release.Snapshot()
		}
	}
	if x.routingInputs == nil && s.deps.Runtime != nil {
		x.routingInputs = s.deps.Runtime.RoutingInputs()
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
