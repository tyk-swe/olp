package kernel

import (
	"context"
	"net/http"
	"time"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/domain/protocols"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/gateway/guardstream"
	"github.com/tyk-swe/olp/internal/interaction"
	"github.com/tyk-swe/olp/internal/platform/telemetry"
	"github.com/tyk-swe/olp/internal/providerinvoke"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/upstream"
	"github.com/tyk-swe/olp/internal/usage"
)

type attemptEnd struct {
	class   string
	failure *AttemptFailure
}

func ended(class string, failure *AttemptFailure) *attemptEnd {
	return &attemptEnd{class: class, failure: failure}
}

type attemptRun struct {
	server   *Server
	ctx      context.Context
	x        *Call
	target   routing.Attempt
	provider *routing.Provider
	slot     routing.Slot
	fact     events.AttemptFact
	state    *attemptState
	traced   context.Context
	trace    *telemetry.AttemptTrace
	retained bool

	config   connectors.Config
	format   wire.Format
	contract *interaction.Plan
	reader   providerinvoke.ResponseReader
	codec    protocols.ResponseCodec
	body     []byte

	sending   context.Context
	cancel    context.CancelFunc
	firstByte *time.Timer
	watchdog  *time.Timer
	streamCap *time.Timer
	parallel  *parallelGuardrail
	guarded   *guardstream.Stream

	response      *http.Response
	completion    *wire.Completion
	committed     bool
	actionable    bool
	incomplete    bool
	deliveryClass string
}

// attempt performs one upstream call with one credential.
func (s *Server) attempt(ctx context.Context, x *Call, a routing.Attempt, provider *routing.Provider, slot routing.Slot, fact events.AttemptFact) (events.AttemptFact, *wire.Completion, *AttemptFailure) {
	run := s.newAttemptRun(ctx, x, a, provider, slot, fact)
	defer run.close()
	if end := run.exchange(); end != nil {
		return run.fail(end.class, end.failure)
	}
	return run.succeed()
}

func (s *Server) newAttemptRun(ctx context.Context, x *Call, a routing.Attempt, provider *routing.Provider, slot routing.Slot, fact events.AttemptFact) *attemptRun {
	if x.guard.engine != nil {
		x.guard.ctx = ctx
	}
	config := provider.Connector()
	run := &attemptRun{server: s, ctx: ctx, x: x, target: a, provider: provider, slot: slot, fact: fact, config: config}
	run.fact.Carried = config.CarriedByPlugin()
	resolved := x.resolvedGenerationResources()
	conversationBound := resolved["/conversation"] != "" || resolved["/conversation/id"] != ""
	run.retained = x.family == wire.Responses && x.providerState && (responseStoreRequested(x.generation.parsed) || conversationBound)
	// A plugin that carries the request reports only whether it was sent, so
	// the attempt must not risk repeating work it may have done.
	atMostOnce := fact.Interaction != nil || run.fact.Carried || run.retained
	run.state = &attemptState{parent: ctx, classifier: upstream.Classifier{ContextWindow: true, AtMostOnce: atMostOnce, Declared: config.Classification()}}
	run.traced, run.trace = x.request.trace.Attempt(ctx, string(provider.Kind), a.ProviderRevisionID, a.UpstreamModel)
	return run
}

func (run *attemptRun) exchange() *attemptEnd {
	if end := run.prepare(); end != nil {
		return end
	}
	if end := run.send(); end != nil {
		return end
	}
	end, err := run.receive()
	if end != nil {
		return end
	}
	return run.failed(run.inspect(err))
}

func (run *attemptRun) receive() (*attemptEnd, error) {
	if run.x.generation.parsed.Stream {
		return run.stream()
	}
	return nil, run.read()
}

func (run *attemptRun) close() {
	if run.watchdog != nil {
		run.watchdog.Stop()
	}
	if run.streamCap != nil {
		run.streamCap.Stop()
	}
	if run.response != nil {
		run.response.Body.Close()
	}
	if run.parallel != nil {
		run.parallel.cancel()
	}
	if run.firstByte != nil {
		run.firstByte.Stop()
	}
	if run.cancel != nil {
		run.cancel()
	}
}

func (run *attemptRun) finishTrace() {
	if run.trace == nil {
		return
	}
	if u := run.fact.Usage; u != nil {
		run.trace.RecordUsage(u.InputTokens, u.OutputTokens, u.CachedInputTokens, u.MediaUnits)
	}
	run.trace.RecordResponse(run.fact.ResponseModel, run.fact.FinishReasons)
	run.trace.Finish(run.fact.Class, run.fact.Status)
}

func (run *attemptRun) fail(reported string, failure *AttemptFailure) (events.AttemptFact, *wire.Completion, *AttemptFailure) {
	x, st := run.x, run.state
	f, class := failure, reported
	if f == nil {
		f = &AttemptFailure{}
	}
	if run.parallel != nil {
		if guardErr := run.parallel.join(x, true); guardErr != nil {
			class = classPolicy
			f.policyCode = guardrailError(guardErr).Code
			f.noRetry = true
			f.overall = true
		}
	}
	f.dispatched = st.dispatched.Load() || (x.workflow != nil && st.connected.Load() && !st.notSent.Load())
	f.credentialRefused = st.credentialRefused
	f.Acceptance = st.evidence().Acceptance()
	f.accepted = st.accepted
	if x.generation.continuation != nil && x.generation.continuation.resource != nil {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(run.ctx), 2*time.Second)
		_ = run.server.Deps.Resources.MarkUnknown(cleanup, x.generation.continuation.resource)
		cancel()
	}

	if run.fact.Interaction != nil || run.fact.Carried || (run.retained && f.Acceptance.Unresolved()) {
		f.noRetry = f.noRetry || f.dispatched
	}
	if run.fact.Interaction != nil {
		run.fact.Interaction.UpstreamState = f.Acceptance
	}
	f.class = class
	run.fact.Class = class
	if run.deliveryClass != "" {
		run.fact.Class = run.deliveryClass
	} else if run.fact.LocalRefusal && st.settled {
		run.fact.Class = ClassSuccess
	}
	run.fact.Committed = f.committed
	run.fact.Duration = run.server.Now().Sub(run.fact.StartedAt)
	if f.retryAfter > 0 {
		retry := f.retryAfter
		run.fact.RetryAfter = &retry
	}
	run.fact.RecordEvidence(f.billingUncertain())
	run.finishTrace()
	return run.fact, nil, f
}

func (run *attemptRun) succeed() (events.AttemptFact, *wire.Completion, *AttemptFailure) {
	run.fact.Class = ClassSuccess
	if run.fact.Interaction != nil {
		run.fact.Interaction.UpstreamState = upstream.Terminal
		if run.x.generation.parsed.Stream {
			run.fact.Interaction.ClientState = usage.ClientTerminal
		}
	}
	run.fact.Committed = run.committed
	run.fact.Duration = run.server.Now().Sub(run.fact.StartedAt)
	// Moderation intentionally has no usage counters. Other successful work
	// without usage may have been billed with nothing this gateway can meter.
	run.fact.RecordEvidence(run.x.operationName() != "moderation")
	run.finishTrace()
	return run.fact, run.completion, nil
}
