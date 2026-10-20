package usage

import (
	"github.com/tyk-swe/olp/internal/protocols/wire"
)

// ObservedAttemptUsage converts provider counters to the accounting dimensions
// shared by live requests and deferred results.
func ObservedAttemptUsage(u *wire.Usage, complete bool, operation string) AttemptUsage {
	evidence := AttemptUsage{
		Observed:         true,
		Complete:         complete,
		BillingUncertain: !complete,
	}
	if u != nil {
		if u.MediaUnits == nil || u.InputTokens != 0 || u.OutputTokens != 0 || u.CachedInputTokens != nil {
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
