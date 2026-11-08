package admission

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/tyk-swe/olp/internal/metric"
	"github.com/tyk-swe/olp/internal/priority"
)

type Class int

const (
	Normal     Class = priority.Normal
	classCount       = priority.Count
)

func ClassOf(name string) Class {
	if rank := priority.Rank(name); rank >= 0 {
		return Class(rank)
	}
	return Normal
}

// queue holds requests a full pool cannot admit yet. A request reserves a
// position when it arrives, so depth bounds the requests held in memory, and
// joins its class's line once it knows its priority. Each released slot passes
// straight to the waiter weighted fair selection picks, never back to the
// semaphore while anyone waits, so arrivals cannot overtake the line.
type queue struct {
	depth    int64
	timeout  time.Duration
	reserved atomic.Int64
	waiting  atomic.Int64

	mu    sync.Mutex
	lines [classCount][]*waiter
	// credit is smooth weighted round-robin state over the non-empty lines.
	credit [classCount]int

	waits   [classCount]atomic.Uint64
	waitNS  [classCount]atomic.Uint64
	expired [classCount]atomic.Uint64
}

type waiter struct{ granted chan struct{} }

// Queue lets the pool hold up to depth requests while it is full, each for
// at most timeout. Configure it before the pool serves; a depth of zero
// leaves the pool rejecting as before.
func (p *Pool) Queue(depth int, timeout time.Duration) *Pool {
	if depth > 0 {
		p.queue = &queue{depth: int64(depth), timeout: timeout}
	}
	return p
}

// Enter takes a slot, or a queue position when the pool is full and queues,
// returning nil and counting the rejection when it can take neither. A queued
// permit must Await its slot before the request does any admitted work.
func (p *Pool) Enter() *Permit {
	if p.tryAcquire() {
		return p.permit(permitHeld)
	}
	if q := p.queue; q != nil {
		if q.reserved.Add(1) <= q.depth {
			return p.permit(permitQueued)
		}
		q.reserved.Add(-1)
	}
	p.rejected.Add(1)
	return nil
}

func (p *Pool) tryAcquire() bool {
	select {
	case p.sem <- struct{}{}:
		p.admitted.Add(1)
		return true
	default:
		return false
	}
}

// await waits in class's line until a released slot is handed over, the
// queue timeout or deadline passes, or ctx ends.
func (p *Pool) await(ctx context.Context, class Class, deadline time.Time) bool {
	q := p.queue
	started := time.Now()
	if p.tryAcquire() {
		q.observe(class, 0)
		return true
	}
	limit := started.Add(q.timeout)
	if !deadline.IsZero() && deadline.Before(limit) {
		limit = deadline
	}
	w := &waiter{granted: make(chan struct{}, 1)}
	q.mu.Lock()
	q.lines[class] = append(q.lines[class], w)
	q.waiting.Add(1)
	q.mu.Unlock()
	// A slot released before this waiter joined its line went back to the
	// semaphore rather than to the line; take it so nothing sits idle.
	if p.tryAcquire() {
		if !q.withdraw(class, w) {
			// A release handed this waiter a slot as well; return one.
			<-w.granted
			p.Release()
		}
		q.observe(class, time.Since(started))
		return true
	}
	timer := time.NewTimer(time.Until(limit))
	defer timer.Stop()
	select {
	case <-w.granted:
		q.observe(class, time.Since(started))
		return true
	case <-timer.C:
	case <-ctx.Done():
	}
	if !q.withdraw(class, w) {
		// The handoff won the race with the timeout: the slot is ours.
		<-w.granted
		q.observe(class, time.Since(started))
		return true
	}
	q.expired[class].Add(1)
	return false
}

// handoff passes a released slot to the next waiter, reporting whether there
// was one. The slot stays admitted; only its holder changes.
func (q *queue) handoff() bool {
	q.mu.Lock()
	class, found := q.next()
	if !found {
		q.mu.Unlock()
		return false
	}
	w := q.lines[class][0]
	q.lines[class] = slices.Delete(q.lines[class], 0, 1)
	q.left(class)
	q.mu.Unlock()
	w.granted <- struct{}{}
	return true
}

// next picks the line to serve by smooth weighted round-robin. Callers hold mu.
func (q *queue) next() (Class, bool) {
	var best Class
	found, total := false, 0
	classes := priority.Classes()
	for class := range Class(classCount) {
		if len(q.lines[class]) == 0 {
			continue
		}
		q.credit[class] += classes[class].Weight
		total += classes[class].Weight
		if !found || q.credit[class] > q.credit[best] {
			best, found = class, true
		}
	}
	if found {
		q.credit[best] -= total
	}
	return best, found
}

// withdraw removes a waiter that gave up, reporting false when a handoff
// already took it from its line.
func (q *queue) withdraw(class Class, w *waiter) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	i := slices.Index(q.lines[class], w)
	if i < 0 {
		return false
	}
	q.lines[class] = slices.Delete(q.lines[class], i, i+1)
	q.left(class)
	return true
}

// left records a waiter leaving class's line. An emptied line forgets its
// credit so it rejoins the rotation at an even start. Callers hold mu.
func (q *queue) left(class Class) {
	q.waiting.Add(-1)
	if len(q.lines[class]) == 0 {
		q.credit[class] = 0
	}
}

func (q *queue) observe(class Class, waited time.Duration) {
	q.waits[class].Add(1)
	q.waitNS[class].Add(uint64(waited))
}

var (
	queueDepth      = metric.Gauge("olp_admission_queue_depth", "Inference requests waiting in the admission queue.", "class")
	queueWait       = prometheus.NewDesc("olp_admission_queue_wait_seconds", "Time admitted requests spent in the admission queue.", []string{"class"}, nil)
	queueRejections = metric.Counter("olp_admission_queue_rejections_total", "Queued inference requests refused when their queue timeout or route deadline passed.", "class")
)

func (q *queue) samples() []prometheus.Metric {
	if q == nil {
		return nil
	}
	var depth [classCount]int
	q.mu.Lock()
	for class := range classCount {
		depth[class] = len(q.lines[class])
	}
	q.mu.Unlock()
	classes := priority.Classes()
	metrics := make([]prometheus.Metric, 0, 3*len(classes))
	for class, priorityClass := range classes {
		metrics = append(metrics,
			queueDepth.Sample(float64(depth[class]), priorityClass.Name),
			waitSummary(q.waits[class].Load(), time.Duration(q.waitNS[class].Load()), priorityClass.Name),
			queueRejections.Sample(float64(q.expired[class].Load()), priorityClass.Name),
		)
	}
	return metrics
}

func waitSummary(count uint64, total time.Duration, class string) prometheus.Metric {
	summary, err := prometheus.NewConstSummary(queueWait, count, total.Seconds(), nil, class)
	if err != nil {
		return prometheus.NewInvalidMetric(queueWait, err)
	}
	return summary
}
