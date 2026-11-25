package kernel

import (
	"context"
	"slices"
	"time"

	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	"github.com/tyk-swe/olp/internal/keyauthority"
)

// System work has no key/group/end-user spend but shares aggregate invoice caps.
func (s *Server) reserveSystemBudgets(ctx context.Context, x *Call) *fault.Error {
	source := s.Deps.BudgetAuthority
	if source == nil {
		return nil
	}
	authority, err := source.BudgetAuthority(x.Route.ProjectID)
	if err != nil {
		return quota.LimitsUnavailable()
	}
	x.authority.BudgetIncreases = authority.BudgetIncreases
	x.authority.OrganizationID = authority.OrganizationID
	x.authority.OrganizationBudget = authority.OrganizationBudget
	x.authority.InstallationID = authority.InstallationID
	x.authority.InstallationBudget = authority.InstallationBudget
	x.authority.ProjectBudget = authority.ProjectBudget
	x.authority.ProjectAttributionBudgets = authority.ProjectAttributionBudgets
	authority.Attribution = x.attribution

	if x.callerCostExempt() {
		authority = quota.WithoutCostBudgets(authority)
	}
	costChecked := callerCostBudgeted(&authority, x.limitRoute())
	if costChecked {
		if failure := s.Deps.Admission.CheckCostAccounting(ctx); failure != nil {
			return failure
		}
	}
	var failure *fault.Error
	x.lease, failure = s.Deps.Admission.ReserveAggregateBudgets(ctx, &authority, time.Duration(x.Route.OverallTimeout)*time.Millisecond, s.costReservation(x, authority), &costChecked)
	return failure
}

func (x *Call) AdmissionAuthority(authority keyauthority.Authority) keyauthority.Authority {
	if x == nil {
		return authority
	}
	authority.Attribution = x.attribution
	if x.retainedAdmission != nil {
		return x.retainedAdmission.authority(authority, x.callerCostExempt())
	}
	if (x.actor == "api_key_resource" && x.unpricedRetainedControl()) || x.callerCostExempt() {
		return quota.WithoutCostBudgets(authority)
	}
	return authority
}

// limitRoute matches the route used by request accounting. Collection reads have
// no single route; collection readers admit each represented route separately.
func (x *Call) limitRoute() string {
	if route := x.named(); route != nil {
		return route.Slug
	}
	return ""
}

// requestEstimate covers any provider the pinned route may select. Connection
// defaults affect what is actually sent, even when the client omitted a bound.
// Each attempt is priced once, for its own provider and model, so the cost
// grows with the targets of a route and not with their square.
func requestEstimate(x *Call) int64 {
	var reserved int64
	for _, attempt := range x.attempts {
		provider := x.Snapshot().Provider(attempt.ProviderID)
		reserved = max(reserved, x.attemptReservation(attempt, &provider))
	}
	return max(reserved, 1)
}

// settledTokens accounts for every attempted provider, not just the last one.
// Uncertain attempts retain an estimate rather than refunding unknown work.
func (x *Call) settledTokens() *int64 {
	return x.settledTokensFor(x.facts, x.estimate)
}

func (x *Call) settledTokensFor(facts []events.AttemptFact, estimate int64) *int64 {
	var total int64
	for index := range facts {
		fact := &facts[index]
		if fact.Cache != nil && x.cache != nil && !slices.Contains(x.cache.policy.CountHitsAgainst, "tokens") {
			continue
		}
		actual := quota.SettledAttemptTokens(fact, estimate)
		switch {
		case actual != nil:
			total = quota.AddBounded(total, *actual)
		case fact.BillingUncertain || fact.UsageObserved:
			total = quota.AddBounded(total, estimate)
		}
	}
	return &total
}
