package kernel

import (
	"context"
	"time"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	limitadmission "github.com/tyk-swe/olp/internal/limits/admission"
	"github.com/tyk-swe/olp/internal/usage/rates"
)

// retainedPageLedger holds what a list page has reserved and spent on each
// route. It is written as reads are admitted and answered because the deferred
// settlement reads it when a later step panics. Route leases stay separate from
// the caller's page lease, so each route's cost budget grows and settles with
// only its own reads and never the whole page total.
type retainedPageLedger struct {
	server      *Server
	x           *Call
	authority   keyauthority.Authority
	callerCosts bool
	costChecked bool
	routes      map[string]*retainedRouteAccount
}

type retainedRouteAccount struct {
	lease                    *limitadmission.Lease
	admitted, costsAdmitted  bool
	reserved, reservedCharge rates.Cost
	actual, actualCharge     rates.Cost
	lastRead                 *PinnedExchange
}

type retainedReadCost struct {
	bound, charge rates.Cost
}

func newRetainedPageLedger(s *Server, x *Call, authority keyauthority.Authority, callerCosts bool) *retainedPageLedger {
	return &retainedPageLedger{server: s, x: x, authority: authority, callerCosts: callerCosts, routes: map[string]*retainedRouteAccount{}}
}

func (g *retainedPageLedger) route(slug string) *retainedRouteAccount {
	if account := g.routes[slug]; account != nil {
		return account
	}
	account := &retainedRouteAccount{}
	g.routes[slug] = account
	return account
}

func (g *retainedPageLedger) settle(ctx context.Context) {
	zero := int64(0)
	for _, account := range g.routes {
		if account.lease == nil {
			continue
		}
		read := account.lastRead
		if read != nil && !read.fact().BillingUncertain {
			account.lease.SetActualCost(account.actual.String(), account.actualCharge.String())
		}
		quota.SettleKey(ctx, account.lease, read != nil, &zero, g.server.Log)
	}
}

func (g *retainedPageLedger) reserve(ctx context.Context, entry retainedListEntry, overall time.Duration) (retainedReadCost, *fault.Error) {
	s, x, p, route := g.server, g.x, entry.pin, entry.route
	if entry.admission.billing != retainedFree && !g.callerCosts {
		if failure := s.admitRetainedPageCost(ctx, x, g.authority, overall); failure != nil {
			return retainedReadCost{}, failure
		}
		g.callerCosts = true
	}
	if refusal := g.admitRoute(ctx, entry, overall); refusal != nil {
		return retainedReadCost{}, refusal
	}
	x.Route = route
	bound := x.attemptCostBound(p.attempt)
	cost := retainedReadCost{bound: bound, charge: x.attemptChargeBound(p.attempt, bound)}
	if costErr := s.reserveRetainedCost(ctx, x, p, route); costErr != nil {
		return retainedReadCost{}, costErr
	}
	account := g.route(route.Slug)
	if account.lease == nil || cost.bound.IsZero() && cost.charge.IsZero() {
		return cost, nil
	}
	total, totalCharge := account.reserved.Add(cost.bound), account.reservedCharge.Add(cost.charge)
	if costErr := s.growCost(ctx, account.lease, total.String(), totalCharge.String(), x.request.accountingID(), time.Duration(route.OverallTimeout)*time.Millisecond); costErr != nil {
		return retainedReadCost{}, costErr
	}
	account.reserved, account.reservedCharge = total, totalCharge
	return cost, nil
}

func (g *retainedPageLedger) admitRoute(ctx context.Context, entry retainedListEntry, overall time.Duration) *fault.Error {
	account := g.route(entry.route.Slug)
	if account.admitted && (entry.admission.billing == retainedFree || account.costsAdmitted) {
		return nil
	}
	authority := entry.admission.authority(g.authority, g.x.callerCostExempt())
	authority.Attribution = g.x.attribution
	if account.admitted {
		// Earlier free reads reserved only this route's rates.
		authority = retainedCostAuthority(authority)
	}
	lease, gateErr := g.server.Deps.Admission.ReserveRouteLimits(ctx, &authority, entry.route.Slug, ResourceEstimate, overall, quota.CostReservation{}, &g.costChecked)
	if gateErr != nil {
		return gateErr
	}
	switch {
	case lease != nil && account.lease != nil:
		account.lease.Join(lease)
	case lease != nil:
		account.lease = lease
	}
	account.admitted = true
	account.costsAdmitted = account.costsAdmitted || entry.admission.billing != retainedFree
	return nil
}

func (g *retainedPageLedger) answered(slug string, exchange *PinnedExchange) {
	g.route(slug).lastRead = exchange
}

func (g *retainedPageLedger) spent(slug string, cost retainedReadCost) {
	account := g.route(slug)
	account.actual, account.actualCharge = account.actual.Add(cost.bound), account.actualCharge.Add(cost.charge)
}
