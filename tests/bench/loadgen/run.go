//go:build bench

package loadgen

import (
	"context"
	"net"
	"net/http"
	"sync"
	"time"
)

// job is one scheduled request.
type job struct {
	k         int64
	scheduled time.Time
	measured  bool
}

// runner is one run's state.
type runner struct {
	cfg    Config
	bodies *bodies
	client *http.Client
	rec    *recorder

	measuredStart time.Time
	wg            sync.WaitGroup

	// beforeDispatch, when set, runs in the scheduling loop once a request is
	// due and before it is dispatched. Tests use it to hold the loop up, which
	// is the one way the loop itself falls behind on a clock that is exact.
	beforeDispatch func(k int64)

	// mu guards the slot accounting: how many requests run, and the FIFO of
	// scheduled requests waiting for one.
	mu                     sync.Mutex
	inflight, peakInFlight int
	backlog                []job
	head                   int
	peakBacklog            int
}

// Run drives cfg's schedule against its target and reports what happened.
// It returns once every request has finished or the drain period has passed.
// Canceling ctx stops scheduling early; requests already sent still get their
// drain period, and the report is marked invalid.
func Run(ctx context.Context, cfg Config) (*Report, error) {
	r, err := newRunner(cfg)
	if err != nil {
		return nil, err
	}
	if cfg.Client == nil {
		defer r.client.CloseIdleConnections()
	}
	return r.run(ctx), nil
}

// newRunner prepares a run of cfg: its request bodies, its recorder and, when
// the configuration brings none, the client.
func newRunner(cfg Config) (*runner, error) {
	cfg, err := cfg.normalize()
	if err != nil {
		return nil, err
	}
	bodies, err := newBodies(cfg)
	if err != nil {
		return nil, err
	}
	r := &runner{cfg: cfg, bodies: bodies, client: cfg.Client, rec: newRecorder(cfg.Duration)}
	if r.client == nil {
		r.client = newClient(cfg)
	}
	return r, nil
}

// newClient builds the client of a run: one that never compresses or follows
// redirects, keeps up to MaxInFlight connections warm so a steady rate does
// not churn sockets, and speaks the protocol asked for.
func newClient(cfg Config) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	if cfg.SlowRead.BytesPerSecond > 0 {
		dialer.Control = limitReceiveBuffer(cfg.SlowRead.RcvBuf)
	}
	tr := &http.Transport{
		DialContext:         dialer.DialContext,
		DisableCompression:  true,
		MaxIdleConns:        0,
		MaxIdleConnsPerHost: cfg.MaxInFlight,
		IdleConnTimeout:     90 * time.Second,
	}
	if cfg.H2C {
		tr.Protocols = new(http.Protocols)
		tr.Protocols.SetUnencryptedHTTP2(true)
	}
	if cfg.SlowRead.BytesPerSecond > 0 {
		// HTTP/2 reads a stream's data into a window of its own, 4 MiB by
		// default, however slowly the body is read, which would hide a slow
		// reader from the server just as a large socket buffer does.
		tr.HTTP2 = &http.HTTP2Config{MaxReceiveBufferPerStream: cfg.SlowRead.RcvBuf}
	}
	return &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (r *runner) run(ctx context.Context) *Report {
	// Requests outlive the caller's cancellation by the drain period, so they
	// are governed by their own context.
	reqCtx, cancelRequests := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelRequests()

	warmup, measured := r.cfg.warmupRequests(), r.cfg.measuredRequests()
	started := time.Now()
	r.measuredStart = started.Add(r.cfg.offset(warmup))

	wake := time.NewTimer(time.Hour)
	defer wake.Stop()
	interrupted := false
schedule:
	for k := int64(0); k < warmup+measured; k++ {
		due := started.Add(r.cfg.offset(k))
		if wait := time.Until(due); wait > 0 {
			wake.Reset(wait)
			select {
			case <-wake.C:
			case <-ctx.Done():
				interrupted = true
				break schedule
			}
		} else if ctx.Err() != nil {
			interrupted = true
			break schedule
		}
		if r.beforeDispatch != nil {
			r.beforeDispatch(k)
		}
		// A send is never early; one that is late keeps its scheduled time,
		// so the lateness is counted in its latency.
		r.dispatch(reqCtx, job{k: k, scheduled: due, measured: k >= warmup})
	}

	finished := make(chan struct{})
	go func() { r.wg.Wait(); close(finished) }()
	drain := time.NewTimer(r.cfg.Drain)
	defer drain.Stop()
	select {
	case <-finished:
	case <-drain.C:
		cancelRequests()
		<-finished
	}
	r.mu.Lock()
	peakInFlight, peakBacklog := r.peakInFlight, r.peakBacklog
	r.mu.Unlock()
	return r.rec.report(r.cfg, started, time.Since(started), interrupted, peakInFlight, peakBacklog)
}

// dispatch gives a due request a slot, or a place in the backlog, or drops it.
func (r *runner) dispatch(ctx context.Context, j job) {
	r.rec.scheduled(j.measured)
	r.mu.Lock()
	switch {
	case r.inflight < r.cfg.MaxInFlight:
		r.inflight++
		r.peakInFlight = max(r.peakInFlight, r.inflight)
		r.mu.Unlock()
		r.wg.Add(1)
		go r.work(ctx, j)
	case len(r.backlog)-r.head < r.cfg.MaxBacklog:
		r.backlog = append(r.backlog, j)
		r.peakBacklog = max(r.peakBacklog, len(r.backlog)-r.head)
		r.mu.Unlock()
	default:
		r.mu.Unlock()
		r.rec.drop(j.measured)
	}
}

// work runs j and then whatever has queued behind it, so a slot is held by
// one goroutine until the backlog is empty.
func (r *runner) work(ctx context.Context, j job) {
	defer r.wg.Done()
	for {
		r.send(ctx, j)
		r.mu.Lock()
		if r.head == len(r.backlog) {
			r.backlog, r.head = r.backlog[:0], 0
			r.inflight--
			r.mu.Unlock()
			return
		}
		j = r.backlog[r.head]
		r.head++
		if r.head > 1024 && r.head*2 > len(r.backlog) {
			r.backlog = append(r.backlog[:0], r.backlog[r.head:]...)
			r.head = 0
		}
		r.mu.Unlock()
	}
}

// send runs one request and records it. A request first run after the drain
// period has expired is abandoned instead of sent.
func (r *runner) send(ctx context.Context, j job) {
	if ctx.Err() != nil {
		r.rec.abandon(j.measured)
		return
	}
	sent := time.Now()
	res := r.exchange(ctx, j.k)
	r.rec.record(res, j.scheduled, sent, r.measuredStart, r.cfg.LateAfter, j.measured)
}
