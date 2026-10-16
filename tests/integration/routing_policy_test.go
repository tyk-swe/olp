//go:build integration

package integration_test

import (
	"sync"

	"github.com/tyk-swe/olp/internal/gateway"
)

type policyCapture struct {
	mu     sync.Mutex
	events []gateway.Envelope
}

func (c *policyCapture) Terminal(e gateway.Envelope) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, e)
}
func (c *policyCapture) last() gateway.Envelope {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.events[len(c.events)-1]
}
