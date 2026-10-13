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
	// Subtract the same known shares from each total. Every share must also
	// be nondecreasing, so preserving this residual preserves the residual
	// for every subset of those shares. Newly reported shares remain unknown
	// in the earlier observation and cannot narrow that comparison.
	nextPartitions, previousPartitions := usagePartitions(next), usagePartitions(previous)
	for index, partition := range previousPartitions {
		if partition[0] == nil {
			continue
		}
		before, after := *partition[0], *nextPartitions[index][0]
		for share := 1; share < len(partition); share++ {
			if partition[share] == nil {
				continue
			}
			if nextPartitions[index][share] == nil {
				return false
			}
			before -= *partition[share]
			after -= *nextPartitions[index][share]
		}
		if after < before {
			return false
		}
	}
	return true
}

// Each row is a total followed by disjoint shares. The residuals cover text,
// cached text, noncached audio/image, and cache writes without a known lifetime.
func usagePartitions(u usage.AttemptUsage) [7][5]*int64 {
	noncached := func(total, cached *int64) *int64 {
		if total == nil || cached == nil {
			return nil
		}
		value := *total - *cached
		return &value
	}
	return [7][5]*int64{
		{u.InputTokens, u.CachedInputTokens, u.CacheWriteInputTokens,
			noncached(u.AudioInputTokens, u.CachedAudioInputTokens), noncached(u.ImageInputTokens, u.CachedImageInputTokens)},
		{u.InputTokens, u.AudioInputTokens, u.ImageInputTokens},
		{u.CachedInputTokens, u.CachedAudioInputTokens, u.CachedImageInputTokens},
		{u.OutputTokens, u.AudioOutputTokens, u.ImageOutputTokens},
		{u.AudioInputTokens, u.CachedAudioInputTokens},
		{u.ImageInputTokens, u.CachedImageInputTokens},
		{u.CacheWriteInputTokens, u.CacheWrite5MInputTokens, u.CacheWrite1HInputTokens},
	}
}
