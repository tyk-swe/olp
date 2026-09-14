package usage

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

func ingestTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func ingestSampleEvent() Event {
	observed := time.Now().UTC()
	provider := "1a2b3c4d-0000-7000-8000-000000000001"
	model := "mock-model"
	status := 200
	return Event{
		EventID: "1a2b3c4d-0000-7000-8000-00000000000a", RequestID: "1a2b3c4d-0000-7000-8000-00000000000b",
		RuntimeGenerationID: "1a2b3c4d-0000-7000-8000-00000000000c",
		APIKeyID:            "1a2b3c4d-0000-7000-8000-00000000000d",
		ProviderID:          &provider, RouteSlug: "team-chat", UpstreamModel: &model,
		Operation: "generation", Surface: "openai",
		RequestStartedAt: observed.Add(-10 * time.Millisecond), RequestCompletedAt: observed,
		ObservedAt: observed, StatusCode: &status, Committed: true, LatencyMS: 10,
		UsageComplete: true,
	}
}

func TestEmitterCountsOverflowInsteadOfBlocking(t *testing.T) {
	t.Parallel()
	emitter := NewEmitter(1)
	if err := emitter.Emit(ingestSampleEvent()); err != nil {
		t.Fatalf("first emit: %v", err)
	}
	if err := emitter.Emit(ingestSampleEvent()); !errors.Is(err, ErrFull) {
		t.Fatalf("second emit = %v, want ErrFull", err)
	}
	snapshot := emitter.Snapshot()
	switch {
	case snapshot.Accepted != 1 || snapshot.Dropped != 1 || snapshot.Persisted != 0:
		t.Fatalf("snapshot = %+v", snapshot)
	case snapshot.FirstLossAt == nil || snapshot.LastLossAt == nil:
		t.Fatal("a dropped event must be timestamped")
	case snapshot.Complete():
		t.Fatal("a snapshot with loss is not complete")
	case snapshot.ProcessEpoch == "":
		t.Fatal("every process reports an epoch")
	}
}

func TestEmitterAccountsForEveryAcceptedEventOnShutdown(t *testing.T) {
	t.Parallel()
	emitter := NewEmitter(2)
	for range 2 {
		if err := emitter.Emit(ingestSampleEvent()); err != nil {
			t.Fatalf("emit: %v", err)
		}
	}
	emitter.abandonAndDrain(0)
	snapshot := emitter.Snapshot()
	switch {
	case snapshot.Accepted != 2 || snapshot.Abandoned != 2 || snapshot.Dropped != 0:
		t.Fatalf("snapshot = %+v", snapshot)
	case snapshot.Pending() != 0 || snapshot.Lost() != 2:
		t.Fatalf("pending = %d, lost = %d", snapshot.Pending(), snapshot.Lost())
	case !snapshot.GracefullyDrained():
		t.Fatal("a drained writer reports a graceful drain")
	}
	if err := emitter.Emit(ingestSampleEvent()); !errors.Is(err, ErrClosed) {
		t.Fatalf("emit after shutdown = %v, want ErrClosed", err)
	}
	if snapshot := emitter.Snapshot(); snapshot.Dropped != 1 || !snapshot.Closed {
		t.Fatalf("snapshot after a closed emit = %+v", snapshot)
	}
	// Draining twice must not close the channel twice or double count.
	emitter.abandonAndDrain(0)
	if snapshot := emitter.Snapshot(); snapshot.Abandoned != 2 {
		t.Fatalf("abandoned after a second drain = %d", snapshot.Abandoned)
	}
}

func TestEmitterShutdownRacesLeaveNoUnaccountedEvent(t *testing.T) {
	t.Parallel()
	for range 64 {
		emitter := NewEmitter(1)
		var start sync.WaitGroup
		var done sync.WaitGroup
		start.Add(1)
		done.Add(1)
		var emitted error
		go func() {
			defer done.Done()
			start.Wait()
			emitted = emitter.Emit(ingestSampleEvent())
		}()
		start.Done()
		emitter.abandonAndDrain(0)
		done.Wait()
		snapshot := emitter.Snapshot()
		if snapshot.Pending() != 0 {
			t.Fatalf("pending = %d after shutdown", snapshot.Pending())
		}
		if emitted == nil && snapshot.Accepted != snapshot.Abandoned {
			t.Fatalf("accepted = %d, abandoned = %d", snapshot.Accepted, snapshot.Abandoned)
		}
		if emitted != nil && snapshot.Dropped != 1 {
			t.Fatalf("a refused emit must be counted once, got %d", snapshot.Dropped)
		}
	}
}

func TestSnapshotReportsBacklogSeparatelyFromLoss(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	cases := []struct {
		name                        string
		snapshot                    Snapshot
		pending, lost               int64
		complete, gracefullyDrained bool
	}{
		{name: "retrying backlog is not loss",
			snapshot: Snapshot{Accepted: 2, Persisted: 1, Retrying: true}, pending: 1},
		{name: "delivered everything",
			snapshot: Snapshot{Accepted: 2, Persisted: 2}, complete: true},
		{name: "closed writer with loss is drained but not complete",
			snapshot:          Snapshot{Accepted: 2, Persisted: 1, Abandoned: 1, Closed: true, FirstLossAt: &now},
			lost:              1,
			gracefullyDrained: true},
		{name: "open writer is never drained",
			snapshot: Snapshot{Accepted: 2, Persisted: 2}, complete: true},
		{name: "counters below their lower bound cannot report a negative backlog",
			snapshot: Snapshot{Accepted: 0, Persisted: 3}, pending: 0, complete: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			switch {
			case test.snapshot.Pending() != test.pending:
				t.Fatalf("pending = %d, want %d", test.snapshot.Pending(), test.pending)
			case test.snapshot.Lost() != test.lost:
				t.Fatalf("lost = %d, want %d", test.snapshot.Lost(), test.lost)
			case test.snapshot.Complete() != test.complete:
				t.Fatalf("complete = %v, want %v", test.snapshot.Complete(), test.complete)
			case test.snapshot.GracefullyDrained() != test.gracefullyDrained:
				t.Fatalf("gracefully drained = %v, want %v",
					test.snapshot.GracefullyDrained(), test.gracefullyDrained)
			}
		})
	}
}

func TestEmitterSnapshotRaisesAcceptedToWhatWasAlreadyDelivered(t *testing.T) {
	t.Parallel()
	emitter := NewEmitter(1)
	// Emit counts an event only after the writer may already have delivered it.
	emitter.persisted.Add(1)
	if snapshot := emitter.Snapshot(); snapshot.Accepted != 1 || snapshot.Pending() != 0 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func TestRunWriterDrainsAndAbandonsOnShutdown(t *testing.T) {
	t.Parallel()
	emitter := NewEmitter(4)
	for range 3 {
		if err := emitter.Emit(ingestSampleEvent()); err != nil {
			t.Fatalf("emit: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	// A cancelled context stops the writer before it takes anything: every
	// buffered event is accounted for as lost rather than forgotten.
	emitter.RunWriter(ctx, nil, "stream", ingestTestLogger())
	snapshot := emitter.Snapshot()
	if snapshot.Abandoned != 3 || snapshot.Pending() != 0 || !snapshot.Closed {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}
