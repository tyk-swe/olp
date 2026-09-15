package usage

import (
	"context"
	"errors"
	"testing"
)

type recordingStream struct {
	payloads []string
	failOnce bool
}

func (s *recordingStream) Do(_ context.Context, args ...string) (any, error) {
	if s.failOnce {
		s.failOnce = false
		return nil, errors.New("temporary outage")
	}
	s.payloads = append(s.payloads, args[4])
	return "1-0", nil
}

func TestCloseFlushesQueuedMetadataAndRetries(t *testing.T) {
	e := NewEmitter(4)
	for range 3 {
		if err := e.Emit(ingestSampleEvent()); err != nil {
			t.Fatal(err)
		}
	}
	e.Close()
	e.Close()
	stream := &recordingStream{failOnce: true}
	e.RunWriter(t.Context(), stream, "stream", ingestTestLogger())
	got := e.Snapshot()
	if len(stream.payloads) != 3 || got.Persisted != 3 || got.Lost() != 0 ||
		!got.GracefullyDrained() || got.Retrying {
		t.Fatalf("flush = %+v, writes = %d", got, len(stream.payloads))
	}
}

func TestDropCountsLossBeforeIntake(t *testing.T) {
	e := NewEmitter(1)
	e.Drop()
	got := e.Snapshot()
	if got.Accepted != 0 || got.Dropped != 1 || got.FirstLossAt == nil || got.LastLossAt == nil {
		t.Fatalf("pre-intake loss = %+v", got)
	}
}

func TestForcedShutdownDoesNotClaimAGracefullyDrainedEpoch(t *testing.T) {
	e := NewEmitter(1)
	e.MarkUnclean()
	e.Close()
	e.RunWriter(t.Context(), &recordingStream{}, "stream", ingestTestLogger())
	if got := e.Snapshot(); got.GracefullyDrained() || !got.Unclean {
		t.Fatalf("forced shutdown claimed a clean epoch: %+v", got)
	}
}
