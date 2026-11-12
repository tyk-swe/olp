package gateway

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/http"
	"slices"
	"time"

	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	"github.com/tyk-swe/olp/internal/routing"
)

type attemptAdapter[Result any] struct {
	estimate func(routing.Attempt, *routing.Provider) int64
	dispatch func(context.Context, routing.Attempt, *routing.Provider, routing.Slot, events.AttemptFact) (events.AttemptFact, Result, *attemptFailure)
}

func dispatchWithLease[Result any](ctx context.Context, hold *dispatchHold, adapter attemptAdapter[Result], s *Server, x *call, attempt routing.Attempt, provider *routing.Provider, slot routing.Slot, fact events.AttemptFact) (events.AttemptFact, Result, *attemptFailure, *fault.Error) {
	dispatchCtx, finish, err := hold.authorize(ctx, s, x)
	if err != nil {
		var result Result
		return fact, result, &attemptFailure{class: classLimitsUnavailable, noRetry: true}, err
	}
	completed, result, failure := adapter.dispatch(dispatchCtx, attempt, provider, slot, fact)
	return completed, result, failure, finish()
}

// attemptOutcome keeps the successful payload typed without coupling shared
// execution to the canonical or media response representation.
type attemptOutcome[Result any] struct {
	result    Result
	err       *fault.Error
	committed bool
	cancelled bool
}

// runAttempts owns the bounded attempt loop against the request's pinned
// release. The caller supplies the overall deadline in ctx; request-wide key
// settlement and final downstream delivery continue after the loop returns.
// When a route's attempts fail for a condition one of its fallbacks names,
// the loop continues on the fallback route within the same deadline and
// attempt budget.
func runAttempts[Result any](ctx context.Context, s *Server, x *call, adapter attemptAdapter[Result]) attemptOutcome[Result] {
	if x.guardrailEngine != nil {
		x.guardrailCtx = ctx
	}
	var generation string
	if x.request.release != nil && x.request.release.Snapshot() != nil {
		generation = x.request.release.Snapshot().Generation().ID
	}
	x.request.trace.RecordSessionContext(x.surfaceName(), x.operationName(), string(x.family), x.named().Slug, generation)
	for {
		out, conditions := runRoute(ctx, s, x, adapter)
		if len(conditions) == 0 || !s.fallBack(ctx, x, conditions) {
			if x.costExcluded && out.err != nil && out.err.Status >= 500 {
				return attemptOutcome[Result]{err: costLimitExceeded()}
			}
			return out
		}
		if e := s.reserveFallbackCost(ctx, x); e != nil {
			return attemptOutcome[Result]{err: e}
		}
	}
}

// runRoute tries the attempts planned for the request's current route. When
// they all fail before anything is committed, it also returns the fallback
// conditions the route met.
func runRoute[Result any](ctx context.Context, s *Server, x *call, adapter attemptAdapter[Result]) (attemptOutcome[Result], []string) {
	deadline, _ := ctx.Deadline()
	snapshot := x.snapshot()
	unmeterable := false
	var last *attemptFailure
	var met []string
route:
	for _, attempt := range x.attempts {
		if x.spent >= x.budget || ctx.Err() != nil {
			break
		}
		provider, ok := snapshot.FindProvider(attempt.ProviderID)
		if !ok || s.health.Open(provider.ID) {
			continue
		}
		if e := x.checkCallerTarget(&provider, attempt.UpstreamModel); e != nil {
			return attemptOutcome[Result]{err: e}, nil
		}
		next := false
		slots := s.slots(x, attempt, &provider)
		// retry counts the repeats of slot retried; any other slot starts
		// afresh.
		retry, retried := 0, -1
		for i := 0; i < len(slots) && !next; i++ {
			slot := slots[i]
			if i != retried {
				retry = 0
			}
			if x.spent >= x.budget || ctx.Err() != nil {
				break
			}
			if !x.servingAllowed(&provider, attempt.TargetID, attempt.UpstreamModel, slot, false) {
				continue
			}
			estimate := adapter.estimate(attempt, &provider)
			gate := s.gateAttempt(ctx, x, attempt, &provider, &slot, estimate, deadline)
			switch gate.verdict {
			case gateExpired:
				return attemptOutcome[Result]{err: (&attemptFailure{class: classTimeout}).toError()}, nil
			case gateDenied:
				next = true
			case gateRejected:
				// A local quota refusal consumes an attempt but provides no
				// upstream evidence, so it never updates provider health.
				x.spent++
				x.facts = append(x.facts, s.rejectedFact(x, attempt, slot, x.spent, gate.rejection))
				last = gate.rejection
				met = meet(met, gate.rejection.class)
				if gate.rejection.quota == quota.Route {
					break route
				}
				next = gate.rejection.quota == quota.Connection // every sibling shares this quota
			case gateUnmeterable:
				unmeterable = true
				if gate.quota == quota.Route {
					break route
				}
				next = gate.quota == quota.Connection
			case gateAdmitted:
				ready, admissionErr := gate.hold.begin(ctx, s, x, attempt, &provider, slot)
				if admissionErr != nil {
					return attemptOutcome[Result]{err: admissionErr}, nil
				}
				if !ready {
					continue
				}
				x.spent++
				x.preparation().activate(attempt)
				x.grantGeneration = 0
				fact := s.newFact(x, attempt, slot, x.spent)
				// Streams can persist accounting before dispatch returns.
				fact.Retry, fact.Budgets = retry, gate.hold.budgets
				if x.guardrailEngine != nil {
					x.startGuardrailAttempt(fact.GuardrailDecisions)
				}
				dispatchCtx, finishLease := gate.hold.dispatchContext(ctx, s, x)
				fact, result, failure, authorityErr := dispatchWithLease(dispatchCtx, gate.hold, adapter, s, x, attempt, &provider, slot, fact)
				if x.guardrailEngine != nil {
					fact.GuardrailDecisions = x.finishGuardrailAttempt()
				}
				if completionErr, cancelled := gate.hold.complete(ctx, s, x, attemptCompletion{fact: fact, failure: failure, authority: authorityErr, attempt: attempt, estimate: estimate, finishLease: finishLease}); completionErr != nil {
					return attemptOutcome[Result]{err: completionErr, cancelled: cancelled}, nil
				}
				if failure == nil {
					return attemptOutcome[Result]{result: result, committed: fact.Committed}, nil
				}
				if failure.overall || !failoverAllowed(failure.class, failure.committed) {
					s.cooldownFailure(ctx, provider.ID, &slot, x.grantGeneration, failure, fact.CredentialSource == "caller")
					out := attemptOutcome[Result]{err: failure.toError(), committed: failure.committed, cancelled: failure.class == classCancelled}
					if failure.class == classContentFilter && !failure.committed {
						// A content filter refuses the request on every target
						// of the route; only a fallback route can serve it.
						return out, []string{routing.FallbackContentFilter}
					}
					return out, nil
				}
				if wait, ok := s.retryWait(x, provider.ID, failure, retry, deadline); ok {
					// The slot is retried before it cools down, so the
					// failure is recorded against it only once.
					if !pause(ctx, wait) {
						break
					}
					retry, retried = retry+1, i
					i-- // the same slot again
					continue
				}
				next = s.cooldownFailure(ctx, provider.ID, &slot, x.grantGeneration, failure, fact.CredentialSource == "caller")
				last = failure
				met = meet(met, failure.class)
			}
		}
	}
	switch {
	case ctx.Err() != nil && errors.Is(context.Cause(ctx), context.DeadlineExceeded):
		return attemptOutcome[Result]{err: (&attemptFailure{class: classTimeout}).toError()}, nil
	case ctx.Err() != nil:
		return attemptOutcome[Result]{err: (&attemptFailure{class: classCancelled}).toError(), cancelled: true}, nil
	}
	// Every attempt the route could make failed in a way another target, or
	// another route, might not.
	met = append(met, routing.FallbackExhausted)
	switch {
	case last != nil:
		return attemptOutcome[Result]{err: last.toError()}, met
	case unmeterable:
		// Every eligible target had a quota that could not be consulted.
		return attemptOutcome[Result]{err: quota.LimitsUnavailable()}, met
	}
	return attemptOutcome[Result]{err: fault.ServerError(http.StatusServiceUnavailable, "upstream_unavailable", "No provider is currently able to serve `"+x.named().Slug+"`.")}, met
}

// meet adds the fallback condition a failure class meets to met.
func meet(met []string, class string) []string {
	if condition := fallbackCondition(class); condition != "" && !slices.Contains(met, condition) {
		return append(met, condition)
	}
	return met
}

// retryWait reports how long to wait before retrying a failed attempt on the
// same slot, when the route's retry policy allows another try within the
// request's attempt budget and deadline. An open circuit ends retries.
func (s *Server) retryWait(x *call, providerID string, failure *attemptFailure, retries int, deadline time.Time) (time.Duration, bool) {
	rule, ok := x.route.Retry.Retries(failure.class)
	if !ok || retries >= rule.MaxRetries || x.spent >= x.budget || s.health.Open(providerID) {
		return 0, false
	}
	wait := rule.Backoff(retries+1, rand.Float64(), failure.retryAfter)
	if !deadline.IsZero() && s.now().Add(wait).After(deadline) {
		return 0, false
	}
	return wait, true
}

// pause waits for d unless ctx ends first, reporting whether it waited.
func pause(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// failoverAllowed reports whether a failure class may select another
// attempt when nothing has been committed to the client. Ambiguous work is
// terminal even without a committed response.
func failoverAllowed(class string, committed bool) bool {
	if committed {
		return false
	}
	switch class {
	case classConnect, classTimeout, classRateLimit, classUpstreamServer, classCredential, classContextWindow:
		return true
	}
	return false
}
