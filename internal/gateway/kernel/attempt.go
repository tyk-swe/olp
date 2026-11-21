package kernel

import (
	"context"
	"net/http"
	"time"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/domain/protocols"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/interaction"
	"github.com/tyk-swe/olp/internal/platform/telemetry"
	"github.com/tyk-swe/olp/internal/providerinvoke"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/upstream"
	"github.com/tyk-swe/olp/internal/usage"
)

type attemptEnd struct {
	class     string
	failure   *AttemptFailure
	delivered string
}

func ended(class string, failure *AttemptFailure) *attemptEnd {
	return &attemptEnd{class: class, failure: failure}
}

type attemptRun struct {
	server    *Server
	ctx       context.Context
	x         *Call
	target    routing.Attempt
	provider  *routing.Provider
	slot      routing.Slot
	connector connectors.Config
	state     *attemptState
	traced    context.Context
	trace     *telemetry.AttemptTrace
	retained  bool
}

type attemptPlan struct {
	config    connectors.Config
	format    wire.Format
	contract  *interaction.Plan
	reader    providerinvoke.ResponseReader
	codec     protocols.ResponseCodec
	body      []byte
	sending   context.Context
	cancel    context.CancelFunc
	firstByte *time.Timer
}

func (plan attemptPlan) release() {
	plan.firstByte.Stop()
	plan.cancel()
}

type attemptSent struct {
	plan     attemptPlan
	config   connectors.Config
	fact     events.AttemptFact
	response *http.Response
	parallel *parallelGuardrail
}

func (sent attemptSent) release() {
	if sent.response != nil {
		sent.response.Body.Close()
	}
	if sent.parallel != nil {
		sent.parallel.cancel()
	}
}

type attemptReply struct {
	fact       events.AttemptFact
	completion *wire.Completion
	committed  bool
}

// attempt performs one upstream call with one credential.
func (s *Server) attempt(ctx context.Context, x *Call, a routing.Attempt, provider *routing.Provider, slot routing.Slot, started events.AttemptFact) (events.AttemptFact, *wire.Completion, *AttemptFailure) {
	run, fact := s.newAttemptRun(ctx, x, a, provider, slot, started)
	plan, end := run.prepare()
	if end != nil {
		return run.fail(fact, nil, end)
	}
	defer plan.release()
	sent, end := run.send(plan, fact)
	defer sent.release()
	if end != nil {
		return run.fail(sent.fact, sent.parallel, end)
	}
	clock := run.streamClock(plan)
	defer clock.stop()
	reply, end := run.receive(sent, clock)
	if end != nil {
		return run.fail(reply.fact, sent.parallel, end)
	}
	return run.succeed(reply)
}

func (s *Server) newAttemptRun(ctx context.Context, x *Call, a routing.Attempt, provider *routing.Provider, slot routing.Slot, started events.AttemptFact) (attemptRun, events.AttemptFact) {
	if x.guard.engine != nil {
		x.guard.ctx = ctx
	}
	connector := provider.Connector()
	fact := started
	fact.Carried = connector.CarriedByPlugin()
	resolved := x.resolvedGenerationResources()
	conversationBound := resolved["/conversation"] != "" || resolved["/conversation/id"] != ""
	retained := x.family == wire.Responses && x.providerState && (responseStoreRequested(x.generation.parsed) || conversationBound)
	// A plugin that carries the request reports only whether it was sent, so
	// the attempt must not risk repeating work it may have done.
	atMostOnce := fact.Interaction != nil || fact.Carried || retained
	state := &attemptState{parent: ctx, classifier: upstream.Classifier{ContextWindow: true, AtMostOnce: atMostOnce, Declared: connector.Classification()}}
	traced, trace := x.request.trace.Attempt(ctx, string(provider.Kind), a.ProviderRevisionID, a.UpstreamModel)
	return attemptRun{server: s, ctx: ctx, x: x, target: a, provider: provider, slot: slot, connector: connector, state: state, traced: traced, trace: trace, retained: retained}, fact
}

func (run attemptRun) receive(sent attemptSent, clock attemptClock) (attemptReply, *attemptEnd) {
	reply, end, err := run.reply(sent, clock)
	if end != nil {
		return reply, end
	}
	inspected, err := run.inspect(sent, reply, err)
	return run.failed(inspected, sent.response.StatusCode, err)
}

func (run attemptRun) reply(sent attemptSent, clock attemptClock) (attemptReply, *attemptEnd, error) {
	if run.x.generation.parsed.Stream {
		return run.stream(sent, clock)
	}
	reply, err := run.read(sent)
	return reply, nil, err
}

func (run attemptRun) finishTrace(fact events.AttemptFact) {
	if run.trace == nil {
		return
	}
	if u := fact.Usage; u != nil {
		run.trace.RecordUsage(u.InputTokens, u.OutputTokens, u.CachedInputTokens, u.MediaUnits)
	}
	run.trace.RecordResponse(fact.ResponseModel, fact.FinishReasons)
	run.trace.Finish(fact.Class, fact.Status)
}

func (run attemptRun) fail(fact events.AttemptFact, parallel *parallelGuardrail, end *attemptEnd) (events.AttemptFact, *wire.Completion, *AttemptFailure) {
	f := run.failure(fact, parallel, end)
	failed := fact
	if failed.Interaction != nil {
		failed.Interaction.UpstreamState = f.Acceptance
	}
	failed.Class = f.class
	if failed.LocalRefusal || f.class == classPolicy {
		failed.ProviderRequestID = ""
	}
	if end.delivered != "" {
		failed.Class = end.delivered
	} else if failed.LocalRefusal && run.state.settled {
		failed.Class = ClassSuccess
	}
	failed.Committed = f.committed
	failed.Duration = run.server.Now().Sub(fact.StartedAt)
	if f.retryAfter > 0 {
		retry := f.retryAfter
		failed.RetryAfter = &retry
	}
	failed.RecordEvidence(f.billingUncertain())
	run.finishTrace(failed)
	return failed, nil, f
}

func (run attemptRun) failure(fact events.AttemptFact, parallel *parallelGuardrail, end *attemptEnd) *AttemptFailure {
	x, st := run.x, run.state
	f := AttemptFailure{class: end.class}
	if end.failure != nil {
		f = *end.failure
		f.class = end.class
	}
	if parallel != nil {
		if guardErr := parallel.join(x, true); guardErr != nil {
			f.class, f.policyCode, f.noRetry, f.overall = classPolicy, guardrailError(guardErr).Code, true, true
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
	if fact.Interaction != nil || fact.Carried || (run.retained && f.Acceptance.Unresolved()) {
		f.noRetry = f.noRetry || f.dispatched
	}
	return &f
}

func (run attemptRun) succeed(reply attemptReply) (events.AttemptFact, *wire.Completion, *AttemptFailure) {
	done := reply.fact
	done.Class = ClassSuccess
	if done.Interaction != nil {
		done.Interaction.UpstreamState = upstream.Terminal
		if run.x.generation.parsed.Stream {
			done.Interaction.ClientState = usage.ClientTerminal
		}
	}
	done.Committed = reply.committed
	done.Duration = run.server.Now().Sub(done.StartedAt)
	// Moderation intentionally has no usage counters. Other successful work
	// without usage may have been billed with nothing this gateway can meter.
	done.RecordEvidence(run.x.OperationName() != "moderation")
	run.finishTrace(done)
	return done, reply.completion, nil
}
