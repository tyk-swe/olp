//go:build integration && oidctest

package integration_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/secrets"
)

type issuerCode struct {
	ClientID, Nonce, Challenge string
	Claims                     map[string]any
}
type testIssuer struct {
	Server *httptest.Server
	Key    ed25519.PrivateKey
	mu     sync.Mutex
	Codes  map[string]issuerCode
}

func newTestIssuer(t *testing.T, authMethods ...string) *testIssuer {
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	issuer := &testIssuer{Key: key, Codes: map[string]issuerCode{}}
	issuer.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			metadata := map[string]any{"issuer": issuer.Server.URL, "authorization_endpoint": issuer.Server.URL + "/authorize", "token_endpoint": issuer.Server.URL + "/token", "jwks_uri": issuer.Server.URL + "/jwks", "id_token_signing_alg_values_supported": []string{"EdDSA"}}
			if authMethods != nil {
				metadata["token_endpoint_auth_methods_supported"] = authMethods
			}
			json.NewEncoder(w).Encode(metadata)
		case "/jwks":
			json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{"kty": "OKP", "crv": "Ed25519", "x": base64.RawURLEncoding.EncodeToString(public), "kid": "test-key", "use": "sig", "alg": "EdDSA"}}})
		case "/token":
			r.ParseForm()
			client, password, ok := r.BasicAuth()
			if authMethods != nil && !slices.Contains(authMethods, "client_secret_basic") {
				client, password = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
				ok = slices.Contains(authMethods, "client_secret_post") && r.Header.Get("Authorization") == ""
			} else {
				ok = ok && !r.PostForm.Has("client_secret")
			}
			issuer.mu.Lock()
			code, exists := issuer.Codes[r.Form.Get("code")]
			delete(issuer.Codes, r.Form.Get("code"))
			issuer.mu.Unlock()
			challenge := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if !ok || client != code.ClientID || password != "write-only-client-secret" || !exists || base64.RawURLEncoding.EncodeToString(challenge[:]) != code.Challenge {
				w.WriteHeader(400)
				json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
				return
			}
			claims := map[string]any{"iss": issuer.Server.URL, "aud": client, "sub": "subject", "email": "oidc@example.com", "email_verified": true, "name": "OIDC Member", "groups": []string{"developers"}, "nonce": code.Nonce, "iat": time.Now().Unix(), "auth_time": time.Now().Unix(), "exp": time.Now().Add(time.Minute).Unix()}
			for k, v := range code.Claims {
				claims[k] = v
			}
			header, _ := json.Marshal(map[string]string{"alg": "EdDSA", "kid": "test-key"})
			payload, _ := json.Marshal(claims)
			signed := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
			token := signed + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, []byte(signed)))
			json.NewEncoder(w).Encode(map[string]any{"access_token": "test-access-token", "token_type": "Bearer", "id_token": token})
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(issuer.Server.Close)
	return issuer
}

func (i *testIssuer) callback(t *testing.T, authorization string, claims map[string]any) string {
	t.Helper()
	u, err := url.Parse(authorization)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("nonce") == "" {
		t.Fatal("missing nonce or PKCE")
	}
	code := secrets.Token()
	i.mu.Lock()
	i.Codes[code] = issuerCode{ClientID: q.Get("client_id"), Nonce: q.Get("nonce"), Challenge: q.Get("code_challenge"), Claims: claims}
	i.mu.Unlock()
	return "/api/v1/oidc/callback?" + url.Values{"code": {code}, "state": {q.Get("state")}}.Encode()
}

func TestOIDCVerifierFlowBindingLinkingAndEnrollment(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	issuer := newTestIssuer(t)
	config := map[string]any{"issuer": issuer.Server.URL, "discovery_url": issuer.Server.URL + "/.well-known/openid-configuration", "client_id": "test-client", "client_secret": "write-only-client-secret", "enabled": true, "default_role": "viewer", "group_role_mappings": []any{map[string]string{"claim_value": "developers", "role": "developer"}}}
	saved := h.want(owner, "PUT", "/api/v1/oidc/configuration", config, nil, 200)
	if saved["has_client_secret"] != true || saved["client_secret"] != nil {
		t.Fatal("OIDC secret contract")
	}
	h.want(nil, "POST", "/api/v1/oidc/login", map[string]any{"return_to": "//evil.test"}, nil, 422)
	begin := func(b *browser, path string, input any) string {
		return h.want(b, "POST", path, input, nil, 200)["authorization_url"].(string)
	}
	expired := &browser{}
	expiredAuthorization := begin(expired, "/api/v1/oidc/login", map[string]any{})
	expiredURL, _ := url.Parse(expiredAuthorization)
	if _, err := h.Pool.Exec(t.Context(), "UPDATE olp.oidc_flows SET expires_at=now()-interval '1 second' WHERE state_digest=$1", h.Server.Auth.Digest(secrets.OIDCStateDigest, expiredURL.Query().Get("state"))); err != nil {
		t.Fatal(err)
	}
	h.want(expired, "GET", issuer.callback(t, expiredAuthorization, nil), nil, nil, 403)
	for field, value := range map[string]any{"iss": "https://wrong.example.com", "aud": "wrong-client", "nonce": "wrong-nonce", "email_verified": false, "exp": time.Now().Add(-time.Minute).Unix()} {
		b := &browser{}
		authorization := begin(b, "/api/v1/oidc/login", map[string]any{})
		callback := issuer.callback(t, authorization, map[string]any{field: value})
		status, _, _ := h.request(b, "GET", callback, nil, nil)
		if status != 403 {
			t.Fatalf("accepted invalid %s: %d", field, status)
		}
		h.want(b, "GET", callback, nil, nil, 403)
	}
	// The cookie belongs to the initiating browser; an attempted theft cannot consume it.
	member := &browser{}
	authorization := begin(member, "/api/v1/oidc/login", map[string]any{"return_to": "/settings/profile"})
	callback := issuer.callback(t, authorization, nil)
	h.want(&browser{}, "GET", callback, nil, nil, 403)
	status, _, headers := h.request(member, "GET", callback, nil, nil)
	if status != 303 || headers.Get("Location") != "/settings/profile" {
		t.Fatal("OIDC login redirect failed", status)
	}
	profile := h.want(member, "GET", "/api/v1/profile", nil, nil, 200)
	if profile["role"] != "developer" {
		t.Fatal("claim role mapping not applied")
	}
	h.want(member, "POST", "/api/v1/profile/password/enroll", map[string]any{"new_password": accessPassword}, etagHeader(profile), 428)
	authorization = begin(member, "/api/v1/oidc/reauthenticate", map[string]any{"purpose": "password_enrollment"})
	callback = issuer.callback(t, authorization, nil)
	status, _, headers = h.request(member, "GET", callback, nil, nil)
	if status != 303 || headers.Get("Location") != "/settings/profile?reauthenticated=password_enrollment" {
		t.Fatal("reauthentication redirect failed", status)
	}
	h.want(member, "POST", "/api/v1/profile/password/enroll", map[string]any{"new_password": accessPassword}, etagHeader(profile), 200)
	local := &browser{}
	h.want(local, "POST", "/api/v1/sessions", map[string]any{"email": "oidc@example.com", "password": accessPassword}, nil, 201)
	h.want(owner, "POST", "/api/v1/oidc/link", nil, nil, 428)
	otherOwnerSession := &browser{}
	h.want(otherOwnerSession, "POST", "/api/v1/sessions", map[string]any{"email": "owner@example.com", "password": accessPassword}, nil, 201)
	h.want(owner, "POST", "/api/v1/profile/reauthenticate", map[string]any{"current_password": accessPassword, "purpose": "oidc_link"}, nil, 204)
	if _, err := h.Pool.Exec(t.Context(), "UPDATE olp.recent_auth SET expires_at=now()-interval '1 second'"); err != nil {
		t.Fatal(err)
	}
	h.want(owner, "POST", "/api/v1/oidc/link", nil, nil, 428)
	h.want(owner, "POST", "/api/v1/profile/reauthenticate", map[string]any{"current_password": accessPassword, "purpose": "oidc_link"}, nil, 204)
	authorization = begin(owner, "/api/v1/oidc/link", nil)
	callback = issuer.callback(t, authorization, map[string]any{"sub": "owner-subject", "email": "owner@example.com"})
	h.want(owner, "GET", callback, nil, nil, 303)
	h.want(otherOwnerSession, "GET", "/api/v1/sessions/current", nil, nil, 401)
	identities := h.want(owner, "GET", "/api/v1/oidc/identities", nil, nil, 200)
	identity := identities["items"].([]any)[0].(map[string]any)["id"].(string)
	localSetting := h.want(owner, "GET", "/api/v1/settings/auth.local_login_enabled", nil, nil, 200)
	localSetting = h.want(owner, "PUT", "/api/v1/settings/auth.local_login_enabled", map[string]any{"value": "false"}, etagHeader(localSetting), 200)
	identities = h.want(owner, "GET", "/api/v1/oidc/identities", nil, nil, 200)
	if identities["items"].([]any)[0].(map[string]any)["can_unlink"] != false {
		t.Fatal("disabled local login cannot authorize removal of the sole usable identity")
	}
	h.want(owner, "PUT", "/api/v1/settings/auth.local_login_enabled", map[string]any{"value": "true"}, etagHeader(localSetting), 200)
	h.want(owner, "DELETE", "/api/v1/oidc/identities/"+identity, nil, nil, 428)
	h.want(owner, "POST", "/api/v1/profile/reauthenticate", map[string]any{"current_password": accessPassword, "purpose": "oidc_unlink", "resource_id": identity}, nil, 204)
	h.want(otherOwnerSession, "POST", "/api/v1/sessions", map[string]any{"email": "owner@example.com", "password": accessPassword}, nil, 201)
	// Unlinking is no sign-in: the rotated session keeps the method and MFA
	// state it had, so local MFA enforcement still reaches it.
	if _, err := h.Pool.Exec(t.Context(), "UPDATE olp.sessions SET auth_method='local',mfa_verified=false WHERE user_id=(SELECT id FROM olp.users WHERE email='owner@example.com')"); err != nil {
		t.Fatal(err)
	}
	h.want(owner, "DELETE", "/api/v1/oidc/identities/"+identity, nil, nil, 204)
	h.want(otherOwnerSession, "GET", "/api/v1/sessions/current", nil, nil, 401)
	var method string
	var verified bool
	if err := h.Pool.QueryRow(t.Context(), "SELECT auth_method,mfa_verified FROM olp.sessions WHERE user_id=(SELECT id FROM olp.users WHERE email='owner@example.com')").Scan(&method, &verified); err != nil || method != "local" || verified {
		t.Fatalf("unlinked session method=%q mfa=%v err=%v, want local without MFA", method, verified, err)
	}
	// Configuration changes invalidate outstanding browser flows.
	pending := &browser{}
	authorization = begin(pending, "/api/v1/oidc/login", map[string]any{})
	callback = issuer.callback(t, authorization, nil)
	h.want(owner, "PUT", "/api/v1/oidc/configuration", config, etagHeader(saved), 200)
	h.want(pending, "GET", callback, nil, nil, 403)
}
