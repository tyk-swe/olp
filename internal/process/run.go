// Package process explicitly composes the resources owned by each process mode.
package process

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/config"
	"github.com/tyk-swe/olp/internal/console"
	"github.com/tyk-swe/olp/internal/coordination"
	"github.com/tyk-swe/olp/internal/database"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/gateway"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/management"
	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/routes"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/usage"
)

// metadataBuffer is how many request metadata events one inference replica may
// hold while the stream writer catches up. Beyond it events are dropped and
// counted as loss, so a slow or unreachable stream costs completeness rather
// than memory or inference latency.
const metadataBuffer = 8192

func Run(ctx context.Context, c config.Config, log *slog.Logger) error {
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	if err := c.Validate(); err != nil {
		return err
	}
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
	// These prefixes must never fall through to the SPA, in any public mode.
	// The gateway owns /v1/ when inference is enabled; the Anthropic and
	// Gemini surfaces arrive with M5 and stay honestly unimplemented.
	for _, prefix := range []string{"/api/", "/v1/", "/anthropic/", "/gemini/", "/v1beta/", "/openai/", "/health/", "/metrics"} {
		handler := http.HandlerFunc(http.NotFound)
		if c.Mode.Inference() {
			switch prefix {
			case "/v1/":
				continue
			case "/anthropic/", "/gemini/":
				handler = management.Unimplemented
			}
		}
		public.Handle(prefix, handler)
	}
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
	if c.Mode.Management() || c.Mode.Inference() {
		auth, keys, bootstrap, err := loadSecrets(c, installation)
		if err != nil {
			return err
		}
		policy := egress.Policy{AllowedNetworks: c.ProviderEgressAllowCIDRs, PlainHTTPHosts: c.ProviderEgressAllowHTTPHosts}
		rt = runtime.NewManager(pool, installation, auth, keys, log)
		gw := gateway.New(rt, &policy, gateway.Config{
			MaxInFlight:      c.MaxInFlightInference,
			MaxBodyBytes:     c.MaxJSONBodyBytes,
			MaxResponseBytes: c.ProviderMaxResponseBytes,
			MaxEventBytes:    c.ProviderMaxEventBytes,
			TrustedProxies:   c.TrustedProxyCIDRs,
		}, log)
		if c.Mode.Inference() {
			// Without shared state there is no admission backend at all: the
			// gateway then refuses traffic that carries hard limits rather
			// than serving it unmetered, and keeps logging its metadata.
			if limiter != nil {
				gw.Admission = gateway.NewAdmission(limiter, outage.Policy, log)
				emitter = usage.NewEmitter(metadataBuffer)
				gw.Sink = &gateway.AccountingSink{Emitter: emitter, Log: log}
			}
			gw.Register(public)
		}
		if c.Mode.Management() {
			control, err := access.New(startup, pool, installation, c.PublicOrigin, auth, keys, bootstrap)
			if err != nil {
				return err
			}
			control.LimitsEnforced = limiter != nil
			// The worker plane that applies retention runs only where shared
			// state is configured, and a control process cannot see whether a
			// separate worker replica is alive, so the console is told what
			// this installation is configured for.
			control.RetentionEnforced = limiter != nil
			control.Register(public)
			catalogue := providers.New(control, &policy)
			catalogue.Health = gw.Health()
			catalogue.Log = log
			if limiter != nil {
				catalogue.Quotas = limiter
			}
			catalogue.Register(public)
			routes.New(control).Register(public)
			(&gateway.Playground{Access: control, Gateway: gw}).Register(public)
			// Usage, pricing, request history and recovery reporting are part
			// of the management surface; their patterns are more specific than
			// its catch-all, which answers everything no surface claims.
			(&usage.Server{Access: control, VendorKind: providers.VendorKind}).Register(public)
		}
	}
	if err := startup.Err(); err != nil {
		return err
	}
	var authority func() bool
	if rt != nil {
		rt.Start(ctx)
		defer rt.Stop()
		authority = func() bool {
			status := rt.Authority()
			return status.Loaded && !status.Stale
		}
	}
	// The delivery plane outlives the listeners: it is cancelled only once the
	// gateway has drained, so every event a served request emitted is written
	// and this gateway's epoch is closed against what it actually delivered.
	delivery, stopDelivery := context.WithCancel(context.WithoutCancel(ctx))
	defer stopDelivery()
	var delivered sync.WaitGroup
	if emitter != nil {
		stream, instance := usage.StreamName(prefix), usage.GatewayInstance()
		delivered.Go(func() { emitter.RunWriter(delivery, vk, stream, log) })
		delivered.Go(func() { usage.RunLossReporter(delivery, pool, emitter, instance, log) })
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
		if limiter != nil {
			// The consumer reads its stream with a blocking XREADGROUP, and a
			// client answers one connection in order: on the shared client a
			// second spent blocking is a second every admission decision and
			// every emitted event waits behind. The consumer gets its own.
			reader, err := openValkey(startup)
			if err != nil {
				return err
			}
			defer reader.Close()
			workersStopped = startWorkers(workers, pool, reader, limiter, usage.StreamName(prefix), log)
		} else {
			log.Warn("worker plane skipped: no shared state is configured", "mode", c.Mode)
		}
	}
	private := healthHandler(ctx, c.RequestTimeout, pool.Ping, vk, authority)
	listeners := []struct {
		name, address string
		handler       http.Handler
	}{{"private", c.ObservabilityListenAddr, private}}
	if c.Mode.Public() {
		listeners = append(listeners, struct {
			name, address string
			handler       http.Handler
		}{"public", c.ListenAddr, public})
	}
	var servers []*http.Server
	var sockets []net.Listener
	for _, listener := range listeners {
		socket, err := (&net.ListenConfig{}).Listen(startup, "tcp", listener.address)
		if err != nil {
			for _, s := range sockets {
				s.Close()
			}
			return fmt.Errorf("bind %s listener: %w", listener.name, err)
		}
		sockets = append(sockets, socket)
		servers = append(servers, &http.Server{Handler: listener.handler, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: time.Minute, MaxHeaderBytes: 32 * 1024, BaseContext: func(net.Listener) context.Context { return ctx }})
	}
	errorsCh := make(chan error, len(servers))
	for i, server := range servers {
		go func() { errorsCh <- server.Serve(sockets[i]) }()
		log.Info("listener started", "mode", c.Mode, "listener", listeners[i].name, "address", sockets[i].Addr().String())
	}
	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-errorsCh:
	}
	stop()
	shutdown, cancel := context.WithTimeout(context.Background(), c.ShutdownTimeout)
	defer cancel()
	// Both listeners drain against the same deadline, then connections are forced
	// closed. Only after handlers finish do the concrete clients close.
	var wg sync.WaitGroup
	for _, server := range servers {
		wg.Go(func() {
			if err := server.Shutdown(shutdown); err != nil {
				server.Close()
			}
		})
	}
	wg.Wait()
	// Nothing serves any longer, so the emitter holds every event this process
	// will ever produce: the writer drains it and the loss reporter closes the
	// epoch before the workers that read the stream are told to stop.
	stopDelivery()
	awaitPlane(c.ShutdownTimeout, delivered.Wait, log, "request metadata delivery")
	stopWorkers()
	if workersStopped != nil {
		awaitPlane(c.ShutdownTimeout, workersStopped, log, "worker plane")
	}
	log.Info("process stopped", "mode", c.Mode)
	if errors.Is(serveErr, http.ErrServerClosed) {
		return nil
	}
	return serveErr
}

// awaitPlane waits for one background plane to close after it has been
// cancelled. Each plane gets the configured shutdown budget of its own, because
// draining buffered request metadata is what keeps an orderly stop from
// becoming a completeness gap; a plane that outlives its budget is left to the
// bounded cleanup it does on its own rather than holding the process open.
func awaitPlane(budget time.Duration, wait func(), log *slog.Logger, plane string) {
	done := make(chan struct{})
	go func() {
		wait()
		close(done)
	}()
	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		log.Warn("shutdown budget reached before the plane closed", "plane", plane)
	}
}

func loadSecrets(c config.Config, installation string) (*secrets.AuthKey, *secrets.KeyRing, string, error) {
	if c.AuthHMACKeyFile == "" || c.MasterKeyFile == "" {
		return nil, nil, "", errors.New("management and inference require OLP_AUTH_HMAC_KEY_FILE and OLP_MASTER_KEY_FILE")
	}
	raw, err := secrets.ReadFile(c.AuthHMACKeyFile)
	if err != nil {
		return nil, nil, "", err
	}
	key, err := secrets.DecodeKey(string(raw))
	if err != nil {
		return nil, nil, "", err
	}
	keys, err := secrets.LoadRing(c.MasterKeyFile)
	if err != nil {
		return nil, nil, "", err
	}
	var bootstrap string
	if c.BootstrapTokenFile != "" {
		data, err := secrets.ReadFile(c.BootstrapTokenFile)
		if err != nil {
			return nil, nil, "", err
		}
		bootstrap = string(data)
		if len(bootstrap) < 32 || len(bootstrap) > 256 {
			return nil, nil, "", errors.New("bootstrap token must have 32–256 bytes")
		}
	}
	return secrets.NewAuthKey(key, installation), keys, bootstrap, nil
}

func healthHandler(process context.Context, timeout time.Duration, pingDB func(context.Context) error, vk *coordination.Client, authority func() bool) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, r *http.Request) {
		writeHealth(w, http.StatusOK, map[string]any{"live": true})
	})
	mux.HandleFunc("GET /health/ready", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		dbReady := pingDB(ctx) == nil
		valkeyReady := true
		if vk != nil {
			valkeyReady = vk.Ping(ctx) == nil
		}
		authorityReady := true
		if authority != nil {
			authorityReady = authority()
		}
		ready := process.Err() == nil && dbReady && valkeyReady && authorityReady
		status := http.StatusOK
		if !ready {
			status = http.StatusServiceUnavailable
		}
		dependencies := map[string]bool{"postgres": dbReady}
		if vk != nil {
			dependencies["valkey"] = valkeyReady
		}
		if authority != nil {
			dependencies["authority"] = authorityReady
		}
		writeHealth(w, status, map[string]any{"ready": ready, "dependencies": dependencies})
	})
	return mux
}

func writeHealth(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}
