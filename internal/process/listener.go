package process

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"
)

// listenerServer gives each accepted connection its own HTTP server. This lets
// the standard library send HTTP/2 GOAWAY at the connection's maximum age while
// already admitted streams drain; closing a raw socket would truncate them.
// The admission cap bounds both connections and their serving goroutines.
type listenerServer struct {
	handler     http.Handler
	limit       int
	age, drain  time.Duration
	mu          sync.Mutex
	socket      net.Listener
	closed      bool
	connections map[*http.Server]struct{}
	requests    context.Context
}

func (s *listenerServer) Serve(socket net.Listener) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		socket.Close()
		return http.ErrServerClosed
	}
	s.socket = socket
	s.connections = make(map[*http.Server]struct{})
	s.mu.Unlock()
	for {
		conn, err := socket.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return http.ErrServerClosed
			}
			return err
		}
		s.mu.Lock()
		if s.closed || len(s.connections) >= s.limit {
			s.mu.Unlock()
			conn.Close()
			continue
		}
		one := &singleConnection{conn: conn, done: make(chan struct{})}
		protocols := new(http.Protocols)
		protocols.SetHTTP1(true)
		protocols.SetUnencryptedHTTP2(true)
		server := &http.Server{
			Handler: s.handler, Protocols: protocols,
			ReadHeaderTimeout: 5 * time.Second, IdleTimeout: time.Minute, MaxHeaderBytes: 32 * 1024,
			BaseContext: func(net.Listener) context.Context { return s.requests },
			ConnState: func(_ net.Conn, state http.ConnState) {
				if state == http.StateClosed || state == http.StateHijacked {
					one.Close()
				}
			},
		}
		s.connections[server] = struct{}{}
		s.mu.Unlock()
		go func() {
			defer conn.Close()
			timer := time.AfterFunc(s.age, func() {
				ctx, cancel := context.WithTimeout(context.Background(), s.drain)
				defer cancel()
				if server.Shutdown(ctx) != nil {
					server.Close()
				}
			})
			defer timer.Stop()
			_ = server.Serve(one)
			// Serve returns when the connection closes or Shutdown closes its
			// listener. Keep admission charged until all streams have drained.
			ctx, cancel := context.WithTimeout(context.Background(), s.drain)
			defer cancel()
			if server.Shutdown(ctx) != nil {
				server.Close()
			}
			s.mu.Lock()
			delete(s.connections, server)
			s.mu.Unlock()
		}()
	}
}

func (s *listenerServer) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	if s.socket != nil {
		s.socket.Close()
	}
	servers := make([]*http.Server, 0, len(s.connections))
	for server := range s.connections {
		servers = append(servers, server)
	}
	s.mu.Unlock()
	var wg sync.WaitGroup
	var mu sync.Mutex
	var result error
	for _, server := range servers {
		wg.Go(func() {
			if err := server.Shutdown(ctx); err != nil {
				server.Close()
				mu.Lock()
				result = errors.Join(result, err)
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return result
}

type singleConnection struct {
	conn     net.Conn
	done     chan struct{}
	once     sync.Once
	accepted bool
}

func (l *singleConnection) Accept() (net.Conn, error) {
	if !l.accepted {
		l.accepted = true
		return l.conn, nil
	}
	<-l.done
	return nil, net.ErrClosed
}
func (l *singleConnection) Close() error   { l.once.Do(func() { close(l.done) }); return nil }
func (l *singleConnection) Addr() net.Addr { return l.conn.LocalAddr() }
