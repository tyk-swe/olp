package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/identity"
	"github.com/tyk-swe/olp/internal/keyauthority"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/apikeys"
	"github.com/tyk-swe/olp/internal/auditlog"
	"github.com/tyk-swe/olp/internal/budgets"
	"github.com/tyk-swe/olp/internal/cache/cacheadmin"
	"github.com/tyk-swe/olp/internal/codemode/codeadmin"
	"github.com/tyk-swe/olp/internal/consumerauth"
	"github.com/tyk-swe/olp/internal/settings"

	"github.com/tyk-swe/olp/internal/codesessions/codeaccounts"
	"github.com/tyk-swe/olp/internal/domain/vendors"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	"github.com/tyk-swe/olp/internal/gateway/surface/playground"
	"github.com/tyk-swe/olp/internal/guardrails/attachment"
	"github.com/tyk-swe/olp/internal/media/jobs"
	"github.com/tyk-swe/olp/internal/platform/secretstore"
	"github.com/tyk-swe/olp/internal/platform/staging"
	"github.com/tyk-swe/olp/internal/usage/completeness"
	"github.com/tyk-swe/olp/internal/usage/pricebook"
	"github.com/tyk-swe/olp/internal/usage/reports"

	"github.com/tyk-swe/olp/internal/guardrails/policy"
	"github.com/tyk-swe/olp/internal/routes/guardrailpreview"

	"github.com/tyk-swe/olp/internal/codesessions"
	"github.com/tyk-swe/olp/internal/guardrails/credential"
	"github.com/tyk-swe/olp/internal/guardrails/definition"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/agents"
	"github.com/tyk-swe/olp/internal/agents/a2a"
	"github.com/tyk-swe/olp/internal/agents/investigations"
	"github.com/tyk-swe/olp/internal/agents/publications"
	"github.com/tyk-swe/olp/internal/agents/runs"
	"github.com/tyk-swe/olp/internal/agents/tasks"
	"github.com/tyk-swe/olp/internal/billing/imports"
	"github.com/tyk-swe/olp/internal/billing/reconciliation"
	"github.com/tyk-swe/olp/internal/cache"
	"github.com/tyk-swe/olp/internal/catalog"
	"github.com/tyk-swe/olp/internal/export/capture"
	"github.com/tyk-swe/olp/internal/export/destination"
	"github.com/tyk-swe/olp/internal/export/focus"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/grants/personal"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/limits/admission"
	"github.com/tyk-swe/olp/internal/management"
	"github.com/tyk-swe/olp/internal/mcp/accounting"
	"github.com/tyk-swe/olp/internal/mcp/managementtools"
	"github.com/tyk-swe/olp/internal/mcp/registration"
	"github.com/tyk-swe/olp/internal/notifications"
	"github.com/tyk-swe/olp/internal/observability"
	"github.com/tyk-swe/olp/internal/platform/egress"
	"github.com/tyk-swe/olp/internal/platform/secrets"
	"github.com/tyk-swe/olp/internal/plugins"
	"github.com/tyk-swe/olp/internal/plugins/pluginindex"
	"github.com/tyk-swe/olp/internal/projects"
	"github.com/tyk-swe/olp/internal/promotion/project"
	promotionserve "github.com/tyk-swe/olp/internal/promotion/serve"
	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/retrieval"
	"github.com/tyk-swe/olp/internal/routes"
	"github.com/tyk-swe/olp/internal/routes/modelcatalog"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/signing"
	"github.com/tyk-swe/olp/internal/sinks"
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

func (m Management) registerGuardrails(mux *http.ServeMux) {
	publish := func(ctx context.Context, tx pgx.Tx, actor string) error {
		_, err := release.Publish(ctx, tx, actor)
		return err
	}
	credential.NewAPI(m.Access).Register(mux)
	definition.NewAPI(m.Access, publish).Register(mux)
	policy.NewAPI(m.Access, publish, func(r *http.Request, p authz.Principal, pinned guardrails.Pinned, in guardrails.Application) (guardrails.Result, error) {
		result, err := m.Gateway.EvaluateGuardrails(r, guardrailsMember(p), pinned, in)
		return result, guardrailsHTTPError(err)
	}, func(r *http.Request, p authz.Principal, batch []guardrails.PinnedApplication) ([]guardrails.Result, error) {
		results, err := m.Gateway.EvaluateGuardrailsBatch(r, guardrailsMember(p), batch)
		return results, guardrailsHTTPError(err)
	}).Register(mux)
	attachment.NewAPI(m.Access, publish).Register(mux)
	guardrailpreview.Register(m.Access, mux, m.Runtime.Release)
}

func guardrailsMember(p authz.Principal) kernel.Member {
	return kernel.Member{
		UserID: p.UserID(),
		Admit: func(route *routing.Route) *fault.Error {
			if err := p.Authorize(authz.Playground); err != nil {
				return fault.PermissionError("guardrail_forbidden", "Playground access is required.")
			}
			if err := p.Project(route.ProjectID, authz.View); err != nil {
				return fault.ModelNotFound(route.Slug)
			}
			return nil
		},
	}
}

func guardrailsHTTPError(err error) error {
	failure, ok := errors.AsType[*fault.Error](err)
	if !ok {
		return err
	}
	if failure.Code == "guardrails_unavailable" || failure.Code == fault.Overloaded.Code {
		return httpkit.Fail(failure.Status, failure.Code, failure.Message)
	}
	return httpkit.Fail(failure.Status, failure.Code, "The guardrail could not be evaluated.")
}

// Register mounts the whole management API: the published contract, every
// feature's routes, and the catch-all that answers 404 for the rest.
func (m Management) Register(mux *http.ServeMux) {
	management.Register(mux)
	managementtools.NewMCP(m.Access, mux).Register(mux)
	m.Identity.Register(mux)
	projects.New(m.Access).Register(mux)
	notifications.NewManagement(m.Access, *m.Egress).Register(mux)
	(&release.DelegationManagement{Access: m.Access, Manager: m.Runtime}).Register(mux)
	release.RegisterGenerations(m.Access, mux)
	cacheadmin.NewAPI(m.Access, m.ResponseCache).Register(mux)
	m.registerGuardrails(mux)
	a2a.NewManagement(m.Access, m.AgentTasks, agents.NewTaskOperations(m.AgentTasks, m.AgentPeers, m.AgentGrants)).Register(mux)
	publications.New(m.Access, m.Egress, m.AgentPeers.ValidateCard).Register(mux)
	runs.NewManagement(m.Access).Register(mux)
	tasks.NewManagement(m.Access).Register(mux)
	investigations.NewManagement(m.Access).Register(mux)
	mcpServers := registration.New(m.Access, m.Egress, registration.NewExecutables(m.Unconfined, m.Access.Pool))
	mcpServers.Register(mux)
	accounting.NewManagement(m.Access).Register(mux)
	personal.New(m.Access, mcpServers).Register(mux)
	(&retrieval.Server{Access: m.Access}).Register(mux)
	(&sinks.Server{Access: m.Access, Egress: m.Egress}).Register(mux)
	(&modelcatalog.Server{Access: m.Access, Runtime: m.Runtime, AuthenticateKey: m.catalogAuthority, Origin: m.Access.Origin}).Register(mux)
	routeServer := &routes.Server{
		Access: m.Access, InlineMedia: m.Gateway.InlineMediaLimits(),
		Workflows: m.Gateway.WorkflowResolver(), WorkflowRelease: m.Runtime.Release,
		Inputs: m.Runtime.RoutingInputs, Region: m.Runtime.Region(),
		UnconfinedPlugins: m.Unconfined != nil, Catalog: m.Catalog,
	}
	if m.Limits != nil {
		routeServer.Supply = quota.NewAdmission(m.Limits.Limiter, m.Limits.Fleet, nil, m.Log)
		routeServer.Fleet = m.Limits.Fleet
	}
	providerServer := providers.New(providers.Deps{
		Access: m.Access, Egress: m.Egress, Getenv: os.Getenv, ExternalSecrets: secretstore.New(*m.Egress, os.Getenv),
		Unconfined: m.Unconfined, Log: m.Log, Plugins: m.PluginHost, Catalog: m.Catalog,
		Activated: routeServer.Activated,
		Quotas: func() providers.QuotaSource {
			if m.Limits == nil {
				return nil
			}
			return m.Limits.Fleet
		}(),
	})
	providerServer.Register(mux)
	codeaccounts.NewAPI(m.Access).Register(mux)
	codeadmin.NewAPI(m.Access).Register(mux)
	settings.NewAPI(m.Access).Register(mux)
	budgets.NewAPI(m.Access).Register(mux)
	apikeys.NewAPI(m.Access, m.KeyOverlaps).Register(mux)
	auditlog.NewAPI(m.Access).Register(mux)
	routeServer.Register(mux)
	playgroundAPI := playground.NewAPI(m.Gateway)
	playgroundLimits := []access.RouteOption{access.MaxBody(playground.BodyLimit), access.Deadline(playground.Timeout)}
	m.Access.Route(mux, "POST /api/v1/playground", playgroundAPI.Handle, playgroundLimits...)
	m.Access.Stream(mux, "POST /api/v1/playground/stream", playgroundAPI.Stream, playgroundLimits...)
	(&jobs.API{Access: m.Access, Jobs: m.Media, Spool: m.Spool, Log: m.Log}).Register(mux)
	codesessions.NewManagement(m.Access).Register(mux)
	(&resources.Management{Access: m.Access, Pool: m.Access.Pool}).Register(mux)
	(&management.Overview{Access: m.Access}).Register(mux)
	health := &observability.Management{Access: m.Access, Cache: m.Health, Pool: m.Access.Pool}
	if m.Limits != nil {
		health.Fleet = m.Limits.Fleet
	}
	health.Register(mux)
	(&plugins.Management{Access: m.Access, Runtime: m.PluginRuntime, Host: m.PluginHost, Unconfined: m.Unconfined, Index: m.PluginIndex}).Register(mux)
	// Usage, pricing, request history and recovery reporting are part
	// of the management surface; their patterns are more specific than
	// its catch-all, which answers everything no surface claims.
	(&reports.API{Access: m.Access}).Register(mux)
	(&pricebook.API{Access: m.Access, Normalizer: pricebook.NewNormalizer(vendors.Kind), Egress: m.Egress, Catalog: m.Catalog, CatalogKeys: m.CatalogKeys}).Register(mux)
	(&completeness.API{Access: m.Access}).Register(mux)
	destination.NewAPI(m.Access, m.Egress).Register(mux)
	focus.NewAPI(m.Access).Register(mux)
	capture.NewAPI(m.Access).Register(mux)
	imports.NewAPI(m.Access).Register(mux)
	reconciliation.NewAPI(m.Access, time.Now).Register(mux)
	promotionserve.Register(mux, promotionserve.Deps{
		Access:     m.Access,
		SignIn:     m.Identity,
		Egress:     m.Egress,
		Unconfined: m.Unconfined,
		Identity:   m.IdentityClient,
		Costs:      project.CostsOf(m.limiter()),
		Normalizer: pricebook.NewNormalizer(vendors.Kind),
		Drafts:     providerServer,
	})
}

func (m Management) catalogAuthority(r *http.Request) (keyauthority.Authority, error) {
	if m.Gateway == nil {
		return keyauthority.Authority{}, httpkit.Fail(503, "authority_unavailable", "Key authority is unavailable.")
	}
	authority, failure := m.Gateway.Gate.Authenticate(r, "models_read")
	if failure != nil {
		return keyauthority.Authority{}, httpkit.Fail(failure.Status, failure.Code, failure.Message)
	}
	return authority, nil
}

func (m Management) limiter() *admission.Limiter {
	if m.Limits == nil {
		return nil
	}
	return m.Limits.Limiter
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
	identityClient := egress.IdentityClient(nil)
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
