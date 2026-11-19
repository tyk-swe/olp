package kernel

import (
	"context"
	"errors"
	"time"

	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/routing"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/operations"
	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/operationplan"
	"github.com/tyk-swe/olp/internal/upstream"
)

func (s *Server) unaryAttempt(ctx context.Context, x *Call, a routing.Attempt, provider *routing.Provider, slot routing.Slot, fact events.AttemptFact) (events.AttemptFact, operationplan.Result, *AttemptFailure) {
	if x.guard.engine != nil {
		x.guard.ctx = ctx
	}
	state := &attemptState{parent: ctx, classifier: upstream.Classifier{ContextWindow: true, AtMostOnce: true, Declared: provider.Connector().Classification()}}
	var parallel *parallelGuardrail
	attemptCtx, trace := x.request.trace.Attempt(ctx, string(provider.Kind), a.ProviderRevisionID, a.UpstreamModel)
	finish := func() {
		if trace != nil {
			if u := fact.Usage; u != nil {
				trace.RecordUsage(u.InputTokens, u.OutputTokens, u.CachedInputTokens, u.MediaUnits)
			}
			trace.Finish(fact.Class, fact.Status)
		}
	}
	fail := func(class string, f *AttemptFailure) (events.AttemptFact, operationplan.Result, *AttemptFailure) {
		if f == nil {
			f = &AttemptFailure{}
		}
		if err := parallel.join(x, true); err != nil {
			class, f.policyCode, f.noRetry, f.overall = classPolicy, guardrailError(err).Code, true, true
		}
		f.dispatched = state.dispatched.Load()
		f.Acceptance = state.evidence().Acceptance()
		f.accepted = state.accepted
		f.noRetry = f.noRetry || f.dispatched
		f.class = class
		fact.Class = class
		fact.Duration = s.Now().Sub(fact.StartedAt)
		if f.retryAfter > 0 {
			v := f.retryAfter
			fact.RetryAfter = &v
		}
		if fact.Interaction != nil {
			fact.Interaction.UpstreamState = f.Acceptance
		}
		fact.RecordEvidence(f.billingUncertain())
		finish()
		return fact, operationplan.Result{}, f
	}
	plan, err := x.unaryPlan(provider, a.TargetID, a.UpstreamModel)
	if err != nil {
		return fail(ClassProtocol, nil)
	}
	submit, err := plan.RequestURL()
	if err != nil {
		return fail(ClassProtocol, nil)
	}
	bound := s.bindExchange(x, provider, slot, a.UpstreamModel, plan.Config(), state)
	if err := bound.validateEndpoint(submit); err != nil {
		return fail(classConnect, nil)
	}
	deadline, _ := ctx.Deadline()
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return fail(ClassTimeout, &AttemptFailure{overall: true})
	}
	actx, cancel := context.WithCancel(attemptCtx)
	defer cancel()
	timer := time.AfterFunc(min(a.Timeout, remaining), func() { state.reason.CompareAndSwap(0, 1); cancel() })
	defer timer.Stop()
	client, err := bound.client(actx)
	if err != nil {
		return fail(classCredential, nil)
	}
	parallel = s.startParallel(actx, x, cancel)
	if parallel != nil {
		defer parallel.cancel()
	}
	cap := plan.Receipt().Obligations.MaxBodyBytes
	if s.Config.MaxResponseBytes > 0 {
		cap = min(cap, int(s.Config.MaxResponseBytes))
	}
	exchange := &unaryExchange{now: s.Now, dispatched: s.markSessionDispatched, ctx: actx, x: x, state: state, plan: plan, client: client, bound: bound, fact: &fact, bodyCap: cap, inject: trace.InjectUpstream}
	raw, failure := exchange.run(plan.Method(), submit, plan.WireBody())
	if failure != nil {
		return fail(failure.class, failure.failure)
	}
	state.settled = true
	result, err := plan.Decode(raw)
	fact.Usage = accountingUsage(result.Usage)
	for _, decision := range result.Decisions {
		recordDecision(x, decision)
	}
	if err != nil {
		var incompatible *oif.Incompatibility
		if errors.As(err, &incompatible) && (incompatible.Code == "content_policy_blocked" || incompatible.Code == "policy_conflict") {
			return fail(classPolicy, &AttemptFailure{policyCode: incompatible.Code})
		}
		return fail(ClassProtocol, &AttemptFailure{contractCode: "fidelity_protocol_violation"})
	}
	if guardErr := parallel.join(x, true); guardErr != nil {
		return fail(classPolicy, &AttemptFailure{policyCode: guardrailError(guardErr).Code, noRetry: true, overall: true})
	}
	if x.guard.engine.HasPhase(guardrails.Output) {
		texts, guardErr := plan.GuardrailOutput(result)
		if guardErr != nil {
			unavailable, e := x.guard.engine.Unavailable(guardrails.Output)
			x.guard.record(unavailable.Decisions)
			guardErr = e
		} else {
			guardErr = x.inspectOperationOutput(texts)
		}
		if guardErr != nil {
			return fail(classPolicy, &AttemptFailure{policyCode: guardrailError(guardErr).Code, noRetry: true, overall: true})
		}
	}
	fact.Class = ClassSuccess
	fact.Duration = s.Now().Sub(fact.StartedAt)
	if fact.Interaction != nil {
		fact.Interaction.UpstreamState = upstream.Terminal
	}
	// Moderation and search have no usage counters; a validated result
	// completes their evidence.
	fact.RecordEvidence(x.operationName() != "moderation" && x.operationName() != "search")
	finish()
	return fact, result, nil
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
