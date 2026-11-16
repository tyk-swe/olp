package gateway

import (
	"encoding/json"
	"strconv"
	"time"

	"github.com/tyk-swe/olp/internal/export/capture"
	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	"github.com/tyk-swe/olp/internal/providerinvoke"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/telemetry"
)

var errResponseTooLarge = providerinvoke.ErrResponseTooLarge

// finish emits the terminal envelope exactly once.
func (s *Server) finish(x *call, out *outcome, status int) {
	x.once.Do(func() {
		s.storeCache(x, out, status)
		completedAt := s.now()
		x.finalizePinned(out, completedAt)
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
			EvaluatorGuardrailID: x.evaluatorGuardrailID, GuardrailDecisions: x.guardrailEvidence(), PolicyDecisions: x.policyDecisions,
			UserID:      x.userID,
			Family:      string(x.family),
			Mode:        x.mode,
			Operation:   x.operationName(),
			Surface:     x.surfaceName(),
			Outcome:     "failure",
			Status:      status,
			StartedAt:   x.request.startedAt,
			CompletedAt: completedAt,
			Duration:    completedAt.Sub(x.request.startedAt),
			FirstByte:   x.firstByte,
			Attempts:    x.facts,
		}
		if x.request.release != nil {
			env.ReleaseSequence = x.request.release.Sequence
			if x.request.release.Snapshot() != nil {
				env.RuntimeGenerationID = x.request.release.Snapshot().Generation().ID
			}
		}
		if x.route != nil {
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
		}
		env.Usage = x.usage()
		switch {
		case out != nil && out.err != nil:
			env.ErrorClass = out.err.Code
			env.BudgetBoundary = quota.BudgetBoundary(out.err)
		case out == nil && x.failure != nil:
			env.ErrorClass = x.failure.Code
			env.BudgetBoundary = quota.BudgetBoundary(x.failure)
		}
		if out != nil {
			env.Committed = out.committed
			switch {
			case out.cancelled:
				env.Outcome = "cancelled"
				env.Status = 0
			case out.err == nil:
				env.Outcome = "success"
			}
		}
		if x.request.trace != nil {
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
		if x.capture != nil {
			x.capture.CaptureDocument(x.captureInputDocument())
			streaming := x.generation.parsed != nil && x.generation.parsed.Stream
			if !streaming {
				x.capture.CaptureResponse(x.family, completionBody(out))
			}
			var traceID, spanID string
			if context := x.request.trace.SpanContext(); context.IsValid() {
				traceID, spanID = context.TraceID().String(), context.SpanID().String()
			}
			env.PayloadCaptured = x.capture.Finish(x.captureContext, capture.Identity{
				Route: env.Route, ProjectID: x.captureProject(), KeyID: env.KeyID, EndUserDigest: env.EndUserDigest,
				TraceID: traceID, SpanID: spanID, Outcome: env.Outcome, Mode: capture.ModeOf(streaming),
			}, completedAt)
		}
		s.finalizeGenerationHTTP(x, env)
		s.recordBusiness(env, x)
		s.deps.Sink.Terminal(env)
	})
}

func (x *call) captureInputDocument() map[string]json.RawMessage {
	var body []byte
	if len(x.facts) != 0 {
		fact := &x.facts[len(x.facts)-1]
		if document := x.preparation().factEvidence(fact).document; document.Valid() {
			body = document.Bytes()
		}
	}
	if len(body) == 0 && x.generation.parsed != nil && (x.route == nil || x.route.ContentPolicy == nil) {
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

func (x *call) captureProject() string {
	if x.authority.ProjectID != nil {
		return *x.authority.ProjectID
	}
	if x.route != nil && x.route.ProjectID != nil {
		return *x.route.ProjectID
	}
	return ""
}

func (s *Server) recordBusiness(env events.Envelope, x *call) {
	if s.deps.Business == nil {
		return
	}
	project := ""
	if x.authority.ProjectID != nil {
		project = *x.authority.ProjectID
	} else if x.route != nil && x.route.ProjectID != nil {
		project = *x.route.ProjectID
	}
	if x.cache != nil {
		s.deps.Business.Cache(env.Route, x.cache.result, x.cache.storedBytes, x.cache.reason != "")
	}
	for _, decision := range env.GuardrailDecisions {
		s.deps.Business.Guardrail(telemetry.BusinessLabels{Route: env.Route, Project: project, Key: env.KeyID}, string(decision.Phase), string(decision.Action), string(decision.Outcome), decision.LatencyMS)
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
			s.deps.Business.Tokens(labels, &u.InputTokens, &u.OutputTokens)
			if fact.Price != nil {
				if cost, ok := fact.Price.Cost(events.AttemptUsage(fact, env.Operation), fact.StartedAt, events.AttemptSuccessful(fact), fact.Discount); ok {
					// A committed attempt prices but owes nothing: record its
					// zero marginal cost rather than skipping the metric.
					if amount, err := strconv.ParseFloat(cost.String(), 64); err == nil {
						if x.factCommitted(fact) {
							amount = 0
						}
						s.deps.Business.Cost(labels, fact.Price.Currency, amount)
					}
				}
			}
		}
		if fact.FirstOutput != nil {
			elapsed := fact.StartedAt.Add(*fact.FirstOutput).Sub(env.StartedAt).Seconds()
			s.deps.Business.FirstToken(labels, elapsed)
			if u := fact.Usage; u != nil && fact.UsageObserved && u.OutputTokens > 0 {
				if seconds := fact.Duration.Seconds() - fact.FirstOutput.Seconds(); seconds > 0 {
					s.deps.Business.OutputRate(labels, u.OutputTokens, seconds)
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

func completionBody(out *outcome) []byte {
	if out == nil || out.completion == nil || len(out.completion.Body) == 0 {
		return nil
	}
	return out.completion.Body
}
