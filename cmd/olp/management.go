package main

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/tyk-swe/olp/internal/guardrails/attachment"
	"github.com/tyk-swe/olp/internal/media/jobs"
	"github.com/tyk-swe/olp/internal/staging"

	"github.com/tyk-swe/olp/internal/guardrails/policy"

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
	"github.com/tyk-swe/olp/internal/billing"
	"github.com/tyk-swe/olp/internal/branding"
	"github.com/tyk-swe/olp/internal/catalog"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/export/capture"
	"github.com/tyk-swe/olp/internal/export/destination"
	"github.com/tyk-swe/olp/internal/export/focus"
	"github.com/tyk-swe/olp/internal/gateway"
	"github.com/tyk-swe/olp/internal/grants/personal"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/limits/admission"
	"github.com/tyk-swe/olp/internal/management"
	"github.com/tyk-swe/olp/internal/mcp"
	"github.com/tyk-swe/olp/internal/mcp/accounting"
	"github.com/tyk-swe/olp/internal/mcp/registration"
	"github.com/tyk-swe/olp/internal/modelcatalog"
	"github.com/tyk-swe/olp/internal/notifications"
	"github.com/tyk-swe/olp/internal/observability"
	"github.com/tyk-swe/olp/internal/pluginindex"
	"github.com/tyk-swe/olp/internal/plugins"
	"github.com/tyk-swe/olp/internal/projects"
	"github.com/tyk-swe/olp/internal/promotion/project"
	promotionserve "github.com/tyk-swe/olp/internal/promotion/serve"
	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/retrieval"
	"github.com/tyk-swe/olp/internal/routes"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/settings"
	"github.com/tyk-swe/olp/internal/signing"
	"github.com/tyk-swe/olp/internal/sinks"
	"github.com/tyk-swe/olp/internal/usage"
)

// Management is everything the management API is composed from. Processes
// and integration tests mount it the same way, so neither can drift from the
// routes the contract declares.
type Management struct {
	IdentityClient *http.Client
	Getenv         func(string) string
	Access         *access.Server
	Egress         *egress.Policy
	Limits         *sharedLimits
	Runtime        *release.Manager
	Gateway        *gateway.Server
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
	management.Register(mux)
	management.NewMCP(m.Access, mux).Register(mux)
	m.Access.Register(mux)
	projects.New(m.Access).Register(mux)
	notifications.NewManagement(m.Access, *m.Egress).Register(mux)
	settings.New(m.Access, m.Access.RuntimePublisher).Register(mux)
	(&release.DelegationManagement{Access: m.Access, Manager: m.Runtime}).Register(mux)
	(&gateway.CacheManagement{Access: m.Access, Gateway: m.Gateway}).Register(mux)
	(&branding.Server{Access: m.Access}).Register(mux)
	publishGuardrails := func(ctx context.Context, tx pgx.Tx, actor string) error {
		_, err := release.Publish(ctx, tx, actor)
		return err
	}
	credential.NewAPI(m.Access).Register(mux)
	definition.NewAPI(m.Access, publishGuardrails).Register(mux)
	policy.NewAPI(m.Access, publishGuardrails, func(r *http.Request, p access.Principal, pinned guardrails.Pinned, in guardrails.Application) (guardrails.Result, error) {
		return m.Gateway.EvaluateGuardrails(r, p, pinned, in)
	}).Register(mux)
	attachment.NewAPI(m.Access, publishGuardrails).Register(mux)
	agentTasks := tasks.New(m.Access.Pool, tasks.Limits{}, m.Access.Auth)
	agentGrants := personal.NewStore(m.Access.Pool, m.Access.Installation, m.Access.Auth, m.Access.Keys, m.Egress)
	agentPeers := a2a.NewPeers(m.Egress, func(ctx context.Context, id string) ([]byte, error) {
		return m.Access.Keys.Read(ctx, m.Access.Pool, m.Access.Installation, id, secrets.AgentCredential)
	})
	a2a.NewManagement(m.Access, agentTasks, agents.NewTaskOperations(agentTasks, agentPeers, agentGrants)).Register(mux)
	publications.New(m.Access, m.Egress, agentPeers.ValidateCard).Register(mux)
	runs.NewManagement(m.Access).Register(mux)
	tasks.NewManagement(m.Access).Register(mux)
	investigations.NewManagement(m.Access).Register(mux)
	registration.New(m.Access, m.Egress, registration.NewExecutables(m.Unconfined, m.Access.Pool)).Register(mux)
	accounting.NewManagement(m.Access).Register(mux)
	personal.New(m.Access).Register(mux)
	(&retrieval.Server{Access: m.Access}).Register(mux)
	(&sinks.Server{Access: m.Access, Egress: m.Egress}).Register(mux)
	(&modelcatalog.Server{Access: m.Access, Runtime: m.Runtime, Gateway: m.Gateway, Origin: m.Access.Origin}).Register(mux)
	catalogue := providers.New(m.Access, m.Egress, m.PluginHost, m.Getenv)
	catalogue.Unconfined = m.Unconfined
	catalogue.Log = m.Log
	catalogue.Plugins = m.PluginHost
	catalogue.Catalog = m.Catalog
	if m.Limits != nil {
		catalogue.Quotas = m.Limits.Fleet
	}
	catalogue.Register(mux)
	routeServer := routes.New(m.Access)
	routeServer.WorkflowTools = func() mcp.WorkflowResolver {
		if m.Gateway == nil {
			return nil
		}
		return m.Gateway.WorkflowResolver()
	}
	routeServer.WorkflowRelease = m.Runtime.Release
	routeServer.WorkflowAvailable = func() bool {
		return m.Gateway.WorkflowResolver() != nil
	}
	routeServer.Inputs = m.Runtime.RoutingInputs
	if m.Runtime != nil {
		routeServer.Region = m.Runtime.Region()
	}
	routeServer.UnconfinedPlugins = m.Unconfined != nil
	routeServer.Catalog = m.Catalog
	if m.Limits != nil {
		routeServer.Supply = gateway.NewAdmission(m.Limits.Limiter, m.Limits.Fleet, nil, m.Log)
		routeServer.Fleet = m.Limits.Fleet
	}
	catalogue.Activated = routeServer.Activated
	routeServer.Register(mux)
	(&gateway.Playground{Access: m.Access, Gateway: m.Gateway}).Register(mux)
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
	(&usage.Server{Access: m.Access, VendorKind: providers.VendorKind, Egress: m.Egress, Catalog: m.Catalog, CatalogKeys: m.CatalogKeys}).Register(mux)
	destination.NewAPI(m.Access, m.Egress).Register(mux)
	focus.NewAPI(m.Access).Register(mux)
	capture.NewAPI(m.Access).Register(mux)
	(&billing.Server{Access: m.Access, Egress: m.Egress}).Register(mux)
	promotionserve.Register(mux, promotionserve.Deps{
		Access:     m.Access,
		Egress:     m.Egress,
		Unconfined: m.Unconfined,
		Identity:   m.IdentityClient,
		Costs:      project.CostsOf(m.limiter()),
		VendorKind: providers.VendorKind,
		Catalogue:  catalogue,
	})
}

func (m Management) limiter() *admission.Limiter {
	if m.Limits == nil {
		return nil
	}
	return m.Limits.Limiter
}
