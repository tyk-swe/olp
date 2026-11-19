package main

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/tyk-swe/olp/internal/admission"
	"github.com/tyk-swe/olp/internal/billing/reconciliation"
	"github.com/tyk-swe/olp/internal/catalog"
	"github.com/tyk-swe/olp/internal/media/jobs"
	"github.com/tyk-swe/olp/internal/observability"
	"github.com/tyk-swe/olp/internal/platform/coordination"
	"github.com/tyk-swe/olp/internal/platform/panics"
	"github.com/tyk-swe/olp/internal/platform/telemetry"
	"github.com/tyk-swe/olp/internal/usage"
	"github.com/tyk-swe/olp/internal/usage/delivery"
)

type workerFeed struct {
	reader *coordination.Client
	stream string
}

// The consumer reads its stream with a blocking XREADGROUP, and a client
// answers one connection in order: on the shared client a second spent
// blocking is a second every admission decision and every emitted event waits
// behind. The consumer gets its own.
func openWorkerFeed(startup context.Context, c Config, stores datastores, open func(context.Context) (*coordination.Client, error)) (workerFeed, error) {
	if stores.limiter == nil || (c.Mode != Worker && c.Mode != All) {
		return workerFeed{}, nil
	}
	reader, err := open(startup)
	if err != nil {
		return workerFeed{}, err
	}
	return workerFeed{reader: reader, stream: usage.StreamName(stores.prefix)}, nil
}

func (w workerFeed) close(context.Context) error {
	w.reader.Close()
	return nil
}

type lifetimes struct {
	root           context.Context
	stop           context.CancelCauseFunc
	auxiliary      context.Context
	stopAuxiliary  context.CancelFunc
	delivery       context.Context
	stopDelivery   context.CancelFunc
	workers        context.Context
	stopWorkers    context.CancelFunc
	probes         context.Context
	stopProbes     context.CancelFunc
	requests       context.Context
	cancelRequests context.CancelFunc
}

func newLifetimes(ctx context.Context, stop context.CancelCauseFunc) lifetimes {
	detached := context.WithoutCancel(ctx)
	auxiliary, stopAuxiliary := context.WithCancel(detached)
	delivery, stopDelivery := context.WithCancel(detached)
	workers, stopWorkers := context.WithCancel(detached)
	probes, stopProbes := context.WithCancel(detached)
	requests, cancelRequests := context.WithCancel(detached)
	return lifetimes{
		root: ctx, stop: stop,
		auxiliary: auxiliary, stopAuxiliary: stopAuxiliary,
		delivery: delivery, stopDelivery: stopDelivery,
		workers: workers, stopWorkers: stopWorkers,
		probes: probes, stopProbes: stopProbes,
		requests: requests, cancelRequests: cancelRequests,
	}
}

type running struct {
	foundation
	lifetimes
	serving   serving
	feed      workerFeed
	traces    *telemetry.Handle
	reference *catalog.Signed
	deadline  *time.Time
	supervise func(func()) func()

	background, delivered sync.WaitGroup
	metadata              *delivery.DeliveryRun
	probesStopped         chan struct{}
	workersStopped        func()
	listeners             []boundListener
}

func (r *running) start(startup context.Context) error {
	r.startTasks()
	if err := r.startDelivery(startup); err != nil {
		return err
	}
	r.startWorkers()
	return r.listen(startup)
}

func (r *running) startTasks() {
	core, log := r.core, r.log
	r.background.Go(r.supervise(func() {
		if err := core.pluginHost.PreparePinned(r.auxiliary); err != nil && r.auxiliary.Err() == nil {
			log.Warn("plugins could not be prepared", "error", err)
		}
	}))
	// Authority must stay fresh while accepted requests drain, so
	// polling outlives the listeners and stops once handlers finish.
	core.rt.Start(context.WithoutCancel(r.root), r.grantRefreshed())
	if r.serving.monitor != nil {
		r.background.Go(r.supervise(func() { r.serving.monitor.Run(r.auxiliary) }))
	}
	sources := r.observed()
	r.background.Go(r.supervise(func() { r.health.Run(r.auxiliary, sources, log) }))
}

func (r *running) grantRefreshed() func(string, string) {
	if r.serving.gateway == nil {
		return nil
	}
	return r.serving.gateway.GrantRefreshed
}

// The observability collectors probe only what this process composes: an
// unconfigured dependency is reported as absent, never as failed.
func (r *running) observed() *observability.Sources {
	c, stores, core, log := r.config, r.stores, r.core, r.log
	return observabilitySources(observed{
		region:        c.Region,
		servesGateway: c.Mode.Inference(),
		pool:          stores.pool,
		limiter:       stores.vk,
		runtime:       core.rt,
		policy:        core.policy,
		gateway:       r.serving.gateway,
		accounting:    r.serving.accounting,
		spool:         r.media.spool,
		media:         jobs.NewStore(stores.pool),
		mediaGaps:     r.mediaGaps,
		loss:          r.loss,
		export: func(ctx context.Context) []prometheus.Metric {
			captureRates, err := reconciliation.CaptureRates(ctx, stores.pool, time.Now())
			if err != nil {
				log.Warn("billing capture rates could not be computed", "error", err)
			}
			return slices.Concat(core.exporter.Metrics(ctx), core.focusWorker.Metrics(ctx), core.capturer.Metrics(), core.importer.Metrics(), captureRates)
		},
	})
}

// The delivery plane outlives the listeners: it is cancelled only once the
// producers have drained, so every emitted event is written and this
// process's epoch is closed against what it actually delivered.
func (r *running) startDelivery(startup context.Context) error {
	gw, outage := r.serving.gateway, r.stores.outage
	if accounting := r.serving.accounting; accounting != nil {
		metadata, err := accounting.Start(startup, r.stores.vk, usage.StreamName(r.stores.prefix))
		if err != nil {
			return err
		}
		r.metadata = metadata
	}
	r.delivered.Go(r.supervise(func() { r.core.capturer.Run(r.delivery) }))
	if outage != nil {
		r.background.Go(r.supervise(func() { outage.run(r.auxiliary) }))
	}
	if gw != nil {
		// Circuits are shared with the fleet only through Valkey; without it
		// this returns at once and local circuits alone decide.
		r.background.Go(r.supervise(func() { gw.RunFleetHealth(r.auxiliary) }))
	}
	return nil
}

// The worker plane is cancelled last of all, because the consumer it runs is
// what turns the events the delivery plane just flushed into accounting.
// Media reconciliation and grant refresh need only PostgreSQL and the provider
// egress client, so the plane runs even when no shared state backend is
// configured. Probes produce accounting events, so they stop before delivery
// closes; the consumer and recovery workers remain alive until delivery drains.
func (r *running) startWorkers() {
	c, stores, core := r.config, r.stores, r.core
	if c.Mode == Worker || c.Mode == All {
		r.workersStopped = startWorkers(r.workers, r.supervise, workerDeps{
			pool: stores.pool, vk: r.feed.reader, store: stores.debitStore, shared: stores.shared, stream: r.feed.stream,
			mediaService: r.media.service, credentials: core.rt, pluginHost: core.pluginHost,
			keys: core.keys, auth: core.auth, installation: stores.installation, region: c.Region, hostname: c.Hostname,
			policy: core.policy, log: r.log, exporter: core.exporter, focusWorker: core.focusWorker,
			importer: core.importer, reference: r.reference,
		})
	}
	if prober := r.serving.prober; prober != nil {
		stopped := make(chan struct{})
		r.probesStopped = stopped
		go r.supervise(func() {
			defer close(stopped)
			prober.RunHealthProbes(r.probes, probeCheckpoint(stores.pool, r.log, c.Region))
		})()
	}
}

func (r *running) listen(startup context.Context) error {
	c := r.config
	live := []prometheus.Collector{runtimeMetrics(r.core.rt), admission.Metrics(r.inferencePool, r.managementPool), r.traces.Metrics(), r.core.business}
	private, err := observability.NewHandler(r.health, live...)
	if err != nil {
		return err
	}
	configs := slices.Concat([]listenerConfig{{name: "private", address: c.ObservabilityListenAddr, handler: private.ServeMux()}}, r.publicListener())
	listeners, err := bindListeners(startup, configs, c, r.requests, r.log)
	if err != nil {
		return err
	}
	r.listeners = listeners
	return nil
}

// Network-permitted public requests enter their surface's pool before routing:
// a full pool rejects without queueing, and tracing spans open only for
// admitted requests.
func (r *running) publicListener() []listenerConfig {
	c := r.config
	if !c.Mode.Public() {
		return nil
	}
	pools := &admission.Public{
		Inference:        r.inferencePool,
		Management:       r.managementPool,
		InferenceEnabled: c.Mode.Inference(),
		Reject:           rejectPublic,
	}
	handler := Perimeter(c.PublicOrigin, managementNetwork(c.ManagementAllowedCIDRs, c.TrustedProxyCIDRs, pools.Wrap(r.traces.Middleware(r.stores.installation)(r.public))))
	return []listenerConfig{{name: "public", address: c.ListenAddr, handler: handler}}
}

func (r *running) serve() error {
	served := serveListeners(r.root, r.listeners, r.config.Mode, r.log)
	if crash, ok := errors.AsType[*panics.Error](context.Cause(r.root)); ok {
		return crash
	}
	if errors.Is(served, http.ErrServerClosed) {
		return nil
	}
	return served
}

func (r *running) shutdown() {
	c, log, accounting := r.config, r.log, r.serving.accounting
	r.stop(nil)
	*r.deadline = time.Now().Add(c.ShutdownTimeout)
	shutdown, cancel := context.WithDeadline(context.Background(), *r.deadline)
	defer cancel()
	r.stopAuxiliary()
	drainListeners(shutdown, r.listeners, accounting)
	r.cancelRequests()
	r.drainShadows(shutdown)
	drainProbes(shutdown, r.stopProbes, r.probesStopped, accounting, log)
	// Accepted requests and probes have stopped producing while their live
	// authority remained available. Runtime joins against the same budget.
	if err := r.core.rt.StopContext(shutdown); err != nil {
		log.Warn("shutdown budget reached before runtime stopped", "error", err)
	}
	if r.metadata != nil {
		if err := r.metadata.Drain(shutdown); err != nil {
			log.Warn("shutdown budget reached before request metadata drained", "error", err)
		}
	}
	r.stopDelivery()
	awaitPlane(shutdown, r.delivered.Wait, log, "request metadata delivery")
	r.stopWorkers()
	if r.workersStopped != nil && !awaitPlane(shutdown, r.workersStopped, log, "worker plane") {
		// A blocking stream read must be explicitly unblocked at deadline;
		// closing it normally before cancellation races active consumers.
		if r.feed.reader != nil {
			closeDependency(r.deadline, c.ShutdownTimeout, log, "forced worker Valkey reader", r.feed.close)
		}
	}
	awaitPlane(shutdown, r.background.Wait, log, "background tasks")
	log.Info("process stopped", "mode", c.Mode)
}

func (r *running) drainShadows(shutdown context.Context) {
	if r.serving.gateway == nil {
		return
	}
	if err := r.serving.gateway.DrainShadows(shutdown); err != nil {
		r.log.Warn("shutdown budget reached before shadows drained", "error", err)
		if r.serving.accounting != nil {
			r.serving.accounting.MarkUnclean()
		}
	}
}
