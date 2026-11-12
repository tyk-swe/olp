package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/admission"
	"github.com/tyk-swe/olp/internal/agents"
	"github.com/tyk-swe/olp/internal/agents/a2a"
	"github.com/tyk-swe/olp/internal/agents/runs"
	"github.com/tyk-swe/olp/internal/agents/tasks"
	"github.com/tyk-swe/olp/internal/batches"
	"github.com/tyk-swe/olp/internal/billing/imports"
	"github.com/tyk-swe/olp/internal/billing/reconciliation"
	"github.com/tyk-swe/olp/internal/cache"
	"github.com/tyk-swe/olp/internal/catalog"
	"github.com/tyk-swe/olp/internal/codesessions"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/console"
	"github.com/tyk-swe/olp/internal/coordination"
	"github.com/tyk-swe/olp/internal/database"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/export/capture"
	"github.com/tyk-swe/olp/internal/export/destination"
	"github.com/tyk-swe/olp/internal/export/focus"
	"github.com/tyk-swe/olp/internal/export/stream"
	"github.com/tyk-swe/olp/internal/gateway"
	"github.com/tyk-swe/olp/internal/grants/personal"
	"github.com/tyk-swe/olp/internal/guardrails"
	limitadmission "github.com/tyk-swe/olp/internal/limits/admission"
	limitdebits "github.com/tyk-swe/olp/internal/limits/debits"
	"github.com/tyk-swe/olp/internal/mcp"
	mcpaccounting "github.com/tyk-swe/olp/internal/mcp/accounting"
	"github.com/tyk-swe/olp/internal/mcp/registration"
	"github.com/tyk-swe/olp/internal/mcp/sessions"
	"github.com/tyk-swe/olp/internal/mcp/upstream"
	"github.com/tyk-swe/olp/internal/media/jobs"
	"github.com/tyk-swe/olp/internal/media/transport"
	"github.com/tyk-swe/olp/internal/observability"
	"github.com/tyk-swe/olp/internal/operationregistry"
	"github.com/tyk-swe/olp/internal/pluginindex"
	"github.com/tyk-swe/olp/internal/plugins"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/providers/codeaccounts"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/secretstore"
	"github.com/tyk-swe/olp/internal/signing"
	"github.com/tyk-swe/olp/internal/staging"
	"github.com/tyk-swe/olp/internal/surface"
	"github.com/tyk-swe/olp/internal/telemetry"
	"github.com/tyk-swe/olp/internal/usage"
	"github.com/tyk-swe/olp/internal/usage/completeness"
	"github.com/tyk-swe/olp/internal/usage/delivery"
)

// metadataBuffer is how many request metadata events one producer replica may
// hold while the stream writer catches up. Beyond it events are dropped and
// counted as loss, so a slow or unreachable stream costs completeness rather
// than memory or inference latency.
const metadataBuffer = 8192

// Version is the build identity recorded on exported traces.
var Version = "dev"

func Run(ctx context.Context, c Config, log *slog.Logger) error {
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	if err := operationregistry.Validate(); err != nil {
		return fmt.Errorf("operation catalog: %w", err)
	}
	if err := c.Validate(); err != nil {
		return err
	}
	// The signed documents a release ships verify before anything starts: a
	// tampered reference catalog must never feed discovery, prices or token
	// estimates.
	catalogKeys, err := signing.Trusted()
	if err != nil {
		return err
	}
	referenceCatalog, err := catalog.Embedded()
	if err != nil {
		return err
	}
	log.Info("reference catalog verified", "sha256", referenceCatalog.SHA256, "key_id", referenceCatalog.KeyID, "published_at", referenceCatalog.Catalog.PublishedAt)
	pluginIndex, err := pluginindex.Embedded()
	if err != nil {
		return err
	}
	log.Info("plugin index verified", "sha256", pluginIndex.SHA256, "key_id", pluginIndex.KeyID, "published_at", pluginIndex.Index.PublishedAt)
	warnEgressExceptions(log, c)
	// Tracing is installed before any listener binds: an invalid endpoint or
	// header file must stop startup rather than trace half a process.
	traces, err := telemetry.Install(telemetry.Config{
		Endpoint:    c.OTLPTracesEndpoint,
		HeadersFile: c.OTLPHeadersFile,
		SampleRatio: c.TraceSampleRatio,
		Propagation: telemetry.Propagation{Upstream: c.TracePropagateUpstream, AcceptInbound: c.TraceAcceptInbound},
		Mode:        string(c.Mode),
		Version:     Version,
	}, os.LookupEnv)
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
		public.Handle("/", assets)
		public.Handle("/health", assets)
	}
	reservePublicPaths(public, c.Mode)
	startup, cancelStartup := context.WithTimeout(ctx, c.StartupTimeout)
	defer cancelStartup()
	pool, err := database.Open(startup, pgConfig)
	if err != nil {
		return err
	}
	defer closeDependency(&shutdownDeadline, c.ShutdownTimeout, log, "PostgreSQL", func(context.Context) error { pool.Close(); return nil })
	var readPool *pgxpool.Pool
	if c.DatabaseReadURL != "" && (c.Mode.Inference() || c.Mode == Worker) {
		readConfig, err := database.Configuration(c.DatabaseReadURL, c.DatabaseMaxConnections, c.RequestTimeout)
		if err != nil {
			return errors.New("invalid OLP_DATABASE_READ_URL")
		}
		readPool, err = database.Open(startup, readConfig)
		if err != nil {
			return errors.New("runtime read database connection failed")
		}
		defer closeDependency(&shutdownDeadline, c.ShutdownTimeout, log, "runtime read PostgreSQL", func(context.Context) error { readPool.Close(); return nil })
	}
	installation, err := database.Installation(startup, pool)
	if err != nil {
		return err
	}
	if readPool != nil {
		readInstallation, err := database.Installation(startup, readPool)
		if err != nil {
			return errors.New("runtime read database is not initialized at the current schema")
		}
		if readInstallation != installation {
			return errors.New("runtime read database belongs to another installation")
		}
	}
	// Shared state comes up before anything that admits or accounts for
	// traffic, because both the gateway and the console are told at
	// composition time whether this installation has a limiter at all.
	prefix := database.ValkeyNamespace(installation)
	debitStore := limitdebits.NewStore(pool)
	var shared *sharedLimits
	var limiter *limitadmission.Limiter
	var outage *outagePolicy
	if openValkey != nil {
		vk, err = openValkey(startup)
		if err != nil {
			return err
		}
		defer closeDependency(&shutdownDeadline, c.ShutdownTimeout, log, "Valkey", func(context.Context) error { vk.Close(); return nil })
		if shared, err = newSharedLimits(vk, prefix+"limits", debitStore); err != nil {
			return err
		}
		limiter = shared.Limiter
		if c.Mode.Inference() {
			if outage, err = newOutagePolicy(startup, pool, log); err != nil {
				return err
			}
		}
	}
	var accounting *delivery.Delivery
	var rt *release.Manager
	var gw *gateway.Server
	var agentMonitor *agents.TaskMonitor
	var agentTasks *tasks.Store
	var agentPeers *a2a.Peers
	var grantStore *personal.Store
	var mediaService *jobs.Service
	var mediaTransport *transport.Transport
	var mediaSpool *staging.Spool
	var pluginHost *plugins.Host
	var policy egress.Policy
	var auth *secrets.AuthKey
	var business *telemetry.BusinessMetrics
	var exporter *stream.Worker
	var capturer *capture.Manager
	var importer *imports.Worker
	var focusWorker *focus.Worker
	// The public listener's process-local admission pools. The inference pool
	// is shared with the gateway so middleware and direct handler calls bound
	// one capacity; media reconciliation gaps and durable metadata loss are
	// process counters the metrics endpoint renders.
	inferencePool := admission.NewPool(c.MaxInFlightInference).Queue(c.AdmissionQueueDepth, c.AdmissionQueueTimeout)
	managementPool := admission.NewPool(c.MaxInFlightManagement)
	lossCounters := &completeness.LossCounters{}
	var mediaGapsTotal atomic.Uint64
	obsCache := observability.NewCache()
	// Worker replicas also need the runtime manager and the key ring: media
	// reconciliation serves jobs against their pinned historical providers
	// with credentials from the manager's credential source.
	var keys *secrets.KeyRing
	var prober *gateway.Server
	if c.Mode.Management() || c.Mode.Inference() || c.Mode == Worker {
		var bootstrap string
		var err error
		auth, keys, bootstrap, err = loadSecrets(startup, pool, c, installation)
		if err != nil {
			return err
		}
		policy = egress.Policy{AllowedNetworks: c.ProviderEgressAllowCIDRs, PlainHTTPHosts: c.ProviderEgressAllowHTTPHosts}
		business = telemetry.NewBusinessMetrics(c.MetricsSeriesCap, c.MetricsTenantLabels)
		var responseCache *cache.Store
		if c.CacheValkeyURL != "" {
			var main cache.Commander
			if vk != nil {
				main = vk
			}
			remote, err := cache.NewRemote(c.CacheValkeyURL, c.CacheValkeyCAFile, main)
			if err != nil {
				return err
			}
			defer remote.Close()
			responseCache = &cache.Store{Client: remote, Keys: keys, Auth: auth, Installation: installation}
		}

		sender := destination.NewSender(&policy, os.Getenv, installation)
		exporter = &stream.Worker{Pool: pool, Keys: keys, Installation: installation, Sender: sender, Log: log}
		focusWorker = &focus.Worker{Pool: pool, Keys: keys, Installation: installation, Sender: sender, Version: Version, Log: log, Now: time.Now}
		capturer = capture.NewManager(pool, keys, installation, sender, log)
		importer = imports.NewWorker(imports.Deps{Pool: pool, Keys: keys, Installation: installation, Egress: policy, Log: log, Now: time.Now})
		var mounted map[string]release.MountedProvider
		if c.ConnectorConfigFile != "" {
			mounted, err = providers.LoadMounted(c.ConnectorConfigFile, &policy)
			if err != nil {
				return err
			}
		}
		var unconfined *plugins.Unconfined
		if c.UnconfinedPluginDir != "" {
			unconfined = plugins.NewUnconfined(c.UnconfinedPluginDir, plugins.DefaultLimits(), log)
		}
		// Gateways and control's probes run signing hooks per upstream
		// request, and unconfined plugins may carry the requests themselves,
		// so their host compiles the modules it keeps. A worker runs a grant's
		// refresh once per access token, which the interpreter is ready for
		// sooner than the compiler.
		engine := plugins.Compiled
		if c.Mode == Worker {
			engine = plugins.Interpreted
		}
		hostRuntime, err := plugins.NewRuntime(startup, engine, plugins.DefaultLimits(), log)
		if err != nil {
			return err
		}
		defer closeDependency(&shutdownDeadline, c.ShutdownTimeout, log, "plugin runtime", hostRuntime.Close)
		pluginHost = plugins.NewHost(hostRuntime, unconfined, pool)
		rt = release.NewManager(release.ManagerConfig{
			Pool: pool, ReadPool: readPool, Installation: installation,
			Auth: auth, Keys: keys, Log: log, Hostname: c.Hostname, Region: c.Region,
			ExternalSecrets: secretstore.New(policy, os.Getenv), Mounted: mounted,
			PrepareGuardrailPlugin: pluginHost.PinGuardrail,
		})
		defer closeDependency(&shutdownDeadline, c.ShutdownTimeout, log, "plugin host", func(ctx context.Context) error { pluginHost.Close(ctx); return nil })
		var gatewayConfig, probeConfig gateway.Config
		gatewayDeps := gateway.Dependencies{Runtime: rt, Egress: &policy, Log: log, Sink: gateway.LogSink{Log: log}}
		probeDeps := gatewayDeps
		if c.Mode.Management() || c.Mode.Inference() {
			gatewayConfig = gateway.Config{
				Region:             c.Region,
				MaxInFlight:        c.MaxInFlightInference,
				MaxShadowInFlight:  c.MaxInFlightShadow,
				CORSAllowedOrigins: c.GatewayCORSAllowedOrigins,
				InlineMedia:        protocols.InlineMediaLimits{Items: c.MaxInlineMediaItems, ItemBytes: c.MaxInlineMediaItemBytes, TotalBytes: c.MaxInlineMediaTotalBytes},
				MaxBodyBytes:       c.MaxJSONBodyBytes,
				MaxMediaBodyBytes:  c.MaxMediaBodyBytes,
				MaxResponseBytes:   c.ProviderMaxResponseBytes,
				MaxEventBytes:      c.ProviderMaxEventBytes,
				TrustedProxies:     c.TrustedProxyCIDRs,
				AdmissionPool:      inferencePool,
				Signer:             pluginHost,
				Predicates:         pluginHost,
				Carrier:            pluginHost,
				UnconfinedPlugins:  unconfined != nil,
			}
			gatewayDeps.Traces = &gateway.TraceDeps{Pool: pool, Limiter: limiter, Auth: auth}
			gatewayDeps.Batches = batches.NewStore(pool)
			gatewayDeps.Cache = responseCache
			gatewayDeps.Business = business
			if limiter != nil {
				var policy func() limitadmission.OutagePolicy
				if outage != nil {
					policy = outage.Policy
				}
				// Control-only processes also execute playground requests.
				gatewayDeps.Admission = gateway.NewAdmission(limiter, shared.Fleet, policy, log)
			}
		}
		if limiter != nil && (c.Mode == Worker || c.Mode == All) {
			// The worker plane's health probes run through a gateway of their
			// own: planned, gated, priced and emitted like any request.
			probeConfig = gateway.Config{
				Region:            c.Region,
				MaxInFlight:       1,
				MaxResponseBytes:  c.ProviderMaxResponseBytes,
				MaxEventBytes:     c.ProviderMaxEventBytes,
				Signer:            pluginHost,
				Carrier:           pluginHost,
				UnconfinedPlugins: unconfined != nil,
			}
			probeDeps.Business = business
			probeDeps.Admission = gateway.NewAdmission(limiter, shared.Fleet, nil, log)
		}
		if c.Mode.Management() || c.Mode.Inference() || c.Mode == Worker {
			spool, err := staging.NewSpool(c.spoolDirectory(), c.MediaSpoolCapacityBytes, log)
			if err != nil {
				return err
			}
			mediaSpool = spool
			defer closeDependency(&shutdownDeadline, c.ShutdownTimeout, log, "media spool", func(context.Context) error { return spool.Close() })
			mediaTransport = transport.New(transport.Deps{
				Client:           policy.Client(gateway.UpstreamHeaderTimeout),
				Auth:             connectors.NewAuth(&policy),
				Egress:           &policy,
				Spool:            spool,
				MaxResponseBytes: c.ProviderMaxResponseBytes,
				Log:              log,
			})
			mediaService = jobs.New(jobs.Deps{
				Store:       jobs.NewStore(pool),
				Transport:   mediaTransport,
				Credentials: rt,
				Gaps:        &mediaGapsTotal,
				Log:         log,
				Now:         time.Now,
			})
		}
		if c.Mode.Management() || c.Mode.Inference() {
			grantStore = personal.NewStore(pool, installation, auth, keys, &policy)
			taskLimits := tasks.Limits{Debits: debitStore}
			if shared != nil {
				taskLimits = tasks.Limits{Debits: debitStore, Syncer: shared.Syncer, Limiter: limiter}
			}
			agentTasks = tasks.New(pool, taskLimits, auth)
			agentPeers = a2a.NewPeers(&policy, func(ctx context.Context, id string) ([]byte, error) {
				return keys.Read(ctx, pool, installation, id, secrets.AgentCredential)
			})
		}
		if c.Mode.Inference() {
			gatewayDeps.Capture = capturer
			gatewayDeps.Media = &gateway.MediaDeps{Transport: mediaTransport, Jobs: mediaService, Store: jobs.NewStore(pool)}
			gatewayDeps.Staging = mediaSpool
			gatewayDeps.Uploads = staging.NewAdmission(mediaSpool.CapacityBytes())
			if pool != nil {
				gatewayDeps.CodeLedger = codesessions.New(pool)
				gatewayDeps.CodeAuthorizer = &codeaccounts.Authorizer{Pool: pool, Credentials: rt, Plugins: pluginHost}
				gatewayDeps.CodeResearch = codesessions.New(pool)
				gatewayDeps.ResearchAuth = &codeaccounts.Authorizer{Pool: pool, Credentials: rt, Plugins: pluginHost}
				gatewayDeps.Resources = resources.NewEncrypted(pool, installation, keys)
				gatewayDeps.Resolver = resources.NewResolver(pool)
			}

			agentExecutor := agents.New(agents.Options{Tasks: agentTasks, Peers: agentPeers, Grants: grantStore, Entra: rt})
			agentServer := a2a.New(a2a.Options{Runtime: rt, Executor: agentExecutor, Peers: agentPeers, Grants: grantStore, TrustedProxies: c.TrustedProxyCIDRs, PublicOrigin: c.PublicOrigin})
			agentServer.Register(public)
			agentMonitor = agents.NewMonitor(agentTasks, agents.NewTaskOperations(agentTasks, agentPeers, grantStore), rt, log)
			var toolLedger mcpaccounting.Ledger
			if limiter != nil {
				toolLedger = mcpaccounting.NewDurableLedger(pool, debitStore, shared.Syncer, auth)
			}
			var sessionStore *sessions.Store
			if vk != nil {
				sessionStore, err = sessions.New(vk, prefix+"mcp")
				if err != nil {
					return err
				}
			}
			toolLimiter := func() mcp.RateLimiter {
				if limiter == nil {
					return nil
				}
				return limiter
			}()
			toolExecutor := mcp.NewExecutor(mcp.ExecutorOptions{
				Authority: rt, Grants: grantStore, Connect: upstream.NewDialer(&policy, registration.NewExecutables(unconfined, pool)),
				Ledger: toolLedger, Auth: auth, Limiter: toolLimiter, Log: log,
				Credentials: func(ctx context.Context, credentialID string) ([]byte, error) {
					return keys.Read(ctx, pool, installation, credentialID, secrets.MCPCredential)
				},
				Guardrails: func(ctx context.Context, in mcp.GuardrailEvaluation) (guardrails.Result, error) {
					return gw.EvaluateToolGuardrails(ctx, in.Authority, in.Release, in.Engine, in.RequestID, in.Phase, in.Segments, in.Masks)
				},
			})
			toolServer := mcp.New(mcp.ServerOptions{Runtime: rt, Executor: toolExecutor, Auth: auth, Grants: grantStore, Sessions: sessionStore, TrustedProxies: c.TrustedProxyCIDRs})
			if limiter != nil {
				gatewayDeps.WorkflowRuns = runs.NewWorkflowStore(pool, auth, keys, installation)
				gatewayDeps.WorkflowTools = toolServer
			}
			toolServer.Register(public)
		}
		// Every process that can spend against shared budgets owns a durable
		// producer epoch, including control-only playground calls and worker-only
		// health probes. No producer may log and forget a failed ledger write.
		// Without shared state, hard limits are refused and metadata stays logged.
		if limiter != nil {
			accounting = delivery.NewDelivery(pool, completeness.GatewayInstance(c.Hostname), metadataBuffer, lossCounters, log)
			for _, server := range []*gateway.Dependencies{&gatewayDeps, &probeDeps} {
				if server.Admission == nil {
					continue
				}
				server.Debits = gateway.DurableSessionDebits(pool, debitStore, shared.Syncer)
				server.Admission.PendingCostAccountingReady = shared.Syncer.CheckOwnerAccounting
				server.Admission.PendingTokenAccountingReady = resources.New(pool).CheckPendingTokenOwner
				server.Admission.CostAccountingReady = accounting.Check
				server.Sink = &gateway.AccountingSink{Delivery: accounting, Next: server.Sink}
			}
		}
		if c.Mode.Management() || c.Mode.Inference() {
			gw = gateway.New(gatewayDeps, gatewayConfig)
		}
		if probeDeps.Admission != nil {
			prober = gateway.New(probeDeps, probeConfig)
		}
		if c.Mode.Inference() {
			gw.Register(public)
		}
		if c.Mode.Management() {
			if err := access.PrepareInstallation(startup, pool, installation, auth, keys); err != nil {
				return err
			}
			identityClient := access.IdentityHTTPClient()
			control := access.New(access.Config{
				Pool: pool, Installation: installation, Origin: c.PublicOrigin, Auth: auth, Keys: keys, Bootstrap: bootstrap, Egress: &policy,
				OIDCClient: identityClient,
				RuntimePublisher: func(ctx context.Context, tx pgx.Tx, actor string) error {
					_, err := release.Publish(ctx, tx, actor)
					return err
				},
				ClientIP:           func(r *http.Request) string { return gateway.ClientIP(r, c.TrustedProxyCIDRs) },
				LocalLoginDisabled: !c.LocalLoginEnabled, ManagementNetworkRestricted: len(c.ManagementAllowedCIDRs) != 0,
				LimitsEnforced: limiter != nil, ResponseCacheConfigured: responseCache != nil, RetentionEnforced: limiter != nil, NotificationsActive: true,
			})
			// Installing reads a module's manifest once, which the
			// interpreter is ready to do several times sooner.
			pluginRuntime, err := plugins.NewRuntime(startup, plugins.Interpreted, plugins.DefaultLimits(), log)
			if err != nil {
				return err
			}
			defer closeDependency(&shutdownDeadline, c.ShutdownTimeout, log, "management plugin runtime", pluginRuntime.Close)
			Management{AgentTasks: agentTasks, AgentPeers: agentPeers, AgentGrants: grantStore, Getenv: os.Getenv, Access: control, IdentityClient: identityClient, Egress: &policy, Limits: shared, Runtime: rt, Gateway: gw, Media: mediaService, Spool: mediaSpool, Health: obsCache, Log: log, PluginRuntime: pluginRuntime, PluginHost: pluginHost, Unconfined: unconfined, Catalog: referenceCatalog, CatalogKeys: catalogKeys, PluginIndex: pluginIndex}.Register(public)
		}
	}
	if err := startup.Err(); err != nil {
		return err
	}
	// Finish dependency acquisition before starting any task. The one cleanup
	// owner below is registered after these closes, so every startup failure
	// drains/cancels and joins tasks before their dependencies are closed.
	var reader *coordination.Client
	var workerStream string
	if limiter != nil && (c.Mode == Worker || c.Mode == All) {
		reader, err = openValkey(startup)
		if err != nil {
			return err
		}
		defer closeDependency(&shutdownDeadline, c.ShutdownTimeout, log, "worker Valkey reader", func(context.Context) error { reader.Close(); return nil })
		workerStream = usage.StreamName(prefix)
	}
	auxiliary, stopAuxiliary := context.WithCancel(context.WithoutCancel(ctx))
	deliveryState, stopDelivery := context.WithCancel(context.WithoutCancel(ctx))
	workers, stopWorkers := context.WithCancel(context.WithoutCancel(ctx))
	probes, stopProbes := context.WithCancel(context.WithoutCancel(ctx))
	requestContext, cancelRequests := context.WithCancel(context.WithoutCancel(ctx))
	var background, delivered sync.WaitGroup
	var metadata *delivery.DeliveryRun
	var probesStopped chan struct{}
	var workersStopped func()
	var listeners []boundListener
	defer func() {
		stop()
		shutdownDeadline = time.Now().Add(c.ShutdownTimeout)
		shutdown, cancel := context.WithDeadline(context.Background(), shutdownDeadline)
		defer cancel()
		stopAuxiliary()
		drainListeners(shutdown, listeners, accounting)
		cancelRequests()
		if gw != nil {
			if err := gw.DrainShadows(shutdown); err != nil {
				log.Warn("shutdown budget reached before shadows drained", "error", err)
				if accounting != nil {
					accounting.MarkUnclean()
				}
			}
		}
		drainProbes(shutdown, stopProbes, probesStopped, accounting, log)
		// Accepted requests and probes have stopped producing while their live
		// authority remained available. Runtime joins against the same budget.
		if rt != nil {
			if err := rt.StopContext(shutdown); err != nil {
				log.Warn("shutdown budget reached before runtime stopped", "error", err)
			}
		}
		if metadata != nil {
			if err := metadata.Drain(shutdown); err != nil {
				log.Warn("shutdown budget reached before request metadata drained", "error", err)
			}
		}
		stopDelivery()
		awaitPlane(shutdown, delivered.Wait, log, "request metadata delivery")
		stopWorkers()
		if workersStopped != nil && !awaitPlane(shutdown, workersStopped, log, "worker plane") {
			// A blocking stream read must be explicitly unblocked at deadline;
			// closing it normally before cancellation races active consumers.
			if reader != nil {
				closeDependency(&shutdownDeadline, c.ShutdownTimeout, log, "forced worker Valkey reader", func(context.Context) error { reader.Close(); return nil })
			}
		}
		awaitPlane(shutdown, background.Wait, log, "background tasks")
		log.Info("process stopped", "mode", c.Mode)
	}()
	if pluginHost != nil {
		background.Go(func() {
			if err := pluginHost.PreparePinned(auxiliary); err != nil && auxiliary.Err() == nil {
				log.Warn("plugins could not be prepared", "error", err)
			}
		})
	}
	if rt != nil {
		// Authority must stay fresh while accepted requests drain, so
		// polling outlives the listeners and stops once handlers finish.
		var grantRefreshed func(string, string)
		if gw != nil {
			grantRefreshed = gw.GrantRefreshed
		}
		rt.Start(context.WithoutCancel(ctx), grantRefreshed)
	}
	if agentMonitor != nil {
		background.Go(func() { agentMonitor.Run(auxiliary) })
	}
	// The observability collectors probe only what this process composes: an
	// unconfigured dependency is reported as absent, never as failed.
	obsSources := observabilitySources(observed{
		region:        c.Region,
		servesGateway: c.Mode.Inference(),
		pool:          pool,
		limiter:       vk,
		runtime:       rt,
		policy:        &policy,
		gateway:       gw,
		accounting:    accounting,
		spool:         mediaSpool,
		media:         jobs.NewStore(pool),
		mediaGaps:     &mediaGapsTotal,
		loss:          lossCounters,
		export: func(ctx context.Context) []prometheus.Metric {
			captureRates, err := reconciliation.CaptureRates(ctx, pool, time.Now())
			if err != nil {
				log.Warn("billing capture rates could not be computed", "error", err)
			}
			return slices.Concat(exporter.Metrics(ctx), focusWorker.Metrics(ctx), capturer.Metrics(), importer.Metrics(), captureRates)
		},
	})
	background.Go(func() { obsCache.Run(auxiliary, obsSources, log) })
	// The delivery plane outlives the listeners: it is cancelled only once the
	// producers have drained, so every emitted event is written and this
	// process's epoch is closed against what it actually delivered.
	if accounting != nil {
		metadata, err = accounting.Start(startup, vk, usage.StreamName(prefix))
		if err != nil {
			return err
		}
	}
	delivered.Go(func() { capturer.Run(deliveryState) })
	if outage != nil {
		background.Go(func() { outage.run(auxiliary) })
	}
	if gw != nil {
		// Circuits are shared with the fleet only through Valkey; without it
		// this returns at once and local circuits alone decide.
		background.Go(func() { gw.RunFleetHealth(auxiliary) })
	}
	// The worker plane is cancelled last of all, because the consumer it runs
	// is what turns the events the delivery plane just flushed into accounting.
	if c.Mode == Worker || c.Mode == All {
		// The consumer reads its stream with a blocking XREADGROUP, and a
		// client answers one connection in order: on the shared client a
		// second spent blocking is a second every admission decision and
		// every emitted event waits behind. The consumer gets its own.
		// Media reconciliation and grant refresh need only PostgreSQL and the
		// provider egress client, so the plane runs even when no shared state
		// backend is configured. It is started exactly once, inside the single
		// worker plane.
		workersStopped = startWorkers(workers, pool, reader, debitStore, shared, workerStream, mediaService, rt, pluginHost, keys, auth, installation, c.Region, c.Hostname, &policy, log, exporter, focusWorker, importer, referenceCatalog)
	}
	// Probes produce accounting events, so they stop before delivery closes;
	// the consumer and recovery workers remain alive until delivery drains.
	if prober != nil {
		probesStopped = make(chan struct{})
		go func() {
			defer close(probesStopped)
			prober.RunHealthProbes(probes, probeCheckpoint(pool, log, c.Region))
		}()
	}
	live := []prometheus.Collector{runtimeMetrics(rt), admission.Metrics(inferencePool, managementPool), traces.Metrics()}
	if business != nil {
		live = append(live, business)
	}
	privateHandler, err := observability.NewHandler(obsCache, live...)
	if err != nil {
		return err
	}
	private := privateHandler.ServeMux()
	listenerConfigs := []listenerConfig{{name: "private", address: c.ObservabilityListenAddr, handler: private}}
	if c.Mode.Public() {
		// Network-permitted public requests enter their surface's pool before
		// routing: a full pool rejects without queueing, and tracing spans
		// open only for admitted requests.
		publicAdmission := &admission.Public{
			Inference:        inferencePool,
			Management:       managementPool,
			InferenceEnabled: c.Mode.Inference(),
			Reject:           rejectPublic,
		}
		listenerConfigs = append(listenerConfigs, listenerConfig{name: "public", address: c.ListenAddr, handler: Perimeter(c.PublicOrigin, managementNetwork(c.ManagementAllowedCIDRs, c.TrustedProxyCIDRs, publicAdmission.Wrap(traces.Middleware(installation)(public))))})
	}
	listeners, err = bindListeners(startup, listenerConfigs, c, requestContext)
	if err != nil {
		return err
	}
	serveErr := serveListeners(ctx, listeners, c.Mode, log)
	if errors.Is(serveErr, http.ErrServerClosed) {
		return nil
	}
	return serveErr
}

// reservePublicPaths keeps reserved prefixes from falling through to the SPA,
// in any public mode. When inference is enabled the gateway answers its
// catch-all prefixes itself and registers more specific handlers under the
// others; anything left over is answered in the prefix's own error envelope
// instead of reaching the console.
func reservePublicPaths(public *http.ServeMux, mode Mode) {
	for _, reserved := range surface.Reserved() {
		if mode.Management() && reserved.ManagementCatchAll {
			continue
		}
		handler := http.HandlerFunc(http.NotFound)
		if mode.Inference() {
			if reserved.GatewayCatchAll {
				continue
			}
			if reserved.Surface.Inference {
				handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					gateway.WriteNotFound(w, r, reserved.Surface.Name)
				})
			}
		}
		public.Handle(reserved.Path, handler)
	}
}

// drainProbes stops synthetic request production before metadata intake closes.
// A producer that exceeds the drain budget leaves its epoch open because a
// terminal event may still arrive after delivery stops.
func drainProbes(ctx context.Context, stop context.CancelFunc, stopped <-chan struct{}, accounting *delivery.Delivery, log *slog.Logger) {
	stop()
	if stopped != nil && !awaitPlane(ctx, func() { <-stopped }, log, "health probes") && accounting != nil {
		accounting.MarkUnclean()
	}
}

// awaitPlane waits for a plane to finish after intake is closed or its context
// is cancelled. All planes share one deadline, including HTTP draining.
// A plane that outlives it records a completeness gap rather than extending
// the deployment grace period.
func awaitPlane(ctx context.Context, wait func(), log *slog.Logger, plane string) bool {
	done := make(chan struct{})
	go func() {
		wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		log.Warn("shutdown budget reached before the plane closed", "plane", plane)
		return false
	}
}

func loadSecrets(ctx context.Context, pool *pgxpool.Pool, c Config, installation string) (*secrets.AuthKey, *secrets.KeyRing, string, error) {
	if c.AuthHMACKeyFile == "" || c.MasterKeyFile == "" && (c.Mode.Management() || c.Mode == Worker || c.ConnectorConfigFile == "") {
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
		keys, err = secrets.LoadRingWithUnwrapper(ctx, c.MasterKeyFile, secretstore.New(egress.Policy{AllowedNetworks: c.ProviderEgressAllowCIDRs, PlainHTTPHosts: c.ProviderEgressAllowHTTPHosts}, os.Getenv))
		if err != nil {
			return nil, nil, "", err
		}
	}
	var bootstrap string
	if c.BootstrapTokenFile != "" {
		data, err := secrets.ReadFile(c.BootstrapTokenFile)
		if errors.Is(err, os.ErrNotExist) {
			var complete bool
			if err := pool.QueryRow(ctx, "SELECT setup_complete FROM olp.installation WHERE singleton").Scan(&complete); err != nil {
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

// warnEgressExceptions announces operator exceptions to the provider egress
// denylist. They are installation-wide trust decisions that let every
// provider reach the listed networks, so each process says so at startup.
func warnEgressExceptions(log *slog.Logger, c Config) {
	if len(c.ProviderEgressAllowCIDRs) == 0 && len(c.ProviderEgressAllowHTTPHosts) == 0 {
		return
	}
	cidrs := make([]string, len(c.ProviderEgressAllowCIDRs))
	for i, prefix := range c.ProviderEgressAllowCIDRs {
		cidrs[i] = prefix.String()
	}
	log.Warn("provider egress exceptions widen what providers may reach",
		"allowed_cidrs", cidrs, "plain_http_hosts", c.ProviderEgressAllowHTTPHosts)
}

// rejectPublic answers a public request its surface's pool could not admit.
// Inference surfaces share the gateway's error shape so clients see the same
// envelope as every other overload; management requests get the problem
// document the console's handlers produce.
func rejectPublic(w http.ResponseWriter, r *http.Request, surface string) {
	if surface == "management" {
		problem := access.Fail(http.StatusServiceUnavailable, "request_admission_overloaded", "The service is temporarily overloaded.")
		if strings.HasPrefix(r.URL.Path, "/scim/") {
			access.WriteSCIMError(w, problem)
		} else {
			access.WriteProblem(w, problem)
		}
		return
	}
	gateway.WriteAdmissionOverload(w, r)
}
