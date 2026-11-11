package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/tyk-swe/olp/internal/usage/delivery"
)

type listenerConfig struct {
	name, address string
	handler       http.Handler
}

type boundListener struct {
	name   string
	socket net.Listener
	server *listenerServer
	served <-chan struct{}
}

// bindListeners opens every socket before serving traffic and closes earlier
// sockets if a later bind fails. Request lifetimes are independent of startup.
func bindListeners(startup context.Context, configs []listenerConfig, c Config, requests context.Context) ([]boundListener, error) {
	listeners := make([]boundListener, 0, len(configs))
	for _, config := range configs {
		socket, err := (&net.ListenConfig{}).Listen(startup, "tcp", config.address)
		if err != nil {
			for _, listener := range listeners {
				listener.socket.Close()
			}
			return nil, fmt.Errorf("bind %s listener: %w", config.name, err)
		}
		listeners = append(listeners, boundListener{
			name:   config.name,
			socket: socket,
			server: &listenerServer{
				handler:  config.handler,
				limit:    c.MaxConnections,
				age:      time.Duration(c.ConnectionMaxAgeSeconds) * time.Second,
				drain:    time.Duration(c.ConnectionDrainTimeoutSeconds) * time.Second,
				requests: requests,
			},
		})
	}
	return listeners, nil
}

func serveListeners(ctx context.Context, listeners []boundListener, mode Mode, log *slog.Logger) error {
	errorsCh := make(chan error, len(listeners))
	for i := range listeners {
		listener := &listeners[i]
		done := make(chan struct{})
		listener.served = done
		go func() {
			defer close(done)
			errorsCh <- listener.server.Serve(listener.socket)
		}()
		log.Info("listener started", "mode", mode, "listener", listener.name, "address", listener.socket.Addr().String())
	}
	select {
	case <-ctx.Done():
		return nil
	case err := <-errorsCh:
		return err
	}
}

// Both listeners drain against the same deadline. Forced connection closure
// does not prove that every handler has finished producing metadata.
func drainListeners(ctx context.Context, listeners []boundListener, accounting *delivery.Delivery) {
	var wg sync.WaitGroup
	for _, listener := range listeners {
		wg.Go(func() {
			if err := listener.server.Shutdown(ctx); err != nil {
				if accounting != nil && listener.name == "public" {
					accounting.MarkUnclean()
				}
			}
			// A bound socket also belongs to this cleanup if serving never
			// started. Once serving did start, join its accept loop explicitly.
			listener.socket.Close()
			if listener.served != nil {
				select {
				case <-listener.served:
				case <-ctx.Done():
				}
			}
		})
	}
	wg.Wait()
}
