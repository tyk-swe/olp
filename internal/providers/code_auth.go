package providers

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"reflect"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codeplans"
	"github.com/tyk-swe/olp/internal/codexauth"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/plugins"
	"github.com/tyk-swe/olp/internal/runtime"
)

type CodeAuthorizer struct {
	Pool        access.Queryer
	Credentials runtime.Credentials
	Plugins     *plugins.Host
}

// AuthorizeCode returns the upstream authorization of one request on account,
// whose provider connection is cfg. The connection's plugin must be the
// adapter's own, unaltered, and the request's protocol one its routes serve.
func (a *CodeAuthorizer) AuthorizeCode(ctx context.Context, cfg runtime.Configuration, account codemode.Account, dispatch codemode.Dispatch) (codemode.Authorization, error) {
	vendor, ok := codeadapter.ForConnection(cfg.Kind, cfg.AuthMode, cfg.ProfileID)
	if a == nil || a.Pool == nil || a.Credentials == nil || a.Plugins == nil || !ok || vendor.Adapter != dispatch.Adapter || !vendor.Serves(dispatch.Protocol) || !codeConfiguration(cfg, vendor) {
		return codemode.Authorization{}, codemode.Refuse(503, "code_configuration_unsupported")
	}
	manifest, err := a.Plugins.Manifest(ctx, cfg.ProfileRevision)
	if err != nil || !reflect.DeepEqual(manifest.Profiles, vendor.Manifest().Profiles) || !reflect.DeepEqual(manifest.Origins, vendor.Manifest().Origins) {
		return codemode.Authorization{}, codemode.Refuse(503, "code_plugin_unavailable")
	}
	// A Codex grant is a refreshed access token that must be current; a
	// pasted key never expires and has nothing to refresh.
	var eligible bool
	err = a.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.provider_credentials c
		JOIN olp.provider_grants g ON g.credential_id=c.id JOIN olp.providers p ON p.id=c.provider_id
		WHERE c.id=$1 AND c.provider_id=$2 AND p.project_id=$3 AND c.principal=$4 AND c.plugin_digest=$5 AND c.profile_id=$7
		AND c.revoked_at IS NULL AND g.lapsed_at IS NULL
		AND CASE WHEN $6 THEN g.expires_at IS NULL AND g.refresh_token_id IS NULL ELSE g.expires_at>now() END)`,
		account.CredentialID, account.ProviderID, account.ProjectID, account.Principal, cfg.ProfileRevision, vendor.KeyGrant, cfg.ProfileID).Scan(&eligible)
	if err != nil || !eligible || !account.Enabled || account.Principal == "" || a.Credentials.Eligibility(account.CredentialID) != runtime.Eligible {
		return codemode.Authorization{}, codemode.Refuse(503, "code_credential_unavailable")
	}
	secret, generation, err := a.Credentials.Secret(ctx, nil, account.CredentialID)
	if err != nil || generation < 1 {
		return codemode.Authorization{}, codemode.Refuse(503, "code_credential_unavailable")
	}
	var auth codemode.Authorization
	if vendor.KeyGrant {
		auth, err = authorizeKey(secret, cfg.ProfileID, account.Principal, codeadapter.CredentialHeader(dispatch.Adapter, dispatch.Protocol))
	} else {
		auth, err = authorizeCodex(secret, account.Principal, time.Now())
	}
	if err != nil {
		return codemode.Authorization{}, err
	}
	auth.CredentialID = account.CredentialID
	auth.GrantGeneration = generation
	return auth, nil
}

// codeConfiguration admits only the adapter's own plugin profile at its own
// upstream, with nothing that would change or add to the forwarded request.
func codeConfiguration(c runtime.Configuration, vendor codeadapter.Vendor) bool {
	o := c.Options
	address, ok := vendor.Address(c.ProfileID)
	return ok && c.Kind == connectors.KindPlugin && c.AuthMode == connectors.AuthGrant && c.ProfileRevision != "" &&
		(c.Endpoint == "" || c.Endpoint == address) && c.CloudRegion == "" && c.CloudProject == "" && c.Deployment == "" && c.APIVersion == "" &&
		len(o.SemanticHeaders) == 0 && len(o.QuerySettings) == 0 && len(o.OperationDefaults) == 0 && len(o.Bindings) == 0 &&
		len(o.Models) == 0 && len(o.ParameterDefaults) == 0 && len(o.CredentialHeaders) == 0 && len(o.PluginOptions) == 0
}

func authorizeCodex(secret []byte, principal string, now time.Time) (codemode.Authorization, error) {
	var credential connectors.GrantCredential
	if len(secret) > 64<<10 || json.Unmarshal(secret, &credential) != nil {
		return codemode.Authorization{}, codemode.Refuse(503, "code_credential_unavailable")
	}
	identity, err := codexauth.TokenIdentity(credential.AccessToken, now)
	if err != nil || identity.Principal() != principal || !maps.Equal(identity.Facts(), credential.Facts) {
		return codemode.Authorization{}, codemode.Refuse(503, "code_principal_mismatch")
	}
	return codemode.Authorization{Principal: identity.Principal(), Headers: http.Header{
		"Authorization":      {"Bearer " + credential.AccessToken},
		"Chatgpt-Account-Id": {identity.Account},
	}}, nil
}

// authorizeKey places a pasted coding-plan key in header, after confirming it
// is still the key whose fingerprint the account's principal is.
func authorizeKey(secret []byte, profileID, principal, header string) (codemode.Authorization, error) {
	var credential connectors.GrantCredential
	if len(secret) > 64<<10 || json.Unmarshal(secret, &credential) != nil || len(credential.Facts) != 0 || !codeplans.ValidKey(credential.AccessToken) {
		return codemode.Authorization{}, codemode.Refuse(503, "code_credential_unavailable")
	}
	if codeplans.Principal(profileID, credential.AccessToken) != principal {
		return codemode.Authorization{}, codemode.Refuse(503, "code_principal_mismatch")
	}
	value := credential.AccessToken
	if header == "Authorization" {
		value = "Bearer " + value
	}
	return codemode.Authorization{Principal: principal, Headers: http.Header{http.CanonicalHeaderKey(header): {value}}}, nil
}
