package process

import (
	"context"
	"log/slog"
	"testing"

	"github.com/tyk-swe/olp/internal/usage"
)

func TestProbeDrainAcceptsTheLastEventBeforeDeliveryCloses(t *testing.T) {
	probes, stop := context.WithCancel(t.Context())
	defer stop()
	emitter := usage.NewEmitter(1)
	stopped := make(chan struct{})
	emitted := make(chan error, 1)
	go func() {
		defer close(stopped)
		<-probes.Done()
		emitted <- emitter.Emit(usage.Event{Version: usage.WireVersion})
	}()
	drainProbes(t.Context(), stop, stopped, emitter, slog.New(slog.DiscardHandler))
	if err := <-emitted; err != nil {
		t.Fatalf("terminal probe event was lost during shutdown: %v", err)
	}
	state := emitter.Snapshot()
	if state.Accepted != 1 || state.Lost() != 0 || state.Closed || state.Unclean {
		t.Fatalf("probe did not drain before delivery: %+v", state)
	}
}

func TestProbeDrainTimeoutLeavesAnUncleanProducerEpoch(t *testing.T) {
	probes, stop := context.WithCancel(t.Context())
	defer stop()
	shutdown, expire := context.WithCancel(t.Context())
	expire()
	emitter := usage.NewEmitter(1)
	stopped := make(chan struct{})
	defer close(stopped)
	drainProbes(shutdown, stop, stopped, emitter, slog.New(slog.DiscardHandler))
	if probes.Err() == nil {
		t.Fatal("probe producer was not stopped")
	}
	emitter.Close()
	state := emitter.Snapshot()
	if !state.Unclean || state.GracefullyDrained() {
		t.Fatalf("unfinished probe falsely closed its accounting epoch: %+v", state)
	}
}
