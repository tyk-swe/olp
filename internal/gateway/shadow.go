package gateway

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/usage"
)

// defaultShadowInFlight bounds mirrored requests when the deployment names no
// bound of its own.
const defaultShadowInFlight = 16

// shadowing runs mirrored traffic in a pool of its own, apart from the
// inference admission pool, so a mirror never holds capacity a caller could
// use. A full pool drops the mirror rather than queueing it.
type shadowing struct {
	slots    chan struct{}
	mirrored atomic.Int64
	dropped  atomic.Int64
	running  sync.WaitGroup
	mu       sync.Mutex
	closed   bool
	ctx      context.Context
	cancel   context.CancelFunc
}

func newShadowing(capacity int) *shadowing {
	if capacity <= 0 {
		capacity = defaultShadowInFlight
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &shadowing{slots: make(chan struct{}, capacity), ctx: ctx, cancel: cancel}
}

// ShadowCounts reports the shadow requests started and those dropped because
// the shadow pool was full.
func (s *Server) ShadowCounts() (mirrored, dropped int64) {
	return s.shadows.mirrored.Load(), s.shadows.dropped.Load()
}

// WaitShadows blocks until every running shadow request has finished.
func (s *Server) WaitShadows() { s.shadows.running.Wait() }

// DrainShadows stops mirror intake and waits for accounting and cap settlement.
// Mirrors still running at the shutdown deadline are cancelled.
func (s *Server) DrainShadows(ctx context.Context) error {
	s.shadows.mu.Lock()
	s.shadows.closed = true
	s.shadows.mu.Unlock()
	defer s.shadows.cancel()
	done := make(chan struct{})
	go func() {
		s.WaitShadows()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// mirrorable reports whether a request may be mirrored: stateless generation,
// embeddings and rerank, whose repetition at another target has no effect a
// caller could observe. Stateful Responses, pinned resources, files,
// batches, realtime and media never are.
func (x *execution) mirrorable() bool {
	if !x.mirrors || x.fixed || x.unary != nil || x.parsed == nil {
		return false
	}
	switch x.family.Operation() {
	case operationGeneration, "embeddings", "rerank":
		return true
	}
	return false
}

// mirrorSeed is the seed shadow targets are sampled against: the request's
// durable identity, so the decision is stable for the request and uniform
// across requests. It is nil unless the route has a shadow target, which
// keeps the sampling out of every other request.
func (x *execution) mirrorSeed() []byte {
	if x.route == nil || !x.route.Shadowed() || !x.mirrorable() {
		return nil
	}
	return []byte(x.request.accountingID())
}

// mirror sends the request to each shadow target its plan sampled, once the
// caller has its response. Each mirror is a request of its own: a fresh
// identity that names the caller's request as its parent, its own deadline and
// attempt, and accounting to the route rather than to the caller's key.
func (s *Server) mirror(x *execution) {
	if len(x.shadows) == 0 {
		return
	}
	s.shadows.mu.Lock()
	defer s.shadows.mu.Unlock()
	if s.shadows.closed {
		s.shadows.dropped.Add(int64(len(x.shadows)))
		return
	}
	for _, attempt := range x.shadows {
		select {
		case s.shadows.slots <- struct{}{}:
		default:
			s.shadows.dropped.Add(1)
			continue
		}
		s.shadows.mirrored.Add(1)
		s.shadows.running.Add(1)
		go func() {
			defer func() {
				<-s.shadows.slots
				s.shadows.running.Done()
			}()
			s.runShadow(x, attempt)
		}()
	}
}

// runShadow serves one mirror. It shares the caller's parsed request, which is
// no longer written once the caller's response is complete; everything the
// attempt prepares from it is built afresh.
func (s *Server) runShadow(parent *execution, attempt runtime.Attempt) {
	id := uuid.Must(uuid.NewV7()).String()
	route := parent.route
	x := &execution{
		request: request{
			id:        id,
			minted:    true,
			clientIP:  parent.request.clientIP,
			startedAt: s.now(),
			release:   parent.request.release,
		},
		family:          parent.family,
		ingress:         parent.ingress,
		parsed:          parent.parsed,
		semanticHeaders: parent.semanticHeaders,
		semanticQuery:   parent.semanticQuery,
		actor:           "system",
		keyID:           parent.keyID,
		authority:       parent.authority,
		affinity:        parent.affinity,
		route:           route,
		primary:         route,
		mode:            parent.mode,
		policy:          parent.policy,
		attempts:        []runtime.Attempt{attempt},
		budget:          1,
		allowance:       1,
		fixed:           true,
		origin:          usage.OriginShadow,
		parent:          parent.request.accountingID(),
		priority:        runtime.PriorityLow,
	}
	if x.parsed.Stream {
		x.emit = func([]byte) error {
			x.delivered(s.now())
			return nil
		}
	}
	ctx := s.shadows.ctx
	out := s.execute(ctx, x)
	status := 200
	if out.err != nil {
		status = out.err.Status
	}
	s.finish(x, out, status)
	s.settleCaps(ctx, x)
}
