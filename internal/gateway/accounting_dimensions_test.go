package gateway

import (
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/wire"
)

func ptr[T any](v T) *T { return &v }

// A provider whose modality detail contradicts its totals bills nothing under
// that detail: the shares drop and the attempt settles incomplete.
func TestAttemptUsageDropsInconsistentModalityDetail(t *testing.T) {
	for _, tc := range []struct {
		name  string
		usage *wire.Usage
	}{
		{name: "audio beyond input", usage: &wire.Usage{InputTokens: 10, OutputTokens: 5, AudioInputTokens: ptr(int64(11))}},
		{name: "cached audio beyond audio", usage: &wire.Usage{InputTokens: 10, OutputTokens: 5, AudioInputTokens: ptr(int64(2)), CachedAudioInputTokens: ptr(int64(3))}},
		{name: "cached shares beyond cached", usage: &wire.Usage{InputTokens: 10, OutputTokens: 5, CachedInputTokens: ptr(int64(2)), CachedAudioInputTokens: ptr(int64(2)), CachedImageInputTokens: ptr(int64(2))}},
		{name: "output shares beyond output", usage: &wire.Usage{InputTokens: 10, OutputTokens: 5, AudioOutputTokens: ptr(int64(4)), ImageOutputTokens: ptr(int64(2))}},
		{name: "bad tool name", usage: &wire.Usage{InputTokens: 10, OutputTokens: 5, ToolCalls: map[string]int64{"UPPER": 1}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fact := &AttemptFact{UsageObserved: true, UsageComplete: true, Usage: tc.usage}
			evidence := attemptUsage(fact, "generation")
			if evidence.Complete || evidence.AudioInputTokens != nil || evidence.ImageInputTokens != nil ||
				evidence.AudioOutputTokens != nil || evidence.CachedAudioInputTokens != nil ||
				evidence.CachedImageInputTokens != nil || evidence.ToolCalls != nil {
				t.Fatalf("inconsistent detail survived: %+v", evidence)
			}
			if evidence.InputTokens == nil || *evidence.InputTokens != 10 {
				t.Fatalf("the totals themselves are still evidence: %+v", evidence)
			}
		})
	}
}

func TestAttemptUsageKeepsConsistentModalityDetail(t *testing.T) {
	fact := &AttemptFact{UsageObserved: true, UsageComplete: true, Usage: &wire.Usage{
		InputTokens: 20, OutputTokens: 10, CachedInputTokens: ptr(int64(5)),
		AudioInputTokens: ptr(int64(7)), CachedAudioInputTokens: ptr(int64(3)),
		ImageInputTokens: ptr(int64(2)), CachedImageInputTokens: ptr(int64(1)),
		AudioOutputTokens: ptr(int64(4)), ImageOutputTokens: ptr(int64(2)),
		ToolCalls: map[string]int64{"web_search": 2},
	}}
	evidence := attemptUsage(fact, "generation")
	if !evidence.Complete || evidence.AudioInputTokens == nil || *evidence.AudioInputTokens != 7 ||
		evidence.CachedAudioInputTokens == nil || *evidence.CachedAudioInputTokens != 3 ||
		evidence.ImageInputTokens == nil || *evidence.ImageInputTokens != 2 ||
		evidence.CachedImageInputTokens == nil || *evidence.CachedImageInputTokens != 1 ||
		evidence.AudioOutputTokens == nil || *evidence.AudioOutputTokens != 4 ||
		evidence.ImageOutputTokens == nil || *evidence.ImageOutputTokens != 2 ||
		len(evidence.ToolCalls) != 1 || evidence.ToolCalls["web_search"] != 2 {
		t.Fatalf("consistent detail was dropped: %+v", evidence)
	}
}
