package access

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/secrets"
	"golang.org/x/oauth2"
)

type roleMapping struct {
	ClaimValue string `json:"claim_value"`
	Role       string `json:"role"`
}

// Persist only the verified inputs needed to assess future sign-in eligibility.
// These facts are private to authorization and are excluded from API responses.
type oidcRoleClaims struct {
	ClientID    string   `json:"client_id"`
	Scopes      []string `json:"scopes"`
	EmailClaim  string   `json:"email_claim"`
	GroupsClaim string   `json:"groups_claim"`
	Email       string   `json:"email"`
	Groups      []string `json:"groups"`
}

type oidcConfiguration struct {
	ID              string        `json:"id"`
	DiscoveryURL    string        `json:"discovery_url"`
	Issuer          string        `json:"issuer"`
	ClientID        string        `json:"client_id"`
	Enabled         bool          `json:"enabled"`
	Scopes          []string      `json:"scopes"`
	EmailClaim      string        `json:"email_claim"`
	GroupsClaim     string        `json:"groups_claim"`
	DefaultRole     *string       `json:"default_role"`
	EmailMappings   []roleMapping `json:"email_role_mappings"`
	GroupMappings   []roleMapping `json:"group_role_mappings"`
	HasClientSecret bool          `json:"has_client_secret"`
	ETag            string        `json:"etag"`
	UpdatedByEmail  string        `json:"updated_by_email"`
}

func loadOIDC(r *http.Request, q Queryer) (oidcConfiguration, error) {
	var c oidcConfiguration
	var data []byte
	err := q.QueryRow(r.Context(), "SELECT c.document||jsonb_build_object('id',c.id,'etag',c.etag,'updated_by_email',u.email,'has_client_secret',EXISTS(SELECT 1 FROM olp.secrets s WHERE s.id=c.id AND s.purpose='oidc_client')) FROM olp.oidc_configuration c JOIN olp.users u ON u.id=c.updated_by WHERE singleton").Scan(&data)
	if err != nil {
		return c, err
	}
	err = json.Unmarshal(data, &c)
	return c, err
}
func (s *Server) oidcConfiguration(r *http.Request) (Reply, error) {
	if _, err := s.Principal(r, s.Pool, "access_read"); err != nil {
		return Reply{}, err
	}
	c, err := loadOIDC(r, s.Pool)
	return Detail(c, c.ETag), err
}
func (s *Server) discover(ctx context.Context, c oidcConfiguration) (*oidc.Provider, *oauth2.Config, error) {
	if err := oidcURL(c.DiscoveryURL); err != nil {
		return nil, nil, Invalid("discovery_url", err.Error())
	}
	request, err := http.NewRequestWithContext(ctx, "GET", c.DiscoveryURL, nil)
	if err != nil {
		return nil, nil, err
	}
	response, err := s.OIDCClient.Do(request)
	if err != nil {
		return nil, nil, Fail(422, "oidc_discovery_failed", "The OIDC discovery endpoint is unavailable or outside the identity egress policy.")
	}
	defer response.Body.Close()
	var metadata struct {
		oidc.ProviderConfig
		AuthMethods []string `json:"token_endpoint_auth_methods_supported"`
	}
	if response.StatusCode != 200 || json.NewDecoder(response.Body).Decode(&metadata) != nil {
		return nil, nil, Invalid("discovery_url", "Invalid OIDC discovery document.")
	}
	if metadata.IssuerURL != c.Issuer {
		return nil, nil, Invalid("issuer", "The issuer must exactly match discovery.")
	}
	for _, endpoint := range []string{metadata.IssuerURL, metadata.AuthURL, metadata.TokenURL, metadata.JWKSURL} {
		if oidcURL(endpoint) != nil {
			return nil, nil, Invalid("discovery_url", "The issuer advertises an unsafe endpoint.")
		}
		u, _ := url.Parse(endpoint) // Syntax was checked by oidcURL.
		if _, err := oidcAddresses(ctx, u.Hostname()); err != nil {
			return nil, nil, Invalid("discovery_url", "The issuer advertises an unsafe endpoint.")
		}
	}
	// Keep the verifier's refresh context independent of this request's
	// cancellation; every fetch still uses the bounded identity HTTP client.
	provider := metadata.NewProvider(oidc.ClientContext(context.Background(), s.OIDCClient))
	oauth := &oauth2.Config{ClientID: c.ClientID, Endpoint: provider.Endpoint(), RedirectURL: s.Origin + "/api/v1/oidc/callback", Scopes: c.Scopes}
	switch {
	case metadata.AuthMethods == nil || slices.Contains(metadata.AuthMethods, "client_secret_basic"):
		// OIDC discovery defaults to client_secret_basic when omitted.
		oauth.Endpoint.AuthStyle = oauth2.AuthStyleInHeader
	case slices.Contains(metadata.AuthMethods, "client_secret_post"):
		oauth.Endpoint.AuthStyle = oauth2.AuthStyleInParams
	default:
		return nil, nil, Invalid("discovery_url", "The token endpoint must support client_secret_basic or client_secret_post.")
	}
	return provider, oauth, nil
}
func (s *Server) putOIDCConfiguration(r *http.Request) (Reply, error) {
	var input struct {
		DiscoveryURL  string        `json:"discovery_url"`
		Issuer        string        `json:"issuer"`
		ClientID      string        `json:"client_id"`
		ClientSecret  *string       `json:"client_secret"`
		Enabled       *bool         `json:"enabled"`
		Scopes        []string      `json:"scopes"`
		EmailClaim    string        `json:"email_claim"`
		GroupsClaim   string        `json:"groups_claim"`
		DefaultRole   *string       `json:"default_role"`
		EmailMappings []roleMapping `json:"email_role_mappings"`
		GroupMappings []roleMapping `json:"group_role_mappings"`
	}
	if err := Decode(r, &input); err != nil {
		return Reply{}, err
	}
	if _, err := s.Principal(r, s.Pool, "access"); err != nil {
		return Reply{}, err
	}
	c := oidcConfiguration{DiscoveryURL: input.DiscoveryURL, Issuer: input.Issuer, ClientID: input.ClientID, Enabled: true, Scopes: input.Scopes, EmailClaim: input.EmailClaim, GroupsClaim: input.GroupsClaim, DefaultRole: input.DefaultRole, EmailMappings: input.EmailMappings, GroupMappings: input.GroupMappings}
	if input.Enabled != nil {
		c.Enabled = *input.Enabled
	}
	if len(c.Scopes) == 0 {
		c.Scopes = []string{"openid", "email", "profile"}
	}
	if !slices.Contains(c.Scopes, "openid") || len(c.Scopes) > 20 {
		return Reply{}, Invalid("scopes", "Include openid and at most 20 scopes.")
	}
	for _, scope := range c.Scopes {
		if ValidText("scopes", scope, 100) != nil || strings.ContainsAny(scope, " \t\r\n") {
			return Reply{}, Invalid("scopes", "Use individual valid scope names.")
		}
	}
	if c.EmailClaim == "" {
		c.EmailClaim = "email"
	}
	if c.GroupsClaim == "" {
		c.GroupsClaim = "groups"
	}
	if c.EmailMappings == nil {
		c.EmailMappings = []roleMapping{}
	}
	if c.GroupMappings == nil {
		c.GroupMappings = []roleMapping{}
	}
	for field, value := range map[string]string{"client_id": c.ClientID, "email_claim": c.EmailClaim, "groups_claim": c.GroupsClaim} {
		if err := ValidText(field, value, 255); err != nil {
			return Reply{}, err
		}
	}
	if c.DefaultRole != nil && !validRole(*c.DefaultRole) {
		return Reply{}, Invalid("default_role", "Use a valid role or leave automatic provisioning disabled.")
	}
	for field, mappings := range map[string][]roleMapping{"email_role_mappings": c.EmailMappings, "group_role_mappings": c.GroupMappings} {
		if len(mappings) > 500 {
			return Reply{}, Invalid("role_mappings", "Use at most 500 mappings.")
		}
		for i, m := range mappings {
			duplicate := slices.ContainsFunc(mappings[:i], func(previous roleMapping) bool {
				return previous.ClaimValue == m.ClaimValue ||
					(field == "email_role_mappings" && strings.EqualFold(previous.ClaimValue, m.ClaimValue))
			})
			if !validRole(m.Role) || ValidText("claim_value", m.ClaimValue, 254) != nil || duplicate {
				return Reply{}, Invalid("role_mappings", "Use unique claim values and valid roles.")
			}
		}
	}
	if input.ClientSecret != nil && len(*input.ClientSecret) > 4096 {
		return Reply{}, Invalid("client_secret", "Use at most 4096 bytes.")
	}
	if c.Enabled {
		if _, _, err := s.discover(r.Context(), c); err != nil {
			return Reply{}, err
		}
	}
	tx, err := s.Begin(r)
	if err != nil {
		return Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.Principal(r, tx, "access")
	if err != nil {
		return Reply{}, err
	}
	old, err := loadOIDC(r, tx)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Reply{}, err
	}
	if err == nil {
		if err = Match(r, old.ETag); err != nil {
			return Reply{}, err
		}
		c.ID = old.ID
		c.HasClientSecret = old.HasClientSecret
	} else {
		if r.Header.Get("If-Match") != "" {
			return Reply{}, Fail(412, "etag_mismatch", "The OIDC configuration does not exist.")
		}
		c.ID = NewID()
	}
	c.ETag = NewID()
	c.UpdatedByEmail = p.Email
	if input.ClientSecret != nil {
		var previous []byte
		if old.HasClientSecret {
			previous, err = s.Keys.Read(r.Context(), tx, s.Installation, c.ID, "oidc_client")
			if err != nil {
				return Reply{}, err
			}
		}
		if !hmac.Equal(previous, []byte(*input.ClientSecret)) {
			// Verified identities prove sign-in only with the old credential.
			// Clear that evidence in the same transaction so owner protection
			// requires an independent path until OIDC succeeds again.
			if _, err = tx.Exec(r.Context(), "UPDATE olp.oidc_identities SET role_claims=NULL WHERE role_claims IS NOT NULL"); err != nil {
				return Reply{}, err
			}
		}
		c.HasClientSecret = *input.ClientSecret != ""
		if c.HasClientSecret {
			if err = s.Keys.Store(r.Context(), tx, s.Installation, c.ID, "oidc_client", []byte(*input.ClientSecret), nil); err != nil {
				return Reply{}, err
			}
		} else {
			if _, err = tx.Exec(r.Context(), "DELETE FROM olp.secrets WHERE id=$1", c.ID); err != nil {
				return Reply{}, err
			}
		}
	}
	data, err := json.Marshal(c)
	if err != nil {
		return Reply{}, err
	}
	if _, err = tx.Exec(r.Context(), "INSERT INTO olp.oidc_configuration(singleton,id,document,etag,updated_by) VALUES(true,$1,$2,$3,$4) ON CONFLICT(singleton) DO UPDATE SET document=excluded.document,etag=excluded.etag,updated_by=excluded.updated_by", c.ID, data, c.ETag, p.UserID()); err != nil {
		return Reply{}, err
	}
	if err = s.usableOwner(r, tx); err != nil {
		return Reply{}, err
	}
	if _, err = tx.Exec(r.Context(), "DELETE FROM olp.secrets WHERE purpose='oidc_flow'"); err != nil {
		return Reply{}, err
	}
	if err = Audit(r.Context(), tx, r, p.ID, "oidc.configuration.update", "oidc_configuration", c.ID, "success"); err != nil {
		return Reply{}, err
	}
	return Commit(r, tx, Detail(c, c.ETag))
}

type oidcFlow struct {
	ID                                                                             string `json:"-"` // Authoritative row ID, used only to address the browser binding cookie.
	Kind, State, Nonce, Verifier, ReturnTo, UserID, SessionID, Purpose, ResourceID string
}

func (s *Server) beginOIDCLogin(r *http.Request) (Reply, error) { return s.beginOIDC(r, "login") }
func (s *Server) beginOIDCLink(r *http.Request) (Reply, error)  { return s.beginOIDC(r, "link") }
func (s *Server) beginOIDCReauthentication(r *http.Request) (Reply, error) {
	return s.beginOIDC(r, "reauthenticate")
}
func (s *Server) beginOIDC(r *http.Request, kind string) (Reply, error) {
	if err := s.admit(r, "oidc_begin", ""); err != nil {
		return Reply{}, err
	}
	var input struct {
		ReturnTo   string `json:"return_to"`
		Purpose    string `json:"purpose"`
		ResourceID string `json:"resource_id"`
	}
	if r.Method == "GET" {
		input.ReturnTo = r.URL.Query().Get("return_to")
	} else if r.ContentLength != 0 {
		if err := Decode(r, &input); err != nil {
			return Reply{}, err
		}
	}
	if !safeReturn(input.ReturnTo) {
		return Reply{}, Invalid("return_to", "Use a local console path.")
	}
	if input.ReturnTo == "" {
		input.ReturnTo = "/"
	}
	if kind == "reauthenticate" {
		if err := validatePurpose(input.Purpose, input.ResourceID); err != nil {
			return Reply{}, err
		}
	}
	c, err := loadOIDC(r, s.Pool)
	if err != nil {
		return Reply{}, err
	}
	if !c.Enabled {
		return Reply{}, Fail(403, "oidc_disabled", "OIDC sign-in is disabled.")
	}
	_, oauth, err := s.discover(r.Context(), c)
	if err != nil {
		return Reply{}, err
	}
	tx, err := s.Begin(r)
	if err != nil {
		return Reply{}, err
	}
	defer tx.Rollback(r.Context())
	current, err := loadOIDC(r, tx)
	if err != nil {
		return Reply{}, err
	}
	if current.ETag != c.ETag {
		return Reply{}, Fail(409, "oidc_configuration_changed", "The OIDC configuration changed. Try again.")
	}
	flow := oidcFlow{Kind: kind, State: secrets.Token(), Nonce: secrets.Token(), Verifier: oauth2.GenerateVerifier(), ReturnTo: input.ReturnTo, Purpose: input.Purpose, ResourceID: input.ResourceID}
	if flow.ReturnTo == "" {
		flow.ReturnTo = "/"
	}
	if kind != "login" {
		p, err := s.sessionPrincipal(r, tx, "read")
		if err != nil {
			return Reply{}, err
		}
		flow.UserID = p.ID
		flow.SessionID = p.SessionID
		if kind == "link" {
			if err = s.consumeRecent(r, tx, p, "oidc_link", ""); err != nil {
				return Reply{}, err
			}
		}
		if kind == "reauthenticate" {
			var linked bool
			if err = tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM olp.oidc_identities WHERE user_id=$1 AND issuer=$2)", p.ID, c.Issuer).Scan(&linked); err != nil {
				return Reply{}, err
			}
			if !linked {
				return Reply{}, Fail(403, "oidc_identity_required", "Link an identity before using OIDC reauthentication.")
			}
		}
		flow.ReturnTo = "/settings/profile"
	}
	id, cookieToken := NewID(), secrets.Token()
	expires := time.Now().Add(10 * time.Minute)
	data, err := json.Marshal(flow)
	if err != nil {
		return Reply{}, err
	}
	if _, err = tx.Exec(r.Context(), "DELETE FROM olp.secrets WHERE id IN(SELECT id FROM olp.secrets WHERE expires_at<=now() LIMIT 100)"); err != nil {
		return Reply{}, err
	}
	if err = s.Keys.Store(r.Context(), tx, s.Installation, id, "oidc_flow", data, &expires); err != nil {
		return Reply{}, err
	}
	if _, err = tx.Exec(r.Context(), "INSERT INTO olp.oidc_flows(id,state_digest,cookie_digest,configuration_etag,expires_at) VALUES($1,$2,$3,$4,$5)", id, s.Auth.Digest("oidc_state", flow.State), s.Auth.Digest("oidc_cookie", cookieToken), c.ETag, expires); err != nil {
		return Reply{}, err
	}
	options := []oauth2.AuthCodeOption{oidc.Nonce(flow.Nonce), oauth2.S256ChallengeOption(flow.Verifier)}
	if kind == "reauthenticate" {
		options = append(options, oauth2.SetAuthURLParam("prompt", "login"), oauth2.SetAuthURLParam("max_age", "0"))
	}
	authorizationURL := oauth.AuthCodeURL(flow.State, options...)
	response := Reply{Status: 200, Body: map[string]string{"authorization_url": authorizationURL}, Cookies: []*http.Cookie{cookie("__Host-olp_oidc_login_"+id, cookieToken, 10*time.Minute, true)}}
	if kind == "link" {
		response.Cookies = append(response.Cookies, clearCookie(recentCookie))
	}
	if r.Method == "GET" {
		response.Status = 303
		response.Body = nil
		response.Location = authorizationURL
	}
	return Commit(r, tx, response)
}

// The flow is consumed and committed before network exchange. Neither a
// repeated callback nor a failed token response can resurrect a login grant.
func (s *Server) consumeFlow(r *http.Request) (oidcFlow, oidcConfiguration, error) {
	var flow oidcFlow
	var config oidcConfiguration
	tx, err := s.Begin(r)
	if err != nil {
		return flow, config, err
	}
	defer tx.Rollback(r.Context())
	state := r.URL.Query().Get("state")
	var id, etag string
	var digest []byte
	err = tx.QueryRow(r.Context(), "SELECT id::text,configuration_etag::text,cookie_digest FROM olp.oidc_flows WHERE state_digest=$1 AND expires_at>now()", s.Auth.Digest("oidc_state", state)).Scan(&id, &etag, &digest)
	if errors.Is(err, pgx.ErrNoRows) {
		return flow, config, Fail(403, "oidc_flow_invalid", "This sign-in request expired or was already used.")
	}
	if err != nil {
		return flow, config, err
	}
	if !hmac.Equal(digest, s.Auth.Digest("oidc_cookie", cookieValue(r, "__Host-olp_oidc_login_"+id))) {
		return flow, config, Fail(403, "oidc_flow_invalid", "The sign-in request belongs to a different browser.")
	}
	data, err := s.Keys.Read(r.Context(), tx, s.Installation, id, "oidc_flow")
	if err != nil {
		return flow, config, err
	}
	if err = json.Unmarshal(data, &flow); err != nil {
		return flow, config, err
	}
	flow.ID = id
	config, err = loadOIDC(r, tx)
	if err != nil {
		return flow, config, err
	}
	if config.ETag != etag || !config.Enabled {
		return flow, config, Fail(403, "oidc_flow_invalid", "OIDC configuration changed during sign-in.")
	}
	if _, err = tx.Exec(r.Context(), "DELETE FROM olp.secrets WHERE id=$1", id); err != nil {
		return flow, config, err
	}
	return flow, config, tx.Commit(r.Context())
}
func (s *Server) oidcCallback(r *http.Request) (reply Reply, callbackErr error) {
	var flow oidcFlow
	defer func() {
		if callbackErr != nil && strings.Contains(r.Header.Get("Accept"), "text/html") && !strings.Contains(r.Header.Get("Accept"), "application/json") {
			reply = oidcFailureRedirect(flow, callbackErr)
			callbackErr = nil
		}
	}()
	if err := s.admit(r, "oidc_callback", ""); err != nil {
		return Reply{}, err
	}
	consumed, c, err := s.consumeFlow(r)
	flow = consumed
	if err != nil {
		return Reply{}, err
	}
	if r.URL.Query().Get("error") != "" || r.URL.Query().Get("code") == "" {
		return Reply{}, Fail(403, "oidc_authorization_denied", "The identity provider did not authorize sign-in.")
	}
	provider, oauth, err := s.discover(r.Context(), c)
	if err != nil {
		return Reply{}, err
	}
	if c.HasClientSecret {
		tx, err := s.Pool.Begin(r.Context())
		if err != nil {
			return Reply{}, err
		}
		defer tx.Rollback(r.Context())
		data, err := s.Keys.Read(r.Context(), tx, s.Installation, c.ID, "oidc_client")
		if err != nil {
			return Reply{}, err
		}
		oauth.ClientSecret = string(data)
		if err = tx.Commit(r.Context()); err != nil {
			return Reply{}, err
		}
	}
	ctx := oidc.ClientContext(r.Context(), s.OIDCClient)
	token, err := oauth.Exchange(ctx, r.URL.Query().Get("code"), oauth2.VerifierOption(flow.Verifier))
	if err != nil {
		return Reply{}, Fail(403, "oidc_token_invalid", "The identity provider token could not be verified.")
	}
	raw, ok := token.Extra("id_token").(string)
	if !ok || len(raw) > 65536 {
		return Reply{}, Fail(403, "oidc_token_invalid", "The identity provider did not supply a valid identity token.")
	}
	verified, err := provider.Verifier(&oidc.Config{ClientID: c.ClientID, SupportedSigningAlgs: []string{oidc.RS256, oidc.ES256, oidc.EdDSA}}).Verify(ctx, raw)
	if err != nil || !hmac.Equal([]byte(verified.Nonce), []byte(flow.Nonce)) {
		return Reply{}, Fail(403, "oidc_token_invalid", "The identity token signature, issuer, audience, expiry, or nonce is invalid.")
	}
	var claims map[string]json.RawMessage
	if verified.Claims(&claims) != nil {
		return Reply{}, Fail(403, "oidc_claims_invalid", "The identity claims are invalid.")
	}
	if len(verified.Subject) < 1 || len(verified.Subject) > 255 {
		return Reply{}, Fail(403, "oidc_claims_invalid", "A bounded identity subject is required.")
	}
	var address string
	var emailVerified bool
	json.Unmarshal(claims[c.EmailClaim], &address)
	json.Unmarshal(claims["email_verified"], &emailVerified)
	address, err = email(address)
	if err != nil || !emailVerified {
		return Reply{}, Fail(403, "oidc_email_unverified", "A verified email address is required.")
	}
	var groups []string
	if raw := claims[c.GroupsClaim]; raw != nil && json.Unmarshal(raw, &groups) != nil {
		return Reply{}, Fail(403, "oidc_claims_invalid", "The groups claim must be an array of strings.")
	}
	if len(groups) > 500 {
		return Reply{}, Fail(403, "oidc_claims_invalid", "The groups claim is too large.")
	}
	roleClaims, err := json.Marshal(oidcRoleClaims{ClientID: c.ClientID, Scopes: c.Scopes, EmailClaim: c.EmailClaim, GroupsClaim: c.GroupsClaim, Email: address, Groups: groups})
	if err != nil {
		return Reply{}, err
	}
	if flow.Kind == "reauthenticate" {
		var authTime int64
		json.Unmarshal(claims["auth_time"], &authTime)
		if authTime < time.Now().Add(-5*time.Minute).Unix() || authTime > time.Now().Add(time.Minute).Unix() {
			return Reply{}, Fail(403, "oidc_recent_auth_required", "The provider must confirm a recent authentication.")
		}
	}
	tx, err := s.Begin(r)
	if err != nil {
		return Reply{}, err
	}
	defer tx.Rollback(r.Context())
	current, err := loadOIDC(r, tx)
	if err != nil {
		return Reply{}, err
	}
	if current.ETag != c.ETag || !current.Enabled {
		return Reply{}, Fail(403, "oidc_flow_invalid", "The OIDC configuration changed during sign-in.")
	}
	var userID, identityID string
	err = tx.QueryRow(r.Context(), "SELECT user_id::text,id::text FROM olp.oidc_identities WHERE issuer=$1 AND subject=$2", c.Issuer, verified.Subject).Scan(&userID, &identityID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Reply{}, err
	}
	// Check the initiating session and exact identity before applying a
	// reauthentication result. Ordinary login is never a recent-auth proof.
	var p Principal
	if flow.Kind != "login" {
		p, err = s.sessionPrincipal(r, tx, "read")
		if err != nil {
			return Reply{}, err
		}
		if p.ID != flow.UserID || p.SessionID != flow.SessionID {
			return Reply{}, Fail(403, "oidc_session_changed", "The browser identity changed during authorization.")
		}
		if flow.Kind == "reauthenticate" && userID != p.ID {
			return Reply{}, Fail(403, "oidc_identity_mismatch", "Use an identity already linked to this account.")
		}
	}
	if identityID != "" {
		if _, err = tx.Exec(r.Context(), "UPDATE olp.oidc_identities SET role_claims=$2,last_login_at=now() WHERE id=$1", identityID, roleClaims); err != nil {
			return Reply{}, err
		}
	}
	if identityID != "" && flow.Kind != "link" {
		changed, allowed, err := syncOIDCAuthority(r, tx, userID, mappedRole(c, address, groups))
		if err != nil {
			return Reply{}, err
		}
		if !allowed || changed && flow.Kind == "reauthenticate" {
			// Verified external deauthorization is authoritative even for the
			// last owner. Commit it before returning an error; rollback must
			// not preserve old sessions, recent-auth grants or invitations.
			if err = tx.Commit(r.Context()); err != nil {
				return Reply{}, err
			}
			if !allowed {
				return Reply{}, Fail(403, "oidc_provisioning_denied", "No role mapping authorizes this identity.")
			}
			return Reply{}, Fail(403, "oidc_session_changed", "Your access changed. Sign in again before verifying your identity.")
		}
	}
	response := Reply{Status: 303, Location: flow.ReturnTo, Cookies: []*http.Cookie{clearCookie("__Host-olp_oidc_login_" + flow.ID)}}
	if flow.Kind != "login" {
		if flow.Kind == "link" {
			if userID != "" {
				return Reply{}, Fail(409, "oidc_identity_linked", "This identity is already linked.")
			}
			identityID = NewID()
			userID = p.ID
			if _, err = tx.Exec(r.Context(), "INSERT INTO olp.oidc_identities(id,user_id,issuer,subject,email_at_link,role_claims,last_login_at) VALUES($1,$2,$3,$4,$5,$6,now())", identityID, p.ID, c.Issuer, verified.Subject, address, roleClaims); err != nil {
				return Reply{}, err
			}
			rotated, err := s.changeSignInMethod(r, tx, p.ID)
			if err != nil {
				return Reply{}, err
			}
			response.Cookies = append(response.Cookies, rotated.Cookies...)
			response.CSRF = rotated.CSRF
		} else {
			if userID != p.ID {
				return Reply{}, Fail(403, "oidc_identity_mismatch", "Use an identity already linked to this account.")
			}
			grant, err := s.grantRecent(r, tx, p, flow.Purpose, flow.ResourceID)
			if err != nil {
				return Reply{}, err
			}
			response.Cookies = append(response.Cookies, grant.Cookies...)
			response.Location = "/settings/profile?reauthenticated=" + url.QueryEscape(flow.Purpose)
			if flow.ResourceID != "" {
				response.Location += "&resource_id=" + url.QueryEscape(flow.ResourceID)
			}
		}
	} else {
		if userID == "" {
			var existing bool
			if err = tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM olp.users WHERE email=$1)", address).Scan(&existing); err != nil {
				return Reply{}, err
			}
			if existing {
				return Reply{}, Fail(409, "oidc_link_required", "Sign in to your existing account and link this identity from your profile.")
			}
			role := mappedRole(c, address, groups)
			if role == "" {
				return Reply{}, Fail(403, "oidc_provisioning_denied", "No role mapping authorizes this identity.")
			}
			var complete bool
			if err = tx.QueryRow(r.Context(), "SELECT setup_complete FROM olp.installation WHERE singleton").Scan(&complete); err != nil {
				return Reply{}, err
			}
			if !complete {
				return Reply{}, Fail(403, "setup_required", "Complete owner setup before OIDC sign-in.")
			}
			userID = NewID()
			identityID = NewID()
			var name string
			json.Unmarshal(claims["name"], &name)
			if ValidText("display_name", name, 100) != nil {
				name = address
			}
			if _, err = tx.Exec(r.Context(), "INSERT INTO olp.users(id,email,display_name,role,etag,role_management) VALUES($1,$2,$3,$4,$5,'oidc')", userID, address, name, role, NewID()); err != nil {
				return Reply{}, err
			}
			if _, err = tx.Exec(r.Context(), "INSERT INTO olp.oidc_identities(id,user_id,issuer,subject,email_at_link,role_claims,last_login_at) VALUES($1,$2,$3,$4,$5,$6,now())", identityID, userID, c.Issuer, verified.Subject, address, roleClaims); err != nil {
				return Reply{}, err
			}
		}
		var active bool
		if err = tx.QueryRow(r.Context(), "SELECT active FROM olp.users WHERE id=$1", userID).Scan(&active); err != nil {
			return Reply{}, err
		}
		if !active {
			return Reply{}, Fail(403, "account_disabled", "This account is disabled.")
		}
		session, err := s.newSession(r, tx, userID)
		if err != nil {
			return Reply{}, err
		}
		response.Cookies = append(response.Cookies, session.Cookies...)
		response.CSRF = session.CSRF
	}
	if err = Audit(r.Context(), tx, r, userID, "oidc."+flow.Kind, "oidc_identity", identityID, "success"); err != nil {
		return Reply{}, err
	}
	return Commit(r, tx, response)
}
func mappedRole(c oidcConfiguration, address string, groups []string) string {
	for _, m := range c.EmailMappings {
		if strings.EqualFold(m.ClaimValue, address) {
			return m.Role
		}
	}
	selected := ""
	rank := map[string]int{"viewer": 1, "developer": 2, "operator": 3, "owner": 4}
	for _, m := range c.GroupMappings {
		if slices.Contains(groups, m.ClaimValue) && rank[m.Role] > rank[selected] {
			selected = m.Role
		}
	}
	if selected != "" {
		return selected
	}
	if c.DefaultRole != nil {
		return *c.DefaultRole
	}
	return ""
}

// Callers restrict identities to the enabled issuer.
func oidcSignInRole(c oidcConfiguration, locallyManaged bool, role string, data []byte) (string, error) {
	// Both supported token authentication methods require the client secret.
	if !c.HasClientSecret || data == nil {
		return "", nil
	}
	var claims oidcRoleClaims
	if err := json.Unmarshal(data, &claims); err != nil {
		return "", err
	}
	// Pairwise subjects can change between clients. Only a verified identity
	// for this client and these claim names proves a usable sign-in path.
	if claims.ClientID != c.ClientID || claims.EmailClaim != c.EmailClaim || claims.GroupsClaim != c.GroupsClaim {
		return "", nil
	}
	// Reducing requested scopes can remove verified email or role claims.
	// Evidence without scopes must be refreshed by a successful sign-in.
	if len(claims.Scopes) == 0 {
		return "", nil
	}
	for _, scope := range claims.Scopes {
		if !slices.Contains(c.Scopes, scope) {
			return "", nil
		}
	}
	if locallyManaged {
		return role, nil
	}
	return mappedRole(c, claims.Email, claims.Groups), nil
}

func usableOIDCIdentities(r *http.Request, q Queryer, p Principal, local bool) (map[string]bool, error) {
	c, err := loadOIDC(r, q)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !c.Enabled {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(r.Context(), "SELECT id::text,role_claims FROM olp.oidc_identities WHERE user_id=$1 AND issuer=$2", p.ID, c.Issuer)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	usable := map[string]bool{}
	for rows.Next() {
		var id string
		var data []byte
		if err := rows.Scan(&id, &data); err != nil {
			return nil, err
		}
		role, err := oidcSignInRole(c, local, p.Role, data)
		if err != nil {
			return nil, err
		}
		if role != "" {
			usable[id] = true
		}
	}
	return usable, rows.Err()
}

func (s *Server) oidcIdentities(r *http.Request) (Reply, error) {
	p, err := s.sessionPrincipal(r, s.Pool, "read")
	if err != nil {
		return Reply{}, err
	}
	var local, localEnabled, enabled, locallyManaged bool
	if err = s.Pool.QueryRow(r.Context(), "SELECT password_hash IS NOT NULL,COALESCE((SELECT value='true' FROM olp.settings WHERE key='auth.local_login_enabled'),true),COALESCE((SELECT (document->>'enabled')::boolean FROM olp.oidc_configuration WHERE singleton),false),role_management='local' FROM olp.users WHERE id=$1", p.ID).Scan(&local, &localEnabled, &enabled, &locallyManaged); err != nil {
		return Reply{}, err
	}
	usable, err := usableOIDCIdentities(r, s.Pool, p, locallyManaged)
	if err != nil {
		return Reply{}, err
	}
	rows, err := s.Pool.Query(r.Context(), `SELECT jsonb_build_object(
        'id',i.id,'issuer',i.issuer,'email_at_link',i.email_at_link,
        'created_at',i.created_at,'last_login_at',i.last_login_at)
        FROM olp.oidc_identities i WHERE i.user_id=$1 ORDER BY i.created_at,i.id LIMIT 100`, p.ID)
	if err != nil {
		return Reply{}, err
	}
	items, err := JSONRows(rows)
	if err != nil {
		return Reply{}, err
	}
	for _, item := range items {
		remaining := len(usable)
		if usable[item["id"].(string)] {
			remaining--
		}
		item["can_unlink"] = local && locallyManaged && localEnabled && !s.LocalLoginDisabled || remaining > 0
	}
	return OK(map[string]any{"items": items, "linking_available": enabled, "has_local_password": local, "oidc_reauthentication_available": len(usable) > 0}), nil
}
func (s *Server) unlinkOIDCIdentity(r *http.Request) (Reply, error) {
	id, err := IDParam(r, "identity_id")
	if err != nil {
		return Reply{}, err
	}
	tx, err := s.Begin(r)
	if err != nil {
		return Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.sessionPrincipal(r, tx, "read")
	if err != nil {
		return Reply{}, err
	}
	var owner string
	if err = tx.QueryRow(r.Context(), "SELECT user_id::text FROM olp.oidc_identities WHERE id=$1", id).Scan(&owner); err != nil {
		return Reply{}, err
	}
	if owner != p.ID {
		return Reply{}, Forbidden()
	}
	if err = s.consumeRecent(r, tx, p, "oidc_unlink", id); err != nil {
		return Reply{}, err
	}
	if _, err = tx.Exec(r.Context(), "DELETE FROM olp.oidc_identities WHERE id=$1", id); err != nil {
		return Reply{}, err
	}
	var local, localEnabled, locallyManaged bool
	if err = tx.QueryRow(r.Context(), `SELECT password_hash IS NOT NULL,COALESCE((SELECT value='true' FROM olp.settings WHERE key='auth.local_login_enabled'),true),role_management='local' FROM olp.users WHERE id=$1`, p.ID).Scan(&local, &localEnabled, &locallyManaged); err != nil {
		return Reply{}, err
	}
	if !local || !locallyManaged || !localEnabled || s.LocalLoginDisabled {
		usable, err := usableOIDCIdentities(r, tx, p, locallyManaged)
		if err != nil {
			return Reply{}, err
		}
		if len(usable) == 0 {
			return Reply{}, Fail(409, "last_sign_in_method", "Keep at least one usable sign-in method.")
		}
	}
	if err = s.usableOwner(r, tx); err != nil {
		return Reply{}, err
	}
	if err = Audit(r.Context(), tx, r, p.ID, "oidc.unlink", "oidc_identity", id, "success"); err != nil {
		return Reply{}, err
	}
	rotated, err := s.changeSignInMethod(r, tx, p.ID)
	if err != nil {
		return Reply{}, err
	}
	rotated.Status, rotated.Body = 204, nil
	return Commit(r, tx, rotated)
}

func (s *Server) changeSignInMethod(r *http.Request, tx pgx.Tx, userID string) (Reply, error) {
	if _, err := tx.Exec(r.Context(), "UPDATE olp.users SET etag=$2,updated_at=now() WHERE id=$1", userID, NewID()); err != nil {
		return Reply{}, err
	}
	if _, err := tx.Exec(r.Context(), "DELETE FROM olp.sessions WHERE user_id=$1", userID); err != nil {
		return Reply{}, err
	}
	if err := Audit(r.Context(), tx, r, userID, "user.authentication_method_change", "user", userID, "success"); err != nil {
		return Reply{}, err
	}
	return s.newSession(r, tx, userID)
}

// Only called after signature, claims, flow and configuration verification.
// Administrative active status remains independent of external authorization.
func syncOIDCAuthority(r *http.Request, tx pgx.Tx, userID, mapped string) (changed, allowed bool, err error) {
	var management, role string
	var authorized bool
	err = tx.QueryRow(r.Context(), "SELECT role_management,role,oidc_authorized FROM olp.users WHERE id=$1", userID).Scan(&management, &role, &authorized)
	if err != nil {
		return false, false, err
	}
	if management != "oidc" {
		return false, true, nil
	}
	allowed = mapped != ""
	if allowed == authorized && (!allowed || mapped == role) {
		return false, allowed, nil
	}
	if !allowed {
		mapped = role
	}
	if _, err = tx.Exec(r.Context(), "UPDATE olp.users SET role=$2,oidc_authorized=$3,etag=$4,updated_at=now() WHERE id=$1", userID, mapped, allowed, NewID()); err != nil {
		return false, false, err
	}
	if _, err = tx.Exec(r.Context(), "DELETE FROM olp.sessions WHERE user_id=$1", userID); err != nil {
		return false, false, err
	}
	if !allowed || mapped != "owner" {
		if err = retireIssuedInvitations(r, tx, userID, "", ""); err != nil {
			return false, false, err
		}
	}
	if _, err = AdvanceAuthority(r, tx); err != nil {
		return false, false, err
	}
	err = Audit(r.Context(), tx, r, "", "user.role_sync_oidc", "user", userID, "success")
	return true, allowed, err
}

func oidcFailureRedirect(flow oidcFlow, err error) Reply {
	reason := "provider"
	var problem *Problem
	if errors.As(err, &problem) {
		switch problem.Code {
		case "oidc_authorization_denied":
			reason = "cancelled"
		case "oidc_flow_invalid", "oidc_session_changed", "oidc_recent_auth_required":
			reason = "expired"
		case "oidc_provisioning_denied", "account_disabled":
			reason = "denied"
		case "oidc_identity_linked", "oidc_link_required", "oidc_identity_mismatch":
			reason = "link"
		}
	}
	destination := "/login"
	query := url.Values{"oidc_error": {reason}}
	if flow.Kind == "link" || flow.Kind == "reauthenticate" {
		destination = "/settings/profile"
	} else if flow.ReturnTo != "" && safeReturn(flow.ReturnTo) {
		query.Set("return_to", flow.ReturnTo)
	}
	reply := Reply{Status: 303, Location: destination + "?" + query.Encode()}
	if flow.ID != "" {
		reply.Cookies = []*http.Cookie{clearCookie("__Host-olp_oidc_login_" + flow.ID)}
	}
	return reply
}
