package geminilive

import (
	"github.com/tyk-swe/olp/internal/domain/geminilifecycle"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/gateway/quota"
)

// mergeUsageShare accumulates one optional usage share across turns.
func mergeUsageShare(slot **int64, value *int64) {
	if value == nil {
		return
	}
	total := *value
	if *slot != nil {
		total = quota.AddBounded(total, **slot)
	}
	*slot = &total
}

// geminiLiveModality applies a Live frame's detail arrays to its usage.
func geminiLiveModality(u *wire.Usage, native *geminilifecycle.LiveUsage) {
	if u == nil {
		return
	}
	if audio, image := kernel.GeminiUsageDetails(native.PromptDetails); audio > 0 || image > 0 {
		if audio > 0 {
			u.AudioInputTokens = &audio
		}
		if image > 0 {
			u.ImageInputTokens = &image
		}
	}
	if audio, image := kernel.GeminiUsageDetails(native.CacheDetails); audio > 0 || image > 0 {
		if audio > 0 {
			u.CachedAudioInputTokens = &audio
		}
		if image > 0 {
			u.CachedImageInputTokens = &image
		}
	}
	if audio, image := kernel.GeminiUsageDetails(native.ResponseDetails); audio > 0 || image > 0 {
		if audio > 0 {
			u.AudioOutputTokens = &audio
		}
		if image > 0 {
			u.ImageOutputTokens = &image
		}
	}
}
