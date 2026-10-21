package resources

import (
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/usage"
)

func usageCount(value int64) *int64   { return &value }
func usageUnits(value string) *string { return &value }

func partialUsage(input, output int64) usage.AttemptUsage {
	return usage.AttemptUsage{Observed: true, BillingUncertain: true,
		InputTokens: usageCount(input), OutputTokens: usageCount(output)}
}

func pendingUsageEvent(evidence usage.AttemptUsage) *usage.Event {
	now := time.Now().UTC()
	provider, model := uuid.NewString(), "model"
	event := &usage.Event{Version: usage.WireVersion, EventID: uuid.NewString(), RequestID: uuid.NewString(),
		RuntimeGenerationID: uuid.NewString(), APIKeyID: uuid.NewString(), ProviderID: &provider,
		RouteSlug: "route", UpstreamModel: &model, Operation: "generation", Surface: "gemini",
		RequestStartedAt: now, RequestCompletedAt: now, ObservedAt: now, Committed: true,
		Attempts: []usage.Attempt{{ID: uuid.NewString(), Ordinal: 1, ProviderID: provider,
			UpstreamModel: model, StartedAt: now, CompletedAt: now, Committed: true}}}
	setPendingUsage(event, evidence)
	return event
}

func TestPendingUsagePreservesKnownDimensionsAndCachePartitions(t *testing.T) {
	previous := partialUsage(100, 10)
	previous.CachedInputTokens, previous.CacheWriteInputTokens = usageCount(20), usageCount(30)
	previous.CacheWrite5MInputTokens, previous.CacheWrite1HInputTokens = usageCount(10), usageCount(5)
	previous.AudioInputTokens, previous.CachedAudioInputTokens = usageCount(8), usageCount(4)
	previous.ImageOutputTokens = usageCount(2)
	previous.ToolCalls = map[string]int64{"web_search": 2}
	previous.MediaUnits = usageUnits("1.000001")
	cases := []struct {
		name    string
		change  func(*usage.AttemptUsage)
		advance bool
	}{
		{"missing cached evidence", func(u *usage.AttemptUsage) { u.CachedInputTokens, u.CachedAudioInputTokens = nil, nil }, false},
		{"missing write detail", func(u *usage.AttemptUsage) { u.CacheWrite1HInputTokens = nil }, false},
		{"missing audio evidence", func(u *usage.AttemptUsage) { u.AudioInputTokens, u.CachedAudioInputTokens = nil, nil }, false},
		{"tool call regression", func(u *usage.AttemptUsage) { u.ToolCalls = map[string]int64{"web_search": 1} }, false},
		{"missing media evidence", func(u *usage.AttemptUsage) { u.MediaUnits = nil }, false},
		{"cache repartition", func(u *usage.AttemptUsage) { u.CachedInputTokens = usageCount(30) }, false},
		{"write repartition", func(u *usage.AttemptUsage) { u.CacheWrite5MInputTokens = usageCount(15) }, false},
		{"media regression", func(u *usage.AttemptUsage) { u.MediaUnits = usageUnits("1.000000") }, false},
		{"equal exact decimal", func(u *usage.AttemptUsage) { u.MediaUnits = usageUnits("01.000001") }, true},
		{"all dimensions advance", func(u *usage.AttemptUsage) {
			u.InputTokens, u.OutputTokens = usageCount(120), usageCount(12)
			u.CachedInputTokens, u.CacheWriteInputTokens = usageCount(25), usageCount(35)
			u.CacheWrite5MInputTokens, u.CacheWrite1HInputTokens = usageCount(12), usageCount(6)
			u.AudioInputTokens, u.CachedAudioInputTokens = usageCount(10), usageCount(5)
			u.ImageOutputTokens = usageCount(3)
			u.ToolCalls = map[string]int64{"web_search": 3}
			u.MediaUnits = usageUnits("1.000002")
		}, true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			next := previous
			test.change(&next)
			event := pendingUsageEvent(previous)
			if err := observePendingUsage(event, next, false); err != nil {
				t.Fatal(err)
			}
			want := previous
			if test.advance {
				want = next
			}
			if !reflect.DeepEqual(*event.Attempts[0].Usage, want) {
				t.Fatalf("selected incompatible snapshot: %+v", event.Attempts[0].Usage)
			}
			if !reflect.DeepEqual(event.CachedInputTokens, want.CachedInputTokens) || !reflect.DeepEqual(event.CacheWriteInputTokens, want.CacheWriteInputTokens) ||
				!reflect.DeepEqual(event.CacheWrite5MInputTokens, want.CacheWrite5MInputTokens) || !reflect.DeepEqual(event.CacheWrite1HInputTokens, want.CacheWrite1HInputTokens) ||
				!reflect.DeepEqual(event.AudioInputTokens, want.AudioInputTokens) || !reflect.DeepEqual(event.CachedAudioInputTokens, want.CachedAudioInputTokens) ||
				!reflect.DeepEqual(event.ImageOutputTokens, want.ImageOutputTokens) || !reflect.DeepEqual(event.ToolCalls, want.ToolCalls) ||
				!reflect.DeepEqual(event.MediaUnits, want.MediaUnits) {
				t.Fatalf("request dimensions diverged: %+v", event)
			}
		})
	}
}

// Complete native evidence remains authoritative even when a
// derived partition is lower than the provisional observation.
