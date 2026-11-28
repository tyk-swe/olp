package main

import (
	"context"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/consumerauth"
	"github.com/tyk-swe/olp/internal/identity"
	"github.com/tyk-swe/olp/internal/observability"
	"github.com/tyk-swe/olp/internal/platform/secrets"
	"github.com/tyk-swe/olp/internal/plugins"
	"github.com/tyk-swe/olp/internal/release"
)

// Management is everything the management API is composed from, so processes
// cannot drift from the routes the contract declares.
type Management struct {
	foundation
	serving        serving
	documents      signedDocuments
	Access         *access.Server
	Identity       *identity.Server
	IdentityClient *http.Client
	PluginRuntime  *plugins.Runtime
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

func (m Management) fleetHealth() observability.FleetHealth {
	if m.stores.shared == nil {
		return nil
	}
	return m.stores.shared.Fleet
}

func newManagement(startup context.Context, base foundation, served serving, documents signedDocuments) (Management, error) {
	c, stores, core, log := base.config, base.stores, base.core, base.log
	if err := access.PrepareInstallation(startup, stores.pool, stores.installation, core.auth, core.keys); err != nil {
		return Management{}, err
	}
	identityClient := access.IdentityHTTPClient()
	control := access.New(access.Config{
		Pool: stores.pool, Installation: stores.installation, Origin: c.PublicOrigin, Auth: core.auth, Keys: core.keys, Egress: core.policy, Log: log,
		OIDCClient: identityClient,
		RuntimePublisher: func(ctx context.Context, tx pgx.Tx, actor string) error {
			_, err := release.Publish(ctx, tx, actor)
			return err
		},
		ClientIP:           func(r *http.Request) string { return consumerauth.ClientIP(r, c.TrustedProxyCIDRs) },
		LocalLoginDisabled: !c.LocalLoginEnabled, LimitsEnforced: stores.limiter != nil,
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
		foundation: base, serving: served, documents: documents,
		Access: control, Identity: signIn, IdentityClient: identityClient, PluginRuntime: pluginRuntime,
	}, nil
}
