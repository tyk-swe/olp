package resources

import (
	"math/big"

	"github.com/tyk-swe/olp/internal/usage"
)

// observePendingUsage keeps one provider-reported snapshot, never a tuple made
// from maxima across different observations. Native complete evidence is the
// protocol owner's authority; earlier snapshots are provisional lower bounds.
func observePendingUsage(event *usage.Event, evidence usage.AttemptUsage, terminal bool) error {
	if len(event.Attempts) == 0 {
		return ErrContract
	}
	if !terminal {
		evidence.Complete, evidence.BillingUncertain = false, true
	}
	// Validate the incoming evidence even if selection will discard it. A stale
	// observation must not conceal malformed accounting state.
	candidate := *event
	candidate.Attempts = append([]usage.Attempt(nil), event.Attempts...)
	setPendingUsage(&candidate, evidence)
	if _, err := usage.Validate(&candidate); err != nil {
		return err
	}
	if !terminal || !evidence.Observed || !evidence.Complete || evidence.BillingUncertain {
		previous := event.Attempts[len(event.Attempts)-1].Usage
		if previous != nil && previous.Observed && (!evidence.Observed || !usageSnapshotDominates(evidence, *previous)) {
			evidence = *previous
		}
		evidence.Complete, evidence.BillingUncertain = false, true
	}
	setPendingUsage(event, evidence)
	return nil
}

func setPendingUsage(event *usage.Event, evidence usage.AttemptUsage) {
	event.Attempts[len(event.Attempts)-1].Usage = &evidence
	event.InputTokens, event.OutputTokens = evidence.InputTokens, evidence.OutputTokens
	event.CachedInputTokens, event.CacheWriteInputTokens = evidence.CachedInputTokens, evidence.CacheWriteInputTokens
	event.CacheWrite5MInputTokens, event.CacheWrite1HInputTokens = evidence.CacheWrite5MInputTokens, evidence.CacheWrite1HInputTokens
	event.AudioInputTokens, event.CachedAudioInputTokens, event.AudioOutputTokens = evidence.AudioInputTokens, evidence.CachedAudioInputTokens, evidence.AudioOutputTokens
	event.ImageInputTokens, event.CachedImageInputTokens, event.ImageOutputTokens = evidence.ImageInputTokens, evidence.CachedImageInputTokens, evidence.ImageOutputTokens
	event.ToolCalls, event.Batch = evidence.ToolCalls, evidence.Batch
	event.MediaUnits, event.UsageComplete = evidence.MediaUnits, evidence.Complete
}

// A partial snapshot covers another only if it preserves each known dimension
// and does not reduce a known cache partition. Missing fields are unknown;
// explicit zero remains evidence. This comparison never changes either tuple.
func usageSnapshotDominates(next, previous usage.AttemptUsage) bool {
	nextCounts := [...]*int64{next.InputTokens, next.OutputTokens, next.CachedInputTokens,
		next.CacheWriteInputTokens, next.CacheWrite5MInputTokens, next.CacheWrite1HInputTokens,
		next.AudioInputTokens, next.CachedAudioInputTokens, next.AudioOutputTokens,
		next.ImageInputTokens, next.CachedImageInputTokens, next.ImageOutputTokens}
	previousCounts := [...]*int64{previous.InputTokens, previous.OutputTokens, previous.CachedInputTokens,
		previous.CacheWriteInputTokens, previous.CacheWrite5MInputTokens, previous.CacheWrite1HInputTokens,
		previous.AudioInputTokens, previous.CachedAudioInputTokens, previous.AudioOutputTokens,
		previous.ImageInputTokens, previous.CachedImageInputTokens, previous.ImageOutputTokens}
	for index, before := range previousCounts {
		if before != nil && (nextCounts[index] == nil || *nextCounts[index] < *before) {
			return false
		}
	}
	for tool, count := range previous.ToolCalls {
		if next.ToolCalls[tool] < count {
			return false
		}
	}
	if previous.MediaUnits != nil {
		if next.MediaUnits == nil {
			return false
		}
		before, beforeOK := new(big.Rat).SetString(*previous.MediaUnits)
		after, afterOK := new(big.Rat).SetString(*next.MediaUnits)
		if !beforeOK || !afterOK || after.Cmp(before) < 0 {
			return false
		}
	}
	// Compare only explicitly known partitions. For example, a rising input
	// total and cache total can still report fewer noncached input tokens.
	for _, partition := range [][]int{{0, 2}, {0, 3}, {0, 2, 3}, {3, 4}, {3, 5}, {3, 4, 5}} {
		before, after := int64(0), int64(0)
		known := true
		for offset, index := range partition {
			if previousCounts[index] == nil || nextCounts[index] == nil {
				known = false
				break
			}
			if offset == 0 {
				before, after = *previousCounts[index], *nextCounts[index]
			} else {
				before -= *previousCounts[index]
				after -= *nextCounts[index]
			}
		}
		if known && after < before {
			return false
		}
	}
	return true
}
