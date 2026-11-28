package main

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/agents/a2a"
	"github.com/tyk-swe/olp/internal/agents/tasks"
	"github.com/tyk-swe/olp/internal/apikeys"
	"github.com/tyk-swe/olp/internal/cache"
	"github.com/tyk-swe/olp/internal/catalog"
	"github.com/tyk-swe/olp/internal/consumerauth"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/grants/personal"
	"github.com/tyk-swe/olp/internal/identity"
	"github.com/tyk-swe/olp/internal/limits/admission"
	"github.com/tyk-swe/olp/internal/media/jobs"
	"github.com/tyk-swe/olp/internal/observability"
	"github.com/tyk-swe/olp/internal/platform/egress"
	"github.com/tyk-swe/olp/internal/platform/secrets"
	"github.com/tyk-swe/olp/internal/platform/staging"
	"github.com/tyk-swe/olp/internal/plugins"
	"github.com/tyk-swe/olp/internal/plugins/pluginindex"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/signing"
)

// Management is everything the management API is composed from, so processes
// cannot drift from the routes the contract declares.
type Management struct {
	AgentTasks     *tasks.Store
	AgentPeers     *a2a.Peers
	AgentGrants    *personal.Store
	IdentityClient *http.Client
	Access         *access.Server
	KeyOverlaps    *apikeys.OverlapRecorder
	Identity       *identity.Server
	Egress         *egress.Policy
	Limits         *sharedLimits
	Runtime        *release.Manager
	Gateway        *kernel.Server
	ResponseCache  *cache.Store
	Media          *jobs.Service
	Spool          *staging.Spool
	Health         *observability.Cache
	Log            *slog.Logger
	PluginRuntime  *plugins.Runtime
	PluginHost     *plugins.Host
	Unconfined     *plugins.Unconfined
	// Catalog is the reference catalog this release ships, verified at
	// start-up.
	Catalog     *catalog.Signed
	CatalogKeys signing.Keyring
	// PluginIndex is the signed index of reviewed plugins this release
	// ships, verified at start-up.
	PluginIndex *pluginindex.Signed
}

// Register mounts the whole management API: the published contract, every
// feature's routes, and the catch-all that answers 404 for the rest.
func (m Management) Register(mux *http.ServeMux) {
	m.registerOperations(mux)
	m.registerAccess(mux)
	m.registerGuardrails(mux)
	m.registerAgents(mux)
	routeServer := m.registerRouting(mux)
	providerServer := m.registerProviders(mux, routeServer.Activated)
	m.registerCode(mux)
	m.registerInference(mux)
	m.registerAccounting(mux)
	m.registerPromotion(mux, providerServer)
}

type managementInputs struct {
	config       Config
	pool         *pgxpool.Pool
	installation string
	core         runtimeCore
	limiter      *admission.Limiter
	shared       *sharedLimits
	gateway      *kernel.Server
	keyOverlaps  *apikeys.OverlapRecorder
	agentTasks   *tasks.Store
	agentPeers   *a2a.Peers
	agentGrants  *personal.Store
	media        *jobs.Service
	spool        *staging.Spool
	health       *observability.Cache
	catalog      *catalog.Signed
	catalogKeys  signing.Keyring
	pluginIndex  *pluginindex.Signed
	log          *slog.Logger
}

func newManagement(startup context.Context, in managementInputs) (Management, error) {
	c, core, log := in.config, in.core, in.log
	if err := access.PrepareInstallation(startup, in.pool, in.installation, core.auth, core.keys); err != nil {
		return Management{}, err
	}
	identityClient := access.IdentityHTTPClient()
	control := access.New(access.Config{
		Pool: in.pool, Installation: in.installation, Origin: c.PublicOrigin, Auth: core.auth, Keys: core.keys, Egress: core.policy, Log: log,
		OIDCClient: identityClient,
		RuntimePublisher: func(ctx context.Context, tx pgx.Tx, actor string) error {
			_, err := release.Publish(ctx, tx, actor)
			return err
		},
		ClientIP:           func(r *http.Request) string { return consumerauth.ClientIP(r, c.TrustedProxyCIDRs) },
		LocalLoginDisabled: !c.LocalLoginEnabled, LimitsEnforced: in.limiter != nil,
	})
	signIn := identity.New(control, identity.Config{
		Bootstrap:                   func() (string, bool) { return secrets.ReadBootstrapToken(c.BootstrapTokenFile) },
		ManagementNetworkRestricted: len(c.ManagementAllowedCIDRs) != 0,
		ResponseCacheConfigured:     core.responseCache != nil,
	})
	// Installing reads a module's manifest once, which the
	// interpreter is ready to do several times sooner.
	pluginRuntime, err := plugins.NewRuntime(startup, plugins.Interpreted, log)
	if err != nil {
		return Management{}, err
	}
	return Management{
		AgentTasks: in.agentTasks, AgentPeers: in.agentPeers, AgentGrants: in.agentGrants,
		Access: control, Identity: signIn, IdentityClient: identityClient, Egress: core.policy, Limits: in.shared, Runtime: core.rt,
		Gateway: in.gateway, KeyOverlaps: in.keyOverlaps, ResponseCache: core.responseCache, Media: in.media, Spool: in.spool, Health: in.health,
		Log: log, PluginRuntime: pluginRuntime, PluginHost: core.pluginHost, Unconfined: core.unconfined,
		Catalog: in.catalog, CatalogKeys: in.catalogKeys, PluginIndex: in.pluginIndex,
	}, nil
}
