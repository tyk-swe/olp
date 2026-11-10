package usage

import (
	"maps"
	"math/big"
	"slices"
	"time"
)

// observePendingUsage keeps one provider-reported snapshot, never a tuple made
// from maxima across different observations. Native complete evidence is the
// protocol owner's authority; earlier snapshots are provisional lower bounds.
func observePendingUsage(event *Event, evidence AttemptUsage, terminal bool) error {
	if len(event.Attempts) == 0 {
		return ErrInvalidEvent
	}
	if !terminal {
		evidence.Complete, evidence.BillingUncertain = false, true
	}
	// Validate the incoming evidence even if selection will discard it. A stale
	// observation must not conceal malformed accounting state.
	candidate := *event
	candidate.Attempts = append([]Attempt(nil), event.Attempts...)
	setPendingUsage(&candidate, evidence)
	if _, err := Validate(&candidate); err != nil {
		return err
	}
	if !terminal || !evidence.Observed || !evidence.Complete || evidence.BillingUncertain {
		previous := event.Attempts[len(event.Attempts)-1].Usage
		if previous != nil && previous.Observed && (!evidence.Observed || !usageSnapshotDominates(evidence, *previous)) {
			evidence = *previous
		}
		evidence.Complete, evidence.BillingUncertain = false, true
	}
	setPendingUsage(event, clonePendingUsage(evidence))
	return nil
}

func setPendingUsage(event *Event, evidence AttemptUsage) {
	event.Attempts[len(event.Attempts)-1].Usage = &evidence
	event.InputTokens, event.OutputTokens = evidence.InputTokens, evidence.OutputTokens
	event.CachedInputTokens, event.CacheWriteInputTokens = evidence.CachedInputTokens, evidence.CacheWriteInputTokens
	event.CacheWrite5MInputTokens, event.CacheWrite1HInputTokens = evidence.CacheWrite5MInputTokens, evidence.CacheWrite1HInputTokens
	event.AudioInputTokens, event.CachedAudioInputTokens, event.AudioOutputTokens = evidence.AudioInputTokens, evidence.CachedAudioInputTokens, evidence.AudioOutputTokens
	event.ImageInputTokens, event.CachedImageInputTokens, event.ImageOutputTokens = evidence.ImageInputTokens, evidence.CachedImageInputTokens, evidence.ImageOutputTokens
	event.ToolCalls, event.Batch = evidence.ToolCalls, evidence.Batch
	event.TrainingTokens = evidence.TrainingTokens
	eveStorage := evidence.StorageByteSeconds
	event.StorageByteSeconds = eveStorage
	event.CacheTokenSeconds = evidence.CacheTokenSeconds
	event.ContainerSessions = evidence.ContainerSessions
	event.MediaUnits, event.UsageComplete = evidence.MediaUnits, evidence.Complete
}

// A partial snapshot covers another only if it preserves each known dimension
// and does not reduce a known cache partition. Missing fields are unknown;
// explicit zero remains evidence. This comparison never changes either tuple.
func usageSnapshotDominates(next, previous AttemptUsage) bool {
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
func usagePartitions(u AttemptUsage) [7][5]*int64 {
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

type GenerationState uint8

const (
	GenerationPending GenerationState = iota
	GenerationTerminal
	GenerationUnknown
)

type PendingGeneration struct {
	event *Event
}

func DecodePendingGeneration(raw []byte) (PendingGeneration, error) {
	event, err := Decode(raw)
	if err != nil {
		return PendingGeneration{}, err
	}
	if _, err := Validate(event); err != nil {
		return PendingGeneration{}, err
	}
	if len(event.Attempts) == 0 {
		return PendingGeneration{}, ErrInvalidEvent
	}
	return PendingGeneration{event: event}, nil
}

func (p PendingGeneration) copyEvent() *Event {
	event := *p.event
	event.Attempts = slices.Clone(p.event.Attempts)
	final := &event.Attempts[len(event.Attempts)-1]
	if final.Routing != nil {
		routing := *final.Routing
		final.Routing = &routing
		if routing.Interaction != nil {
			interaction := *routing.Interaction
			routing.Interaction = &interaction
		}
	}
	return &event
}

func (p PendingGeneration) Observe(evidence AttemptUsage, state GenerationState) (PendingGeneration, error) {
	event := p.copyEvent()
	if err := observePendingUsage(event, evidence, state == GenerationTerminal); err != nil {
		return PendingGeneration{}, err
	}
	return PendingGeneration{event: event}, nil
}

func (p PendingGeneration) CompleteHTTP(completion HTTPCompletion) (PendingGeneration, error) {
	event := p.copyEvent()
	if err := completePendingHTTP(event, completion); err != nil {
		return PendingGeneration{}, err
	}
	return PendingGeneration{event: event}, nil
}

func (p PendingGeneration) Encode() ([]byte, error) { return Encode(p.event) }

func (p PendingGeneration) AccountingEvent(now time.Time, state GenerationState) (AccountingEvent, error) {
	event := p.copyEvent()
	event.ObservedAt = now
	final := event.Attempts[len(event.Attempts)-1]
	if final.Routing != nil && final.Routing.Interaction != nil {
		if state == GenerationTerminal {
			final.Routing.Interaction.UpstreamState = UpstreamTerminal
		} else {
			final.Routing.Interaction.UpstreamState = UpstreamUnknown
		}
	}
	return NewAccountingEvent(event)
}

func (event Event) WithFinalUsage(evidence AttemptUsage) *Event {
	owned := event
	owned.Attempts = slices.Clone(event.Attempts)
	setPendingUsage(&owned, clonePendingUsage(evidence))
	return &owned
}

func clonePendingQuantity[T int64 | string](value *T) *T {
	if value == nil {
		return nil
	}
	return new(*value)
}

func clonePendingUsage(evidence AttemptUsage) AttemptUsage {
	return AttemptUsage{Observed: evidence.Observed, Complete: evidence.Complete,
		BillingUncertain: evidence.BillingUncertain, Deferred: evidence.Deferred,
		UsageQuantities: UsageQuantities{
			InvocationCount:         clonePendingQuantity(evidence.InvocationCount),
			TrainingTokens:          clonePendingQuantity(evidence.TrainingTokens),
			StorageByteSeconds:      clonePendingQuantity(evidence.StorageByteSeconds),
			CacheTokenSeconds:       clonePendingQuantity(evidence.CacheTokenSeconds),
			ContainerSessions:       maps.Clone(evidence.ContainerSessions),
			InputTokens:             clonePendingQuantity(evidence.InputTokens),
			OutputTokens:            clonePendingQuantity(evidence.OutputTokens),
			CachedInputTokens:       clonePendingQuantity(evidence.CachedInputTokens),
			CacheWriteInputTokens:   clonePendingQuantity(evidence.CacheWriteInputTokens),
			CacheWrite5MInputTokens: clonePendingQuantity(evidence.CacheWrite5MInputTokens),
			CacheWrite1HInputTokens: clonePendingQuantity(evidence.CacheWrite1HInputTokens),
			AudioInputTokens:        clonePendingQuantity(evidence.AudioInputTokens),
			CachedAudioInputTokens:  clonePendingQuantity(evidence.CachedAudioInputTokens),
			AudioOutputTokens:       clonePendingQuantity(evidence.AudioOutputTokens),
			ImageInputTokens:        clonePendingQuantity(evidence.ImageInputTokens),
			CachedImageInputTokens:  clonePendingQuantity(evidence.CachedImageInputTokens),
			ImageOutputTokens:       clonePendingQuantity(evidence.ImageOutputTokens),
			MediaUnits:              clonePendingQuantity(evidence.MediaUnits),
			ToolCalls:               maps.Clone(evidence.ToolCalls), Batch: evidence.Batch,
		},
	}
}
