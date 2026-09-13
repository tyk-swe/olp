// Package process explicitly composes the resources owned by each process mode.
package process

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/tyk-swe/olp/internal/config"
	"github.com/tyk-swe/olp/internal/console"
	"github.com/tyk-swe/olp/internal/coordination"
	"github.com/tyk-swe/olp/internal/database"
	"github.com/tyk-swe/olp/internal/management"
)

func Run(ctx context.Context, c config.Config, log *slog.Logger) error {
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	if err := c.Validate(); err != nil {
		return err
	}
	pgConfig, err := database.Configuration(c.DatabaseURL, c.DatabaseMaxConnections, c.RequestTimeout)
	if err != nil {
		return err
	}
	// Parse every dependency and asset before opening any listener.
	var vk *coordination.Client
	var openValkey func(context.Context) (*coordination.Client, error)
	if c.ValkeyURL != "" {
		vc, err := coordination.Configuration(c.ValkeyURL, c.ValkeyCAFile, c.RequestTimeout)
		if err != nil {
			return err
		}
		openValkey = func(ctx context.Context) (*coordination.Client, error) { return coordination.Open(ctx, vc) }
	}
	public := http.NewServeMux()
	if c.Mode.Management() {
		assets, closeAssets, err := console.Handler(c.ConsoleDir)
		if err != nil {
			return err
		}
		defer closeAssets()
		management.Register(public)
		public.Handle("/", assets)
		public.Handle("/health", assets)
	}
	// These prefixes must never fall through to the SPA, in any public mode.
	for _, prefix := range []string{"/api/", "/v1/", "/anthropic/", "/gemini/", "/v1beta/", "/openai/", "/health/", "/metrics"} {
		handler := http.HandlerFunc(http.NotFound)
		if c.Mode.Inference() && (prefix == "/v1/" || prefix == "/anthropic/" || prefix == "/gemini/") {
			handler = management.Unimplemented
		}
		public.Handle(prefix, handler)
	}
	startup, cancelStartup := context.WithTimeout(ctx, c.StartupTimeout)
	defer cancelStartup()
	pool, err := database.Open(startup, pgConfig)
	if err != nil {
		return err
	}
	defer pool.Close()
	if openValkey != nil {
		vk, err = openValkey(startup)
		if err != nil {
			return err
		}
		defer vk.Close()
	}
	if err := startup.Err(); err != nil {
		return err
	}
	private := healthHandler(ctx, c.RequestTimeout, pool.Ping, vk)
	listeners := []struct {
		name, address string
		handler       http.Handler
	}{{"private", c.ObservabilityListenAddr, private}}
	if c.Mode.Public() {
		listeners = append(listeners, struct {
			name, address string
			handler       http.Handler
		}{"public", c.ListenAddr, public})
	}
	var servers []*http.Server
	var sockets []net.Listener
	for _, listener := range listeners {
		socket, err := (&net.ListenConfig{}).Listen(startup, "tcp", listener.address)
		if err != nil {
			for _, s := range sockets {
				s.Close()
			}
			return fmt.Errorf("bind %s listener: %w", listener.name, err)
		}
		sockets = append(sockets, socket)
		servers = append(servers, &http.Server{Handler: listener.handler, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: time.Minute, MaxHeaderBytes: 32 * 1024, BaseContext: func(net.Listener) context.Context { return ctx }})
	}
	errorsCh := make(chan error, len(servers))
	for i, server := range servers {
		go func() { errorsCh <- server.Serve(sockets[i]) }()
		log.Info("listener started", "mode", c.Mode, "listener", listeners[i].name, "address", sockets[i].Addr().String())
	}
	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-errorsCh:
	}
	stop()
	shutdown, cancel := context.WithTimeout(context.Background(), c.ShutdownTimeout)
	defer cancel()
	// Both listeners drain against the same deadline, then connections are forced
	// closed. Only after handlers finish do the concrete clients close.
	var wg sync.WaitGroup
	for _, server := range servers {
		wg.Go(func() {
			if err := server.Shutdown(shutdown); err != nil {
				server.Close()
			}
		})
	}
	wg.Wait()
	log.Info("process stopped", "mode", c.Mode)
	if errors.Is(serveErr, http.ErrServerClosed) {
		return nil
	}
	return serveErr
}

func healthHandler(process context.Context, timeout time.Duration, pingDB func(context.Context) error, vk *coordination.Client) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, r *http.Request) {
		writeHealth(w, http.StatusOK, map[string]any{"live": true})
	})
	mux.HandleFunc("GET /health/ready", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		dbReady := pingDB(ctx) == nil
		valkeyReady := true
		if vk != nil {
			valkeyReady = vk.Ping(ctx) == nil
		}
		ready := process.Err() == nil && dbReady && valkeyReady
		status := http.StatusOK
		if !ready {
			status = http.StatusServiceUnavailable
		}
		dependencies := map[string]bool{"postgres": dbReady}
		if vk != nil {
			dependencies["valkey"] = valkeyReady
		}
		writeHealth(w, status, map[string]any{"ready": ready, "dependencies": dependencies})
	})
	return mux
}

func writeHealth(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}
