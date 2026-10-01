package process

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// listenerServer gives each accepted connection its own HTTP server. This lets
// the standard library send HTTP/2 GOAWAY at the connection's maximum age while
// already admitted streams drain; closing a raw socket would truncate them.
// The admission cap bounds both connections and their serving goroutines.
// A hijacked connection (a WebSocket upgrade) belongs to its handler: it stays
// admitted until the handler closes it, and Shutdown closes it at its deadline.
type listenerServer struct {
	handler     http.Handler
	limit       int
	age, drain  time.Duration
	mu          sync.Mutex
	socket      net.Listener
	closed      bool
	connections map[*connection]struct{}
	requests    context.Context
}

type connection struct {
	server   *http.Server
	conn     *trackedConn
	hijacked atomic.Bool
}

// awaitHijacked waits for the handler that owns a hijacked connection to close
// it, closing it itself if ctx ends first.
func (c *connection) awaitHijacked(ctx context.Context) error {
	select {
	case <-c.conn.done:
		return nil
	case <-ctx.Done():
		c.conn.Close()
		return ctx.Err()
	}
}

func (s *listenerServer) Serve(socket net.Listener) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		socket.Close()
		return http.ErrServerClosed
	}
	s.socket = socket
	s.connections = make(map[*connection]struct{})
	s.mu.Unlock()
	var delay time.Duration
	for {
		raw, err := socket.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return http.ErrServerClosed
			}
			// Running out of descriptors pauses admission, as in net/http,
			// rather than stopping the listener and the process with it.
			if temporaryAccept(err) {
				delay = min(max(2*delay, 5*time.Millisecond), time.Second)
				time.Sleep(delay)
				continue
			}
			return err
		}
		delay = 0
		s.mu.Lock()
		if s.closed || len(s.connections) >= s.limit {
			s.mu.Unlock()
			raw.Close()
			continue
		}
		conn := &trackedConn{Conn: raw, done: make(chan struct{})}
		entry := &connection{conn: conn}
		one := &singleConnection{conn: conn, done: make(chan struct{})}
		protocols := new(http.Protocols)
		protocols.SetHTTP1(true)
		protocols.SetUnencryptedHTTP2(true)
		server := &http.Server{
			Handler: s.handler, Protocols: protocols,
			ReadHeaderTimeout: 5 * time.Second, IdleTimeout: time.Minute, MaxHeaderBytes: 32 * 1024,
			BaseContext: func(net.Listener) context.Context { return s.requests },
			ConnState: func(_ net.Conn, state http.ConnState) {
				if state == http.StateHijacked {
					entry.hijacked.Store(true)
				}
				if state == http.StateClosed || state == http.StateHijacked {
					one.Close()
				}
			},
		}
		entry.server = server
		s.connections[entry] = struct{}{}
		s.mu.Unlock()
		go func() {
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
			if entry.hijacked.Load() {
				// The handler owns the socket now; it stays admitted until
				// the handler or Shutdown closes it.
				<-conn.done
			} else {
				conn.Close()
			}
			s.mu.Lock()
			delete(s.connections, entry)
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
	connections := make([]*connection, 0, len(s.connections))
	for entry := range s.connections {
		connections = append(connections, entry)
	}
	s.mu.Unlock()
	var wg sync.WaitGroup
	var mu sync.Mutex
	var result error
	for _, entry := range connections {
		wg.Go(func() {
			err := entry.server.Shutdown(ctx)
			if err != nil {
				entry.server.Close()
			}
			// http.Server does not track hijacked connections.
			if entry.hijacked.Load() {
				err = errors.Join(err, entry.awaitHijacked(ctx))
			}
			if err != nil {
				mu.Lock()
				result = errors.Join(result, err)
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return result
}

// temporaryAccept reports Accept errors that pass once resources free up.
func temporaryAccept(err error) bool {
	return errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE) ||
		errors.Is(err, syscall.ENOBUFS) || errors.Is(err, syscall.ENOMEM) ||
		errors.Is(err, syscall.ECONNABORTED)
}

// trackedConn reports when the connection is closed, so a hijacked connection
// stays admitted for as long as its handler holds it.
type trackedConn struct {
	net.Conn
	once sync.Once
	done chan struct{}
}

func (c *trackedConn) Close() error {
	c.once.Do(func() { close(c.done) })
	return c.Conn.Close()
}

// ReadFrom and CloseWrite keep the TCP fast paths net/http looks for.
func (c *trackedConn) ReadFrom(r io.Reader) (int64, error) {
	if rf, ok := c.Conn.(io.ReaderFrom); ok {
		return rf.ReadFrom(r)
	}
	return io.Copy(struct{ io.Writer }{c.Conn}, r)
}

func (c *trackedConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
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
