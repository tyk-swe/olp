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
	fact     events.AttemptFact
	state    *attemptState
	traced   context.Context
	trace    *telemetry.AttemptTrace
	plan     *operationplan.Plan

	cancel   context.CancelFunc
	timer    *time.Timer
	parallel *parallelGuardrail
}

func (s *Server) unaryAttempt(ctx context.Context, x *Call, a routing.Attempt, provider *routing.Provider, slot routing.Slot, fact events.AttemptFact) (events.AttemptFact, operationplan.Result, *AttemptFailure) {
	if x.guard.engine != nil {
		x.guard.ctx = ctx
	}
	state := &attemptState{parent: ctx, classifier: upstream.Classifier{ContextWindow: true, AtMostOnce: true, Declared: provider.Connector().Classification()}}
	run := &unaryRun{server: s, ctx: ctx, x: x, target: a, provider: provider, slot: slot, fact: fact, state: state}
	run.traced, run.trace = x.request.trace.Attempt(ctx, string(provider.Kind), a.ProviderRevisionID, a.UpstreamModel)
	defer run.close()
	result, end := run.exchange()
	if end != nil {
		return run.fail(end.class, end.failure)
	}
	return run.succeed(result)
}

func (run *unaryRun) exchange() (operationplan.Result, *attemptEnd) {
	raw, end := run.send()
	if end != nil {
		return operationplan.Result{}, end
	}
	result, end := run.decode(raw)
	if end != nil {
		return operationplan.Result{}, end
	}
	return result, run.inspect(result)
}

func (run *unaryRun) close() {
	if run.parallel != nil {
		run.parallel.cancel()
	}
	if run.timer != nil {
		run.timer.Stop()
	}
	if run.cancel != nil {
		run.cancel()
	}
}

func (run *unaryRun) send() ([]byte, *attemptEnd) {
	s, x, state := run.server, run.x, run.state
	plan, err := x.unaryPlan(run.provider, run.target.TargetID, run.target.UpstreamModel)
	if err != nil {
		return nil, ended(ClassProtocol, nil)
	}
	run.plan = plan
	submit, err := plan.RequestURL()
	if err != nil {
		return nil, ended(ClassProtocol, nil)
	}
	bound := s.bindExchange(x, run.provider, run.slot, run.target.UpstreamModel, plan.Config(), state)
	if err := bound.validateEndpoint(submit); err != nil {
		return nil, ended(ClassConnect, nil)
	}
	deadline, _ := run.ctx.Deadline()
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return nil, ended(ClassTimeout, &AttemptFailure{overall: true})
	}
	sending, cancel := context.WithCancel(run.traced)
	run.cancel = cancel
	run.timer = time.AfterFunc(min(run.target.Timeout, remaining), func() { state.reason.CompareAndSwap(0, 1); cancel() })
	client, err := bound.client(sending)
	if err != nil {
		return nil, ended(classCredential, nil)
	}
	run.parallel = s.startParallel(sending, x, cancel)
	exchange := &unaryExchange{now: s.Now, dispatched: s.markSessionDispatched, ctx: sending, x: x, state: state, plan: plan, client: client, bound: bound, fact: &run.fact, bodyCap: run.replyLimit(), inject: run.trace.InjectUpstream}
	raw, failure := exchange.run(plan.Method(), submit, plan.WireBody())
	if failure != nil {
		return nil, ended(failure.class, failure.failure)
	}
	return raw, nil
}

func (run *unaryRun) replyLimit() int {
	limit := run.plan.Receipt().Obligations.MaxBodyBytes
	if run.server.Config.MaxResponseBytes > 0 {
		return min(limit, int(run.server.Config.MaxResponseBytes))
	}
	return limit
}

func (run *unaryRun) decode(raw []byte) (operationplan.Result, *attemptEnd) {
	run.state.settled = true
	result, err := run.plan.Decode(raw)
	run.fact.Usage = accountingUsage(result.Usage)
	for _, decision := range result.Decisions {
		recordDecision(run.x, decision)
	}
	if err == nil {
		return result, nil
	}
	var incompatible *oif.Incompatibility
	if errors.As(err, &incompatible) && (incompatible.Code == "content_policy_blocked" || incompatible.Code == "policy_conflict") {
		return result, ended(classPolicy, &AttemptFailure{policyCode: incompatible.Code})
	}
	return result, ended(ClassProtocol, &AttemptFailure{contractCode: "fidelity_protocol_violation"})
}

func (run *unaryRun) inspect(result operationplan.Result) *attemptEnd {
	x := run.x
	if guardErr := run.parallel.join(x, true); guardErr != nil {
		return ended(classPolicy, &AttemptFailure{policyCode: guardrailError(guardErr).Code, noRetry: true, overall: true})
	}
	if !x.guard.engine.HasPhase(guardrails.Output) {
		return nil
	}
	if guardErr := run.inspectOutput(result); guardErr != nil {
		return ended(classPolicy, &AttemptFailure{policyCode: guardrailError(guardErr).Code, noRetry: true, overall: true})
	}
	return nil
}

func (run *unaryRun) inspectOutput(result operationplan.Result) error {
	x := run.x
	texts, err := run.plan.GuardrailOutput(result)
	if err != nil {
		unavailable, unavailableErr := x.guard.engine.Unavailable(guardrails.Output)
		x.guard.record(unavailable.Decisions)
		return unavailableErr
	}
	return x.inspectOperationOutput(texts)
}

func (run *unaryRun) finishTrace() {
	if run.trace == nil {
		return
	}
	if u := run.fact.Usage; u != nil {
		run.trace.RecordUsage(u.InputTokens, u.OutputTokens, u.CachedInputTokens, u.MediaUnits)
	}
	run.trace.Finish(run.fact.Class, run.fact.Status)
}

func (run *unaryRun) fail(reported string, failure *AttemptFailure) (events.AttemptFact, operationplan.Result, *AttemptFailure) {
	state := run.state
	class, f := reported, failure
	if f == nil {
		f = &AttemptFailure{}
	}
	if err := run.parallel.join(run.x, true); err != nil {
		class, f.policyCode, f.noRetry, f.overall = classPolicy, guardrailError(err).Code, true, true
	}
	f.dispatched = state.dispatched.Load()
	f.Acceptance = state.evidence().Acceptance()
	f.accepted = state.accepted
	f.noRetry = f.noRetry || f.dispatched
	f.class = class
	run.fact.Class = class
	run.fact.Duration = run.server.Now().Sub(run.fact.StartedAt)
	if f.retryAfter > 0 {
		wait := f.retryAfter
		run.fact.RetryAfter = &wait
	}
	if run.fact.Interaction != nil {
		run.fact.Interaction.UpstreamState = f.Acceptance
	}
	run.fact.RecordEvidence(f.billingUncertain())
	run.finishTrace()
	return run.fact, operationplan.Result{}, f
}

func (run *unaryRun) succeed(result operationplan.Result) (events.AttemptFact, operationplan.Result, *AttemptFailure) {
	run.fact.Class = ClassSuccess
	run.fact.Duration = run.server.Now().Sub(run.fact.StartedAt)
	if run.fact.Interaction != nil {
		run.fact.Interaction.UpstreamState = upstream.Terminal
	}
	// Moderation and search have no usage counters; a validated result
	// completes their evidence.
	operation := run.x.operationName()
	run.fact.RecordEvidence(operation != "moderation" && operation != "search")
	run.finishTrace()
	return run.fact, result, nil
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
