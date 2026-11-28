package geminiinteractions

import (
	"context"
	"encoding/json"

	"github.com/tyk-swe/olp/internal/domain/geminilifecycle"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/usage"
)

type snapshotInstaller interface {
	InstallSnapshots(context.Context, []limits.Snapshot) error
}

// Terminal observations belong to the original generation. The resource owner
// serializes competing GETs, cancellations and resumed streams across replicas.
func (s surface) observeInteraction(ctx context.Context, owner, localID, status string, object oif.Value) error {
	result, err := s.store.ObserveInteraction(ctx, owner, localID, status, terminalEvidence(status, object), s.kernel.Now())
	if err != nil {
		return err
	}
	if err := s.admission.InstallSnapshots(ctx, result.CostSnapshots); err != nil {
		s.kernel.Log.Warn("interaction cost snapshot application failed; reconciliation will repair it", "error_type", "cost_snapshot")
	}
	return nil
}

func terminalEvidence(status string, object oif.Value) usage.AttemptUsage {
	observed := interactionUsage(object)
	if observed == nil {
		return usage.AttemptUsage{Evidence: usage.EvidenceUnobservedUncertain}
	}
	return usage.ObservedAttemptUsage(observed, geminilifecycle.InteractionTurnFinished(status), "generation")
}

func (t turn) recordInteractionUsage(exchange *kernel.PinnedExchange, object oif.Value) {
	if t.x.ResourceCall() {
		return
	}
	observed := interactionUsage(object)
	if observed == nil {
		return
	}
	statusValue, _ := object.Lookup("status")
	status, _ := statusValue.Text()
	exchange.ObserveUsage(observed, geminilifecycle.InteractionTurnFinished(status))
	exchange.RecordEvidence()
}

func interactionUsage(object oif.Value) *wire.Usage {
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
	extended := kernel.GeminiExtendedUsage(*native.Input, *native.Output, native.Cached, native.Thoughts, native.ToolUse, native.Total)
	if extended == nil {
		return nil
	}
	if audio, image := kernel.GeminiUsageDetails(native.PromptDetails); audio > 0 || image > 0 {
		if audio > 0 {
			extended.AudioInputTokens = &audio
		}
		if image > 0 {
			extended.ImageInputTokens = &image
		}
	}
	if audio, image := kernel.GeminiUsageDetails(native.CacheDetails); audio > 0 || image > 0 {
		if audio > 0 {
			extended.CachedAudioInputTokens = &audio
		}
		if image > 0 {
			extended.CachedImageInputTokens = &image
		}
	}
	if audio, image := kernel.GeminiUsageDetails(native.CandidateDetails); audio > 0 || image > 0 {
		if audio > 0 {
			extended.AudioOutputTokens = &audio
		}
		if image > 0 {
			extended.ImageOutputTokens = &image
		}
	}
	if audio, image := kernel.GeminiUsageDetails(native.ResponseDetails); audio > 0 || image > 0 {
		if audio > 0 {
			extended.AudioOutputTokens = &audio
		}
		if image > 0 {
			extended.ImageOutputTokens = &image
		}
	}
	return extended
}
