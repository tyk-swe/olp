package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/tyk-swe/olp/internal/identity"

	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/admission"
	"github.com/tyk-swe/olp/internal/console"
	"github.com/tyk-swe/olp/internal/domain/operationregistry"
	"github.com/tyk-swe/olp/internal/domain/surface"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/observability"
	"github.com/tyk-swe/olp/internal/platform/coordination"
	"github.com/tyk-swe/olp/internal/platform/database"
	"github.com/tyk-swe/olp/internal/platform/egress"
	"github.com/tyk-swe/olp/internal/platform/panics"
	"github.com/tyk-swe/olp/internal/platform/secrets"
	"github.com/tyk-swe/olp/internal/platform/secretstore"
	"github.com/tyk-swe/olp/internal/platform/telemetry"
	"github.com/tyk-swe/olp/internal/usage/completeness"
	"github.com/tyk-swe/olp/internal/usage/delivery"
)

// Version is the build identity recorded on exported traces.
var Version = "dev"

func Run(ctx context.Context, c Config, log *slog.Logger) error {
	ctx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	if err := operationregistry.Validate(); err != nil {
		return fmt.Errorf("operation catalog: %w", err)
	}
	if err := c.Validate(); err != nil {
		return err
	}
	documents, err := loadSignedDocuments(log)
	if err != nil {
		return err
	}
	warnEgressExceptions(log, c)
	// Tracing is installed before any listener binds: an invalid endpoint or
	// header file must stop startup rather than trace half a process.
	traces, err := installTracing(c)
	if err != nil {
		return err
	}
	var shutdownDeadline time.Time
	defer flushTraces(traces, &shutdownDeadline, log)
	closing := func(d namedCloser) { closeDependency(&shutdownDeadline, c.ShutdownTimeout, log, d.name, d.close) }
	pgConfig, err := database.Configuration(c.DatabaseURL, c.DatabaseMaxConnections, c.RequestTimeout)
	if err != nil {
		return err
	}
	// Parse every dependency and asset before opening any listener.
	openValkey, err := valkeyOpener(c)
	if err != nil {
		return err
	}
	public, closeAssets, err := publicMux(c)
	if err != nil {
		return err
	}
	defer closeAssets()
	startup, cancelStartup := context.WithTimeout(ctx, c.StartupTimeout)
	defer cancelStartup()
	stores, err := openDatastores(startup, c, pgConfig, openValkey, &shutdownDeadline, log)
	if err != nil {
		return err
	}
	for _, closer := range stores.closers {
		defer closing(closer)
	}
	core, err := newRuntimeCore(startup, c, stores.pool, stores.readPool, stores.installation, stores.vk, &shutdownDeadline, log)
	if err != nil {
		return err
	}
	for _, closer := range core.closers {
		defer closing(closer)
	}
	var mediaGaps atomic.Uint64
	media, err := newMediaPlane(c, stores.pool, core.policy, core.rt, &mediaGaps, log)
	if err != nil {
		return err
	}
	defer closing(media.closer)
	base := foundation{
		config: c, log: log, stores: stores, core: core, media: media, public: public,
		inferencePool:  admission.NewPool(c.MaxInFlightInference).Queue(c.AdmissionQueueDepth, c.AdmissionQueueTimeout),
		managementPool: admission.NewPool(c.MaxInFlightManagement),
		health:         observability.NewCache(), loss: &completeness.LossCounters{}, mediaGaps: &mediaGaps,
	}
	serving, err := newServing(base)
	if err != nil {
		return err
	}
	if c.Mode.Management() {
		mgmt, err := newManagement(startup, managementInputs{
			config: c, pool: stores.pool, installation: stores.installation, core: core,
			limiter: stores.limiter, shared: stores.shared, gateway: serving.gateway,
			keyOverlaps: serving.overlaps,
			agentTasks:  serving.agents.tasks, agentPeers: serving.agents.peers, agentGrants: serving.agents.grants,
			media: media.service, spool: media.spool, health: base.health,
			catalog: documents.catalog, catalogKeys: documents.catalogKeys, pluginIndex: documents.pluginIndex, log: log,
		})
		if err != nil {
			return err
		}
		defer closing(namedCloser{name: "management plugin runtime", close: mgmt.PluginRuntime.Close})
		mgmt.Register(public)
	}
	if err := startup.Err(); err != nil {
		return err
	}
	// Finish dependency acquisition before starting any task. The one cleanup
	// owner below is registered after these closes, so every startup failure
	// drains/cancels and joins tasks before their dependencies are closed.
	feed, err := openWorkerFeed(startup, c, stores, openValkey)
	if err != nil {
		return err
	}
	if feed.reader != nil {
		defer closing(namedCloser{name: "worker Valkey reader", close: feed.close})
	}
	run := &running{
		foundation: base, lifetimes: newLifetimes(ctx, stop), serving: serving, feed: feed, traces: traces,
		reference: documents.catalog, deadline: &shutdownDeadline, supervise: supervisor(log, stop),
	}
	defer run.shutdown()
	if err := run.start(startup); err != nil {
		return err
	}
	return run.serve()
}

func installTracing(c Config) (*telemetry.Handle, error) {
	return telemetry.Install(telemetry.Config{
		Endpoint:    c.OTLPTracesEndpoint,
		HeadersFile: c.OTLPHeadersFile,
		SampleRatio: c.TraceSampleRatio,
		Propagation: telemetry.Propagation{Upstream: c.TracePropagateUpstream, AcceptInbound: c.TraceAcceptInbound},
		Mode:        string(c.Mode),
		Version:     Version,
	}, os.LookupEnv)
}

func flushTraces(traces *telemetry.Handle, deadline *time.Time, log *slog.Logger) {
	if deadline.IsZero() {
		*deadline = time.Now().Add(5 * time.Second)
	}
	shutdown, cancel := context.WithDeadline(context.Background(), *deadline)
	defer cancel()
	if err := traces.Shutdown(shutdown); err != nil {
		log.Warn("trace export did not flush cleanly", "error", err)
	}
}

func valkeyOpener(c Config) (func(context.Context) (*coordination.Client, error), error) {
	if c.ValkeyURL == "" {
		return nil, nil
	}
	configuration, err := coordination.Configuration(c.ValkeyURL, c.ValkeyCAFile, c.RequestTimeout)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context) (*coordination.Client, error) { return coordination.Open(ctx, configuration) }, nil
}

func publicMux(c Config) (*http.ServeMux, func() error, error) {
	public := http.NewServeMux()
	if !c.Mode.Management() {
		reservePublicPaths(public, c.Mode)
		return public, func() error { return nil }, nil
	}
	assets, closeAssets, err := console.Handler(c.ConsoleDir)
	if err != nil {
		return nil, nil, err
	}
	public.Handle("/", assets)
	public.Handle("/health", assets)
	reservePublicPaths(public, c.Mode)
	return public, closeAssets, nil
}

func supervisor(log *slog.Logger, stop context.CancelCauseFunc) func(func()) func() {
	return func(task func()) func() {
		return func() {
			defer panics.Recover(log, "background task", stop)
			task()
		}
	}
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
					kernel.WriteNotFound(w, r, reserved.Surface.Name)
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

func loadSecrets(ctx context.Context, c Config, installation string) (*secrets.AuthKey, *secrets.KeyRing, error) {
	if c.AuthHMACKeyFile == "" || c.MasterKeyFile == "" && (c.Mode.Management() || c.Mode == Worker || c.ConnectorConfigFile == "") {
		return nil, nil, errors.New("this mode requires OLP_AUTH_HMAC_KEY_FILE and OLP_MASTER_KEY_FILE")
	}
	raw, err := secrets.ReadFile(c.AuthHMACKeyFile)
	if err != nil {
		return nil, nil, err
	}
	key, err := secrets.DecodeKey(string(raw))
	if err != nil {
		return nil, nil, err
	}
	var keys *secrets.KeyRing
	if c.MasterKeyFile != "" {
		keys, err = secrets.LoadRingWithUnwrapper(ctx, c.MasterKeyFile, secretstore.New(egress.Policy{AllowedNetworks: c.ProviderEgressAllowCIDRs, PlainHTTPHosts: c.ProviderEgressAllowHTTPHosts}, os.Getenv))
		if err != nil {
			return nil, nil, err
		}
	}
	return secrets.NewAuthKey(key, installation), keys, nil
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
		problem := httpkit.Fail(http.StatusServiceUnavailable, "request_admission_overloaded", "The service is temporarily overloaded.")
		if strings.HasPrefix(r.URL.Path, "/scim/") {
			identity.WriteSCIMError(w, problem)
		} else {
			httpkit.WriteProblem(w, problem)
		}
		return
	}
	kernel.WriteAdmissionOverload(w, r)
}
