package auth

import (
	"context"
	"reflect"
	"slices"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/code"
	"github.com/tyk-swe/olp/internal/code/vendor"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/plugins"
	"github.com/tyk-swe/olp/internal/providerconfig"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/plugin/abi"
)

type Authorizer struct {
	Pool        access.Queryer
	Credentials release.Credentials
	Plugins     *plugins.Host
}

func (a *Authorizer) AuthorizeResearch(ctx context.Context, cfg routing.Configuration, account code.Account, service string) (code.Authorization, error) {
	v, ok := vendor.ForConnection(cfg.Kind, cfg.AuthMode, cfg.ProfileID)
	if !ok || !v.SupportsResearch(cfg.ProfileID) || code.ResearchEndpoint(service) == "" {
		return code.Authorization{}, code.Refuse(503, "code_configuration_unsupported")
	}
	auth, err := a.AuthorizeCode(ctx, cfg, account, code.Dispatch{Adapter: v.Adapter, Protocol: code.ProtocolChat, ClientPath: "v1/chat/completions"})
	if err != nil {
		return code.Authorization{}, err
	}
	var current bool
	if err = a.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.provider_grants WHERE credential_id=$1 AND generation=$2 AND lapsed_at IS NULL)`, account.CredentialID, auth.GrantGeneration).Scan(&current); err != nil || !current {
		return code.Authorization{}, code.Refuse(503, "code_credential_unavailable")
	}
	return auth, nil
}

func (a *Authorizer) AuthorizeCode(ctx context.Context, cfg routing.Configuration, account code.Account, dispatch code.Dispatch) (code.Authorization, error) {
	v, ok := vendor.ForConnection(cfg.Kind, cfg.AuthMode, cfg.ProfileID)
	endpoint, served := v.Endpoint(dispatch.ClientPath)
	if a == nil || a.Pool == nil || a.Credentials == nil || !ok || v.Adapter != dispatch.Adapter || !served || endpoint.Protocol != dispatch.Protocol {
		return code.Authorization{}, code.Refuse(503, "code_configuration_unsupported")
	}
	if err := ValidateConnection(ctx, a.Plugins, cfg, v); err != nil {
		return code.Authorization{}, err
	}
	var eligible bool
	err := a.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.provider_credentials c
		JOIN olp.provider_grants g ON g.credential_id=c.id JOIN olp.providers p ON p.id=c.provider_id
		WHERE c.id=$1 AND c.provider_id=$2 AND p.project_id=$3 AND c.principal=$4 AND c.plugin_digest=$5 AND c.profile_id=$7
		AND c.revoked_at IS NULL AND g.lapsed_at IS NULL
		AND CASE WHEN $6 THEN g.expires_at IS NULL AND g.refresh_token_id IS NULL ELSE g.expires_at>now() END)`,
		account.CredentialID, account.ProviderID, account.ProjectID, account.Principal, cfg.ProfileRevision, v.KeyGrant, cfg.ProfileID).Scan(&eligible)
	if err != nil || !eligible || !account.Enabled || account.Principal == "" || a.Credentials.Eligibility(account.CredentialID) != routing.Eligible {
		return code.Authorization{}, code.Refuse(503, "code_credential_unavailable")
	}
	secret, generation, err := a.Credentials.Secret(ctx, nil, account.CredentialID)
	if err != nil || generation < 1 {
		return code.Authorization{}, code.Refuse(503, "code_credential_unavailable")
	}
	auth, err := v.Authorize(secret, cfg.ProfileID, account.Principal, dispatch, time.Now())
	if err != nil {
		return code.Authorization{}, err
	}
	auth.CredentialID = account.CredentialID
	auth.GrantGeneration = generation
	return auth, nil
}

func ValidateConnection(ctx context.Context, host *plugins.Host, cfg routing.Configuration, v vendor.Vendor) error {
	return ValidateProviderConfig(ctx, host, cfg.Identity, providerconfig.Options{
		SemanticHeaders:   cfg.Options.SemanticHeaders,
		QuerySettings:     cfg.Options.QuerySettings,
		OperationDefaults: cfg.Options.OperationDefaults,
		Bindings:          cfg.Options.Bindings,
		Models:            cfg.Options.Models,
		ParameterDefaults: cfg.Options.ParameterDefaults,
		CredentialHeaders: cfg.Options.CredentialHeaders,
		PluginOptions:     cfg.Options.PluginOptions,
	}, v)
}

func ValidateProviderConfig[T string | *string](ctx context.Context, host *plugins.Host, id providerconfig.Identity[T], o providerconfig.Options, v vendor.Vendor) error {
	address, ok := v.Address(id.ProfileID)
	endpoint := providerconfig.Value(id.Endpoint)
	valid := ok && id.Kind == connectors.KindPlugin && id.AuthMode == connectors.AuthGrant && id.ProfileRevision != "" &&
		(endpoint == "" || endpoint == address) && providerconfig.Value(id.CloudRegion) == "" && providerconfig.Value(id.CloudProject) == "" &&
		providerconfig.Value(id.Deployment) == "" && providerconfig.Value(id.APIVersion) == "" &&
		len(o.SemanticHeaders) == 0 && len(o.QuerySettings) == 0 && len(o.OperationDefaults) == 0 && len(o.Bindings) == 0 &&
		len(o.Models) == 0 && len(o.ParameterDefaults) == 0 && len(o.CredentialHeaders) == 0
	if host == nil || !valid {
		return code.Refuse(503, "code_configuration_unsupported")
	}
	manifest, err := host.Manifest(ctx, id.ProfileRevision)
	expected := v.Manifest()
	if err != nil || !reflect.DeepEqual(manifest.Profiles, expected.Profiles) || !reflect.DeepEqual(manifest.Origins, expected.Origins) {
		return code.Refuse(503, "code_plugin_unavailable")
	}
	if len(o.PluginOptions) == 0 && !slices.ContainsFunc(expected.Profiles, func(profile abi.Profile) bool {
		return profile.ID == id.ProfileID && len(profile.Options) != 0
	}) {
		return nil
	}
	profile, profileErr := connectors.NewPluginProfile(id.ProfileRevision, expected, id.ProfileID)
	if profileErr != nil || profile.ValidateOptions(o.PluginOptions) != nil {
		return code.Refuse(503, "code_configuration_unsupported")
	}
	return nil
}
