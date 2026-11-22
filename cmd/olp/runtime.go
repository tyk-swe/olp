package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/billing/imports"
	"github.com/tyk-swe/olp/internal/cache"
	"github.com/tyk-swe/olp/internal/export/capture"
	"github.com/tyk-swe/olp/internal/export/destination"
	"github.com/tyk-swe/olp/internal/export/focus"
	"github.com/tyk-swe/olp/internal/export/stream"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	limitadmission "github.com/tyk-swe/olp/internal/limits/admission"
	"github.com/tyk-swe/olp/internal/platform/coordination"
	"github.com/tyk-swe/olp/internal/platform/egress"
	"github.com/tyk-swe/olp/internal/platform/secrets"
	"github.com/tyk-swe/olp/internal/platform/secretstore"
	"github.com/tyk-swe/olp/internal/platform/telemetry"
	"github.com/tyk-swe/olp/internal/plugins"
	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/routing"
)

type namedCloser struct {
	name  string
	close func(context.Context) error
}

type runtimeCore struct {
	auth          *secrets.AuthKey
	keys          *secrets.KeyRing
	policy        *egress.Policy
	business      *telemetry.BusinessMetrics
	responseCache *cache.Store
	exporter      *stream.Worker
	focusWorker   *focus.Worker
	capturer      *capture.Manager
	importer      *imports.Worker
	unconfined    *plugins.Unconfined
	pluginHost    *plugins.Host
	rt            *release.Manager
	closers       []namedCloser
}

func newRuntimeCore(startup context.Context, c Config, pool, readPool *pgxpool.Pool, installation string, vk *coordination.Client, limiter *limitadmission.Limiter, deadline *time.Time, log *slog.Logger) (runtimeCore, error) {
	auth, keys, err := loadSecrets(startup, c, installation)
	if err != nil {
		return runtimeCore{}, err
	}
	policy := &egress.Policy{AllowedNetworks: c.ProviderEgressAllowCIDRs, PlainHTTPHosts: c.ProviderEgressAllowHTTPHosts}
	core := runtimeCore{
		auth:     auth,
		keys:     keys,
		policy:   policy,
		business: telemetry.NewBusinessMetrics(c.MetricsSeriesCap, c.MetricsTenantLabels),
	}
	fail := func(err error) (runtimeCore, error) {
		for i := len(core.closers) - 1; i >= 0; i-- {
			closer := core.closers[i]
			closeDependency(deadline, c.ShutdownTimeout, log, closer.name, closer.close)
		}
		return runtimeCore{}, err
	}
	if c.CacheValkeyURL != "" {
		var main cache.Commander
		if vk != nil {
			main = vk
		}
		remote, err := cache.NewRemote(c.CacheValkeyURL, c.CacheValkeyCAFile, main)
		if err != nil {
			return runtimeCore{}, err
		}
		core.closers = append(core.closers, namedCloser{name: "response cache", close: func(context.Context) error { remote.Close(); return nil }})
		core.responseCache = &cache.Store{Client: remote, Keys: keys, Auth: auth, Installation: installation}
	}
	sender := destination.NewSender(policy, os.Getenv, installation)
	core.exporter = &stream.Worker{Pool: pool, Keys: keys, Installation: installation, Sender: sender, Log: log}
	core.focusWorker = &focus.Worker{Pool: pool, Keys: keys, Installation: installation, Sender: sender, Version: Version, Log: log, Now: time.Now}
	core.capturer = capture.NewManager(pool, keys, installation, sender, log)
	core.importer = imports.NewWorker(imports.Deps{Pool: pool, Keys: keys, Installation: installation, Egress: *policy, Getenv: os.Getenv, Log: log, Now: time.Now})
	var mounted map[string]release.MountedProvider
	if c.ConnectorConfigFile != "" {
		mounted, err = providers.LoadMounted(c.ConnectorConfigFile, policy)
		if err != nil {
			return fail(err)
		}
	}
	if c.UnconfinedPluginDir != "" {
		core.unconfined = plugins.NewUnconfined(c.UnconfinedPluginDir, log)
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
	hostRuntime, err := plugins.NewRuntime(startup, engine, log)
	if err != nil {
		return fail(err)
	}
	core.closers = append(core.closers, namedCloser{name: "plugin runtime", close: hostRuntime.Close})
	core.pluginHost = plugins.NewHost(hostRuntime, core.unconfined, pool)
	core.rt = release.NewManager(release.ManagerConfig{
		Pool: pool, ReadPool: readPool, Installation: installation,
		Auth: auth, Keys: keys, Log: log, Hostname: c.Hostname, Region: c.Region,
		ExternalSecrets: secretstore.New(*policy, os.Getenv), Mounted: mounted,
		PrepareGuardrailPlugin: core.pluginHost.PinGuardrail,
		SyncSharedQuotas: func(ctx context.Context, previous, next *routing.View) error {
			return quota.SyncSharedQuotas(ctx, limiter, previous, next)
		},
	})
	pluginHost := core.pluginHost
	core.closers = append(core.closers, namedCloser{name: "plugin host", close: func(ctx context.Context) error { pluginHost.Close(ctx); return nil }})
	return core, nil
}
