package process

import (
	"context"
	"log/slog"
	"testing"

	"github.com/tyk-swe/olp/internal/config"
)

func TestDrainJoinsListenerServingTask(t *testing.T) {
	listeners, err := bindListeners(t.Context(), []listenerConfig{{name: "private", address: "127.0.0.1:0"}}, config.Config{}, t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := serveListeners(ctx, listeners, config.Worker, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	drainListeners(t.Context(), listeners, nil)
	select {
	case <-listeners[0].served:
	default:
		t.Fatal("accept loop still owns its listener after drain")
	}
}
