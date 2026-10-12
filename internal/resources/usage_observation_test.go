package resources

import (
	"math"
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

func TestPendingUsageRetainsCoherentPartialSnapshots(t *testing.T) {
	cases := []struct {
		name     string
		previous usage.AttemptUsage
		next     usage.AttemptUsage
		advance  bool
	}{
		{"stale cumulative output", partialUsage(10, 4), partialUsage(10, 1), false},
		{"coherent cumulative advance", partialUsage(10, 4), partialUsage(11, 5), true},
		{"incomparable counters", partialUsage(10, 4), partialUsage(11, 1), false},
		{"known zero", partialUsage(0, 0), partialUsage(0, 0), true},
		{"exact large counters", partialUsage(math.MaxInt64-1, 4), partialUsage(math.MaxInt64, 5), true},
		{"missing observation", partialUsage(10, 4), usage.AttemptUsage{BillingUncertain: true}, false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			event := pendingUsageEvent(test.previous)
			if err := observePendingUsage(event, test.next, false); err != nil {
				t.Fatal(err)
			}
			want := test.previous
			if test.advance {
				want = test.next
			}
			if got := *event.Attempts[0].Usage; !reflect.DeepEqual(got, want) {
				t.Fatalf("snapshot=%+v want=%+v", got, want)
			}
			if event.UsageComplete || !reflect.DeepEqual(event.InputTokens, want.InputTokens) || !reflect.DeepEqual(event.OutputTokens, want.OutputTokens) {
				t.Fatalf("request evidence diverged: %+v", event)
			}
		})
	}
}

func TestPendingUsagePreservesKnownDimensionsAndCachePartitions(t *testing.T) {
	previous := partialUsage(100, 10)
	previous.CachedInputTokens, previous.CacheWriteInputTokens = usageCount(20), usageCount(30)
	previous.CacheWrite5MInputTokens, previous.CacheWrite1HInputTokens = usageCount(10), usageCount(5)
	previous.MediaUnits = usageUnits("1.000001")
	cases := []struct {
		name    string
		change  func(*usage.AttemptUsage)
		advance bool
	}{
		{"missing cached evidence", func(u *usage.AttemptUsage) { u.CachedInputTokens = nil }, false},
		{"missing write detail", func(u *usage.AttemptUsage) { u.CacheWrite1HInputTokens = nil }, false},
		{"missing media evidence", func(u *usage.AttemptUsage) { u.MediaUnits = nil }, false},
		{"cache repartition", func(u *usage.AttemptUsage) { u.CachedInputTokens = usageCount(30) }, false},
		{"write repartition", func(u *usage.AttemptUsage) { u.CacheWrite5MInputTokens = usageCount(15) }, false},
		{"media regression", func(u *usage.AttemptUsage) { u.MediaUnits = usageUnits("1.000000") }, false},
		{"equal exact decimal", func(u *usage.AttemptUsage) { u.MediaUnits = usageUnits("01.000001") }, true},
		{"all dimensions advance", func(u *usage.AttemptUsage) {
			u.InputTokens, u.OutputTokens = usageCount(120), usageCount(12)
			u.CachedInputTokens, u.CacheWriteInputTokens = usageCount(25), usageCount(35)
			u.CacheWrite5MInputTokens, u.CacheWrite1HInputTokens = usageCount(12), usageCount(6)
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
				!reflect.DeepEqual(event.CacheWrite5MInputTokens, want.CacheWrite5MInputTokens) || !reflect.DeepEqual(event.CacheWrite1HInputTokens, want.CacheWrite1HInputTokens) || !reflect.DeepEqual(event.MediaUnits, want.MediaUnits) {
				t.Fatalf("request dimensions diverged: %+v", event)
			}
		})
	}
}

func TestPendingUsageKeepsProtocolOwnedTerminalAuthority(t *testing.T) {
	previous := partialUsage(10, 4)
	previous.CachedInputTokens = usageCount(0)
	for _, final := range []usage.AttemptUsage{
		{Observed: true, Complete: true, InputTokens: usageCount(10), OutputTokens: usageCount(1)},
		{Observed: true, Complete: true, InputTokens: usageCount(0), OutputTokens: usageCount(0), CachedInputTokens: usageCount(0)},
		{Observed: true, Complete: true, InputTokens: usageCount(11), OutputTokens: usageCount(4), CachedInputTokens: usageCount(11)},
	} {
		event := pendingUsageEvent(previous)
		if err := observePendingUsage(event, final, true); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(*event.Attempts[0].Usage, final) || !event.UsageComplete {
			t.Fatalf("provisional usage overrode terminal authority: %+v", event.Attempts[0].Usage)
		}
	}
	for _, terminal := range []usage.AttemptUsage{partialUsage(10, 1), {BillingUncertain: true}} {
		event := pendingUsageEvent(previous)
		if err := observePendingUsage(event, terminal, true); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(*event.Attempts[0].Usage, previous) || event.UsageComplete {
			t.Fatalf("uncertain termination lost stronger evidence: %+v", event.Attempts[0].Usage)
		}
	}
}

func TestPendingUsageRejectsInvalidDiscardedObservations(t *testing.T) {
	previous := partialUsage(10, 4)
	for _, invalid := range []usage.AttemptUsage{
		partialUsage(-1, 1),
		{Observed: true, BillingUncertain: true, InputTokens: usageCount(10), OutputTokens: usageCount(1), CachedInputTokens: usageCount(11)},
		{Observed: true, BillingUncertain: true, InputTokens: usageCount(10), OutputTokens: usageCount(1), MediaUnits: usageUnits("0.0000001")},
	} {
		event := pendingUsageEvent(previous)
		if err := observePendingUsage(event, invalid, false); err == nil {
			t.Fatalf("discarded malformed observation was accepted: %+v", invalid)
		}
		if !reflect.DeepEqual(*event.Attempts[0].Usage, previous) {
			t.Fatal("invalid observation changed the retained snapshot")
		}
	}
}
