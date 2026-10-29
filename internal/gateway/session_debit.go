package gateway

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage"
)

type SessionDebitLedger interface {
	Prepare(context.Context, limits.DebitIntent, []limits.DebitOwner) (limits.DebitState, error)
	Admit(context.Context, string, string, []limits.Request) error
	MarkDispatched(context.Context, string, string) error
	MarkUnknown(context.Context, string, string) error
	ReleaseUndispatched(context.Context, string, string) error
	HasUnknownExposure(context.Context, string, time.Duration) (bool, error)
	Currency(context.Context) (string, error)
}

type durableSessionDebits struct{ store *limits.DebitStore }

func DurableSessionDebits(store *limits.DebitStore) SessionDebitLedger {
	if store == nil || store.Pool == nil {
		return nil
	}
	return durableSessionDebits{store: store}
}

func (d durableSessionDebits) Prepare(ctx context.Context, intent limits.DebitIntent, owners []limits.DebitOwner) (limits.DebitState, error) {
	return d.store.Prepare(ctx, intent, owners)
}

func (d durableSessionDebits) Admit(ctx context.Context, id, claim string, requests []limits.Request) error {
	return d.store.Admit(ctx, id, claim, requests)
}

func (d durableSessionDebits) MarkDispatched(ctx context.Context, id, claim string) error {
	return d.store.MarkDispatched(ctx, id, claim)
}

func (d durableSessionDebits) MarkUnknown(ctx context.Context, id, claim string) error {
	return d.store.MarkUnknown(ctx, id, claim)
}

func (d durableSessionDebits) ReleaseUndispatched(ctx context.Context, id, claim string) error {
	return d.store.ReleaseUndispatched(ctx, id, claim)
}

func (d durableSessionDebits) MarkNotSent(ctx context.Context, id, claim string) error {
	return d.store.MarkNotSent(ctx, id, claim)
}

func (d durableSessionDebits) HasUnknownExposure(ctx context.Context, keyID string, maxDispatch time.Duration) (bool, error) {
	return d.store.HasUnknownDebit(ctx, keyID, maxDispatch)
}

func (d durableSessionDebits) Currency(ctx context.Context) (string, error) {
	var currency string
	err := d.store.Pool.QueryRow(ctx, `SELECT COALESCE((SELECT btrim(currency) FROM olp.pricing_currency WHERE singleton),'USD')`).Scan(&currency)
	return currency, err
}

type sessionDebit struct {
	id              string
	claim           string
	runID           string
	stepID          string
	targetID        string
	routeRevisionID string
	dispatched      bool
}

func (d *sessionDebit) reference() *usage.ExecutionReference {
	if d == nil {
		return nil
	}
	return &usage.ExecutionReference{DispatchID: d.id, RunID: d.runID, StepID: d.stepID, TargetID: d.targetID, RouteRevisionID: d.routeRevisionID}
}

func canonicalSessionID(id string) string {
	parsed, err := uuid.Parse(id)
	if err != nil || parsed == uuid.Nil || parsed.String() != id {
		return ""
	}
	return id
}

func newSessionDebitID() (string, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return "", err
	}
	return id.String(), nil
}

func (s *Server) sessionExposureHeld(ctx context.Context, keyID string) (bool, *Error) {
	if s.webrtcExposureHeld(keyID) {
		return true, nil
	}
	if s.Debits == nil {
		return false, nil
	}
	held, err := s.Debits.HasUnknownExposure(ctx, keyID, max(s.cfg.WebRTCBound, nativeDuplexSession))
	if err != nil {
		return false, serverError(http.StatusInternalServerError, "debit_ledger_unavailable", "The durable debit ledger is unavailable, so unresolved session exposure cannot be checked.")
	}
	return held, nil
}

func (s *Server) reserveSessionDebit(ctx context.Context, x *call, authority access.Authority, attempt routing.Attempt, supply []cappedOwner) *Error {
	if s.Debits == nil || x.callerCostExempt() && x.workflow == nil {
		return nil
	}
	snapshot := x.snapshot()
	if snapshot == nil || x.route == nil {
		return serverError(http.StatusInternalServerError, "debit_ledger_unavailable", "The session cannot be reserved without a pinned release.")
	}
	releaseID := canonicalSessionID(snapshot.Generation.ID)
	routeRevisionID := canonicalSessionID(x.route.RevisionID)
	if releaseID == "" {
		return serverError(http.StatusInternalServerError, "debit_ledger_unavailable", "The session cannot be reserved without a pinned release.")
	}
	revisionID := routeRevisionID
	if revisionID == "" {
		revisionID = releaseID
	}
	currency := ""
	if attempt.Price != nil {
		currency = attempt.Price.Currency
	}
	if currency == "" {
		resolved, err := s.Debits.Currency(ctx)
		if err != nil || resolved == "" {
			return limitsUnavailable()
		}
		currency = resolved
	}
	bound := x.attemptCostBound(attempt)
	charge := x.attemptChargeBound(attempt, bound)
	dispatchID, err := newSessionDebitID()
	if err != nil {
		return limitsUnavailable()
	}
	claimID, err := newSessionDebitID()
	if err != nil {
		return limitsUnavailable()
	}
	projectID := ""
	if authority.ProjectID != nil {
		projectID = *authority.ProjectID
	}
	debit := &sessionDebit{id: dispatchID, claim: claimID, targetID: canonicalSessionID(attempt.TargetID), routeRevisionID: routeRevisionID}
	intent := limits.DebitIntent{
		ID: dispatchID, APIKeyID: authority.ID, ProjectID: projectID, Kind: "api", ClaimID: claimID,
		Currency: currency, CostBound: bound.String(), ChargeBound: charge.String(),
		PreparedUntil: time.Now().Add(90 * time.Second),
		Affinity: limits.ExecutionAffinity{
			ReleaseID: releaseID, RevisionID: revisionID, RouteRevisionID: routeRevisionID,
			ProviderID: canonicalSessionID(attempt.ProviderID), ProviderRevisionID: canonicalSessionID(attempt.ProviderRevisionID),
			TargetID: debit.targetID,
		},
	}
	if x.workflow != nil {
		stepID, err := newSessionDebitID()
		if err != nil {
			return limitsUnavailable()
		}
		debit.runID, debit.stepID = x.workflow.runID, stepID
		intent.Kind, intent.RunID, intent.StepID = "model", debit.runID, debit.stepID
	}
	owners, requests, ok := sessionDebitBoundaries(x, authority)
	if !ok {
		return limitsUnavailable()
	}
	for _, owner := range supply {
		if owner.limits == nil {
			continue
		}
		owners = append(owners, limits.DebitOwner{ID: owner.id, Kind: "supply", Basis: limits.BasisCost})
		requests = append(requests, limits.Request{LookupID: owner.lookup, CostOwnerID: owner.id, CostBasis: limits.BasisCost, DailyCostLimit: owner.limits.DailyCostLimit, MonthlyCostLimit: owner.limits.MonthlyCostLimit, LeaseTTL: time.Hour})
	}
	var state limits.DebitState
	if x.workflow != nil {
		state, err = s.WorkflowRuns.Prepare(ctx, authority, intent, x.workflow.ordinal, owners)
	} else {
		state, err = s.Debits.Prepare(ctx, intent, owners)
	}
	if err != nil || state != limits.DebitPrepared {
		return limitsUnavailable()
	}
	if x.workflow != nil {
		x.workflow.ordinal++
	}
	if err := s.Debits.Admit(ctx, dispatchID, claimID, requests); err != nil {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), webrtcCloseTimeout)
		_ = s.Debits.ReleaseUndispatched(releaseCtx, dispatchID, claimID)
		cancel()
		if exceeded, ok := errors.AsType[*limits.ExceededError](err); ok {
			if s.Admission != nil {
				s.Admission.recordRejection(exceeded.Dimension)
			}
			return rateLimitedBasis(exceeded.Dimension, exceeded.RetryAfter, exceeded.Estimate, exceeded.Basis)
		}
		return limitsUnavailable()
	}
	x.sessionDebit = debit
	return nil
}

func sessionDebitBoundaries(x *call, authority access.Authority) ([]limits.DebitOwner, []limits.Request, bool) {
	admission := x.admissionAuthority(authority)
	owners := []limits.DebitOwner{{ID: admission.ID, Kind: "key", Basis: limits.Basis(admission.Policy.CostBasis)}}
	key := keyRequest(admission, x.estimate, time.Hour)
	key.CostIncreases = admission.BudgetIncreases[admission.ID]
	requests := []limits.Request{key}
	add := func(owner limits.DebitOwner, request limits.Request) {
		owners = append(owners, owner)
		requests = append(requests, request)
	}
	if admission.BudgetGroupID != nil && admission.GroupLimits().CostBudgeted() {
		if group := groupRequest(&admission, time.Hour); group != nil {
			group.CostIncreases = admission.BudgetIncreases[group.CostOwnerID]
			group.RequestedTokens = x.estimate
			add(limits.DebitOwner{ID: group.CostOwnerID, Kind: "group", Basis: group.CostBasis}, *group)
		}
	}
	if route := x.limitRoute(); route != "" {
		if policy := admission.Policy.RouteLimits[route]; policy.CostBudgeted() {
			owner := limits.KeyRouteBudgetID(admission.ID, route)
			add(limits.DebitOwner{ID: owner, Kind: "aggregate", Basis: limits.Basis(policy.CostBasis)}, limits.Request{
				CostOwnerID: owner, CostIncreases: admission.BudgetIncreases[owner], LookupID: limits.AggregateBudgetLookup(owner),
				RequestsPerMinute: policy.RequestsPerMinute, TokensPerMinute: policy.TokensPerMinute, MaxConcurrency: policy.MaxConcurrency,
				DailyCostLimit: policy.DailyCostLimit, MonthlyCostLimit: policy.MonthlyCostLimit, WeeklyCostLimit: policy.WeeklyCostLimit,
				CostBasis: limits.Basis(policy.CostBasis), RequestedTokens: x.estimate, LeaseTTL: time.Hour,
			})
		}
	}
	for _, boundary := range [...]struct {
		level, subject string
		policy         *access.BudgetPolicy
	}{
		{"installation", admission.InstallationID, admission.InstallationBudget},
		{"organization", projectBudgetSubject(admission.OrganizationID), admission.OrganizationBudget},
		{"project", projectBudgetSubject(admission.ProjectID), admission.ProjectBudget},
	} {
		if !boundary.policy.Limited() {
			continue
		}
		if boundary.subject == "" {
			return nil, nil, false
		}
		owner := limits.AggregateBudgetID(boundary.level, boundary.subject)
		add(limits.DebitOwner{ID: owner, Kind: "aggregate", Basis: boundary.policy.Basis()}, limits.Request{
			CostOwnerID: owner, CostIncreases: admission.BudgetIncreases[owner], LookupID: limits.AggregateBudgetLookup(owner),
			DailyCostLimit: boundary.policy.DailyCostLimit, WeeklyCostLimit: boundary.policy.WeeklyCostLimit, MonthlyCostLimit: boundary.policy.MonthlyCostLimit,
			LeaseTTL: time.Hour, CostBasis: boundary.policy.Basis(),
		})
	}
	for _, key := range slices.Sorted(maps.Keys(admission.Attribution)) {
		value := admission.Attribution[key]
		policy, ok := admission.ProjectAttributionBudgets[key][value]
		if !ok || !policy.Limited() {
			continue
		}
		if admission.ProjectID == nil {
			return nil, nil, false
		}
		owner := limits.AttributionBudgetID(*admission.ProjectID, key, value)
		add(limits.DebitOwner{ID: owner, Kind: "aggregate", Basis: policy.Basis()}, limits.Request{
			CostOwnerID: owner, CostIncreases: admission.BudgetIncreases[owner], LookupID: limits.AggregateBudgetLookup(owner),
			DailyCostLimit: policy.DailyCostLimit, WeeklyCostLimit: policy.WeeklyCostLimit, MonthlyCostLimit: policy.MonthlyCostLimit,
			LeaseTTL: time.Hour, CostBasis: policy.Basis(),
		})
	}
	if admission.EndUserDigest != "" {
		for _, boundary := range [...]struct {
			policy  *access.EndUserPolicy
			project bool
		}{
			{admission.ProjectEndUserPolicy, true}, {admission.Policy.EndUserPolicy, false},
		} {
			policy := boundary.policy.Limits(admission.EndUserDigest)
			if !policy.CostBudgeted() {
				continue
			}
			owner := limits.EndUserKeyID(admission.ID, admission.EndUserDigest)
			if boundary.project {
				if admission.ProjectID == nil {
					return nil, nil, false
				}
				owner = limits.EndUserProjectID(*admission.ProjectID, admission.EndUserDigest)
			}
			add(limits.DebitOwner{ID: owner, Kind: "end_user", Basis: limits.Basis(policy.CostBasis)}, limits.Request{
				CostOwnerID: owner, CostIncreases: admission.BudgetIncreases[owner], LookupID: limits.EndUserLookup(owner),
				CostBasis:         limits.Basis(policy.CostBasis),
				RequestsPerMinute: policy.RequestsPerMinute, TokensPerMinute: policy.TokensPerMinute,
				MaxConcurrency: policy.MaxConcurrency, DailyCostLimit: policy.DailyCostLimit, MonthlyCostLimit: policy.MonthlyCostLimit, WeeklyCostLimit: policy.WeeklyCostLimit,
				RequestedTokens: x.estimate, LeaseTTL: time.Hour,
			})
		}
	}
	return owners, requests, true
}

func (s *Server) markSessionDispatched(ctx context.Context, x *call) *Error {
	if x.sessionDebit == nil || s.Debits == nil {
		return nil
	}
	if err := s.Debits.MarkDispatched(ctx, x.sessionDebit.id, x.sessionDebit.claim); err != nil {
		return serverError(http.StatusInternalServerError, "debit_ledger_unavailable", "The durable debit could not be dispatched, so the provider session was not created.")
	}
	x.sessionDebit.dispatched = true
	return nil
}

func (s *Server) abandonSessionDebit(ctx context.Context, x *call) {
	if x.sessionDebit == nil || x.sessionDebit.dispatched || s.Debits == nil {
		return
	}
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), webrtcCloseTimeout)
	if err := s.Debits.ReleaseUndispatched(releaseCtx, x.sessionDebit.id, x.sessionDebit.claim); err != nil && s.log != nil {
		s.log.Warn("session debit release failed", "error", err.Error())
	}
	cancel()
	for i := range x.facts {
		x.facts[i].Execution = nil
	}
	x.sessionDebit = nil
}

func (s *Server) markSessionUnknown(ctx context.Context, x *call) {
	if x.sessionDebit == nil || !x.sessionDebit.dispatched || s.Debits == nil {
		return
	}
	markCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), webrtcCloseTimeout)
	if err := s.Debits.MarkUnknown(markCtx, x.sessionDebit.id, x.sessionDebit.claim); err != nil && s.log != nil {
		s.log.Warn("session debit exposure could not be marked unknown", "error", err.Error())
	}
	cancel()
}

func (s *Server) finishSessionDebit(ctx context.Context, x *call, failed bool) {
	if x.sessionDebit == nil {
		return
	}
	if !x.sessionDebit.dispatched {
		s.abandonSessionDebit(ctx, x)
		return
	}
	if failed {
		s.markSessionUnknown(ctx, x)
	}
}
