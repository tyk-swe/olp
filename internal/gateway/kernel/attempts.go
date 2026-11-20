package kernel

import (
	"cmp"
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
	dispatch func(context.Context, routing.Attempt, *routing.Provider, routing.Slot, events.AttemptFact) (events.AttemptFact, Result, *AttemptFailure)
}

func dispatchWithLease[Result any](ctx context.Context, hold *dispatchHold, adapter attemptAdapter[Result], s *Server, x *Call, attempt routing.Attempt, provider *routing.Provider, slot routing.Slot, fact events.AttemptFact) (events.AttemptFact, Result, *AttemptFailure, *fault.Error) {
	dispatchCtx, finish, err := hold.authorize(ctx, s, x)
	if err != nil {
		var result Result
		return fact, result, &AttemptFailure{class: classLimitsUnavailable, noRetry: true}, err
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
func runAttempts[Result any](ctx context.Context, s *Server, x *Call, adapter attemptAdapter[Result]) attemptOutcome[Result] {
	if x.guard.engine != nil {
		x.guard.ctx = ctx
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
		if e := s.ReserveFallbackCost(ctx, x); e != nil {
			return attemptOutcome[Result]{err: e}
		}
	}
}

// runRoute tries the attempts planned for the request's current route. When
// they all fail before anything is committed, it also returns the fallback
// conditions the route met.
func runRoute[Result any](ctx context.Context, s *Server, x *Call, adapter attemptAdapter[Result]) (attemptOutcome[Result], []string) {
	deadline, _ := ctx.Deadline()
	run := routeRun[Result]{ctx: ctx, server: s, call: x, adapter: adapter, deadline: deadline}
	snapshot := x.Snapshot()
	var seen routeEvidence
	for _, attempt := range x.attempts {
		if run.over() {
			break
		}
		provider, ok := snapshot.FindProvider(attempt.ProviderID)
		if !ok || s.health.Open(provider.ID) {
			continue
		}
		if e := x.checkCallerTarget(&provider, attempt.UpstreamModel); e != nil {
			return attemptOutcome[Result]{err: e}, nil
		}
		step := run.target(attempt, &provider)
		if step.then == finished {
			return step.out, step.conditions
		}
		seen = seen.and(step.seen)
		if step.then == leaveRoute {
			break
		}
	}
	return run.exhausted(seen)
}

type routeRun[Result any] struct {
	ctx      context.Context
	server   *Server
	call     *Call
	adapter  attemptAdapter[Result]
	deadline time.Time
}

// routeEvidence is what failed attempts leave behind for the answer a route
// gives when none of them served.
type routeEvidence struct {
	last        *AttemptFailure
	unmeterable bool
	met         []string
}

func (e routeEvidence) and(later routeEvidence) routeEvidence {
	met := slices.Clone(e.met)
	for _, condition := range later.met {
		if !slices.Contains(met, condition) {
			met = append(met, condition)
		}
	}
	return routeEvidence{last: cmp.Or(later.last, e.last), unmeterable: e.unmeterable || later.unmeterable, met: met}
}

func refusedBy(failure *AttemptFailure) routeEvidence {
	if condition := fallbackCondition(failure.class); condition != "" {
		return routeEvidence{last: failure, met: []string{condition}}
	}
	return routeEvidence{last: failure}
}

type routeFlow int

const (
	tryNextSlot routeFlow = iota
	retrySlot
	tryNextTarget
	leaveRoute
	finished
)

type routeStep[Result any] struct {
	then       routeFlow
	out        attemptOutcome[Result]
	conditions []string
	seen       routeEvidence
}

func (r routeRun[Result]) over() bool {
	return r.call.spent >= r.call.budget || r.ctx.Err() != nil
}

func (r routeRun[Result]) finish(out attemptOutcome[Result], conditions ...string) routeStep[Result] {
	return routeStep[Result]{then: finished, out: out, conditions: conditions}
}

func (r routeRun[Result]) target(attempt routing.Attempt, provider *routing.Provider) routeStep[Result] {
	slots := r.server.slots(r.call, attempt, provider)
	retry := 0
	var seen routeEvidence
	for i := 0; i < len(slots) && !r.over(); {
		if !r.call.servingAllowed(provider, attempt.TargetID, attempt.UpstreamModel, slots[i], false) {
			i, retry = i+1, 0
			continue
		}
		step := r.slot(attempt, provider, slots[i], retry)
		seen = seen.and(step.seen)
		switch step.then {
		case retrySlot:
			retry++
		case tryNextSlot:
			i, retry = i+1, 0
		case finished:
			return step
		default:
			return routeStep[Result]{then: step.then, seen: seen}
		}
	}
	return routeStep[Result]{then: tryNextTarget, seen: seen}
}

func (r routeRun[Result]) slot(attempt routing.Attempt, provider *routing.Provider, slot routing.Slot, retry int) routeStep[Result] {
	estimate := r.adapter.estimate(attempt, provider)
	gate := r.server.gateAttempt(r.ctx, r.call, attempt, provider, &slot, estimate, r.deadline)
	switch gate.verdict {
	case gateExpired:
		return r.finish(attemptOutcome[Result]{err: (&AttemptFailure{class: ClassTimeout}).Fault()})
	case gateDenied:
		return routeStep[Result]{then: tryNextTarget}
	case gateRejected:
		return r.rejected(attempt, slot, gate.rejection)
	case gateUnmeterable:
		return routeStep[Result]{then: afterQuota(gate.quota), seen: routeEvidence{unmeterable: true}}
	case gateAdmitted:
		return r.dispatch(attempt, provider, slot, retry, estimate, gate.hold)
	}
	return routeStep[Result]{then: tryNextSlot}
}

func afterQuota(scope string) routeFlow {
	switch scope {
	case quota.Route:
		return leaveRoute
	case quota.Connection:
		return tryNextTarget
	}
	return tryNextSlot
}

// rejected records a local quota refusal. It consumes an attempt but provides
// no upstream evidence, so it never updates provider health.
func (r routeRun[Result]) rejected(attempt routing.Attempt, slot routing.Slot, rejection *AttemptFailure) routeStep[Result] {
	x := r.call
	x.spent++
	x.facts = append(x.facts, r.server.rejectedFact(x, attempt, slot, x.spent, rejection))
	return routeStep[Result]{then: afterQuota(rejection.quota), seen: refusedBy(rejection)}
}

func (r routeRun[Result]) dispatch(attempt routing.Attempt, provider *routing.Provider, slot routing.Slot, retry int, estimate int64, hold *dispatchHold) routeStep[Result] {
	s, x := r.server, r.call
	ready, admissionErr := hold.begin(r.ctx, s, x, attempt, provider, slot)
	if admissionErr != nil {
		return r.finish(attemptOutcome[Result]{err: admissionErr})
	}
	if !ready {
		return routeStep[Result]{then: tryNextSlot}
	}
	x.spent++
	x.preparation().activate(attempt)
	x.grantGeneration = 0
	fact := s.newFact(x, attempt, slot, x.spent)
	// Streams can persist accounting before dispatch returns.
	fact.Retry, fact.Budgets = retry, hold.budgets
	if x.guard.engine != nil {
		x.guard.ledger.startAttempt(fact.GuardrailDecisions)
	}
	dispatchCtx, finishLease := hold.dispatchContext(r.ctx, s, x)
	fact, result, failure, authorityErr := dispatchWithLease(dispatchCtx, hold, r.adapter, s, x, attempt, provider, slot, fact)
	if x.guard.engine != nil {
		fact.GuardrailDecisions = x.guard.ledger.finishAttempt()
	}
	if completionErr, cancelled := hold.complete(r.ctx, s, x, attemptCompletion{fact: fact, failure: failure, authority: authorityErr, attempt: attempt, estimate: estimate, finishLease: finishLease}); completionErr != nil {
		return r.finish(attemptOutcome[Result]{err: completionErr, cancelled: cancelled})
	}
	if failure == nil {
		return r.finish(attemptOutcome[Result]{result: result, committed: fact.Committed})
	}
	return r.failed(provider, slot, retry, failure, fact.CredentialSource == "caller")
}

func (r routeRun[Result]) failed(provider *routing.Provider, slot routing.Slot, retry int, failure *AttemptFailure, callerCredential bool) routeStep[Result] {
	s, x := r.server, r.call
	if failure.overall || !failoverAllowed(failure.class, failure.committed) {
		s.cooldownFailure(r.ctx, provider.ID, &slot, x.grantGeneration, failure, callerCredential)
		out := attemptOutcome[Result]{err: failure.Fault(), committed: failure.committed, cancelled: failure.class == ClassCancelled}
		if failure.class == classContentFilter && !failure.committed {
			// A content filter refuses the request on every target of the
			// route; only a fallback route can serve it.
			return r.finish(out, routing.FallbackContentFilter)
		}
		return r.finish(out)
	}
	if wait, ok := s.retryWait(x, provider.ID, failure, retry, r.deadline); ok {
		// The slot is retried before it cools down, so the failure is
		// recorded against it only once.
		if !pause(r.ctx, wait) {
			return routeStep[Result]{then: leaveRoute}
		}
		return routeStep[Result]{then: retrySlot}
	}
	if s.cooldownFailure(r.ctx, provider.ID, &slot, x.grantGeneration, failure, callerCredential) {
		return routeStep[Result]{then: tryNextTarget, seen: refusedBy(failure)}
	}
	return routeStep[Result]{then: tryNextSlot, seen: refusedBy(failure)}
}

func (r routeRun[Result]) exhausted(seen routeEvidence) (attemptOutcome[Result], []string) {
	switch {
	case r.ctx.Err() != nil && errors.Is(context.Cause(r.ctx), context.DeadlineExceeded):
		return attemptOutcome[Result]{err: (&AttemptFailure{class: ClassTimeout}).Fault()}, nil
	case r.ctx.Err() != nil:
		return attemptOutcome[Result]{err: (&AttemptFailure{class: ClassCancelled}).Fault(), cancelled: true}, nil
	}
	// Every attempt the route could make failed in a way another target, or
	// another route, might not.
	met := append(slices.Clone(seen.met), routing.FallbackExhausted)
	switch {
	case seen.last != nil:
		return attemptOutcome[Result]{err: seen.last.Fault()}, met
	case seen.unmeterable:
		// Every eligible target had a quota that could not be consulted.
		return attemptOutcome[Result]{err: quota.LimitsUnavailable()}, met
	}
	return attemptOutcome[Result]{err: fault.ServerError(http.StatusServiceUnavailable, "upstream_unavailable", "No provider is currently able to serve `"+r.call.named().Slug+"`.")}, met
}

// retryWait reports how long to wait before retrying a failed attempt on the
// same slot, when the route's retry policy allows another try within the
// request's attempt budget and deadline. An open circuit ends retries.
func (s *Server) retryWait(x *Call, providerID string, failure *AttemptFailure, retries int, deadline time.Time) (time.Duration, bool) {
	rule, ok := x.Route.Retry.Retries(failure.class)
	if !ok || retries >= rule.MaxRetries || x.spent >= x.budget || s.health.Open(providerID) {
		return 0, false
	}
	wait := rule.Backoff(retries+1, rand.Float64(), failure.retryAfter)
	if !deadline.IsZero() && s.Now().Add(wait).After(deadline) {
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
	case ClassConnect, ClassTimeout, classRateLimit, ClassUpstreamServer, classCredential, classContextWindow:
		return true
	}
	return false
}
