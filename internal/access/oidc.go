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

func loadOIDC(r *http.Request, q queryer) (oidcConfiguration, error) {
	var c oidcConfiguration
	var data []byte
	err := q.QueryRow(r.Context(), "SELECT c.document||jsonb_build_object('id',c.id,'etag',c.etag,'updated_by_email',u.email,'has_client_secret',EXISTS(SELECT 1 FROM olp_go.secrets s WHERE s.id=c.id AND s.purpose='oidc_client')) FROM olp_go.oidc_configuration c JOIN olp_go.users u ON u.id=c.updated_by WHERE singleton").Scan(&data)
	if err != nil {
		return c, err
	}
	err = json.Unmarshal(data, &c)
	return c, err
}
func (s *Server) oidcConfiguration(r *http.Request) (reply, error) {
	if _, err := s.principal(r, s.Pool, "access_read"); err != nil {
		return reply{}, err
	}
	c, err := loadOIDC(r, s.Pool)
	return detail(c, c.ETag), err
}
func (s *Server) discover(ctx context.Context, c oidcConfiguration) (*oidc.Provider, *oauth2.Config, error) {
	if err := oidcURL(c.DiscoveryURL); err != nil {
		return nil, nil, invalid("discovery_url", err.Error())
	}
	request, err := http.NewRequestWithContext(ctx, "GET", c.DiscoveryURL, nil)
	if err != nil {
		return nil, nil, err
	}
	response, err := s.OIDCClient.Do(request)
	if err != nil {
		return nil, nil, fail(422, "oidc_discovery_failed", "The OIDC discovery endpoint is unavailable or outside the identity egress policy.")
	}
	defer response.Body.Close()
	var metadata struct {
		oidc.ProviderConfig
		AuthMethods []string `json:"token_endpoint_auth_methods_supported"`
	}
	if response.StatusCode != 200 || json.NewDecoder(response.Body).Decode(&metadata) != nil {
		return nil, nil, invalid("discovery_url", "Invalid OIDC discovery document.")
	}
	if metadata.IssuerURL != c.Issuer {
		return nil, nil, invalid("issuer", "The issuer must exactly match discovery.")
	}
	for _, endpoint := range []string{metadata.IssuerURL, metadata.AuthURL, metadata.TokenURL, metadata.JWKSURL} {
		if oidcURL(endpoint) != nil {
			return nil, nil, invalid("discovery_url", "The issuer advertises an unsafe endpoint.")
		}
		u, _ := url.Parse(endpoint) // Syntax was checked by oidcURL.
		if _, err := oidcAddresses(ctx, u.Hostname()); err != nil {
			return nil, nil, invalid("discovery_url", "The issuer advertises an unsafe endpoint.")
		}
	}
	// Keep the verifier's refresh context independent of this request's
	// cancellation; every fetch still uses the bounded identity HTTP client.
	provider := metadata.NewProvider(oidc.ClientContext(context.Background(), s.OIDCClient))
	oauth := &oauth2.Config{ClientID: c.ClientID, Endpoint: provider.Endpoint(), RedirectURL: s.Origin + "/api/v3/oidc/callback", Scopes: c.Scopes}
	switch {
	case metadata.AuthMethods == nil || slices.Contains(metadata.AuthMethods, "client_secret_basic"):
		// OIDC discovery defaults to client_secret_basic when omitted.
		oauth.Endpoint.AuthStyle = oauth2.AuthStyleInHeader
	case slices.Contains(metadata.AuthMethods, "client_secret_post"):
		oauth.Endpoint.AuthStyle = oauth2.AuthStyleInParams
	default:
		return nil, nil, invalid("discovery_url", "The token endpoint must support client_secret_basic or client_secret_post.")
	}
	return provider, oauth, nil
}
func (s *Server) putOIDCConfiguration(r *http.Request) (reply, error) {
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
	if err := decode(r, &input); err != nil {
		return reply{}, err
	}
	if _, err := s.principal(r, s.Pool, "access"); err != nil {
		return reply{}, err
	}
	c := oidcConfiguration{DiscoveryURL: input.DiscoveryURL, Issuer: input.Issuer, ClientID: input.ClientID, Enabled: true, Scopes: input.Scopes, EmailClaim: input.EmailClaim, GroupsClaim: input.GroupsClaim, DefaultRole: input.DefaultRole, EmailMappings: input.EmailMappings, GroupMappings: input.GroupMappings}
	if input.Enabled != nil {
		c.Enabled = *input.Enabled
	}
	if len(c.Scopes) == 0 {
		c.Scopes = []string{"openid", "email", "profile"}
	}
	if !slices.Contains(c.Scopes, "openid") || len(c.Scopes) > 20 {
		return reply{}, invalid("scopes", "Include openid and at most 20 scopes.")
	}
	for _, scope := range c.Scopes {
		if validText("scopes", scope, 100) != nil || strings.ContainsAny(scope, " \t\r\n") {
			return reply{}, invalid("scopes", "Use individual valid scope names.")
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
		if err := validText(field, value, 255); err != nil {
			return reply{}, err
		}
	}
	if c.DefaultRole != nil && !validRole(*c.DefaultRole) {
		return reply{}, invalid("default_role", "Use a valid role or leave automatic provisioning disabled.")
	}
	for field, mappings := range map[string][]roleMapping{"email_role_mappings": c.EmailMappings, "group_role_mappings": c.GroupMappings} {
		if len(mappings) > 500 {
			return reply{}, invalid("role_mappings", "Use at most 500 mappings.")
		}
		for i, m := range mappings {
			duplicate := slices.ContainsFunc(mappings[:i], func(previous roleMapping) bool {
				return previous.ClaimValue == m.ClaimValue ||
					(field == "email_role_mappings" && strings.EqualFold(previous.ClaimValue, m.ClaimValue))
			})
			if !validRole(m.Role) || validText("claim_value", m.ClaimValue, 254) != nil || duplicate {
				return reply{}, invalid("role_mappings", "Use unique claim values and valid roles.")
			}
		}
	}
	if input.ClientSecret != nil && len(*input.ClientSecret) > 4096 {
		return reply{}, invalid("client_secret", "Use at most 4096 bytes.")
	}
	if c.Enabled {
		if _, _, err := s.discover(r.Context(), c); err != nil {
			return reply{}, err
		}
	}
	tx, err := s.begin(r)
	if err != nil {
		return reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.principal(r, tx, "access")
	if err != nil {
		return reply{}, err
	}
	old, err := loadOIDC(r, tx)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return reply{}, err
	}
	if err == nil {
		if err = match(r, old.ETag); err != nil {
			return reply{}, err
		}
		c.ID = old.ID
		c.HasClientSecret = old.HasClientSecret
	} else {
		if r.Header.Get("If-Match") != "" {
			return reply{}, fail(412, "etag_mismatch", "The OIDC configuration does not exist.")
		}
		c.ID = newID()
	}
	c.ETag = newID()
	c.UpdatedByEmail = p.Email
	if input.ClientSecret != nil {
		var previous []byte
		if old.HasClientSecret {
			previous, err = s.Keys.Read(r.Context(), tx, s.Installation, c.ID, "oidc_client")
			if err != nil {
				return reply{}, err
			}
		}
		if !hmac.Equal(previous, []byte(*input.ClientSecret)) {
			// Verified identities prove sign-in only with the old credential.
			// Clear that evidence in the same transaction so owner protection
			// requires an independent path until OIDC succeeds again.
			if _, err = tx.Exec(r.Context(), "UPDATE olp_go.oidc_identities SET role_claims=NULL WHERE role_claims IS NOT NULL"); err != nil {
				return reply{}, err
			}
		}
		c.HasClientSecret = *input.ClientSecret != ""
		if c.HasClientSecret {
			if err = s.Keys.Store(r.Context(), tx, s.Installation, c.ID, "oidc_client", []byte(*input.ClientSecret), nil); err != nil {
				return reply{}, err
			}
		} else {
			if _, err = tx.Exec(r.Context(), "DELETE FROM olp_go.secrets WHERE id=$1", c.ID); err != nil {
				return reply{}, err
			}
		}
	}
	data, err := json.Marshal(c)
	if err != nil {
		return reply{}, err
	}
	if _, err = tx.Exec(r.Context(), "INSERT INTO olp_go.oidc_configuration(singleton,id,document,etag,updated_by) VALUES(true,$1,$2,$3,$4) ON CONFLICT(singleton) DO UPDATE SET document=excluded.document,etag=excluded.etag,updated_by=excluded.updated_by", c.ID, data, c.ETag, p.ID); err != nil {
		return reply{}, err
	}
	if err = usableOwner(r, tx); err != nil {
		return reply{}, err
	}
	if _, err = tx.Exec(r.Context(), "DELETE FROM olp_go.secrets WHERE purpose='oidc_flow'"); err != nil {
		return reply{}, err
	}
	if err = audit(r.Context(), tx, r, p.ID, "oidc.configuration.update", "oidc_configuration", c.ID, "success"); err != nil {
		return reply{}, err
	}
	return commit(r, tx, detail(c, c.ETag))
}

type oidcFlow struct{ Kind, State, Nonce, Verifier, ReturnTo, UserID, SessionID, Purpose, ResourceID string }

func (s *Server) beginOIDCLogin(r *http.Request) (reply, error) { return s.beginOIDC(r, "login") }
func (s *Server) beginOIDCLink(r *http.Request) (reply, error)  { return s.beginOIDC(r, "link") }
func (s *Server) beginOIDCReauthentication(r *http.Request) (reply, error) {
	return s.beginOIDC(r, "reauthenticate")
}
func (s *Server) beginOIDC(r *http.Request, kind string) (reply, error) {
	if err := s.admit(r, "oidc_begin", ""); err != nil {
		return reply{}, err
	}
	var input struct {
		ReturnTo   string `json:"return_to"`
		Purpose    string `json:"purpose"`
		ResourceID string `json:"resource_id"`
	}
	if r.Method == "GET" {
		input.ReturnTo = r.URL.Query().Get("return_to")
	} else if r.ContentLength != 0 {
		if err := decode(r, &input); err != nil {
			return reply{}, err
		}
	}
	if !safeReturn(input.ReturnTo) {
		return reply{}, invalid("return_to", "Use a local console path.")
	}
	if input.ReturnTo == "" {
		input.ReturnTo = "/"
	}
	if kind == "reauthenticate" {
		if err := validatePurpose(input.Purpose, input.ResourceID); err != nil {
			return reply{}, err
		}
	}
	c, err := loadOIDC(r, s.Pool)
	if err != nil {
		return reply{}, err
	}
	if !c.Enabled {
		return reply{}, fail(403, "oidc_disabled", "OIDC sign-in is disabled.")
	}
	_, oauth, err := s.discover(r.Context(), c)
	if err != nil {
		return reply{}, err
	}
	tx, err := s.begin(r)
	if err != nil {
		return reply{}, err
	}
	defer tx.Rollback(r.Context())
	current, err := loadOIDC(r, tx)
	if err != nil {
		return reply{}, err
	}
	if current.ETag != c.ETag {
		return reply{}, fail(409, "oidc_configuration_changed", "The OIDC configuration changed. Try again.")
	}
	flow := oidcFlow{Kind: kind, State: secrets.Token(), Nonce: secrets.Token(), Verifier: oauth2.GenerateVerifier(), ReturnTo: input.ReturnTo, Purpose: input.Purpose, ResourceID: input.ResourceID}
	if flow.ReturnTo == "" {
		flow.ReturnTo = "/"
	}
	if kind != "login" {
		p, err := s.principal(r, tx, "read")
		if err != nil {
			return reply{}, err
		}
		flow.UserID = p.ID
		flow.SessionID = p.SessionID
		if kind == "link" {
			if err = s.consumeRecent(r, tx, p, "oidc_link", ""); err != nil {
				return reply{}, err
			}
		}
		if kind == "reauthenticate" {
			var linked bool
			if err = tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM olp_go.oidc_identities WHERE user_id=$1 AND issuer=$2)", p.ID, c.Issuer).Scan(&linked); err != nil {
				return reply{}, err
			}
			if !linked {
				return reply{}, fail(403, "oidc_identity_required", "Link an identity before using OIDC reauthentication.")
			}
		}
		flow.ReturnTo = "/settings/profile"
	}
	id, cookieToken := newID(), secrets.Token()
	expires := time.Now().Add(10 * time.Minute)
	data, err := json.Marshal(flow)
	if err != nil {
		return reply{}, err
	}
	if _, err = tx.Exec(r.Context(), "DELETE FROM olp_go.secrets WHERE id IN(SELECT id FROM olp_go.secrets WHERE expires_at<=now() LIMIT 100)"); err != nil {
		return reply{}, err
	}
	if err = s.Keys.Store(r.Context(), tx, s.Installation, id, "oidc_flow", data, &expires); err != nil {
		return reply{}, err
	}
	if _, err = tx.Exec(r.Context(), "INSERT INTO olp_go.oidc_flows(id,state_digest,cookie_digest,configuration_etag,expires_at) VALUES($1,$2,$3,$4,$5)", id, s.Auth.Digest("oidc_state", flow.State), s.Auth.Digest("oidc_cookie", cookieToken), c.ETag, expires); err != nil {
		return reply{}, err
	}
	options := []oauth2.AuthCodeOption{oidc.Nonce(flow.Nonce), oauth2.S256ChallengeOption(flow.Verifier)}
	if kind == "reauthenticate" {
		options = append(options, oauth2.SetAuthURLParam("prompt", "login"), oauth2.SetAuthURLParam("max_age", "0"))
	}
	authorizationURL := oauth.AuthCodeURL(flow.State, options...)
	response := reply{Status: 200, Body: map[string]string{"authorization_url": authorizationURL}, Cookies: []*http.Cookie{cookie("__Host-olp_oidc_login_"+flow.State, cookieToken, 10*time.Minute, true)}}
	if kind == "link" {
		response.Cookies = append(response.Cookies, clearCookie(recentCookie))
	}
	if r.Method == "GET" {
		response.Status = 303
		response.Body = nil
		response.Location = authorizationURL
	}
	return commit(r, tx, response)
}

// The flow is consumed and committed before network exchange. Neither a
// repeated callback nor a failed token response can resurrect a login grant.
func (s *Server) consumeFlow(r *http.Request) (oidcFlow, oidcConfiguration, error) {
	var flow oidcFlow
	var config oidcConfiguration
	tx, err := s.begin(r)
	if err != nil {
		return flow, config, err
	}
	defer tx.Rollback(r.Context())
	state := r.URL.Query().Get("state")
	var id, etag string
	var digest []byte
	err = tx.QueryRow(r.Context(), "SELECT id::text,configuration_etag::text,cookie_digest FROM olp_go.oidc_flows WHERE state_digest=$1 AND expires_at>now()", s.Auth.Digest("oidc_state", state)).Scan(&id, &etag, &digest)
	if errors.Is(err, pgx.ErrNoRows) {
		return flow, config, fail(403, "oidc_flow_invalid", "This sign-in request expired or was already used.")
	}
	if err != nil {
		return flow, config, err
	}
	if !hmac.Equal(digest, s.Auth.Digest("oidc_cookie", cookieValue(r, "__Host-olp_oidc_login_"+state))) {
		return flow, config, fail(403, "oidc_flow_invalid", "The sign-in request belongs to a different browser.")
	}
	data, err := s.Keys.Read(r.Context(), tx, s.Installation, id, "oidc_flow")
	if err != nil {
		return flow, config, err
	}
	if err = json.Unmarshal(data, &flow); err != nil {
		return flow, config, err
	}
	config, err = loadOIDC(r, tx)
	if err != nil {
		return flow, config, err
	}
	if config.ETag != etag || !config.Enabled {
		return flow, config, fail(403, "oidc_flow_invalid", "OIDC configuration changed during sign-in.")
	}
	if _, err = tx.Exec(r.Context(), "DELETE FROM olp_go.secrets WHERE id=$1", id); err != nil {
		return flow, config, err
	}
	return flow, config, tx.Commit(r.Context())
}
func (s *Server) oidcCallback(r *http.Request) (reply, error) {
	if err := s.admit(r, "oidc_callback", ""); err != nil {
		return reply{}, err
	}
	flow, c, err := s.consumeFlow(r)
	if err != nil {
		return reply{}, err
	}
	if r.URL.Query().Get("error") != "" || r.URL.Query().Get("code") == "" {
		return reply{}, fail(403, "oidc_authorization_denied", "The identity provider did not authorize sign-in.")
	}
	provider, oauth, err := s.discover(r.Context(), c)
	if err != nil {
		return reply{}, err
	}
	if c.HasClientSecret {
		tx, err := s.Pool.Begin(r.Context())
		if err != nil {
			return reply{}, err
		}
		defer tx.Rollback(r.Context())
		data, err := s.Keys.Read(r.Context(), tx, s.Installation, c.ID, "oidc_client")
		if err != nil {
			return reply{}, err
		}
		oauth.ClientSecret = string(data)
		if err = tx.Commit(r.Context()); err != nil {
			return reply{}, err
		}
	}
	ctx := oidc.ClientContext(r.Context(), s.OIDCClient)
	token, err := oauth.Exchange(ctx, r.URL.Query().Get("code"), oauth2.VerifierOption(flow.Verifier))
	if err != nil {
		return reply{}, fail(403, "oidc_token_invalid", "The identity provider token could not be verified.")
	}
	raw, ok := token.Extra("id_token").(string)
	if !ok || len(raw) > 65536 {
		return reply{}, fail(403, "oidc_token_invalid", "The identity provider did not supply a valid identity token.")
	}
	verified, err := provider.Verifier(&oidc.Config{ClientID: c.ClientID, SupportedSigningAlgs: []string{oidc.RS256, oidc.ES256, oidc.EdDSA}}).Verify(ctx, raw)
	if err != nil || !hmac.Equal([]byte(verified.Nonce), []byte(flow.Nonce)) {
		return reply{}, fail(403, "oidc_token_invalid", "The identity token signature, issuer, audience, expiry, or nonce is invalid.")
	}
	var claims map[string]json.RawMessage
	if verified.Claims(&claims) != nil {
		return reply{}, fail(403, "oidc_claims_invalid", "The identity claims are invalid.")
	}
	if len(verified.Subject) < 1 || len(verified.Subject) > 255 {
		return reply{}, fail(403, "oidc_claims_invalid", "A bounded identity subject is required.")
	}
	var address string
	var emailVerified bool
	json.Unmarshal(claims[c.EmailClaim], &address)
	json.Unmarshal(claims["email_verified"], &emailVerified)
	address, err = email(address)
	if err != nil || !emailVerified {
		return reply{}, fail(403, "oidc_email_unverified", "A verified email address is required.")
	}
	var groups []string
	if raw := claims[c.GroupsClaim]; raw != nil && json.Unmarshal(raw, &groups) != nil {
		return reply{}, fail(403, "oidc_claims_invalid", "The groups claim must be an array of strings.")
	}
	if len(groups) > 500 {
		return reply{}, fail(403, "oidc_claims_invalid", "The groups claim is too large.")
	}
	roleClaims, err := json.Marshal(oidcRoleClaims{ClientID: c.ClientID, Scopes: c.Scopes, EmailClaim: c.EmailClaim, GroupsClaim: c.GroupsClaim, Email: address, Groups: groups})
	if err != nil {
		return reply{}, err
	}
	if flow.Kind == "reauthenticate" {
		var authTime int64
		json.Unmarshal(claims["auth_time"], &authTime)
		if authTime < time.Now().Add(-5*time.Minute).Unix() || authTime > time.Now().Add(time.Minute).Unix() {
			return reply{}, fail(403, "oidc_recent_auth_required", "The provider must confirm a recent authentication.")
		}
	}
	tx, err := s.begin(r)
	if err != nil {
		return reply{}, err
	}
	defer tx.Rollback(r.Context())
	current, err := loadOIDC(r, tx)
	if err != nil {
		return reply{}, err
	}
	if current.ETag != c.ETag || !current.Enabled {
		return reply{}, fail(403, "oidc_flow_invalid", "The OIDC configuration changed during sign-in.")
	}
	var userID, identityID string
	err = tx.QueryRow(r.Context(), "SELECT user_id::text,id::text FROM olp_go.oidc_identities WHERE issuer=$1 AND subject=$2", c.Issuer, verified.Subject).Scan(&userID, &identityID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return reply{}, err
	}
	if identityID != "" {
		if _, err = tx.Exec(r.Context(), "UPDATE olp_go.oidc_identities SET role_claims=$2,last_login_at=now() WHERE id=$1", identityID, roleClaims); err != nil {
			return reply{}, err
		}
	}
	response := reply{Status: 303, Location: flow.ReturnTo, Cookies: []*http.Cookie{clearCookie("__Host-olp_oidc_login_" + flow.State)}}
	if flow.Kind != "login" {
		p, err := s.principal(r, tx, "read")
		if err != nil {
			return reply{}, err
		}
		if p.ID != flow.UserID || p.SessionID != flow.SessionID {
			return reply{}, fail(403, "oidc_session_changed", "The browser identity changed during authorization.")
		}
		if flow.Kind == "link" {
			if userID != "" {
				return reply{}, fail(409, "oidc_identity_linked", "This identity is already linked.")
			}
			identityID = newID()
			userID = p.ID
			if _, err = tx.Exec(r.Context(), "INSERT INTO olp_go.oidc_identities(id,user_id,issuer,subject,email_at_link,role_claims,last_login_at) VALUES($1,$2,$3,$4,$5,$6,now())", identityID, p.ID, c.Issuer, verified.Subject, address, roleClaims); err != nil {
				return reply{}, err
			}
			rotated, err := s.changeSignInMethod(r, tx, p.ID)
			if err != nil {
				return reply{}, err
			}
			response.Cookies = append(response.Cookies, rotated.Cookies...)
			response.CSRF = rotated.CSRF
		} else {
			if userID != p.ID {
				return reply{}, fail(403, "oidc_identity_mismatch", "Use an identity already linked to this account.")
			}
			grant, err := s.grantRecent(r, tx, p, flow.Purpose, flow.ResourceID)
			if err != nil {
				return reply{}, err
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
			if err = tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM olp_go.users WHERE email=$1)", address).Scan(&existing); err != nil {
				return reply{}, err
			}
			if existing {
				return reply{}, fail(409, "oidc_link_required", "Sign in to your existing account and link this identity from your profile.")
			}
			role := mappedRole(c, address, groups)
			if role == "" {
				return reply{}, fail(403, "oidc_provisioning_denied", "No role mapping authorizes this identity.")
			}
			var complete bool
			if err = tx.QueryRow(r.Context(), "SELECT setup_complete FROM olp_go.installation WHERE singleton").Scan(&complete); err != nil {
				return reply{}, err
			}
			if !complete {
				return reply{}, fail(403, "setup_required", "Complete owner setup before OIDC sign-in.")
			}
			userID = newID()
			identityID = newID()
			var name string
			json.Unmarshal(claims["name"], &name)
			if validText("display_name", name, 100) != nil {
				name = address
			}
			if _, err = tx.Exec(r.Context(), "INSERT INTO olp_go.users(id,email,display_name,role,etag) VALUES($1,$2,$3,$4,$5)", userID, address, name, role, newID()); err != nil {
				return reply{}, err
			}
			if _, err = tx.Exec(r.Context(), "INSERT INTO olp_go.oidc_identities(id,user_id,issuer,subject,email_at_link,role_claims,last_login_at) VALUES($1,$2,$3,$4,$5,$6,now())", identityID, userID, c.Issuer, verified.Subject, address, roleClaims); err != nil {
				return reply{}, err
			}
		}
		var active bool
		if err = tx.QueryRow(r.Context(), "SELECT active FROM olp_go.users WHERE id=$1", userID).Scan(&active); err != nil {
			return reply{}, err
		}
		if !active {
			return reply{}, fail(403, "account_disabled", "This account is disabled.")
		}
		var local bool
		var role string
		if err = tx.QueryRow(r.Context(), "SELECT password_hash IS NOT NULL,role FROM olp_go.users WHERE id=$1", userID).Scan(&local, &role); err != nil {
			return reply{}, err
		}
		if !local {
			mapped := mappedRole(c, address, groups)
			if mapped == "" {
				return reply{}, fail(403, "oidc_provisioning_denied", "No role mapping authorizes this identity.")
			}
			if mapped != role {
				if _, err = tx.Exec(r.Context(), "UPDATE olp_go.users SET role=$2,etag=$3,updated_at=now() WHERE id=$1", userID, mapped, newID()); err != nil {
					return reply{}, err
				}
				if err = usableOwner(r, tx); err != nil {
					return reply{}, err
				}
				if _, err = tx.Exec(r.Context(), "DELETE FROM olp_go.sessions WHERE user_id=$1", userID); err != nil {
					return reply{}, err
				}
				if mapped != "owner" {
					if err = retireIssuedInvitations(r, tx, userID, ""); err != nil {
						return reply{}, err
					}
				}
				if _, err = advanceAuthority(r, tx); err != nil {
					return reply{}, err
				}
				if err = audit(r.Context(), tx, r, "", "user.role_sync_oidc", "user", userID, "success"); err != nil {
					return reply{}, err
				}
			}
		}
		session, err := s.newSession(r, tx, userID)
		if err != nil {
			return reply{}, err
		}
		response.Cookies = append(response.Cookies, session.Cookies...)
		response.CSRF = session.CSRF
	}
	if err = audit(r.Context(), tx, r, userID, "oidc."+flow.Kind, "oidc_identity", identityID, "success"); err != nil {
		return reply{}, err
	}
	return commit(r, tx, response)
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
func oidcSignInRole(c oidcConfiguration, local bool, role string, data []byte) (string, error) {
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
	if local {
		return role, nil
	}
	return mappedRole(c, claims.Email, claims.Groups), nil
}

func usableOIDCIdentities(r *http.Request, q queryer, p principal, local bool) (map[string]bool, error) {
	c, err := loadOIDC(r, q)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !c.Enabled {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(r.Context(), "SELECT id::text,role_claims FROM olp_go.oidc_identities WHERE user_id=$1 AND issuer=$2", p.ID, c.Issuer)
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

func (s *Server) oidcIdentities(r *http.Request) (reply, error) {
	p, err := s.principal(r, s.Pool, "read")
	if err != nil {
		return reply{}, err
	}
	var local, localEnabled, enabled bool
	if err = s.Pool.QueryRow(r.Context(), "SELECT password_hash IS NOT NULL,COALESCE((SELECT value='true' FROM olp_go.settings WHERE key='auth.local_login_enabled'),true),COALESCE((SELECT (document->>'enabled')::boolean FROM olp_go.oidc_configuration WHERE singleton),false) FROM olp_go.users WHERE id=$1", p.ID).Scan(&local, &localEnabled, &enabled); err != nil {
		return reply{}, err
	}
	usable, err := usableOIDCIdentities(r, s.Pool, p, local)
	if err != nil {
		return reply{}, err
	}
	rows, err := s.Pool.Query(r.Context(), `SELECT jsonb_build_object(
        'id',i.id,'issuer',i.issuer,'email_at_link',i.email_at_link,
        'created_at',i.created_at,'last_login_at',i.last_login_at)
        FROM olp_go.oidc_identities i WHERE i.user_id=$1 ORDER BY i.created_at,i.id LIMIT 100`, p.ID)
	if err != nil {
		return reply{}, err
	}
	items, err := jsonRows(rows)
	if err != nil {
		return reply{}, err
	}
	for _, item := range items {
		remaining := len(usable)
		if usable[item["id"].(string)] {
			remaining--
		}
		item["can_unlink"] = local && localEnabled || remaining > 0
	}
	return ok(map[string]any{"items": items, "linking_available": enabled, "has_local_password": local}), nil
}
func (s *Server) unlinkOIDCIdentity(r *http.Request) (reply, error) {
	id, err := idParam(r, "identity_id")
	if err != nil {
		return reply{}, err
	}
	tx, err := s.begin(r)
	if err != nil {
		return reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.principal(r, tx, "read")
	if err != nil {
		return reply{}, err
	}
	var owner string
	if err = tx.QueryRow(r.Context(), "SELECT user_id::text FROM olp_go.oidc_identities WHERE id=$1", id).Scan(&owner); err != nil {
		return reply{}, err
	}
	if owner != p.ID {
		return reply{}, forbidden()
	}
	if err = s.consumeRecent(r, tx, p, "oidc_unlink", id); err != nil {
		return reply{}, err
	}
	if _, err = tx.Exec(r.Context(), "DELETE FROM olp_go.oidc_identities WHERE id=$1", id); err != nil {
		return reply{}, err
	}
	var local, localEnabled bool
	if err = tx.QueryRow(r.Context(), `SELECT password_hash IS NOT NULL,COALESCE((SELECT value='true' FROM olp_go.settings WHERE key='auth.local_login_enabled'),true) FROM olp_go.users WHERE id=$1`, p.ID).Scan(&local, &localEnabled); err != nil {
		return reply{}, err
	}
	if !local || !localEnabled {
		usable, err := usableOIDCIdentities(r, tx, p, local)
		if err != nil {
			return reply{}, err
		}
		if len(usable) == 0 {
			return reply{}, fail(409, "last_sign_in_method", "Keep at least one usable sign-in method.")
		}
	}
	if err = usableOwner(r, tx); err != nil {
		return reply{}, err
	}
	if err = audit(r.Context(), tx, r, p.ID, "oidc.unlink", "oidc_identity", id, "success"); err != nil {
		return reply{}, err
	}
	rotated, err := s.changeSignInMethod(r, tx, p.ID)
	if err != nil {
		return reply{}, err
	}
	rotated.Status, rotated.Body = 204, nil
	return commit(r, tx, rotated)
}

func (s *Server) changeSignInMethod(r *http.Request, tx pgx.Tx, userID string) (reply, error) {
	if _, err := tx.Exec(r.Context(), "UPDATE olp_go.users SET etag=$2,updated_at=now() WHERE id=$1", userID, newID()); err != nil {
		return reply{}, err
	}
	if _, err := tx.Exec(r.Context(), "DELETE FROM olp_go.sessions WHERE user_id=$1", userID); err != nil {
		return reply{}, err
	}
	if err := audit(r.Context(), tx, r, userID, "user.authentication_method_change", "user", userID, "success"); err != nil {
		return reply{}, err
	}
	return s.newSession(r, tx, userID)
}
