package gateway

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/limits"
)

func (a *Admission) reserveAggregateBudgets(ctx context.Context, authority *access.Authority, ttl time.Duration, hold costReservation) (*limits.Lease, *Error) {
	if !authority.OrganizationBudget.Limited() && !authority.InstallationBudget.Limited() && !authority.ProjectBudget.Limited() && !authority.ProjectAttributionBudgets.Matches(authority.Attribution) {
		return nil, nil
	}
	var chain *limits.Lease
	for _, boundary := range [...]struct {
		level, subject string
		policy         *access.BudgetPolicy
	}{
		{"installation", authority.InstallationID, authority.InstallationBudget},
		{"organization", projectBudgetSubject(authority.OrganizationID), authority.OrganizationBudget},
		{"project", projectBudgetSubject(authority.ProjectID), authority.ProjectBudget},
	} {
		if !boundary.policy.Limited() {
			continue
		}
		if !a.ready() || boundary.subject == "" {
			settleKey(ctx, chain, false, nil, a.logger())
			return nil, limitsUnavailable()
		}
		owner := limits.AggregateBudgetID(boundary.level, boundary.subject)
		lease, failure := a.reserveCostBoundary(ctx, authority.ID, owner, boundary.level, "budget."+boundary.level, authority.BudgetIncreases[owner], boundary.policy, ttl, hold)
		if failure != nil {
			settleKey(ctx, chain, false, nil, a.logger())
			return nil, failure
		}
		lease.Attach(chain)
		chain = lease
	}
	for _, key := range slices.Sorted(maps.Keys(authority.Attribution)) {
		value := authority.Attribution[key]
		policy, ok := authority.ProjectAttributionBudgets[key][value]
		if !ok || !policy.Limited() {
			continue
		}
		if !a.ready() || authority.ProjectID == nil {
			settleKey(ctx, chain, false, nil, a.logger())
			return nil, limitsUnavailable()
		}
		owner := limits.AttributionBudgetID(*authority.ProjectID, key, value)
		lease, failure := a.reserveCostBoundary(ctx, authority.ID, owner, "project attribution", "attribution_budgets."+key+"."+value, authority.BudgetIncreases[owner], &policy, ttl, hold)
		if failure != nil {
			settleKey(ctx, chain, false, nil, a.logger())
			return nil, failure
		}
		lease.Attach(chain)
		chain = lease
	}
	return chain, nil
}

// Every aggregate boundary uses the same reservation and fail-closed semantics.
// The caller attaches successful leases or refunds earlier boundaries on refusal.
func (a *Admission) reserveCostBoundary(ctx context.Context, keyID, owner, label, param string, increases string, policy *access.BudgetPolicy, ttl time.Duration, hold costReservation) (*limits.Lease, *Error) {
	request := limits.Request{
		CostOwnerID:      owner,
		CostIncreases:    increases,
		LookupID:         limits.AggregateBudgetLookup(owner),
		DailyCostLimit:   policy.DailyCostLimit,
		WeeklyCostLimit:  policy.WeeklyCostLimit,
		MonthlyCostLimit: policy.MonthlyCostLimit,
		LeaseTTL:         ttl,
		CostEstimate:     hold.amount,
		RequestID:        hold.requestID,
	}
	decision, cancel := context.WithTimeout(ctx, reserveTimeout)
	defer cancel()
	lease, err := a.limiter.Reserve(decision, request)
	if err == nil {
		return lease, nil
	}
	if exceeded, ok := errors.AsType[*limits.ExceededError](err); ok {
		a.recordRejection(exceeded.Dimension)
		failure := rateLimited(exceeded.Dimension, exceeded.RetryAfter, exceeded.Estimate)
		failure.Message = "The " + label + " cost budget was exhausted. Unpriced attempts accrue 0."
		failure.Param = &param
		return nil, failure
	}
	return nil, a.outage(keyID, true, err)
}

func projectBudgetSubject(project *string) string {
	if project == nil {
		return ""
	}
	return *project
}

// System work has no key/group/end-user spend but shares aggregate invoice caps.
func (s *Server) reserveSystemBudgets(ctx context.Context, x *execution) *Error {
	source, ok := s.Runtime.(interface {
		BudgetAuthority(*string) (access.Authority, error)
	})
	if !ok {
		return nil
	}
	authority, err := source.BudgetAuthority(x.route.ProjectID)
	if err != nil {
		return limitsUnavailable()
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
		authority = withoutCostBudgets(authority)
	}
	if callerCostBudgeted(&authority, x.limitRoute()) {
		if failure := s.Admission.checkCostAccounting(ctx); failure != nil {
			return failure
		}
	}
	var failure *Error
	x.lease, failure = s.Admission.reserveAggregateBudgets(ctx, &authority, time.Duration(x.route.OverallTimeout)*time.Millisecond, s.costReservation(x, authority))
	return failure
}

func (x *execution) admissionAuthority(authority access.Authority) access.Authority {
	if x != nil {
		authority.Attribution = x.attribution
		if x.callerCostExempt() {
			authority = withoutCostBudgets(authority)
		}
	}
	return authority
}

func budgetBoundaryError(e *Error, name, param string) *Error {
	e.Message = strings.Replace(e.Message, "API key", name, 1)
	e.Param = &param
	return e
}

// budgetBoundary retains only a fixed hierarchy label, never a request label/value,
// route name, owner identifier or provider error text.
func budgetBoundary(err *Error) string {
	if err == nil || err.Code != "budget_exhausted" {
		return ""
	}
	if err.Param != nil {
		switch *err.Param {
		case "budget.installation":
			return "installation"
		case "budget.organization":
			return "organization"
		case "budget.project":
			return "project"
		case "budget_group":
			return "budget_group"
		case "end_user.key":
			return "key_end_user"
		case "end_user.project":
			return "project_end_user"
		}
		if strings.HasPrefix(*err.Param, "route_limits.") {
			return "key_route"
		}
		if strings.HasPrefix(*err.Param, "attribution_budgets.") {
			return "attribution"
		}
	}
	return "api_key"
}
