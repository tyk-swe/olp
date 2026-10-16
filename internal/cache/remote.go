package cache

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/tyk-swe/olp/internal/coordination"
	"github.com/valkey-io/valkey-glide/go/v2/config"
)

// Remote connects lazily: losing an optional cache never prevents a gateway
// from starting. The native client reconnects after a connection is established.
type Remote struct {
	mu            sync.Mutex
	configuration *config.ClientConfiguration
	main          Commander
	client        *coordination.Client
	retry         time.Time
	closed        bool
}

func NewRemote(url, ca string, main Commander) (*Remote, error) {
	c, err := coordination.Configuration(url, ca, 250*time.Millisecond)
	if err != nil {
		return nil, errors.New("invalid response cache connection configuration")
	}
	return &Remote{configuration: c, main: main}, nil
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
func (r *Remote) Do(ctx context.Context, args ...string) (any, error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, ErrUnavailable
	}
	if r.client == nil {
		if time.Now().Before(r.retry) {
			r.mu.Unlock()
			return nil, ErrUnavailable
		}
		c, err := coordination.Open(ctx, r.configuration)
		if err == nil && r.main != nil {
			own, e1 := c.Do(ctx, "INFO", "server")
			other, e2 := r.main.Do(ctx, "INFO", "server")
			if e1 != nil || e2 != nil || serverID(own) == "" || serverID(other) == "" || serverID(own) == serverID(other) {
				err = ErrUnavailable
			}
		}
		if err != nil {
			if c != nil {
				c.Close()
			}
			r.retry = time.Now().Add(time.Second)
			r.mu.Unlock()
			return nil, ErrUnavailable
		}
		r.client = c
	}
	c := r.client
	r.mu.Unlock()
	return c.Do(ctx, args...)
}
func (r *Remote) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	if r.client != nil {
		r.client.Close()
	}
}
