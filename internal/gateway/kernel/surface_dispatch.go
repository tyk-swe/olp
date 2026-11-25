package kernel

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	"github.com/tyk-swe/olp/internal/routing"
)

type SingleTarget struct {
	Attempt  routing.Attempt
	Provider *routing.Provider
	Slots    []routing.Slot
}

type SingleSlotHold struct {
	hold *dispatchHold
	Slot routing.Slot
}

func (s *Server) FirstPlannedTarget(x *Call) (SingleTarget, *fault.Error) {
	attempt := x.attempts[0]
	provider, ok := x.request.release.Snapshot().FindProvider(attempt.ProviderID)
	if !ok {
		return SingleTarget{}, fault.ServerError(http.StatusServiceUnavailable, "upstream_unavailable", "No provider is currently able to serve `"+x.Route.Slug+"`.")
	}
	if e := x.checkCallerTarget(&provider, attempt.UpstreamModel); e != nil {
		return SingleTarget{}, e
	}
	slots := s.slots(x, attempt, &provider)
	if len(slots) == 0 {
		return SingleTarget{}, fault.ServerError(http.StatusServiceUnavailable, "upstream_unavailable", "No provider credential is currently able to serve `"+x.Route.Slug+"`.")
	}
	return SingleTarget{Attempt: attempt, Provider: &provider, Slots: slots}, nil
}

func (s *Server) AdmitSingleTarget(ctx context.Context, x *Call, target SingleTarget, demand quota.TokenDemand, deadline time.Time) (*SingleSlotHold, *fault.Error) {
	used := 0
	unmeterable := false
	var last *AttemptFailure
	for _, slot := range target.Slots {
		if used >= x.budget || ctx.Err() != nil {
			break
		}
		gate := s.gateAttempt(ctx, x, target.Attempt, target.Provider, &slot, demand, deadline)
		switch gate.verdict {
		case gateExpired:
			return nil, (&AttemptFailure{class: ClassTimeout}).Fault()
		case gateDenied:
			return nil, s.singleAdmissionError(ctx, x, last, unmeterable)
		case gateRejected:
			used++
			x.facts = append(x.facts, s.rejectedFact(x, target.Attempt, slot, used, gate.rejection))
			last = gate.rejection
			if gate.rejection.quota == quota.Connection || gate.rejection.quota == quota.Route {
				return nil, s.singleAdmissionError(ctx, x, last, unmeterable)
			}
		case gateUnmeterable:
			unmeterable = true
			if gate.quota == quota.Connection || gate.quota == quota.Route {
				return nil, s.singleAdmissionError(ctx, x, last, unmeterable)
			}
		case gateAdmitted:
			return &SingleSlotHold{hold: gate.hold, Slot: slot}, nil
		}
	}
	return nil, s.singleAdmissionError(ctx, x, last, unmeterable)
}

func (s *Server) singleAdmissionError(ctx context.Context, x *Call, last *AttemptFailure, unmeterable bool) *fault.Error {
	switch {
	case ctx.Err() != nil && errors.Is(context.Cause(ctx), context.DeadlineExceeded):
		return (&AttemptFailure{class: ClassTimeout}).Fault()
	case ctx.Err() != nil:
		return (&AttemptFailure{class: ClassCancelled}).Fault()
	case last != nil:
		return last.Fault()
	case unmeterable:
		return quota.LimitsUnavailable()
	}
	return fault.ServerError(http.StatusServiceUnavailable, "upstream_unavailable", "No provider credential is currently able to serve `"+x.Route.Slug+"`.")
}

func (s *Server) SettleSingleHold(ctx context.Context, x *Call, hold *SingleSlotHold) {
	if hold == nil || hold.hold == nil {
		return
	}
	hold.hold.settle(ctx, x.dispatched, totalUsage(x.settledTokens()))
}

func (s *Server) ReleaseSingleHold(ctx context.Context, hold *SingleSlotHold) {
	if hold == nil {
		return
	}
	s.ReleaseHold(ctx, hold.hold)
}

func (s *Server) NewSingleAttemptFact(x *Call, attempt routing.Attempt, hold *SingleSlotHold) events.AttemptFact {
	planned := s.newFact(x, attempt, hold.Slot, len(x.facts)+1)
	planned.Budgets = hold.hold.budgets
	return planned
}

func (s *Server) CompleteSingleAttempt(x *Call, hold *SingleSlotHold, attempt routing.Attempt, providerID string, fact events.AttemptFact, failure *AttemptFailure) {
	x.dispatched = failure == nil || failure.dispatched
	if x.dispatched {
		x.spendCaps(hold.hold.budgets, attempt)
	}
	x.facts = append(x.facts, fact)
	s.health.Record(providerID, fact, hold.hold.probed)
}

func (s *Server) RecordProviderHealth(providerID string, fact events.AttemptFact, probed bool) {
	s.health.Record(providerID, fact, probed)
}

func RateLimitedTarget(refusal *quota.TargetRefusal) *AttemptFailure {
	return rateLimitedTarget(refusal)
}
