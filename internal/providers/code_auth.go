package providers

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"reflect"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/codemode"
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

func (a *CodeAuthorizer) AuthorizeCode(ctx context.Context, cfg runtime.Configuration, account codemode.Account) (codemode.Authorization, error) {
	if a == nil || a.Pool == nil || a.Credentials == nil || a.Plugins == nil || !codeConfiguration(cfg) {
		return codemode.Authorization{}, codemode.Refuse(503, "code_configuration_unsupported")
	}
	manifest, err := a.Plugins.Manifest(ctx, cfg.ProfileRevision)
	if err != nil || !reflect.DeepEqual(manifest.Profiles, codexauth.Manifest().Profiles) || !reflect.DeepEqual(manifest.Origins, codexauth.Manifest().Origins) {
		return codemode.Authorization{}, codemode.Refuse(503, "code_plugin_unavailable")
	}
	var eligible bool
	err = a.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.provider_credentials c
		JOIN olp.provider_grants g ON g.credential_id=c.id JOIN olp.providers p ON p.id=c.provider_id
		WHERE c.id=$1 AND c.provider_id=$2 AND p.project_id=$3 AND c.principal=$4 AND c.plugin_digest=$5
		AND c.revoked_at IS NULL AND g.lapsed_at IS NULL AND g.expires_at>now())`,
		account.CredentialID, account.ProviderID, account.ProjectID, account.Principal, cfg.ProfileRevision).Scan(&eligible)
	if err != nil || !eligible || !account.Enabled || account.Principal == "" || a.Credentials.Eligibility(account.CredentialID) != runtime.Eligible {
		return codemode.Authorization{}, codemode.Refuse(503, "code_credential_unavailable")
	}
	secret, generation, err := a.Credentials.Secret(ctx, nil, account.CredentialID)
	if err != nil || generation < 1 {
		return codemode.Authorization{}, codemode.Refuse(503, "code_credential_unavailable")
	}
	auth, err := authorizeCodex(secret, account.Principal, time.Now())
	if err != nil {
		return codemode.Authorization{}, err
	}
	auth.CredentialID = account.CredentialID
	auth.GrantGeneration = generation
	return auth, nil
}

func codeConfiguration(c runtime.Configuration) bool {
	o := c.Options
	return c.Kind == connectors.KindPlugin && c.AuthMode == connectors.AuthGrant && c.ProfileID == codexauth.ProfileID && c.ProfileRevision != "" &&
		(c.Endpoint == "" || c.Endpoint == codexauth.Upstream) && c.CloudRegion == "" && c.CloudProject == "" && c.Deployment == "" && c.APIVersion == "" &&
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
