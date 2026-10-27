package gateway

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/routing"

	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operationplan"
	"github.com/tyk-swe/olp/internal/operations"
	"github.com/tyk-swe/olp/internal/upstream"
	"github.com/tyk-swe/olp/internal/usage"
)

func (s *Server) unaryAttempt(ctx context.Context, x *call, a routing.Attempt, provider *routing.Provider, slot routing.Slot, fact AttemptFact) (AttemptFact, operationplan.Result, *attemptFailure) {
	if x.guardrailEngine != nil {
		x.guardrailCtx = context.WithValue(ctx, guardrailCallerKey{}, x)
	}
	state := &attemptState{parent: ctx, classifier: upstream.Classifier{ContextWindow: true, AtMostOnce: true}}
	var parallel *parallelGuardrail
	attemptCtx, trace := x.request.trace.Attempt(ctx, provider.Kind, a.ProviderRevisionID, a.UpstreamModel)
	finish := func() {
		if trace != nil {
			if u := fact.Usage; u != nil {
				trace.RecordUsage(&u.InputTokens, &u.OutputTokens, u.CachedInputTokens, u.MediaUnits)
			}
			trace.Finish(fact.Class, fact.Status)
		}
	}
	fail := func(class string, f *attemptFailure) (AttemptFact, operationplan.Result, *attemptFailure) {
		if f == nil {
			f = &attemptFailure{}
		}
		if err := parallel.join(x, true); err != nil {
			class, f.policyCode, f.noRetry, f.overall = classPolicy, guardrailError(err).Code, true, true
		}
		f.dispatched = state.dispatched.Load()
		f.acceptance = state.evidence().Acceptance()
		f.accepted = state.accepted
		f.noRetry = f.noRetry || f.dispatched
		f.class = class
		fact.Class = class
		fact.Duration = s.now().Sub(fact.StartedAt)
		if f.retryAfter > 0 {
			v := f.retryAfter
			fact.RetryAfter = &v
		}
		if fact.Interaction != nil {
			fact.Interaction.UpstreamState = string(f.acceptance)
		}
		fact.recordEvidence(f.billingUncertain())
		finish()
		return fact, operationplan.Result{}, f
	}
	plan, err := x.unaryPlan(provider, a.TargetID, a.UpstreamModel)
	if err != nil {
		return fail(classProtocol, nil)
	}
	submit, err := plan.RequestURL()
	if err != nil {
		return fail(classProtocol, nil)
	}
	if err := s.validateUnaryEndpoint(submit); err != nil {
		return fail(classConnect, nil)
	}
	deadline, _ := ctx.Deadline()
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return fail(classTimeout, &attemptFailure{overall: true})
	}
	actx, cancel := context.WithCancel(attemptCtx)
	defer cancel()
	timer := time.AfterFunc(min(a.Timeout, remaining), func() { state.reason.CompareAndSwap(0, 1); cancel() })
	defer timer.Stop()
	client, err := s.providerClient(actx, x.request.release, provider, slot)
	if err != nil {
		return fail(classCredential, nil)
	}
	parallel = x.startParallel(actx, cancel)
	if parallel != nil {
		defer parallel.cancel()
	}
	cap := plan.Receipt().Obligations.MaxBodyBytes
	if s.cfg.MaxResponseBytes > 0 {
		cap = min(cap, int(s.cfg.MaxResponseBytes))
	}
	exchange := &unaryExchange{server: s, ctx: actx, x: x, state: state, plan: plan, client: client, slot: slot, provider: provider, model: a.UpstreamModel, fact: &fact, bodyCap: cap, inject: func(header http.Header) {
		if trace != nil {
			trace.InjectUpstream(header, x.request.trace.PropagateUpstream())
		}
	}}
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
			return fail(classPolicy, &attemptFailure{policyCode: incompatible.Code})
		}
		return fail(classProtocol, &attemptFailure{contractCode: "fidelity_protocol_violation"})
	}
	if guardErr := parallel.join(x, true); guardErr != nil {
		return fail(classPolicy, &attemptFailure{policyCode: guardrailError(guardErr).Code, noRetry: true, overall: true})
	}
	if x.guardrailEngine.HasPhase(guardrails.Output) {
		texts, guardErr := plan.GuardrailOutput(result)
		if guardErr != nil {
			unavailable, e := x.guardrailEngine.Unavailable(guardrails.Output)
			x.recordGuardrails(unavailable.Decisions)
			guardErr = e
		} else {
			guardErr = x.inspectOperationOutput(texts)
		}
		if guardErr != nil {
			return fail(classPolicy, &attemptFailure{policyCode: guardrailError(guardErr).Code, noRetry: true, overall: true})
		}
	}
	fact.Class = classSuccess
	fact.Duration = s.now().Sub(fact.StartedAt)
	if fact.Interaction != nil {
		fact.Interaction.UpstreamState = usage.UpstreamTerminal
	}
	// Moderation and search have no usage counters; a validated result
	// completes their evidence.
	fact.recordEvidence(x.operationName() != "moderation" && x.operationName() != "search")
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
