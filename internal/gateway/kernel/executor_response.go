package kernel

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/tyk-swe/olp/internal/export/capture"
	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	"github.com/tyk-swe/olp/internal/platform/telemetry"
	"github.com/tyk-swe/olp/internal/providerinvoke"
	"github.com/tyk-swe/olp/internal/routing"
)

var ErrResponseTooLarge = providerinvoke.ErrResponseTooLarge

// Finish emits the terminal envelope exactly once.
func (s *Server) Finish(x *Call, out *Outcome, status int) {
	s.finishContext(context.Background(), x, out, status)
}

func (s *Server) finishContext(ctx context.Context, x *Call, out *Outcome, status int) {
	x.once.Do(func() {
		s.storeCache(x, out, status)
		completedAt := s.Now()
		x.finalizePinned()
		env := x.envelope(out, status, completedAt)
		x.traceTerminal(env)
		env.PayloadCaptured = x.captureTerminal(ctx, out, env, completedAt)
		s.finalizeGenerationHTTP(x, env)
		s.recordBusiness(env, x)
		s.Deps.Sink.Terminal(env)
	})
}

func (x *Call) envelope(out *Outcome, status int, completedAt time.Time) events.Envelope {
	env := events.Envelope{
		RequestID:            x.request.id,
		AccountingID:         x.request.accountingID(),
		ClientIP:             x.request.clientIP,
		Actor:                x.actor,
		KeyID:                x.keyID,
		AgentPrincipalID:     x.authority.DelegationAgentID,
		MemberPrincipalID:    x.authority.DelegationMemberID,
		DelegationPairDigest: x.authority.DelegationPairDigest,
		EndUserDigest:        x.endUserDigest,
		BudgetGroupID:        x.budgetGroupID,
		Attribution:          x.attribution,
		EvaluatorGuardrailID: x.evaluatorGuardrailID, GuardrailDecisions: x.guard.evidence(),
		UserID:      x.userID,
		Family:      string(x.family),
		Mode:        x.mode,
		Operation:   x.OperationName(),
		Surface:     x.surfaceName(),
		Outcome:     "failure",
		Status:      status,
		StartedAt:   x.request.startedAt,
		CompletedAt: completedAt,
		Duration:    completedAt.Sub(x.request.startedAt),
		FirstByte:   x.firstByte,
		Attempts:    x.facts,
	}
	x.scopeEnvelope(&env)
	x.resolveEnvelope(&env, out)
	return env
}

func (x *Call) scopeEnvelope(env *events.Envelope) {
	if x.request.release != nil {
		env.ReleaseSequence = x.request.release.Sequence
		if x.request.release.Snapshot() != nil {
			env.RuntimeGenerationID = x.request.release.Snapshot().Generation().ID
		}
	}
	if x.Route != nil {
		env.Route = x.named().Slug
		env.RouteRevisionID = x.named().RevisionID
	}
	if x.origin != "" {
		env.Origin, env.ParentRequestID = x.origin, x.parent
		if x.origin.Keyless() {
			env.KeyID = ""
			env.EndUserDigest = ""
			env.AgentPrincipalID = ""
			env.MemberPrincipalID = ""
			env.DelegationPairDigest = ""
		}
	}
	if x.cache != nil {
		env.CacheResult = x.cache.result
		env.CacheLookupReason = x.cache.lookupReason
	}
}

func (x *Call) resolveEnvelope(env *events.Envelope, out *Outcome) {
	env.Usage = x.usage()
	switch {
	case out != nil && out.Err != nil:
		env.ErrorClass = out.Err.Code
		env.BudgetBoundary = quota.BudgetBoundary(out.Err)
	case out == nil && x.Failure != nil:
		env.ErrorClass = x.Failure.Code
		env.BudgetBoundary = quota.BudgetBoundary(x.Failure)
	}
	if out != nil {
		env.Committed = out.Committed
		switch {
		case out.Cancelled:
			env.Outcome = "cancelled"
			env.Status = 0
		case out.Err == nil:
			env.Outcome = "success"
		}
	}
}

func (x *Call) traceTerminal(env events.Envelope) {
	if x.request.trace == nil {
		return
	}
	var firstByte, total time.Duration
	if x.firstByte != nil {
		firstByte = *x.firstByte
	}
	total = env.Duration
	x.request.trace.RecordInferenceContext(env.Surface, env.Operation, env.Family, env.Route, env.KeyID, env.RuntimeGenerationID)
	if len(x.facts) != 0 {
		serving := &x.facts[len(x.facts)-1]
		var providerKind string
		var snapshot *routing.View
		if x.historicalSnapshot != nil {
			snapshot = x.historicalSnapshot
		} else if x.request.release != nil {
			snapshot = x.request.release.Snapshot()
		}
		if snapshot != nil {
			if provider, ok := snapshot.FindProvider(serving.ProviderID); ok {
				providerKind = string(provider.Kind)
			}
		}
		var inputTokens, outputTokens *int64
		if u := serving.Usage; u != nil {
			inputTokens, outputTokens = &u.InputTokens, &u.OutputTokens
		}
		x.request.trace.RecordResponse(providerKind, serving.ResponseModel, inputTokens, outputTokens, serving.FinishReasons)
	}
	x.request.trace.RecordTerminal(env.Status, env.ErrorClass, len(x.facts), firstByte, total)
}

func (x *Call) captureTerminal(ctx context.Context, out *Outcome, env events.Envelope, completedAt time.Time) bool {
	if x.capture == nil {
		return false
	}
	x.capture.CaptureDocument(x.captureInputDocument())
	streaming := x.generation.parsed != nil && x.generation.parsed.Stream
	if !streaming {
		x.capture.CaptureResponse(x.family, completionBody(out))
	}
	var traceID, spanID string
	if spanCtx := x.request.trace.SpanContext(); spanCtx.IsValid() {
		traceID, spanID = spanCtx.TraceID().String(), spanCtx.SpanID().String()
	}
	return x.capture.Finish(ctx, capture.Identity{
		Route: env.Route, ProjectID: x.captureProject(), KeyID: env.KeyID, EndUserDigest: env.EndUserDigest,
		TraceID: traceID, SpanID: spanID, Outcome: env.Outcome, Mode: capture.ModeOf(streaming),
	}, completedAt)
}

func (x *Call) captureInputDocument() map[string]json.RawMessage {
	var body []byte
	if len(x.facts) != 0 {
		fact := &x.facts[len(x.facts)-1]
		if document := x.preparation().factEvidence(fact).document; document.Valid() {
			body = document.Bytes()
		}
	}
	if len(body) == 0 && x.generation.parsed != nil {
		if document := x.generation.parsed.OIF().Document(); document.Valid() {
			body = document.Bytes()
		}
	}
	if len(body) == 0 {
		return nil
	}
	var document map[string]json.RawMessage
	if json.Unmarshal(body, &document) != nil {
		return nil
	}
	return document
}

func (x *Call) captureProject() string {
	if x.authority.ProjectID != nil {
		return *x.authority.ProjectID
	}
	if x.Route != nil && x.Route.ProjectID != nil {
		return *x.Route.ProjectID
	}
	return ""
}

func (s *Server) recordBusiness(env events.Envelope, x *Call) {
	if s.Deps.Business == nil {
		return
	}
	project := ""
	if x.authority.ProjectID != nil {
		project = *x.authority.ProjectID
	} else if x.Route != nil && x.Route.ProjectID != nil {
		project = *x.Route.ProjectID
	}
	if x.cache != nil {
		s.Deps.Business.Cache(env.Route, x.cache.result, x.cache.storedBytes, x.cache.backendIssue)
	}
	for _, decision := range env.GuardrailDecisions {
		s.Deps.Business.Guardrail(telemetry.BusinessLabels{Route: env.Route, Project: project, Key: env.KeyID}, string(decision.Phase), string(decision.Action), string(decision.Outcome), decision.LatencyMS)
	}
	var snapshot *routing.View
	if x.historicalSnapshot != nil {
		snapshot = x.historicalSnapshot
	} else if x.request.release != nil {
		snapshot = x.request.release.Snapshot()
	}
	for index := range env.Attempts {
		fact := &env.Attempts[index]
		route := env.Route
		if fact.Leg != nil && fact.Leg.Route != "" {
			route = fact.Leg.Route
		}
		labels := telemetry.BusinessLabels{Route: route, Project: project, Key: env.KeyID, EndUser: env.EndUserDigest}
		if snapshot != nil {
			if provider, ok := snapshot.FindProvider(fact.ProviderID); ok {
				labels.ProviderKind = string(provider.Kind)
			}
		}
		if u := fact.Usage; u != nil && fact.UsageObserved {
			s.Deps.Business.Tokens(labels, &u.InputTokens, &u.OutputTokens)
			if fact.Price != nil {
				if cost, ok := fact.Price.Cost(events.AttemptUsage(fact, env.Operation), fact.StartedAt, events.AttemptSuccessful(fact), fact.Discount); ok {
					// A committed attempt prices but owes nothing: record its
					// zero marginal cost rather than skipping the metric.
					if amount, err := strconv.ParseFloat(cost.String(), 64); err == nil {
						if x.factCommitted(fact) {
							amount = 0
						}
						s.Deps.Business.Cost(labels, fact.Price.Currency, amount)
					}
				}
			}
		}
		if fact.FirstOutput != nil {
			elapsed := fact.StartedAt.Add(*fact.FirstOutput).Sub(env.StartedAt).Seconds()
			s.Deps.Business.FirstToken(labels, elapsed)
			if u := fact.Usage; u != nil && fact.UsageObserved && u.OutputTokens > 0 {
				if seconds := fact.Duration.Seconds() - fact.FirstOutput.Seconds(); seconds > 0 {
					s.Deps.Business.OutputRate(labels, u.OutputTokens, seconds)
				}
			}
		}
	}
}

func credentialHealthKey(slot *routing.Slot, generation int64) string {
	if slot.CredentialID != nil {
		key := "credential:" + *slot.CredentialID
		if generation != 0 {
			key += ":" + strconv.FormatInt(generation, 10)
		}
		return key
	}
	return "credential:ambient"
}

func completionBody(out *Outcome) []byte {
	if out == nil || out.Completion == nil || len(out.Completion.Body) == 0 {
		return nil
	}
	return out.Completion.Body
}
