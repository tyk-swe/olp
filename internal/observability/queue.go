package observability

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Admission priority classes, highest first. Dequeueing is weighted fair
// across them, 8:4:2:1, so load slows a lower class but never starves it.
const (
	ClassCritical = iota
	ClassHigh
	ClassNormal
	ClassLow
	classCount
)

// ClassNames names the priority classes in order.
var ClassNames = [classCount]string{"critical", "high", "normal", "low"}

var classWeights = [classCount]int{8, 4, 2, 1}

// ClassOf returns the class a priority names; any other name is normal.
func ClassOf(name string) int {
	if class := slices.Index(ClassNames[:], name); class >= 0 {
		return class
	}
	return ClassNormal
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
	if depth < 0 || depth > MaxAdmissionCapacity || (depth > 0 && timeout <= 0) {
		panic("observability: admission queue out of range")
	}
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
func (p *Pool) await(ctx context.Context, class int, deadline time.Time) bool {
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
	class := q.next()
	if class < 0 {
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

// next picks the line to serve by smooth weighted round-robin, or -1 when
// every line is empty. Callers hold mu.
func (q *queue) next() int {
	best, total := -1, 0
	for class := range classCount {
		if len(q.lines[class]) == 0 {
			continue
		}
		q.credit[class] += classWeights[class]
		total += classWeights[class]
		if best < 0 || q.credit[class] > q.credit[best] {
			best = class
		}
	}
	if best >= 0 {
		q.credit[best] -= total
	}
	return best
}

// withdraw removes a waiter that gave up, reporting false when a handoff
// already took it from its line.
func (q *queue) withdraw(class int, w *waiter) bool {
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
func (q *queue) left(class int) {
	q.waiting.Add(-1)
	if len(q.lines[class]) == 0 {
		q.credit[class] = 0
	}
}

func (q *queue) observe(class int, waited time.Duration) {
	q.waits[class].Add(1)
	q.waitNS[class].Add(uint64(waited))
}

// metrics renders the queue's per-class series, one family at a time.
func (q *queue) metrics(body *strings.Builder) {
	var depth [classCount]int
	q.mu.Lock()
	for class := range classCount {
		depth[class] = len(q.lines[class])
	}
	q.mu.Unlock()
	body.WriteString("# HELP olp_admission_queue_depth Inference requests waiting in the admission queue.\n# TYPE olp_admission_queue_depth gauge\n")
	for class, name := range ClassNames {
		fmt.Fprintf(body, "olp_admission_queue_depth{class=%q} %d\n", name, depth[class])
	}
	body.WriteString("# HELP olp_admission_queue_wait_seconds Time admitted requests spent in the admission queue.\n# TYPE olp_admission_queue_wait_seconds summary\n")
	for class, name := range ClassNames {
		fmt.Fprintf(body, "olp_admission_queue_wait_seconds_sum{class=%q} %g\nolp_admission_queue_wait_seconds_count{class=%q} %d\n",
			name, time.Duration(q.waitNS[class].Load()).Seconds(), name, q.waits[class].Load())
	}
	body.WriteString("# HELP olp_admission_queue_rejections_total Queued inference requests refused when their queue timeout or route deadline passed.\n# TYPE olp_admission_queue_rejections_total counter\n")
	for class, name := range ClassNames {
		fmt.Fprintf(body, "olp_admission_queue_rejections_total{class=%q} %d\n", name, q.expired[class].Load())
	}
}
