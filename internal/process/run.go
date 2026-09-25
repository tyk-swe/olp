// Package process explicitly composes the resources owned by each process mode.
package process

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/config"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/console"
	"github.com/tyk-swe/olp/internal/coordination"
	"github.com/tyk-swe/olp/internal/database"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/gateway"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/management"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/observability"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/telemetry"
	"github.com/tyk-swe/olp/internal/usage"
)

// mediaUpstreamHeaderTimeout mirrors the gateway's upstream header wait bound.
const mediaUpstreamHeaderTimeout = 5 * time.Minute

// metadataBuffer is how many request metadata events one inference replica may
// hold while the stream writer catches up. Beyond it events are dropped and
// counted as loss, so a slow or unreachable stream costs completeness rather
// than memory or inference latency.
const metadataBuffer = 8192

// Version is the build identity recorded on exported traces.
var Version = "dev"

func Run(ctx context.Context, c config.Config, log *slog.Logger) error {
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	if err := c.Validate(); err != nil {
		return err
	}
	// Tracing is installed before any listener binds: an invalid endpoint or
	// header file must stop startup rather than trace half a process.
	traces, err := telemetry.Install(telemetry.Config{
		Endpoint:          c.OTLPTracesEndpoint,
		HeadersFile:       c.OTLPHeadersFile,
		SampleRatio:       c.TraceSampleRatio,
		PropagateUpstream: c.TracePropagateUpstream,
		AcceptInbound:     c.TraceAcceptInbound,
		Mode:              string(c.Mode),
		Version:           Version,
	})
	if err != nil {
		return err
	}
	var shutdownDeadline time.Time
	defer func() {
		if shutdownDeadline.IsZero() {
			shutdownDeadline = time.Now().Add(5 * time.Second)
		}
		shutdown, cancel := context.WithDeadline(context.Background(), shutdownDeadline)
		defer cancel()
		if err := traces.Shutdown(shutdown); err != nil {
			log.Warn("trace export did not flush cleanly", "error", err)
		}
	}()
	pgConfig, err := database.Configuration(c.DatabaseURL, c.DatabaseMaxConnections, c.RequestTimeout)
	if err != nil {
		return err
	}
	// Parse every dependency and asset before opening any listener.
	var vk *coordination.Client
	var openValkey func(context.Context) (*coordination.Client, error)
	if c.ValkeyURL != "" {
		vc, err := coordination.Configuration(c.ValkeyURL, c.ValkeyCAFile, c.RequestTimeout)
		if err != nil {
			return err
		}
		openValkey = func(ctx context.Context) (*coordination.Client, error) { return coordination.Open(ctx, vc) }
	}
	public := http.NewServeMux()
	if c.Mode.Management() {
		assets, closeAssets, err := console.Handler(c.ConsoleDir)
		if err != nil {
			return err
		}
		defer closeAssets()
		management.Register(public)
		public.Handle("/", assets)
		public.Handle("/health", assets)
	}
	guardPublicPrefixes(public, c.Mode.Inference())
	startup, cancelStartup := context.WithTimeout(ctx, c.StartupTimeout)
	defer cancelStartup()
	pool, err := database.Open(startup, pgConfig)
	if err != nil {
		return err
	}
	defer pool.Close()
	installation, err := database.Installation(startup, pool)
	if err != nil {
		return err
	}
	// Shared state comes up before anything that admits or accounts for
	// traffic, because both the gateway and the console are told at
	// composition time whether this installation has a limiter at all.
	prefix := database.ValkeyNamespace(installation)
	var limiter *limits.Limiter
	var outage *outagePolicy
	if openValkey != nil {
		vk, err = openValkey(startup)
		if err != nil {
			return err
		}
		defer vk.Close()
		if limiter, err = limits.New(vk, prefix+"limits"); err != nil {
			return err
		}
		if c.Mode.Inference() {
			if outage, err = newOutagePolicy(startup, pool, log); err != nil {
				return err
			}
		}
	}
	var emitter *usage.Emitter
	var rt *runtime.Manager
	var gw *gateway.Server
	var mediaService *media.Service
	var mediaSpool *media.Spool
	var policy egress.Policy
	// The public listener's process-local admission pools. The inference pool
	// is shared with the gateway so middleware and direct handler calls bound
	// one capacity; media reconciliation gaps and durable metadata loss are
	// process counters the metrics endpoint renders.
	inferencePool := observability.NewPool(c.MaxInFlightInference)
	managementPool := observability.NewPool(c.MaxInFlightManagement)
	lossCounters := &usage.LossCounters{}
	var mediaGapsTotal atomic.Uint64
	obsCache := observability.NewCache()
	// Worker replicas also need the runtime manager and the key ring: media
	// reconciliation serves jobs against their pinned historical providers and
	// checks the live credential revocation authority.
	var keys *secrets.KeyRing
	if c.Mode.Management() || c.Mode.Inference() || c.Mode == config.Worker {
		var auth *secrets.AuthKey
		var bootstrap string
		var err error
		auth, keys, bootstrap, err = loadSecrets(startup, pool, c, installation)
		if err != nil {
			return err
		}
		policy = egress.Policy{AllowedNetworks: c.ProviderEgressAllowCIDRs, PlainHTTPHosts: c.ProviderEgressAllowHTTPHosts}
		rt = runtime.NewManager(pool, installation, auth, keys, log)
		if c.ConnectorConfigFile != "" {
			rt.Mounted, err = providers.LoadMounted(c.ConnectorConfigFile, &policy)
			if err != nil {
				return err
			}
		}
		if c.Mode.Management() || c.Mode.Inference() {
			gw = gateway.New(rt, &policy, gateway.Config{
				MaxInFlight:        c.MaxInFlightInference,
				CORSAllowedOrigins: c.GatewayCORSAllowedOrigins,
				InlineMedia:        protocols.InlineMediaLimits{Items: c.MaxInlineMediaItems, ItemBytes: c.MaxInlineMediaItemBytes, TotalBytes: c.MaxInlineMediaTotalBytes},
				MaxBodyBytes:       c.MaxJSONBodyBytes,
				MaxMediaBodyBytes:  c.MaxMediaBodyBytes,
				MaxResponseBytes:   c.ProviderMaxResponseBytes,
				MaxEventBytes:      c.ProviderMaxEventBytes,
				TrustedProxies:     c.TrustedProxyCIDRs,
				AdmissionPool:      inferencePool,
			}, log)
			if limiter != nil {
				var policy func() limits.OutagePolicy
				if outage != nil {
					policy = outage.Policy
				}
				// Control-only processes also execute playground requests.
				gw.Admission = gateway.NewAdmission(limiter, policy, log)
			}
		}
		spoolDir := c.MediaSpoolDir
		if spoolDir == "" {
			spoolDir = filepath.Join(os.TempDir(), "olp-media-spool")
		}
		spool, err := media.NewSpool(spoolDir, c.MediaSpoolCapacityBytes, log)
		if err != nil {
			return err
		}
		mediaSpool = spool
		defer spool.Close()
		mediaService = &media.Service{
			Pool:         pool,
			Keys:         keys,
			Installation: installation,
			Gaps:         &mediaGapsTotal,
			Transport: &media.Transport{
				Client:           policy.Client(mediaUpstreamHeaderTimeout),
				Auth:             connectors.NewAuth(&policy),
				Egress:           &policy,
				Spool:            spool,
				MaxResponseBytes: c.ProviderMaxResponseBytes,
			},
			Revoked: rt.Revoked,
			Log:     log,
		}
		if c.Mode.Inference() {
			gw.Media = &gateway.MediaDeps{Jobs: mediaService, Admission: media.NewAdmissionState(c.MediaSpoolCapacityBytes)}
			gw.Resources = resources.NewEncrypted(pool, installation, keys)
			gw.Resolver = resources.NewResolver(pool, installation, keys)
			// Without shared state there is no admission backend at all: the
			// gateway then refuses traffic that carries hard limits rather
			// than serving it unmetered, and keeps logging its metadata.
			if limiter != nil {
				emitter = usage.NewEmitter(metadataBuffer)
				gw.Sink = &gateway.AccountingSink{Emitter: emitter, Log: log, Next: gw.Sink}
			}
			gw.Register(public)
		}
		if c.Mode.Management() {
			control, err := access.New(startup, pool, installation, c.PublicOrigin, auth, keys, bootstrap)
			if err != nil {
				return err
			}
			control.LocalLoginDisabled = !c.LocalLoginEnabled
			control.ClientIP = func(r *http.Request) string { return gateway.ClientIP(r, c.TrustedProxyCIDRs) }
			control.LimitsEnforced = limiter != nil
			// The worker plane that applies retention runs only where shared
			// state is configured, and a control process cannot see whether a
			// separate worker replica is alive, so the console is told what
			// this installation is configured for.
			control.RetentionEnforced = limiter != nil
			control.NotificationsActive = limiter != nil
			registerManagement(public, control, &policy, limiter, rt, gw, mediaService, obsCache, log)
		}
	}
	if err := startup.Err(); err != nil {
		return err
	}
	if rt != nil {
		rt.Start(ctx)
		defer rt.Stop()
	}
	// The observability collectors probe only what this process composes: an
	// unconfigured dependency is reported as absent, never as failed.
	obsState := &observability.State{
		Pool:          pool,
		PingDB:        pool.Ping,
		ServesGateway: c.Mode.Inference(),
		Limiter: func(ctx context.Context) (configured, healthy bool) {
			return vk != nil, vk != nil && vk.Ping(ctx) == nil
		},
		MediaGaps:    func() int64 { return int64(mediaGapsTotal.Load()) },
		LossCounters: lossCounters.Totals,
	}
	configureObservability(obsState, rt, gw, emitter, mediaSpool, &policy)
	go obsCache.Run(ctx, obsState, log)
	// The delivery plane outlives the listeners: it is cancelled only once the
	// gateway has drained, so every event a served request emitted is written
	// and this gateway's epoch is closed against what it actually delivered.
	delivery, stopDelivery := context.WithCancel(context.WithoutCancel(ctx))
	defer stopDelivery()
	var delivered sync.WaitGroup
	var writerDone chan struct{}
	if emitter != nil {
		stream, instance := usage.StreamName(prefix), usage.GatewayInstance()
		// Register before listeners bind: even a crash before the first tick
		// must leave an epoch that recovery can detect.
		if _, err := usage.CheckpointEpoch(startup, pool, instance, emitter.Snapshot(), false); err != nil {
			return err
		}
		log.Info("request metadata gateway registered", "gateway_instance", instance)
		writerDone = make(chan struct{})
		delivered.Go(func() {
			defer close(writerDone)
			emitter.RunWriter(delivery, vk, stream, log)
		})
		delivered.Go(func() { usage.RunLossReporter(delivery, pool, emitter, instance, lossCounters, log) })
	}
	if outage != nil {
		go outage.run(ctx)
	}
	// The worker plane is cancelled last of all, because the consumer it runs
	// is what turns the events the delivery plane just flushed into accounting.
	workers, stopWorkers := context.WithCancel(context.WithoutCancel(ctx))
	defer stopWorkers()
	var workersStopped func()
	if c.Mode == config.Worker || c.Mode == config.All {
		// The consumer reads its stream with a blocking XREADGROUP, and a
		// client answers one connection in order: on the shared client a
		// second spent blocking is a second every admission decision and
		// every emitted event waits behind. The consumer gets its own.
		var reader *coordination.Client
		var stream string
		if limiter != nil {
			var err error
			reader, err = openValkey(startup)
			if err != nil {
				return err
			}
			defer reader.Close()
			stream = usage.StreamName(prefix)
		}
		// Media reconciliation needs only PostgreSQL and the provider egress
		// client, so it runs even when no shared state backend is configured.
		// It is started exactly once, inside the single worker plane.
		if mediaService == nil && limiter == nil {
			log.Warn("worker plane skipped: no shared state is configured", "mode", c.Mode)
		} else {
			workersStopped = startWorkers(workers, pool, reader, limiter, stream, mediaService, keys, installation, &policy, log)
		}
	}
	liveMetrics := newLiveMetrics(rt, inferencePool, managementPool)
	private := observability.NewHandler(obsCache, liveMetrics).ServeMux()
	listenerConfigs := []listenerConfig{{name: "private", address: c.ObservabilityListenAddr, handler: private}}
	if c.Mode.Public() {
		// Every public request is charged against its surface's pool before
		// routing: a full pool rejects without queueing, and tracing spans
		// open only for admitted requests.
		admission := &observability.PublicAdmission{
			Inference:        inferencePool,
			Management:       managementPool,
			InferenceEnabled: c.Mode.Inference(),
			Reject:           rejectPublic,
			Tracer:           traces.Tracer(),
		}
		if runtimeConfig := traces.Runtime(); runtimeConfig != nil {
			request := runtimeConfig.ForInstallation(installation)
			admission.Tracing = &request
		}
		listenerConfigs = append(listenerConfigs, listenerConfig{name: "public", address: c.ListenAddr, handler: admission.Wrap(public)})
	}
	requestContext, cancelRequests := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelRequests()
	listeners, err := bindListeners(startup, listenerConfigs, c, requestContext)
	if err != nil {
		return err
	}
	serveErr := serveListeners(ctx, listeners, c.Mode, log)
	stop()
	shutdownDeadline = time.Now().Add(c.ShutdownTimeout)
	shutdown, cancel := context.WithDeadline(context.Background(), shutdownDeadline)
	defer cancel()
	drainListeners(shutdown, listeners, emitter)
	cancelRequests()
	// Stop intake and flush accepted events before stopping the workers. If
	// HTTP draining was forced, the epoch stays open to report the uncertainty
	// from handlers that could still finish after intake closes.
	if emitter != nil {
		emitter.Close()
		awaitPlane(shutdown, func() { <-writerDone }, log, "request metadata flush")
	}
	stopDelivery()
	awaitPlane(shutdown, delivered.Wait, log, "request metadata delivery")
	stopWorkers()
	if workersStopped != nil {
		awaitPlane(shutdown, workersStopped, log, "worker plane")
	}
	log.Info("process stopped", "mode", c.Mode)
	if errors.Is(serveErr, http.ErrServerClosed) {
		return nil
	}
	return serveErr
}

// awaitPlane waits for a plane to finish after intake is closed or its context
// is cancelled. All planes share one deadline, including HTTP draining.
// A plane that outlives it records a completeness gap rather than extending
// the deployment grace period.
func awaitPlane(ctx context.Context, wait func(), log *slog.Logger, plane string) {
	done := make(chan struct{})
	go func() {
		wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		log.Warn("shutdown budget reached before the plane closed", "plane", plane)
	}
}

func loadSecrets(ctx context.Context, pool *pgxpool.Pool, c config.Config, installation string) (*secrets.AuthKey, *secrets.KeyRing, string, error) {
	if c.AuthHMACKeyFile == "" || c.MasterKeyFile == "" && (c.Mode.Management() || c.Mode == config.Worker || c.ConnectorConfigFile == "") {
		return nil, nil, "", errors.New("this mode requires OLP_AUTH_HMAC_KEY_FILE and OLP_MASTER_KEY_FILE")
	}
	raw, err := secrets.ReadFile(c.AuthHMACKeyFile)
	if err != nil {
		return nil, nil, "", err
	}
	key, err := secrets.DecodeKey(string(raw))
	if err != nil {
		return nil, nil, "", err
	}
	var keys *secrets.KeyRing
	if c.MasterKeyFile != "" {
		keys, err = secrets.LoadRing(c.MasterKeyFile)
		if err != nil {
			return nil, nil, "", err
		}
	}
	var bootstrap string
	if c.BootstrapTokenFile != "" {
		data, err := secrets.ReadFile(c.BootstrapTokenFile)
		if errors.Is(err, os.ErrNotExist) {
			var complete bool
			if err := pool.QueryRow(ctx, "SELECT setup_complete FROM olp_go.installation WHERE singleton").Scan(&complete); err != nil {
				return nil, nil, "", errors.New("cannot inspect installation setup state")
			}
			if !complete {
				return nil, nil, "", errors.New("bootstrap token file is required until setup is complete")
			}
		} else if err != nil {
			return nil, nil, "", err
		} else {
			bootstrap = string(data)
			if len(bootstrap) < 32 || len(bootstrap) > 256 {
				return nil, nil, "", errors.New("bootstrap token must have 32–256 bytes")
			}
		}
	}
	return secrets.NewAuthKey(key, installation), keys, bootstrap, nil
}

// guardPublicPrefixes keeps protocol and private prefixes from falling through
// to the SPA, in any public mode. When inference is enabled the gateway
// registers its own, more specific handlers under /v1/, /bedrock/,
// /anthropic/, and /gemini/; anything left over is answered honestly instead
// of reaching the console.
func guardPublicPrefixes(public *http.ServeMux, inference bool) {
	for _, prefix := range []string{"/api/", "/v1/", "/bedrock/", "/native/", "/ws/", "/anthropic/", "/gemini/", "/v1beta/", "/openai/", "/health/", "/metrics"} {
		handler := http.HandlerFunc(http.NotFound)
		if inference {
			switch prefix {
			case "/v1/", "/bedrock/":
				continue
			case "/anthropic/", "/gemini/":
				handler = management.NotFound
			}
		}
		public.Handle(prefix, handler)
	}
}

// rejectPublic answers a public request its surface's pool could not admit.
// Inference surfaces share the gateway's error shape so clients see the same
// envelope as every other overload; management requests get the problem
// document the console's handlers produce.
func rejectPublic(w http.ResponseWriter, r *http.Request, surface string) {
	if surface == "management" {
		access.WriteProblem(w, access.Fail(http.StatusServiceUnavailable, "request_admission_overloaded", "The service is temporarily overloaded."))
		return
	}
	gateway.WriteAdmissionOverload(w, r)
}
