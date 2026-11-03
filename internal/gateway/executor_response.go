package gateway

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"time"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/export"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/telemetry"
	"github.com/tyk-swe/olp/internal/usage"
)

// aggregate reads the stream that an upstream serving only streams answers a
// non-streaming request with, and returns the dialect's non-streaming result.
// The attempt records the usage the stream stated, also when it fails, as it
// would for a streaming caller.
func (s *Server) aggregate(fact *AttemptFact, resp *http.Response, cfg connectors.Config, encodedWire wire.Format) ([]byte, error) {
	if mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mediaType != "text/event-stream" {
		return nil, &wire.ProtocolError{Detail: "the forced stream is not an event stream"}
	}
	limit := int(s.cfg.MaxEventBytes)
	streamed, err := openai.Aggregate(encodedWire, cfg.StreamPayload(resp.Body, limit), limit, int(s.cfg.MaxResponseBytes))
	if streamed == nil {
		return nil, err
	}
	fact.observeUsage(streamed.Usage, streamed.UsageComplete)
	return streamed.Body, err
}

// countingReader enforces the buffered unary response byte limit.
type countingReader struct {
	r        io.Reader
	limit    int64
	read     int64
	exceeded bool
}

var errResponseTooLarge = errors.New("upstream response exceeds byte limit")

func (c *countingReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if c.exceeded {
		return 0, errResponseTooLarge
	}
	if c.read == c.limit {
		// Reaching the limit is valid if the upstream ends here. Read at most
		// one additional byte to distinguish EOF from an oversized response.
		var extra [1]byte
		n, err := c.r.Read(extra[:])
		if n > 0 {
			c.exceeded = true
			return 0, errResponseTooLarge
		}
		return 0, err
	}
	if int64(len(p)) > c.limit-c.read {
		p = p[:c.limit-c.read]
	}
	n, err := c.r.Read(p)
	c.read += int64(n)
	return n, err
}

// finish emits the terminal envelope exactly once.
func (s *Server) finish(x *call, out *outcome, status int) {
	x.once.Do(func() {
		s.storeCache(x, out, status)
		completedAt := s.now()
		x.finalizePinned(out, completedAt)
		env := Envelope{
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
			if x.request.release.Snapshot != nil {
				env.RuntimeGenerationID = x.request.release.Snapshot.Generation.ID
			}
		}
		if x.route != nil {
			env.Route = x.named().Slug
			env.RouteRevisionID = x.named().RevisionID
		}
		if x.origin != "" {
			env.Origin, env.ParentRequestID = x.origin, x.parent
			if usage.Keyless(x.origin) {
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
			env.BudgetBoundary = budgetBoundary(out.err)
		case out == nil && x.failure != nil:
			env.ErrorClass = x.failure.Code
			env.BudgetBoundary = budgetBoundary(x.failure)
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
				var snapshot *routing.Snapshot
				if x.historicalSnapshot != nil {
					snapshot = x.historicalSnapshot
				} else if x.request.release != nil {
					snapshot = x.request.release.Snapshot
				}
				if snapshot != nil {
					if provider, ok := snapshot.Providers[serving.ProviderID]; ok {
						providerKind = provider.Kind
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
			mode := "unary"
			if x.generation.parsed != nil && x.generation.parsed.Stream {
				mode = "streaming"
			} else if out != nil && out.completion != nil && len(out.completion.Body) != 0 {
				x.capture.CaptureResponse(x.family, out.completion.Body)
			} else {
				x.capture.CaptureResponse(x.family, nil)
			}
			var traceID, spanID string
			if context := x.request.trace.SpanContext(); context.IsValid() {
				traceID, spanID = context.TraceID().String(), context.SpanID().String()
			}
			env.PayloadCaptured = x.capture.FinishContext(x.captureContext, export.CaptureIdentity{
				Route: env.Route, ProjectID: x.captureProject(), KeyID: env.KeyID, EndUserDigest: env.EndUserDigest,
				TraceID: traceID, SpanID: spanID, Outcome: env.Outcome, Mode: mode,
			}, completedAt)
		}
		s.finalizeGenerationHTTP(x, env)
		s.recordBusiness(env, x)
		s.Sink.Terminal(env)
	})
}

func (x *call) captureInputDocument() map[string]json.RawMessage {
	var body []byte
	if len(x.facts) != 0 {
		fact := &x.facts[len(x.facts)-1]
		if record := x.targets[preparationKey{target: fact.TargetID, provider: fact.ProviderID, revision: fact.ProviderRevisionID, model: fact.UpstreamModel}]; record != nil {
			prepared := record.provider
			if document := prepared.invocation.Prepared.Document(); document.Valid() {
				body = document.Bytes()
			}
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

func (s *Server) recordBusiness(env Envelope, x *call) {
	if s.Business == nil {
		return
	}
	project := ""
	if x.authority.ProjectID != nil {
		project = *x.authority.ProjectID
	} else if x.route != nil && x.route.ProjectID != nil {
		project = *x.route.ProjectID
	}
	if x.cache != nil {
		s.Business.Cache(env.Route, x.cache.result, x.cache.storedBytes, x.cache.reason != "")
	}
	for _, decision := range env.GuardrailDecisions {
		s.Business.Guardrail(telemetry.BusinessLabels{Route: env.Route, Project: project, Key: env.KeyID}, decision.Phase, decision.Action, decision.Outcome, decision.LatencyMS)
	}
	var snapshot *routing.Snapshot
	if x.historicalSnapshot != nil {
		snapshot = x.historicalSnapshot
	} else if x.request.release != nil {
		snapshot = x.request.release.Snapshot
	}
	for index := range env.Attempts {
		fact := &env.Attempts[index]
		route := env.Route
		if fact.Leg != nil && fact.Leg.Route != "" {
			route = fact.Leg.Route
		}
		labels := telemetry.BusinessLabels{Route: route, Project: project, Key: env.KeyID, EndUser: env.EndUserDigest}
		if snapshot != nil {
			if provider, ok := snapshot.Providers[fact.ProviderID]; ok {
				labels.ProviderKind = provider.Kind
			}
		}
		if u := fact.Usage; u != nil && fact.UsageObserved {
			s.Business.Tokens(labels, &u.InputTokens, &u.OutputTokens)
			if fact.Price != nil {
				if cost, ok := fact.Price.Cost(attemptUsage(fact, env.Operation), fact.StartedAt, attemptSuccessful(fact), fact.Discount); ok {
					// A committed attempt prices but owes nothing: record its
					// zero marginal cost rather than skipping the metric.
					if amount, err := strconv.ParseFloat(cost.String(), 64); err == nil {
						if x.factCommitted(fact) {
							amount = 0
						}
						s.Business.Cost(labels, fact.Price.Currency, amount)
					}
				}
			}
		}
		if fact.FirstOutput != nil {
			elapsed := fact.StartedAt.Add(*fact.FirstOutput).Sub(env.StartedAt).Seconds()
			s.Business.FirstToken(labels, elapsed)
			if u := fact.Usage; u != nil && fact.UsageObserved && u.OutputTokens > 0 {
				if seconds := fact.Duration.Seconds() - fact.FirstOutput.Seconds(); seconds > 0 {
					s.Business.OutputRate(labels, u.OutputTokens, seconds)
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

func (x *call) captureDeliveredFrame(frame []byte) {
	x.capture.CaptureStreamFrame(x.family, frame)
}
