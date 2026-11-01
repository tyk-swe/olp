package codeadapter

import (
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/antigravityauth"
	"github.com/tyk-swe/olp/internal/claudeauth"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codeplans"
	"github.com/tyk-swe/olp/internal/codexauth"
	"github.com/tyk-swe/olp/internal/connectors"
)

// Authorize constructs credentials after the provider has checked the live
// grant, verified the plugin, and resolved its secret.
func (v Vendor) Authorize(secret []byte, profileID, principal string, dispatch codemode.Dispatch, now time.Time) (codemode.Authorization, error) {
	endpoint, ok := v.Endpoint(dispatch.ClientPath)
	if !ok || dispatch.Adapter != v.Adapter || endpoint.Protocol != dispatch.Protocol || v.credential == nil || !slices.Contains(v.profiles, profileID) {
		return codemode.Authorization{}, codemode.Refuse(503, "code_configuration_unsupported")
	}
	return v.credential(secret, profileID, principal, endpoint, now)
}

// ValidateAuthorization applies the same boundary to HTTP and WebSocket
// authorization, including case-insensitive duplicate header names.
func (v Vendor) ValidateAuthorization(endpoint Endpoint, auth codemode.Authorization, principal string) error {
	if auth.Principal == "" || auth.Principal != principal {
		return codemode.Refuse(409, "code_principal_mismatch")
	}
	seen := map[string]bool{}
	credential := false
	for name, values := range auth.Headers {
		name = http.CanonicalHeaderKey(name)
		if seen[name] || name != endpoint.CredentialHeader && !slices.ContainsFunc(v.extra, func(extra string) bool { return strings.EqualFold(name, extra) }) || len(values) != 1 {
			return codemode.Refuse(503, "code_authorization_invalid")
		}
		seen[name] = true
		for i := range len(values[0]) {
			c := values[0][i]
			if c < 32 && c != '\t' || c == 127 {
				return codemode.Refuse(503, "code_authorization_invalid")
			}
		}
		if name == endpoint.CredentialHeader {
			credential = strings.TrimSpace(values[0]) != ""
		}
	}
	if !credential {
		return codemode.Refuse(503, "code_authorization_invalid")
	}
	return nil
}

func codexCredential(secret []byte, _ string, principal string, _ Endpoint, now time.Time) (codemode.Authorization, error) {
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

// keyCredential places a pasted coding-plan key in header, after confirming it
// is still the key whose fingerprint the account's principal is.
func keyCredential(secret []byte, profileID, principal string, endpoint Endpoint, _ time.Time) (codemode.Authorization, error) {
	var credential connectors.GrantCredential
	if len(secret) > 64<<10 || json.Unmarshal(secret, &credential) != nil || len(credential.Facts) != 0 || !codeplans.ValidKey(credential.AccessToken) {
		return codemode.Authorization{}, codemode.Refuse(503, "code_credential_unavailable")
	}
	if codeplans.Principal(profileID, credential.AccessToken) != principal {
		return codemode.Authorization{}, codemode.Refuse(503, "code_principal_mismatch")
	}
	header := endpoint.CredentialHeader
	value := credential.AccessToken
	if header == "Authorization" {
		value = "Bearer " + value
	}
	return codemode.Authorization{Principal: principal, Headers: http.Header{http.CanonicalHeaderKey(header): {value}}}, nil
}

func claudeCredential(secret []byte, _ string, principal string, _ Endpoint, _ time.Time) (codemode.Authorization, error) {
	var credential connectors.GrantCredential
	if len(secret) > 64<<10 || json.Unmarshal(secret, &credential) != nil || !claudeauth.ValidAccessToken(credential.AccessToken) {
		return codemode.Authorization{}, codemode.Refuse(503, "code_credential_unavailable")
	}
	verified := claudeauth.Principal(credential.Facts)
	if verified == "" || verified != principal {
		return codemode.Authorization{}, codemode.Refuse(503, "code_principal_mismatch")
	}
	return codemode.Authorization{Principal: verified, Headers: http.Header{"Authorization": {"Bearer " + credential.AccessToken}, "Anthropic-Beta": {claudeauth.OAuthBeta}}}, nil
}

func antigravityCredential(secret []byte, _ string, principal string, _ Endpoint, _ time.Time) (codemode.Authorization, error) {
	var credential connectors.GrantCredential
	if len(secret) > 64<<10 || json.Unmarshal(secret, &credential) != nil || credential.AccessToken == "" || strings.ContainsAny(credential.AccessToken, "\r\n\x00") {
		return codemode.Authorization{}, codemode.Refuse(503, "code_credential_unavailable")
	}
	observed := antigravityauth.Principal(credential.Facts["subject"], credential.Facts["project_id"])
	if observed == "" || observed != principal || len(credential.Facts) != 2 {
		return codemode.Authorization{}, codemode.Refuse(503, "code_principal_mismatch")
	}
	return codemode.Authorization{Principal: observed, ProjectID: credential.Facts["project_id"], Headers: http.Header{"Authorization": {"Bearer " + credential.AccessToken}}}, nil
}
