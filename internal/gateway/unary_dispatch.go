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
	"github.com/tyk-swe/olp/internal/usage"
)

func (s *Server) unaryAttempt(ctx context.Context, x *execution, a runtime.Attempt, provider *runtime.Provider, slot runtime.Slot, ordinal int) (AttemptFact, operationplan.Result, *attemptFailure) {
	fact := s.newFact(x, a, slot, ordinal)
	state := &attemptState{parent: ctx}
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
		f.noRetry = f.dispatched
		if f.dispatched && state.upstream.Load() != 3 && (class == classConnect || class == classTimeout || class == classUpstreamServer) {
			class = classAmbiguous
		}
		f.class = class
		fact.Class = class
		fact.Duration = s.now().Sub(fact.StartedAt)
		if f.retryAfter > 0 {
			v := f.retryAfter
			fact.RetryAfter = &v
		}
		if fact.Interaction != nil {
			fact.Interaction.UpstreamState = state.upstreamState()
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
	req.Header.Set("User-Agent", "olp-go/gateway")
	var secret []byte
	if slot.CredentialID != nil {
		secret, _ = x.request.release.Credential(*slot.CredentialID)
	}
	credentialValues, err := s.auth.Apply(actx, req, plan.Config(), secret, body)
	if err != nil {
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
		if response.StatusCode < 500 {
			state.upstream.Store(3)
		}
		raw, _ := io.ReadAll(io.LimitReader(response.Body, errorBodyLimit))
		failure := &attemptFailure{status: response.StatusCode, upstream: openai.ParseErrorBody(raw)}
		if failure.upstream != nil {
			failure.upstream.Message = redactCredentials(failure.upstream.Message, credentialValues)
		}
		switch {
		case response.StatusCode == 401 || response.StatusCode == 403:
			return fail(classCredential, failure)
		case response.StatusCode == 429:
			failure.retryAfter = retryAfter(response.Header.Get("Retry-After"), s.now())
			return fail(classRateLimit, failure)
		case response.StatusCode >= 500:
			return fail(classUpstreamServer, failure)
		case contextWindowError(failure.upstream):
			return fail(classContextWindow, failure)
		}
		return fail(classUpstreamClient, failure)
	}
	state.upstream.Store(2)
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
	state.upstream.Store(3)
	result, err := plan.Decode(raw)
	fact.Usage = legacyAccountingUsage(result.Usage)
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

// The accounting owner predates OIF. This adapter carries only observed usage;
// operation requests, results, vectors and token counts never enter generation.
func legacyAccountingUsage(value *operations.Usage) *openai.Usage {
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
	if value.SearchUnits != nil {
		out.MediaUnits = value.SearchUnits
	}
	return out
}
