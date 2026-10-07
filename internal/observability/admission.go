package observability

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"
)

// MaxAdmissionCapacity bounds a configured admission pool.
const MaxAdmissionCapacity = 1_000_000

// Pool is a process-local bounded request pool with the counters the metrics
// endpoint renders. A request that cannot enter is rejected unless the pool
// has a queue with room for it.
type Pool struct {
	sem      chan struct{}
	admitted atomic.Int64
	rejected atomic.Uint64
	queue    *queue
}

// NewPool builds a pool with capacity slots. Capacity must be at least one.
func NewPool(capacity int) *Pool {
	if capacity < 1 || capacity > MaxAdmissionCapacity {
		panic("observability: admission capacity out of range")
	}
	return &Pool{sem: make(chan struct{}, capacity)}
}

// Acquire takes a slot or counts the rejection. It never blocks.
func (p *Pool) Acquire() bool {
	return p.AcquirePermit() != nil
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

func (p *Pool) permit(state int32) *Permit {
	permit := &Permit{pool: p}
	permit.state.Store(state)
	return permit
}

// Release returns a slot. It pairs strictly with a successful Acquire. While
// requests wait in the queue the slot passes to one of them instead.
func (p *Pool) Release() {
	if q := p.queue; q != nil && q.waiting.Load() > 0 && q.handoff() {
		return
	}
	// Uncount before freeing, so Admitted never overstates the held slots.
	p.admitted.Add(-1)
	<-p.sem
}

// Capacity is the configured slot count.
func (p *Pool) Capacity() int { return cap(p.sem) }

// Admitted is the number of slots currently held.
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

const (
	permitReleased int32 = iota
	permitHeld
	permitQueued
)

// Queued reports whether the permit still waits for its slot.
func (p *Permit) Queued() bool { return p != nil && p.state.Load() == permitQueued }

// Await waits for a queued permit's slot in class's line, at most until the
// queue timeout or deadline, whichever comes first. It reports whether the
// permit now holds a slot; a held permit returns at once.
func (p *Permit) Await(ctx context.Context, class int, deadline time.Time) bool {
	if p == nil || p.state.Load() != permitQueued {
		return p != nil && p.state.Load() == permitHeld
	}
	if class < 0 || class >= classCount {
		class = ClassNormal
	}
	if !p.pool.await(ctx, class, deadline) {
		return false
	}
	p.state.Store(permitHeld)
	p.pool.queue.reserved.Add(-1)
	return true
}

// Release returns the slot, or the queue position, once.
func (p *Permit) Release() {
	if p == nil {
		return
	}
	switch p.state.Swap(permitReleased) {
	case permitHeld:
		p.pool.Release()
	case permitQueued:
		p.pool.queue.reserved.Add(-1)
	}
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

// Metrics renders the surface's admission series.
func (p *Pool) metrics(surface string, body *strings.Builder) {
	fmt.Fprintf(body,
		"olp_http_admission_capacity{surface=%q} %d\n"+
			"olp_http_admitted_requests{surface=%q} %d\n"+
			"olp_http_admission_rejections_total{surface=%q} %d\n",
		surface, p.Capacity(), surface, p.admitted.Load(), surface, p.rejected.Load())
}

// AdmissionMetrics renders both surfaces' admission series.
func AdmissionMetrics(inference, management *Pool, body *strings.Builder) {
	body.WriteString("# HELP olp_http_admission_capacity Configured process-local HTTP request admission capacity.\n" +
		"# TYPE olp_http_admission_capacity gauge\n" +
		"# HELP olp_http_admitted_requests Current process-local admitted HTTP requests whose responses have not completed.\n" +
		"# TYPE olp_http_admitted_requests gauge\n" +
		"# HELP olp_http_admission_rejections_total HTTP requests rejected because the process-local admission pool was full.\n" +
		"# TYPE olp_http_admission_rejections_total counter\n")
	if inference != nil {
		inference.metrics("inference", body)
	}
	if management != nil {
		management.metrics("management", body)
	}
	if inference != nil && inference.queue != nil {
		inference.queue.metrics(body)
	}
}
