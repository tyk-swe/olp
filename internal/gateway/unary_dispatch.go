package gateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/http/httptrace"
	"time"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operationplan"
	"github.com/tyk-swe/olp/internal/operations"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/upstream"
	"github.com/tyk-swe/olp/internal/usage"
)

func (s *Server) unaryAttempt(ctx context.Context, x *execution, a runtime.Attempt, provider *runtime.Provider, slot runtime.Slot, ordinal int) (AttemptFact, operationplan.Result, *attemptFailure) {
	fact := s.newFact(x, a, slot, ordinal)
	state := &attemptState{parent: ctx, classifier: upstream.Classifier{ContextWindow: true, AtMostOnce: true}}
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
		f.dispatched = state.dispatched.Load()
		f.acceptance = state.evidence().Acceptance()
		f.noRetry = f.dispatched
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
	plan, err := x.unaryPlan(provider, a.UpstreamModel)
	if err != nil {
		return fail(classProtocol, nil)
	}
	endpoint, err := plan.Endpoint()
	if err != nil {
		return fail(classProtocol, nil)
	}
	if _, err = s.egress.ValidateEndpoint(endpoint); err != nil {
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
	body := plan.Body()
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(actx, state.trace()), http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fail(classConnect, nil)
	}
	if trace != nil {
		trace.InjectUpstream(req.Header, x.request.trace.PropagateUpstream())
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "olp/gateway")
	if err := s.applySlotCredential(actx, x, req, plan.Config(), slot, body); err != nil {
		if actx.Err() != nil {
			return fail(state.classify(err, false), nil)
		}
		return fail(classCredential, nil)
	}
	client, err := s.providerClient(actx, x.request.release, provider, slot)
	if err != nil {
		return fail(classCredential, nil)
	}
	response, err := client.Do(req)
	if err != nil {
		return fail(state.classify(err, false), nil)
	}
	defer response.Body.Close()
	received := s.now().Sub(fact.StartedAt)
	fact.FirstByte = &received
	fact.Status = response.StatusCode
	if response.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(response.Body, errorBodyLimit))
		failure := &attemptFailure{status: response.StatusCode, upstream: x.redacted(openai.ParseErrorBody(raw))}
		class := state.rejected(response.StatusCode, failure.upstream)
		if class == classRateLimit {
			failure.retryAfter = retryAfter(response.Header.Get("Retry-After"), s.now())
		}
		return fail(class, failure)
	}
	state.accepted = true
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return fail(classProtocol, &attemptFailure{contractCode: "fidelity_protocol_violation"})
	}
	cap := plan.Receipt().Obligations.MaxBodyBytes
	if s.cfg.MaxResponseBytes > 0 {
		cap = min(cap, int(s.cfg.MaxResponseBytes))
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, int64(cap)+1))
	if err != nil {
		return fail(state.classify(err, false), nil)
	}
	if len(raw) > cap {
		return fail(classProtocol, &attemptFailure{contractCode: "fidelity_protocol_violation"})
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
	fact.Class = classSuccess
	fact.Duration = s.now().Sub(fact.StartedAt)
	if fact.Interaction != nil {
		fact.Interaction.UpstreamState = usage.UpstreamTerminal
	}
	fact.recordEvidence(true)
	finish()
	return fact, result, nil
}

// accountingUsage converts operation usage into the usage an accounting fact
// records. It carries only observed usage; operation requests, results, vectors
// and token counts never enter generation.
func accountingUsage(value *operations.Usage) *openai.Usage {
	if value == nil {
		return nil
	}
	out := &openai.Usage{CachedInputTokens: value.CachedInputTokens}
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
