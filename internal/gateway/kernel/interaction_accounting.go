package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/tyk-swe/olp/internal/domain/geminilifecycle"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/usage"
	"github.com/tyk-swe/olp/internal/usage/ingest"
)

// Terminal observations belong to the original generation. The resource owner
// serializes competing GETs, cancellations and resumed streams across replicas.
func (s *Server) observeGeminiInteraction(ctx context.Context, owner, localID, status string, object oif.Value) (ingest.Persisted, error) {
	evidence := usage.AttemptUsage{Evidence: usage.EvidenceUnobservedUncertain}
	if observed := geminiInteractionUsage(object); observed != nil {
		evidence = events.ObservedUsage(observed, geminilifecycle.InteractionTurnFinished(status), "generation")
	}
	result, err := s.Deps.Resources.ObserveInteraction(ctx, owner, localID, status, evidence, s.Now())
	if err != nil {
		return ingest.Persisted{}, err
	}
	if err := s.Deps.Admission.InstallSnapshots(ctx, result.CostSnapshots); err != nil {
		s.log.Warn("interaction cost snapshot application failed; reconciliation will repair it", "error_type", "cost_snapshot")
	}
	return result, nil
}

func (s *Server) storeGeminiInteraction(ctx context.Context, exchange *pinnedExchange, p *Pin, owner, upstreamID, state string, parent *resources.Resource, deferUsage bool) (*resources.Resource, error) {
	x := exchange.request
	version := resources.InteractionContract
	expiry := s.Now().Add(resources.ContinuationLifetime - time.Minute)
	model, _ := json.Marshal(p.Model)
	metadataFields := map[string]json.RawMessage{"upstream_model": model}
	if deferUsage && !s.pendingGenerationUsage(x, exchange.fact(), metadataFields) {
		return nil, errors.New("Interaction generation accounting template unavailable")
	}
	metadata, err := json.Marshal(metadataFields)
	if err != nil {
		return nil, err
	}
	r := &resources.Resource{Kind: resources.KindInteraction, APIKeyID: owner, RouteSlug: x.Route.Slug,
		ProviderID: p.Provider.ID, ProviderRevisionID: p.Provider.RevisionID, RouteRevisionID: x.Route.RevisionID,
		TargetID: p.Target.ID, SlotID: p.Slot.ID, CredentialID: p.Slot.CredentialID, State: state, Metadata: metadata,
		ContractVersion: &version, ExpiresAt: &expiry}
	if parent != nil {
		r.ParentID = &parent.UUID
	}
	commitCtx, stopCommit := ResourceCommitContext(ctx)
	defer stopCommit()
	stored, err := s.Deps.Resources.PutInteractionContract(commitCtx, r, upstreamID)
	if err == nil && deferUsage {
		if err = attachGenerationHTTP(x, stored); err != nil {
			return nil, err
		}
		exchange.fact().GenerationUsageDeferred = true
		exchange.recordEvidence()
	}
	return stored, err
}

func recordGeminiInteractionUsage(exchange *pinnedExchange, object oif.Value) {
	if exchange.request.actor == "api_key_resource" {
		return
	}
	observed := geminiInteractionUsage(object)
	if observed == nil {
		return
	}
	fact := exchange.fact()
	statusValue, _ := object.Lookup("status")
	status, _ := statusValue.Text()
	complete := geminilifecycle.InteractionTurnFinished(status)
	fact.ObserveUsage(observed, complete)
	exchange.recordEvidence()
}

func geminiInteractionUsage(object oif.Value) *wire.Usage {
	usageValue, ok := object.Lookup("usage")
	if !ok || usageValue.Kind() != oif.Object {
		return nil
	}
	var native struct {
		Input            *int64                            `json:"total_input_tokens"`
		Output           *int64                            `json:"total_output_tokens"`
		Cached           *int64                            `json:"total_cached_tokens"`
		Thoughts         *int64                            `json:"total_thought_tokens"`
		ToolUse          *int64                            `json:"total_tool_use_tokens"`
		Total            *int64                            `json:"total_tokens"`
		PromptDetails    []geminilifecycle.LiveUsageDetail `json:"prompt_tokens_details"`
		CacheDetails     []geminilifecycle.LiveUsageDetail `json:"cache_tokens_details"`
		CandidateDetails []geminilifecycle.LiveUsageDetail `json:"candidates_tokens_details"`
		ResponseDetails  []geminilifecycle.LiveUsageDetail `json:"response_tokens_details"`
	}
	if json.Unmarshal(usageValue.Bytes(), &native) != nil || native.Input == nil || native.Output == nil || *native.Input < 0 || *native.Output < 0 || native.Cached != nil && (*native.Cached < 0 || *native.Cached > *native.Input) {
		return nil
	}
	usage := geminiExtendedUsage(*native.Input, *native.Output, native.Cached, native.Thoughts, native.ToolUse, native.Total)
	if usage == nil {
		return nil
	}
	if audio, image := geminiUsageDetails(native.PromptDetails); audio > 0 || image > 0 {
		if audio > 0 {
			usage.AudioInputTokens = &audio
		}
		if image > 0 {
			usage.ImageInputTokens = &image
		}
	}
	if audio, image := geminiUsageDetails(native.CacheDetails); audio > 0 || image > 0 {
		if audio > 0 {
			usage.CachedAudioInputTokens = &audio
		}
		if image > 0 {
			usage.CachedImageInputTokens = &image
		}
	}
	if audio, image := geminiUsageDetails(native.CandidateDetails); audio > 0 || image > 0 {
		if audio > 0 {
			usage.AudioOutputTokens = &audio
		}
		if image > 0 {
			usage.ImageOutputTokens = &image
		}
	}
	if audio, image := geminiUsageDetails(native.ResponseDetails); audio > 0 || image > 0 {
		if audio > 0 {
			usage.AudioOutputTokens = &audio
		}
		if image > 0 {
			usage.ImageOutputTokens = &image
		}
	}
	return usage
}
