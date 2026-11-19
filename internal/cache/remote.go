package cache

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/tyk-swe/olp/internal/platform/coordination"
)

// reconnectDelay backs off reconnect attempts so a down cache does not spin.
const reconnectDelay = time.Second

// Remote connects lazily: losing an optional cache never prevents a gateway
// from starting. The native client reconnects after a connection is established.
type Remote struct {
	mu            sync.Mutex
	configuration coordination.Config
	control       Commander
	client        *coordination.Client
	retry         time.Time
	closed        bool
}

func NewRemote(url, ca string, control Commander) (*Remote, error) {
	c, err := coordination.Configuration(url, ca, 250*time.Millisecond)
	if err != nil {
		return nil, errors.New("invalid response cache connection configuration")
	}
	return &Remote{configuration: c, control: control}, nil
}

func serverID(v any) string {
	s, ok := v.(string)
	if !ok {
		return ""
	}
	for _, line := range strings.Split(s, "\n") {
		if id, ok := strings.CutPrefix(strings.TrimSpace(line), "run_id:"); ok {
			return id
		}
	}
	return ""
}

// sharesServer reports whether the connected cache is the coordination store
// itself. Sharing a server would mix cached responses into control-plane data.
func (r *Remote) sharesServer(ctx context.Context, c *coordination.Client) bool {
	if r.control == nil {
		return false
	}
	own, e1 := c.Do(ctx, "INFO", "server")
	other, e2 := r.control.Do(ctx, "INFO", "server")
	return e1 != nil || e2 != nil || serverID(own) == "" || serverID(other) == "" || serverID(own) == serverID(other)
}

func (r *Remote) Do(ctx context.Context, args ...string) (any, error) {
	c, err := r.connection(ctx)
	if err != nil {
		return nil, err
	}
	return c.Do(ctx, args...)
}

func (r *Remote) Search(ctx context.Context, args ...string) ([]coordination.SearchDocument, error) {
	c, err := r.connection(ctx)
	if err != nil {
		return nil, err
	}
	return c.Search(ctx, args...)
}

func (r *Remote) connection(ctx context.Context) (*coordination.Client, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, ErrUnavailable
	}
	if r.client == nil {
		if time.Now().Before(r.retry) {
			return nil, ErrUnavailable
		}
		c, err := coordination.Open(ctx, r.configuration)
		if err == nil && r.sharesServer(ctx, c) {
			err = ErrUnavailable
		}
		if err != nil {
			if c != nil {
				c.Close()
			}
			r.retry = time.Now().Add(reconnectDelay)
			return nil, ErrUnavailable
		}
		r.client = c
	}
	return r.client, nil
}

func (r *Remote) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	if r.client != nil {
		r.client.Close()
	}
}
