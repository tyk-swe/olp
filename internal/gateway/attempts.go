package gateway

import (
	"context"
	"errors"
	"net/http"

	"github.com/tyk-swe/olp/internal/runtime"
)

// attemptAdapter supplies the operation-specific estimate and one upstream
// attempt. Dispatch owns transport deadlines and delivery, including draining
// streams before returning their final fact. The loop owns admission, attempt
// numbering, settlement, health and retries.
type attemptAdapter[Result any] struct {
	estimate func(*runtime.Provider) int64
	dispatch func(context.Context, runtime.Attempt, *runtime.Provider, runtime.Slot, int) (AttemptFact, Result, *attemptFailure)
}

// attemptOutcome keeps the successful payload typed without coupling shared
// execution to the canonical or media response representation.
type attemptOutcome[Result any] struct {
	result    Result
	err       *Error
	committed bool
	cancelled bool
}

// runAttempts owns the bounded attempt loop against the request's pinned
// release. The caller supplies the overall deadline in ctx; request-wide key
// settlement and final downstream delivery continue after the loop returns.
func runAttempts[Result any](ctx context.Context, s *Server, x *execution, adapter attemptAdapter[Result]) attemptOutcome[Result] {
	deadline, _ := ctx.Deadline()
	snapshot := x.request.release.Snapshot
	used := 0
	unmeterable := false
	var last *attemptFailure
	for _, attempt := range x.attempts {
		if used >= x.budget || ctx.Err() != nil {
			break
		}
		provider, ok := snapshot.Providers[attempt.ProviderID]
		if !ok || s.health.open(provider.ID) {
			continue
		}
		next := false
		for _, slot := range s.slots(x, attempt, &provider) {
			if used >= x.budget || ctx.Err() != nil {
				break
			}
			gate := s.gateSlot(ctx, &provider, &slot, adapter.estimate(&provider), deadline)
			switch gate.verdict {
			case gateExpired:
				return attemptOutcome[Result]{err: (&attemptFailure{class: classTimeout}).toError()}
			case gateDenied:
				next = true
			case gateRejected:
				// A local quota refusal consumes an attempt but provides no
				// upstream evidence, so it never updates provider health.
				used++
				x.facts = append(x.facts, s.rejectedFact(x, attempt, slot, used, gate.rejection))
				last = gate.rejection
				next = gate.rejection.quota == quotaConnection // every sibling shares this quota
			case gateUnmeterable:
				unmeterable = true
			case gateAdmitted:
				used++
				fact, result, failure := adapter.dispatch(ctx, attempt, &provider, slot, used)
				// Only work handed to an upstream spends the request's key
				// reservation. Local failures remain refundable.
				dispatched := failure == nil || failure.dispatched
				x.dispatched = x.dispatched || dispatched
				gate.hold.settle(ctx, dispatched, totalTokens(fact.Usage))
				x.facts = append(x.facts, fact)
				// Recording the attempt also releases its half-open probe.
				s.health.record(provider.ID, fact)
				if failure == nil {
					return attemptOutcome[Result]{result: result, committed: fact.Committed}
				}
				next = s.cooldownFailure(ctx, provider.ID, &slot, failure)
				if failure.overall || !failoverAllowed(failure.class, failure.committed) {
					return attemptOutcome[Result]{err: failure.toError(), committed: failure.committed, cancelled: failure.class == classCancelled}
				}
				last = failure
			}
			if next {
				break
			}
		}
	}
	switch {
	case ctx.Err() != nil && errors.Is(context.Cause(ctx), context.DeadlineExceeded):
		return attemptOutcome[Result]{err: (&attemptFailure{class: classTimeout}).toError()}
	case ctx.Err() != nil:
		return attemptOutcome[Result]{err: (&attemptFailure{class: classCancelled}).toError(), cancelled: true}
	case last != nil:
		return attemptOutcome[Result]{err: last.toError()}
	case unmeterable:
		// Every eligible target had a quota that could not be consulted.
		return attemptOutcome[Result]{err: limitsUnavailable()}
	}
	return attemptOutcome[Result]{err: serverError(http.StatusServiceUnavailable, "upstream_unavailable", "No provider is currently able to serve `"+x.route.Slug+"`.")}
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
