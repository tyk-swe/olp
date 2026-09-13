// sdkfixture implements only the process/metadata boundary in M1. Official SDK
// inference scenarios are added with their protocols in M3/M5, never simulated
// by fabricated success responses here.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tyk-swe/olp/internal/management"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	path := os.Getenv("OLP_SDK_SMOKE_METADATA")
	if path == "" {
		return fmt.Errorf("OLP_SDK_SMOKE_METADATA is required")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer listener.Close()
	mux := http.NewServeMux()
	management.Register(mux)
	mux.HandleFunc("/", management.Unimplemented)
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	errCh := make(chan error, 1)
	go func() { errCh <- server.Serve(listener) }()
	defer server.Close()
	metadata, err := json.Marshal(map[string]string{"origin": "http://" + listener.Addr().String(), "api_key": "olp_go_fixture_key", "conflict_api_key": "olp_go_fixture_conflict_key", "route_slug": "sdk-smoke-route"})
	if err != nil {
		return err
	}
	// Publish atomically only after binding; consumers never see partial JSON.
	if err := os.WriteFile(path+".tmp", metadata, 0600); err != nil {
		return err
	}
	defer os.Remove(path + ".tmp")
	if err := os.Rename(path+".tmp", path); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	select {
	case <-ctx.Done():
	case err := <-errCh:
		return err
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return server.Shutdown(shutdown)
}
