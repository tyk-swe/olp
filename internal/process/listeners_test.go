package process

import (
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
