package admission

import (
	"context"
	"slices"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/tyk-swe/olp/internal/metric"
)

// Pool is a process-local bounded request pool with the counters the metrics
// endpoint renders. A request that cannot enter is rejected unless the pool
// has a queue with room for it.
type Pool struct {
	sem      chan struct{}
	admitted atomic.Int64
	rejected atomic.Uint64
	queue    *queue
}

func NewPool(capacity int) *Pool {
	return &Pool{sem: make(chan struct{}, capacity)}
}

// AcquirePermit takes a slot and returns it as a Permit, or counts the
// rejection and returns nil. It never blocks and never queues.
func (p *Pool) AcquirePermit() *Permit {
	if p.tryAcquire() {
		return p.permit(permitHeld)
	}
	p.rejected.Add(1)
	return nil
}

func (p *Pool) permit(state permitState) *Permit {
	permit := &Permit{pool: p}
	permit.state.Store(int32(state))
	return permit
}

// Release returns a slot. It pairs strictly with a successful AcquirePermit. While
// requests wait in the queue the slot passes to one of them instead.
func (p *Pool) Release() {
	q := p.queue
	for {
		if q != nil && q.waiting.Load() > 0 && q.handoff() {
			return
		}
		// Uncount before freeing, so Admitted never overstates the held slots.
		p.admitted.Add(-1)
		<-p.sem
		// A waiter that joined after the check above may have found the slot
		// still held. Retake the slot and hand it over so that waiter wakes.
		if q == nil || q.waiting.Load() == 0 || !p.tryAcquire() {
			return
		}
	}
}

func (p *Pool) Capacity() int { return cap(p.sem) }

func (p *Pool) Admitted() int64 { return p.admitted.Load() }

// Permit is one admission slot, held or still queued for. It is safe to
// release early — a handler that finishes its expensive phase before
// responding can hand the slot back — and double release is a no-op, so the
// owning middleware always defers one final release without knowing what the
// handler already did.
type Permit struct {
	pool  *Pool
	state atomic.Int32
}

type permitState int32

const (
	permitReleased permitState = iota
	permitHeld
	permitQueued
)

func (p *Permit) current() permitState { return permitState(p.state.Load()) }

func (p *Permit) Queued() bool { return p != nil && p.current() == permitQueued }

// Await waits for a queued permit's slot in class's line, at most until the
// queue timeout or deadline, whichever comes first. It reports whether the
// permit now holds a slot; a held permit returns at once.
func (p *Permit) Await(ctx context.Context, class Class, deadline time.Time) bool {
	if p == nil || p.current() != permitQueued {
		return p != nil && p.current() == permitHeld
	}
	if !p.pool.await(ctx, class, deadline) {
		return false
	}
	p.state.Store(int32(permitHeld))
	p.pool.queue.reserved.Add(-1)
	return true
}

// Release returns the slot, or the queue position, once.
func (p *Permit) Release() {
	if p == nil {
		return
	}
	switch permitState(p.state.Swap(int32(permitReleased))) {
	case permitHeld:
		p.pool.Release()
	case permitQueued:
		p.pool.queue.reserved.Add(-1)
	}
}

func (p *Permit) Transfer() *Permit {
	if p == nil || !p.state.CompareAndSwap(int32(permitHeld), int32(permitReleased)) {
		return nil
	}
	return p.pool.permit(permitHeld)
}

type permitContextKey struct{}

// PermitFromContext returns the admission slot an outer middleware charged
// this request, or nil when admission happens at a lower layer.
func PermitFromContext(ctx context.Context) *Permit {
	p, _ := ctx.Value(permitContextKey{}).(*Permit)
	return p
}

// WithPermit carries an acquired slot on the request context so inner
// handlers release the same slot rather than acquiring again.
func WithPermit(ctx context.Context, p *Permit) context.Context {
	return context.WithValue(ctx, permitContextKey{}, p)
}

var (
	admissionCapacity   = metric.Gauge("olp_http_admission_capacity", "Configured process-local HTTP request admission capacity.", "surface")
	admittedRequests    = metric.Gauge("olp_http_admitted_requests", "Current process-local admitted HTTP requests whose responses have not completed.", "surface")
	admissionRejections = metric.Counter("olp_http_admission_rejections_total", "HTTP requests rejected because the process-local admission pool was full.", "surface")
)

func (p *Pool) samples(surface string) []prometheus.Metric {
	return []prometheus.Metric{
		admissionCapacity.Sample(float64(p.Capacity()), surface),
		admittedRequests.Sample(float64(p.admitted.Load()), surface),
		admissionRejections.Sample(float64(p.rejected.Load()), surface),
	}
}

func Metrics(inference, management *Pool) prometheus.Collector {
	return metric.Collector(func() []prometheus.Metric {
		return slices.Concat(inference.samples("inference"), management.samples("management"), inference.queue.samples())
	})
}
