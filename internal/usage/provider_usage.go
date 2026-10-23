package usage

import (
	"github.com/tyk-swe/olp/internal/protocols/wire"
)

// ObservedAttemptUsage converts provider counters to the accounting dimensions
// shared by live requests and deferred results.
func ObservedAttemptUsage(u *wire.Usage, complete bool, operation string) AttemptUsage {
	if u == nil {
		return AttemptUsage{BillingUncertain: true}
	}
	evidence := AttemptUsage{
		Observed:         true,
		Complete:         complete,
		BillingUncertain: !complete,
	}
	// Retained control and search calls consume no tokens: an empty
	// counter set is tokenless evidence, not explicit zero token
	// dimensions, so a per-request-only price can still apply.
	tokenless := u.MediaUnits == nil && u.InputTokens == 0 && u.OutputTokens == 0 && u.CachedInputTokens == nil &&
		(operation == "fine_tuning" || operation == "vector_store" || operation == "retrieval" || operation == "conversation" ||
			operation == "container" || operation == "eval" || operation == "skill" || operation == "context_cache" || operation == "agent_resource")
	if u != nil {
		if !tokenless && (u.MediaUnits == nil || u.InputTokens != 0 || u.OutputTokens != 0 || u.CachedInputTokens != nil) {
			input, output := max(u.InputTokens, 0), max(u.OutputTokens, 0)
			evidence.InputTokens, evidence.OutputTokens = &input, &output
		}
		if u.CachedInputTokens != nil {
			cached := max(*u.CachedInputTokens, 0)
			evidence.CachedInputTokens = &cached
		}
		evidence.CacheWriteInputTokens = u.CacheWriteInputTokens
		evidence.CacheWrite5MInputTokens = u.CacheWrite5MInputTokens
		evidence.CacheWrite1HInputTokens = u.CacheWrite1HInputTokens
		evidence.MediaUnits = u.MediaUnits
		evidence.AudioInputTokens = u.AudioInputTokens
		evidence.CachedAudioInputTokens = u.CachedAudioInputTokens
		evidence.AudioOutputTokens = u.AudioOutputTokens
		evidence.ImageInputTokens = u.ImageInputTokens
		evidence.CachedImageInputTokens = u.CachedImageInputTokens
		evidence.ImageOutputTokens = u.ImageOutputTokens
		evidence.ToolCalls = u.ToolCalls
	}
	if operation == "embeddings" {
		evidence.OutputTokens = nil
	}
	if !ConsistentUsage(&evidence) {
		evidence.CachedAudioInputTokens = nil
		evidence.AudioInputTokens = nil
		evidence.AudioOutputTokens = nil
		evidence.CachedImageInputTokens = nil
		evidence.ImageInputTokens = nil
		evidence.ImageOutputTokens = nil
		evidence.ToolCalls = nil
		evidence.Complete = false
	}
	return evidence
}
