package kernel

import (
	"context"
	"errors"
	"time"

	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/platform/telemetry"
	"github.com/tyk-swe/olp/internal/routing"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/operations"
	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/operationplan"
	"github.com/tyk-swe/olp/internal/upstream"
)

type unaryRun struct {
	server   *Server
	ctx      context.Context
	x        *Call
	target   routing.Attempt
	provider *routing.Provider
	slot     routing.Slot
	state    *attemptState
	traced   context.Context
	trace    *telemetry.AttemptTrace
}

type unarySent struct {
	fact     events.AttemptFact
	plan     *operationplan.Plan
	raw      []byte
	cancel   context.CancelFunc
	timer    *time.Timer
	parallel *parallelGuardrail
}

func (sent unarySent) release() {
	if sent.parallel != nil {
		sent.parallel.cancel()
	}
	if sent.timer != nil {
		sent.timer.Stop()
	}
	if sent.cancel != nil {
		sent.cancel()
	}
}

func (s *Server) unaryAttempt(ctx context.Context, x *Call, a routing.Attempt, provider *routing.Provider, slot routing.Slot, fact events.AttemptFact) (events.AttemptFact, operationplan.Result, *AttemptFailure) {
	if x.guard.engine != nil {
		x.guard.ctx = ctx
	}
	state := &attemptState{parent: ctx, classifier: upstream.Classifier{ContextWindow: true, AtMostOnce: true, Declared: provider.Connector().Classification()}}
	traced, trace := x.request.trace.Attempt(ctx, string(provider.Kind), a.ProviderRevisionID, a.UpstreamModel)
	run := unaryRun{server: s, ctx: ctx, x: x, target: a, provider: provider, slot: slot, state: state, traced: traced, trace: trace}
	sent, end := run.send(fact)
	defer sent.release()
	if end != nil {
		return run.fail(sent.fact, sent.parallel, end)
	}
	result, measured, end := run.decode(sent)
	if end != nil {
		return run.fail(measured, sent.parallel, end)
	}
	if end := run.inspect(sent, result); end != nil {
		return run.fail(measured, sent.parallel, end)
	}
	return run.succeed(measured, result)
}

func (run unaryRun) send(fact events.AttemptFact) (unarySent, *attemptEnd) {
	s, x, state := run.server, run.x, run.state
	plan, err := x.unaryPlan(run.provider, run.target.TargetID, run.target.UpstreamModel)
	if err != nil {
		return unarySent{fact: fact}, ended(ClassProtocol, nil)
	}
	submit, err := plan.RequestURL()
	if err != nil {
		return unarySent{fact: fact, plan: plan}, ended(ClassProtocol, nil)
	}
	bound := s.bindExchange(x, run.provider, run.slot, run.target.UpstreamModel, plan.Config(), state)
	if err := bound.validateEndpoint(submit); err != nil {
		return unarySent{fact: fact, plan: plan}, ended(ClassConnect, nil)
	}
	deadline, _ := run.ctx.Deadline()
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return unarySent{fact: fact, plan: plan}, ended(ClassTimeout, &AttemptFailure{overall: true})
	}
	sending, cancel := context.WithCancel(run.traced)
	timer := time.AfterFunc(min(run.target.Timeout, remaining), func() { state.reason.CompareAndSwap(0, 1); cancel() })
	client, err := bound.client(sending)
	if err != nil {
		return unarySent{fact: fact, plan: plan, cancel: cancel, timer: timer}, ended(ClassCredential, nil)
	}
	parallel := s.startParallel(sending, x, cancel)
	observed := fact
	exchange := &unaryExchange{now: s.Now, dispatched: s.MarkSessionDispatched, ctx: sending, x: x, state: state, plan: plan, client: client, bound: bound, fact: &observed, bodyCap: run.replyLimit(plan), inject: run.trace.InjectUpstream}
	raw, failure := exchange.run(plan.Method(), submit, plan.WireBody())
	sent := unarySent{fact: observed, plan: plan, raw: raw, cancel: cancel, timer: timer, parallel: parallel}
	if failure != nil {
		return sent, ended(failure.class, failure.failure)
	}
	return sent, nil
}

func (run unaryRun) replyLimit(plan *operationplan.Plan) int {
	limit := plan.Receipt().Obligations.MaxBodyBytes
	if run.server.Config.MaxResponseBytes > 0 {
		return min(limit, int(run.server.Config.MaxResponseBytes))
	}
	return limit
}

func (run unaryRun) decode(sent unarySent) (operationplan.Result, events.AttemptFact, *attemptEnd) {
	run.state.settled = true
	result, err := sent.plan.Decode(sent.raw)
	measured := sent.fact
	measured.Usage = accountingUsage(result.Usage)
	for _, decision := range result.Decisions {
		recordDecision(run.x, decision)
	}
	if err == nil {
		return result, measured, nil
	}
	var incompatible *oif.Incompatibility
	if errors.As(err, &incompatible) && (incompatible.Code == "content_policy_blocked" || incompatible.Code == "policy_conflict") {
		return result, measured, ended(classPolicy, &AttemptFailure{policyCode: incompatible.Code})
	}
	return result, measured, ended(ClassProtocol, &AttemptFailure{contractCode: "fidelity_protocol_violation"})
}

func (run unaryRun) inspect(sent unarySent, result operationplan.Result) *attemptEnd {
	x := run.x
	if guardErr := sent.parallel.join(x, true); guardErr != nil {
		return ended(classPolicy, &AttemptFailure{policyCode: guardrailError(guardErr).Code, noRetry: true, overall: true})
	}
	if !x.guard.engine.HasPhase(guardrails.Output) {
		return nil
	}
	if guardErr := run.inspectOutput(sent.plan, result); guardErr != nil {
		return ended(classPolicy, &AttemptFailure{policyCode: guardrailError(guardErr).Code, noRetry: true, overall: true})
	}
	return nil
}

func (run unaryRun) inspectOutput(plan *operationplan.Plan, result operationplan.Result) error {
	x := run.x
	texts, err := plan.GuardrailOutput(result)
	if err != nil {
		unavailable, unavailableErr := x.guard.engine.Unavailable(guardrails.Output)
		x.guard.record(unavailable.Decisions)
		return unavailableErr
	}
	return x.inspectOperationOutput(texts)
}

func (run unaryRun) finishTrace(fact events.AttemptFact) {
	if run.trace == nil {
		return
	}
	if u := fact.Usage; u != nil {
		run.trace.RecordUsage(u.InputTokens, u.OutputTokens, u.CachedInputTokens, u.MediaUnits)
	}
	run.trace.Finish(fact.Class, fact.Status)
}

func (run unaryRun) fail(fact events.AttemptFact, parallel *parallelGuardrail, end *attemptEnd) (events.AttemptFact, operationplan.Result, *AttemptFailure) {
	f := run.failure(parallel, end)
	failed := fact
	failed.Class = f.class
	failed.Duration = run.server.Now().Sub(fact.StartedAt)
	if f.retryAfter > 0 {
		wait := f.retryAfter
		failed.RetryAfter = &wait
	}
	if failed.Interaction != nil {
		failed.Interaction.UpstreamState = f.Acceptance
	}
	failed.RecordEvidence(f.billingUncertain())
	run.finishTrace(failed)
	return failed, operationplan.Result{}, f
}

func (run unaryRun) failure(parallel *parallelGuardrail, end *attemptEnd) *AttemptFailure {
	state := run.state
	f := AttemptFailure{class: end.class}
	if end.failure != nil {
		f = *end.failure
		f.class = end.class
	}
	if err := parallel.join(run.x, true); err != nil {
		f.class, f.policyCode, f.noRetry, f.overall = classPolicy, guardrailError(err).Code, true, true
	}
	f.dispatched = state.dispatched.Load()
	f.Acceptance = state.evidence().Acceptance()
	f.accepted = state.accepted
	f.noRetry = f.noRetry || f.dispatched
	return &f
}

func (run unaryRun) succeed(fact events.AttemptFact, result operationplan.Result) (events.AttemptFact, operationplan.Result, *AttemptFailure) {
	done := fact
	done.Class = ClassSuccess
	done.Duration = run.server.Now().Sub(fact.StartedAt)
	if done.Interaction != nil {
		done.Interaction.UpstreamState = upstream.Terminal
	}
	// Moderation and search have no usage counters; a validated result
	// completes their evidence.
	operation := run.x.operationName()
	done.RecordEvidence(operation != "moderation" && operation != "search")
	run.finishTrace(done)
	return done, result, nil
}

// accountingUsage converts operation usage into the usage an accounting fact
// records. It carries only observed usage; operation requests, results, vectors
// and token counts never enter generation.
func accountingUsage(value *operations.Usage) *wire.Usage {
	if value == nil {
		return nil
	}
	out := &wire.Usage{CachedInputTokens: value.CachedInputTokens, CacheWriteInputTokens: value.CacheWriteInputTokens, ReasoningTokens: value.ReasoningTokens}
	if value.InputTokens != nil {
		out.InputTokens = *value.InputTokens
	}
	if value.OutputTokens != nil {
		out.OutputTokens = *value.OutputTokens
	}
	if value.TotalTokens != nil {
		out.TotalTokens = *value.TotalTokens
	} else {
		out.TotalTokens = out.InputTokens + out.OutputTokens
	}
	if value.MediaUnits != nil {
		out.MediaUnits = value.MediaUnits
	}
	return out
}
