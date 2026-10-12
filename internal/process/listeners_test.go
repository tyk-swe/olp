package process

import (
	"context"
	"log/slog"
	"net"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/config"
)

func TestBindListenersClosesEarlierSocketsOnFailure(t *testing.T) {
	first, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	address := first.Addr().String()
	blocked, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer blocked.Close()
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	listeners, err := bindListeners(t.Context(), []listenerConfig{
		{name: "private", address: address},
		{name: "public", address: blocked.Addr().String()},
	}, config.Config{}, t.Context())
	for _, listener := range listeners {
		defer listener.socket.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "bind public listener") {
		t.Fatalf("expected public listener bind failure, got %v", err)
	}
	if listeners != nil {
		t.Fatal("failed binding returned partially initialized listeners")
	}
	reopened, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatalf("earlier listener is still bound: %v", err)
	}
	reopened.Close()
}

func TestDrainClosesBoundListenerBeforeServingStarts(t *testing.T) {
	listeners, err := bindListeners(t.Context(), []listenerConfig{{name: "private", address: "127.0.0.1:0"}}, config.Config{}, t.Context())
	if err != nil {
		t.Fatal(err)
	}
	address := listeners[0].socket.Addr().String()
	drainListeners(t.Context(), listeners, nil)
	rebound, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatal("unserved bound listener remained open", err)
	}
	rebound.Close()
}

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
