package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/usage"
)

func backgroundResponseRequested(parsed *openai.Request) bool {
	var background bool
	return json.Unmarshal(parsed.Field("background"), &background) == nil && background
}

// A background response may outlive its POST or stream. Keep a content-free
// accounting template on the resource so polling uses the generation's key,
// attribution, pricing and request identity.
func (s *Server) pendingResponseUsage(x *execution, fact *AttemptFact, metadata map[string]json.RawMessage) bool {
	if !backgroundResponseRequested(x.parsed) {
		return false
	}
	return s.pendingGenerationUsage(x, fact, metadata)
}

func (s *Server) pendingGenerationUsage(x *execution, fact *AttemptFact, metadata map[string]json.RawMessage) bool {
	copyFact := *fact
	copyFact.GenerationUsageDeferred = false
	copyFact.recordEvidence(true)
	// Native acceptance and financial evidence are already known. The original
	// HTTP exchange is still unfinished and may refuse its local projection.
	copyFact.Class = ""
	copyFact.Committed = false
	if fact.Interaction != nil {
		interaction := *fact.Interaction
		interaction.ClientState = usage.ClientUnobserved
		if x.exchange != nil {
			interaction.UpstreamState = string(x.exchange.state.evidence().Acceptance())
		} else if interaction.UpstreamState == usage.UpstreamNotSent {
			// The operation only creates this template after validating native
			// creation. Stream finish may not yet have copied its acceptance.
			interaction.UpstreamState = usage.UpstreamAccepted
		}
		copyFact.Interaction = &interaction
	}
	copyFact.UsageComplete, copyFact.BillingUncertain = false, true
	facts := append([]AttemptFact{}, x.facts...)
	if len(facts) > 0 && facts[len(facts)-1].Ordinal == fact.Ordinal {
		facts[len(facts)-1] = copyFact
	} else {
		facts = append(facts, copyFact)
	}
	now := s.now()
	ev := accountingEvent(Envelope{
		AccountingID: x.request.accountingID(), KeyID: x.keyID,
		BudgetGroupID: x.budgetGroupID, Attribution: x.attribution, EndUserDigest: x.endUserDigest,
		PolicyDecisions: x.policyDecisions, Route: x.named().Slug,
		Operation: x.family.Operation(), Surface: x.family.Surface(),
		RuntimeGenerationID: x.request.release.Snapshot.Generation.ID,
		StartedAt:           x.request.startedAt, CompletedAt: now,
		Duration: now.Sub(x.request.startedAt),
		Attempts: facts,
	})
	if ev == nil {
		return false
	}
	ev.HTTPOutcomePending = true
	encoded, err := usage.Encode(ev)
	if err != nil {
		return false
	}
	metadata["pending_usage"] = encoded
	x.generationHTTP = &generationHTTPFinalizer{event: ev}
	return true
}

type generationHTTPFinalizer struct {
	event   *usage.Event
	localID string
}

// Attach only the generation identity actually published by this execution.
// Reused native mappings may belong to another original request.
func attachGenerationHTTP(x *execution, resource *resources.Resource) error {
	if x.generationHTTP == nil || resource == nil {
		return errors.New("deferred generation HTTP owner is unavailable")
	}
	var metadata map[string]json.RawMessage
	if err := json.Unmarshal(resource.Metadata, &metadata); err != nil {
		return err
	}
	retained, err := usage.Decode(metadata["pending_usage"])
	if err != nil {
		return err
	}
	owner := x.generationHTTP
	if retained.EventID != owner.event.EventID || retained.RequestID != owner.event.RequestID || retained.APIKeyID != owner.event.APIKeyID ||
		resource.APIKeyID != owner.event.APIKeyID || owner.event.ProviderID == nil || resource.ProviderID != *owner.event.ProviderID || len(retained.Attempts) != len(owner.event.Attempts) {
		return errors.New("deferred generation HTTP identity does not own the retained resource")
	}
	for index := range retained.Attempts {
		if retained.Attempts[index].ID != owner.event.Attempts[index].ID {
			return errors.New("deferred generation HTTP attempt identity does not own the retained resource")
		}
	}
	owner.localID = resource.ID
	return nil
}

func (s *Server) finalizeGenerationHTTP(x *execution, env Envelope) {
	owner := x.generationHTTP
	if owner == nil || owner.localID == "" || s.Resources == nil || len(env.Attempts) == 0 {
		return
	}
	last := &owner.event.Attempts[len(owner.event.Attempts)-1]
	fact := &env.Attempts[len(env.Attempts)-1]
	if fact.Ordinal != last.Ordinal || fact.ProviderID != last.ProviderID || fact.UpstreamModel != last.UpstreamModel || !fact.StartedAt.Equal(last.StartedAt) {
		s.log.Warn("deferred generation HTTP outcome has a different final attempt", "request_id", env.RequestID)
		return
	}
	completion := usage.HTTPCompletion{
		EventID: owner.event.EventID, RequestID: owner.event.RequestID, APIKeyID: owner.event.APIKeyID,
		RequestStartedAt: owner.event.RequestStartedAt, CompletedAt: env.CompletedAt,
		StatusCode: optionalStatus(env.Status), ErrorClass: optionalText(env.ErrorClass),
		LatencyMS: milliseconds(env.Duration), FirstByteMS: optionalMilliseconds(env.FirstByte),
		BudgetBoundary: env.BudgetBoundary, PayloadCaptured: env.PayloadCaptured,
		Attempt: usage.HTTPAttemptCompletion{ID: last.ID, Ordinal: last.Ordinal, ProviderID: last.ProviderID, UpstreamModel: last.UpstreamModel,
			StartedAt: last.StartedAt, CompletedAt: fact.StartedAt.Add(max(fact.Duration, 0)), StatusCode: optionalStatus(fact.Status),
			Committed: fact.Committed, LatencyMS: milliseconds(fact.Duration), FirstByteMS: optionalMilliseconds(fact.FirstByte)},
	}
	if fact.Class != classSuccess {
		completion.Attempt.ErrorClass = optionalText(fact.Class)
	}
	if fact.Interaction != nil {
		completion.Attempt.ClientState = fact.Interaction.ClientState
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Resources.CompleteGenerationHTTP(ctx, owner.localID, completion); err != nil {
		// A crash or failed bounded finalization leaves explicit missing HTTP
		// metadata. Never manufacture success or replay its financial event.
		s.log.Warn("deferred generation HTTP outcome not recorded", "request_id", env.RequestID, "error", err)
	}
}

func (s *Server) reconcileResponse(ctx context.Context, localID string, body []byte) *Error {
	observation, err := openai.ObserveResponseUsage(body)
	if err != nil {
		return serverError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned malformed response usage.")
	}
	return s.applyResponseObservation(ctx, localID, observation)
}

func (s *Server) applyResponseObservation(ctx context.Context, localID string, observation openai.ResponseUsageObservation) *Error {
	evidence := usage.AttemptUsage{Observed: false, Complete: false, BillingUncertain: true}
	if observation.Usage != nil {
		evidence = observedAttemptUsage(observation.Usage, observation.Complete, "generation")
	}
	result, err := s.Resources.ObserveResponseUsage(ctx, localID, observation.Status, evidence, s.now())
	if err != nil {
		return serverError(http.StatusServiceUnavailable, "response_usage_unavailable", "The stored response usage could not be reconciled; retry the request.")
	}
	if s.Admission != nil && s.Admission.limiter != nil {
		for _, snapshot := range result.CostSnapshots {
			if _, _, err := s.Admission.limiter.ApplyCostSnapshot(ctx, snapshot); err != nil {
				s.log.Warn("response cost snapshot application failed; reconciliation will repair it", "error", err)
			}
		}
	}
	return nil
}
