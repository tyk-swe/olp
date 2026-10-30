package providers

import (
	"context"
	"reflect"
	"time"

	"github.com/tyk-swe/olp/internal/routing"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codeplans"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/plugins"
	"github.com/tyk-swe/olp/internal/runtime"
)

type CodeAuthorizer struct {
	Pool        access.Queryer
	Credentials runtime.Credentials
	Plugins     *plugins.Host
}

// AuthorizeResearch reuses the enrolled grant's authorization boundary. Global
// Z.ai research uses the same bearer credential as its native chat endpoint.
func (a *CodeAuthorizer) AuthorizeResearch(ctx context.Context, cfg routing.Configuration, account codemode.Account, service string) (codemode.Authorization, error) {
	if cfg.ProfileID != codeplans.ZAIProfile || codemode.ResearchEndpoint(service) == "" {
		return codemode.Authorization{}, codemode.Refuse(503, "code_configuration_unsupported")
	}
	auth, err := a.AuthorizeCode(ctx, cfg, account, codemode.Dispatch{Adapter: codemode.AdapterZAICoding, Protocol: codemode.ProtocolChat, ClientPath: "v1/chat/completions"})
	if err != nil {
		return codemode.Authorization{}, err
	}
	// A cached grant may lag a credential update. Never let an old generation
	// keep a research session alive while the runtime catches up.
	var current bool
	if err = a.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.provider_grants WHERE credential_id=$1 AND generation=$2 AND lapsed_at IS NULL)`, account.CredentialID, auth.GrantGeneration).Scan(&current); err != nil || !current {
		return codemode.Authorization{}, codemode.Refuse(503, "code_credential_unavailable")
	}
	return auth, nil
}

// AuthorizeCode returns the upstream authorization of one request on account,
// whose provider connection is cfg. The connection's plugin must be the
// adapter's own, unaltered, and the request's protocol one its routes serve.
func (a *CodeAuthorizer) AuthorizeCode(ctx context.Context, cfg routing.Configuration, account codemode.Account, dispatch codemode.Dispatch) (codemode.Authorization, error) {
	vendor, ok := codeadapter.ForConnection(cfg.Kind, cfg.AuthMode, cfg.ProfileID)
	endpoint, served := vendor.Endpoint(dispatch.ClientPath)
	if a == nil || a.Pool == nil || a.Credentials == nil || !ok || vendor.Adapter != dispatch.Adapter || !served || endpoint.Protocol != dispatch.Protocol {
		return codemode.Authorization{}, codemode.Refuse(503, "code_configuration_unsupported")
	}
	if err := codeConnection(ctx, a.Plugins, cfg, vendor); err != nil {
		return codemode.Authorization{}, err
	}
	// A Codex grant is a refreshed access token that must be current; a
	// pasted key never expires and has nothing to refresh.
	var eligible bool
	err := a.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.provider_credentials c
		JOIN olp.provider_grants g ON g.credential_id=c.id JOIN olp.providers p ON p.id=c.provider_id
		WHERE c.id=$1 AND c.provider_id=$2 AND p.project_id=$3 AND c.principal=$4 AND c.plugin_digest=$5 AND c.profile_id=$7
		AND c.revoked_at IS NULL AND g.lapsed_at IS NULL
		AND CASE WHEN $6 THEN g.expires_at IS NULL AND g.refresh_token_id IS NULL ELSE g.expires_at>now() END)`,
		account.CredentialID, account.ProviderID, account.ProjectID, account.Principal, cfg.ProfileRevision, vendor.KeyGrant, cfg.ProfileID).Scan(&eligible)
	if err != nil || !eligible || !account.Enabled || account.Principal == "" || a.Credentials.Eligibility(account.CredentialID) != routing.Eligible {
		return codemode.Authorization{}, codemode.Refuse(503, "code_credential_unavailable")
	}
	secret, generation, err := a.Credentials.Secret(ctx, nil, account.CredentialID)
	if err != nil || generation < 1 {
		return codemode.Authorization{}, codemode.Refuse(503, "code_credential_unavailable")
	}
	auth, err := vendor.Authorize(secret, cfg.ProfileID, account.Principal, dispatch, time.Now())
	if err != nil {
		return codemode.Authorization{}, err
	}
	auth.CredentialID = account.CredentialID
	auth.GrantGeneration = generation
	return auth, nil
}

// codeConfiguration admits only the adapter's own plugin profile at its own
// upstream, with nothing that would change or add to the forwarded request.
func codeConfiguration(c routing.Configuration, vendor codeadapter.Vendor) bool {
	o := c.Options
	address, ok := vendor.Address(c.ProfileID)
	return ok && c.Kind == connectors.KindPlugin && c.AuthMode == connectors.AuthGrant && c.ProfileRevision != "" &&
		(c.Endpoint == "" || c.Endpoint == address) && c.CloudRegion == "" && c.CloudProject == "" && c.Deployment == "" && c.APIVersion == "" &&
		len(o.SemanticHeaders) == 0 && len(o.QuerySettings) == 0 && len(o.OperationDefaults) == 0 && len(o.Bindings) == 0 &&
		len(o.Models) == 0 && len(o.ParameterDefaults) == 0 && len(o.CredentialHeaders) == 0 && len(o.PluginOptions) == 0
}

func codeConnection(ctx context.Context, host *plugins.Host, cfg routing.Configuration, vendor codeadapter.Vendor) error {
	if host == nil || !codeConfiguration(cfg, vendor) {
		return codemode.Refuse(503, "code_configuration_unsupported")
	}
	manifest, err := host.Manifest(ctx, cfg.ProfileRevision)
	expected := vendor.Manifest()
	if err != nil || !reflect.DeepEqual(manifest.Profiles, expected.Profiles) || !reflect.DeepEqual(manifest.Origins, expected.Origins) {
		return codemode.Refuse(503, "code_plugin_unavailable")
	}
	return nil
}
