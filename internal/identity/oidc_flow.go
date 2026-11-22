package identity

import (
	"crypto/hmac"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/authz"
	"github.com/tyk-swe/olp/internal/platform/secrets"
	"golang.org/x/oauth2"
)

type oidcFlow struct {
	ID                                                                             string `json:"-"` // Authoritative row ID, used only to address the browser binding cookie.
	Kind, State, Nonce, Verifier, ReturnTo, UserID, SessionID, Purpose, ResourceID string
}

func (s *Server) beginOIDCLogin(r *http.Request) (httpkit.Reply, error) {
	return s.beginOIDC(r, "login")
}

func (s *Server) beginOIDCLink(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	return s.beginOIDC(r, "link")
}

func (s *Server) beginOIDCReauthentication(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	return s.beginOIDC(r, "reauthenticate")
}

type oidcBeginInput struct {
	ReturnTo   string `json:"return_to"`
	Purpose    string `json:"purpose"`
	ResourceID string `json:"resource_id"`
}

func parseOIDCBeginInput(r *http.Request, kind string) (oidcBeginInput, error) {
	var input oidcBeginInput
	if r.Method == "GET" {
		input.ReturnTo = r.URL.Query().Get("return_to")
	} else if r.ContentLength != 0 {
		if err := httpkit.Decode(r, &input); err != nil {
			return oidcBeginInput{}, err
		}
	}
	if !safeReturn(input.ReturnTo) {
		return oidcBeginInput{}, httpkit.Invalid("return_to", "Use a local console path.")
	}
	if input.ReturnTo == "" {
		input.ReturnTo = "/"
	}
	if kind == "reauthenticate" {
		if err := validatePurpose(input.Purpose, input.ResourceID); err != nil {
			return oidcBeginInput{}, err
		}
	}
	return input, nil
}

func (s *Server) bindOIDCFlowPrincipal(r *http.Request, tx pgx.Tx, kind, issuer string, flow *oidcFlow) error {
	if kind == "login" {
		return nil
	}
	p, err := s.access.Reauthorize(r, tx)
	if err != nil {
		return err
	}
	flow.UserID = p.ID
	flow.SessionID = p.SessionID
	if kind == "link" {
		if err = s.access.ConsumeRecent(r, tx, p, "oidc_link", ""); err != nil {
			return err
		}
	}
	if kind == "reauthenticate" {
		var linked bool
		if err = tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM olp.oidc_identities WHERE user_id=$1 AND issuer=$2)", p.ID, issuer).Scan(&linked); err != nil {
			return err
		}
		if !linked {
			return httpkit.Fail(403, "oidc_identity_required", "Link an identity before using OIDC reauthentication.")
		}
	}
	flow.ReturnTo = "/settings/profile"
	return nil
}

func (s *Server) beginOIDC(r *http.Request, kind string) (httpkit.Reply, error) {
	if err := s.admit(r, "oidc_begin", ""); err != nil {
		return httpkit.Reply{}, err
	}
	input, err := parseOIDCBeginInput(r, kind)
	if err != nil {
		return httpkit.Reply{}, err
	}
	c, err := loadOIDC(r.Context(), s.access.Pool)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if !c.Enabled {
		return httpkit.Reply{}, httpkit.Fail(403, "oidc_disabled", "OIDC sign-in is disabled.")
	}
	_, oauth, err := s.discover(r.Context(), &c)
	if err != nil {
		return httpkit.Reply{}, err
	}
	tx, err := s.access.Begin(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	current, err := loadOIDC(r.Context(), tx)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if current.ETag != c.ETag {
		return httpkit.Reply{}, httpkit.Fail(409, "oidc_configuration_changed", "The OIDC configuration changed. Try again.")
	}
	flow := oidcFlow{Kind: kind, State: secrets.Token(), Nonce: secrets.Token(), Verifier: oauth2.GenerateVerifier(), ReturnTo: input.ReturnTo, Purpose: input.Purpose, ResourceID: input.ResourceID}
	if err := s.bindOIDCFlowPrincipal(r, tx, kind, c.Issuer, &flow); err != nil {
		return httpkit.Reply{}, err
	}
	id, cookieToken := httpkit.NewID(), secrets.Token()
	expires := time.Now().Add(10 * time.Minute)
	data, err := json.Marshal(flow)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if _, err = tx.Exec(r.Context(), "DELETE FROM olp.secrets WHERE id IN(SELECT id FROM olp.secrets WHERE expires_at<=now() LIMIT 100)"); err != nil {
		return httpkit.Reply{}, err
	}
	if err = s.access.Keys.Store(r.Context(), tx, s.access.Installation, id, secrets.OIDCFlow, data, &expires); err != nil {
		return httpkit.Reply{}, err
	}
	if _, err = tx.Exec(r.Context(), "INSERT INTO olp.oidc_flows(id,state_digest,cookie_digest,configuration_etag,expires_at) VALUES($1,$2,$3,$4,$5)", id, s.access.Auth.Digest(secrets.OIDCStateDigest, flow.State), s.access.Auth.Digest(secrets.OIDCCookieDigest, cookieToken), c.ETag, expires); err != nil {
		return httpkit.Reply{}, err
	}
	options := []oauth2.AuthCodeOption{oidc.Nonce(flow.Nonce), oauth2.S256ChallengeOption(flow.Verifier)}
	if kind == "reauthenticate" {
		options = append(options, oauth2.SetAuthURLParam("prompt", "login"), oauth2.SetAuthURLParam("max_age", "0"))
	}
	authorizationURL := oauth.AuthCodeURL(flow.State, options...)
	response := httpkit.Reply{Status: 200, Body: map[string]string{"authorization_url": authorizationURL}, Cookies: []*http.Cookie{access.NewCookie("__Host-olp_oidc_login_"+id, cookieToken, 10*time.Minute, true)}}
	if kind == "link" {
		response.Cookies = append(response.Cookies, access.ClearCookie(access.RecentCookie))
	}
	if r.Method == "GET" {
		response.Status = 303
		response.Body = nil
		response.Location = authorizationURL
	}
	return response, tx.Commit(r.Context())
}

// The flow is consumed and committed before network exchange. Neither a
// repeated callback nor a failed token response can resurrect a login grant.
func (s *Server) consumeFlow(r *http.Request) (oidcFlow, oidcConfiguration, error) {
	var flow oidcFlow
	var config oidcConfiguration
	tx, err := s.access.Begin(r)
	if err != nil {
		return flow, config, err
	}
	defer tx.Rollback(r.Context())
	state := r.URL.Query().Get("state")
	var id, etag string
	var digest []byte
	err = tx.QueryRow(r.Context(), "SELECT id::text,configuration_etag::text,cookie_digest FROM olp.oidc_flows WHERE state_digest=$1 AND expires_at>now()", s.access.Auth.Digest(secrets.OIDCStateDigest, state)).Scan(&id, &etag, &digest)
	if errors.Is(err, pgx.ErrNoRows) {
		return flow, config, httpkit.Fail(403, "oidc_flow_invalid", "This sign-in request expired or was already used.")
	}
	if err != nil {
		return flow, config, err
	}
	if !hmac.Equal(digest, s.access.Auth.Digest(secrets.OIDCCookieDigest, access.CookieValue(r, "__Host-olp_oidc_login_"+id))) {
		return flow, config, httpkit.Fail(403, "oidc_flow_invalid", "The sign-in request belongs to a different browser.")
	}
	data, err := s.access.Keys.Read(r.Context(), tx, s.access.Installation, id, secrets.OIDCFlow)
	if err != nil {
		return flow, config, err
	}
	if err = json.Unmarshal(data, &flow); err != nil {
		return flow, config, err
	}
	flow.ID = id
	config, err = loadOIDC(r.Context(), tx)
	if err != nil {
		return flow, config, err
	}
	if config.ETag != etag || !config.Enabled {
		return flow, config, httpkit.Fail(403, "oidc_flow_invalid", "OIDC configuration changed during sign-in.")
	}
	if _, err = tx.Exec(r.Context(), "DELETE FROM olp.secrets WHERE id=$1", id); err != nil {
		return flow, config, err
	}
	return flow, config, tx.Commit(r.Context())
}

type verifiedOIDCIdentity struct {
	subject    string
	email      string
	groups     []string
	claims     map[string]json.RawMessage
	roleClaims []byte
}

func (s *Server) exchangeOIDCIdentity(r *http.Request, flow oidcFlow, c *oidcConfiguration) (verifiedOIDCIdentity, error) {
	provider, oauth, err := s.discover(r.Context(), c)
	if err != nil {
		return verifiedOIDCIdentity{}, err
	}
	if c.HasClientSecret {
		tx, err := s.access.Pool.Begin(r.Context())
		if err != nil {
			return verifiedOIDCIdentity{}, err
		}
		defer tx.Rollback(r.Context())
		data, err := s.access.Keys.Read(r.Context(), tx, s.access.Installation, c.ID, secrets.OIDCClientSecret)
		if err != nil {
			return verifiedOIDCIdentity{}, err
		}
		oauth.ClientSecret = string(data)
		if err = tx.Commit(r.Context()); err != nil {
			return verifiedOIDCIdentity{}, err
		}
	}
	ctx := oidc.ClientContext(r.Context(), s.access.IdentityClient())
	token, err := oauth.Exchange(ctx, r.URL.Query().Get("code"), oauth2.VerifierOption(flow.Verifier))
	if err != nil {
		return verifiedOIDCIdentity{}, httpkit.Fail(403, "oidc_token_invalid", "The identity provider token could not be verified.")
	}
	raw, ok := token.Extra("id_token").(string)
	if !ok || len(raw) > 65536 {
		return verifiedOIDCIdentity{}, httpkit.Fail(403, "oidc_token_invalid", "The identity provider did not supply a valid identity token.")
	}
	verified, err := provider.Verifier(&oidc.Config{ClientID: c.ClientID, SupportedSigningAlgs: []string{oidc.RS256, oidc.ES256, oidc.EdDSA}}).Verify(ctx, raw)
	if err != nil || !hmac.Equal([]byte(verified.Nonce), []byte(flow.Nonce)) {
		return verifiedOIDCIdentity{}, httpkit.Fail(403, "oidc_token_invalid", "The identity token signature, issuer, audience, expiry, or nonce is invalid.")
	}
	var claims map[string]json.RawMessage
	if verified.Claims(&claims) != nil {
		return verifiedOIDCIdentity{}, httpkit.Fail(403, "oidc_claims_invalid", "The identity claims are invalid.")
	}
	if len(verified.Subject) < 1 || len(verified.Subject) > 255 {
		return verifiedOIDCIdentity{}, httpkit.Fail(403, "oidc_claims_invalid", "A bounded identity subject is required.")
	}
	var address string
	var emailVerified bool
	json.Unmarshal(claims[c.EmailClaim], &address)
	json.Unmarshal(claims["email_verified"], &emailVerified)
	address, err = email(address)
	if err != nil || !emailVerified {
		return verifiedOIDCIdentity{}, httpkit.Fail(403, "oidc_email_unverified", "A verified email address is required.")
	}
	var groups []string
	if raw := claims[c.GroupsClaim]; raw != nil && json.Unmarshal(raw, &groups) != nil {
		return verifiedOIDCIdentity{}, httpkit.Fail(403, "oidc_claims_invalid", "The groups claim must be an array of strings.")
	}
	if len(groups) > 500 {
		return verifiedOIDCIdentity{}, httpkit.Fail(403, "oidc_claims_invalid", "The groups claim is too large.")
	}
	roleClaims, err := json.Marshal(oidcRoleClaims{ClientID: c.ClientID, Scopes: c.Scopes, EmailClaim: c.EmailClaim, GroupsClaim: c.GroupsClaim, Email: address, Groups: groups})
	if err != nil {
		return verifiedOIDCIdentity{}, err
	}
	if flow.Kind == "reauthenticate" {
		var authTime int64
		json.Unmarshal(claims["auth_time"], &authTime)
		if authTime < time.Now().Add(-5*time.Minute).Unix() || authTime > time.Now().Add(time.Minute).Unix() {
			return verifiedOIDCIdentity{}, httpkit.Fail(403, "oidc_recent_auth_required", "The provider must confirm a recent authentication.")
		}
	}
	return verifiedOIDCIdentity{subject: verified.Subject, email: address, groups: groups, claims: claims, roleClaims: roleClaims}, nil
}

func (s *Server) oidcCallback(r *http.Request) (reply httpkit.Reply, callbackErr error) {
	var flow oidcFlow
	defer func() {
		if callbackErr != nil && strings.Contains(r.Header.Get("Accept"), "text/html") && !strings.Contains(r.Header.Get("Accept"), "application/json") {
			reply = oidcFailureRedirect(flow, callbackErr)
			callbackErr = nil
		}
	}()
	if err := s.admit(r, "oidc_callback", ""); err != nil {
		return httpkit.Reply{}, err
	}
	consumed, c, err := s.consumeFlow(r)
	flow = consumed
	if err != nil {
		return httpkit.Reply{}, err
	}
	if r.URL.Query().Get("error") != "" || r.URL.Query().Get("code") == "" {
		return httpkit.Reply{}, httpkit.Fail(403, "oidc_authorization_denied", "The identity provider did not authorize sign-in.")
	}
	verified, err := s.exchangeOIDCIdentity(r, flow, &c)
	if err != nil {
		return httpkit.Reply{}, err
	}
	tx, err := s.access.Begin(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	return s.finishOIDCCallback(r, tx, flow, c, verified)
}

func (s *Server) finishOIDCCallback(r *http.Request, tx pgx.Tx, flow oidcFlow, c oidcConfiguration, verified verifiedOIDCIdentity) (httpkit.Reply, error) {
	current, err := loadOIDC(r.Context(), tx)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if current.ETag != c.ETag || !current.Enabled {
		return httpkit.Reply{}, httpkit.Fail(403, "oidc_flow_invalid", "The OIDC configuration changed during sign-in.")
	}
	var userID, identityID string
	err = tx.QueryRow(r.Context(), "SELECT user_id::text,id::text FROM olp.oidc_identities WHERE issuer=$1 AND subject=$2", c.Issuer, verified.subject).Scan(&userID, &identityID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return httpkit.Reply{}, err
	}
	// Check the initiating session and exact identity before applying a
	// reauthentication result. Ordinary login is never a recent-auth proof.
	var p authz.Principal
	if flow.Kind != "login" {
		p, err = s.access.Authenticate(r, tx)
		if err == nil {
			err = p.Authorize(authz.Self)
		}
		if err != nil {
			return httpkit.Reply{}, err
		}
		if p.ID != flow.UserID || p.SessionID != flow.SessionID {
			return httpkit.Reply{}, httpkit.Fail(403, "oidc_session_changed", "The browser identity changed during authorization.")
		}
		if flow.Kind == "reauthenticate" && userID != p.ID {
			return httpkit.Reply{}, httpkit.Fail(403, "oidc_identity_mismatch", "Use an identity already linked to this account.")
		}
	}
	if identityID != "" {
		if _, err = tx.Exec(r.Context(), "UPDATE olp.oidc_identities SET role_claims=$2,last_login_at=now() WHERE id=$1", identityID, verified.roleClaims); err != nil {
			return httpkit.Reply{}, err
		}
	}
	if identityID != "" && flow.Kind != "link" {
		changed, allowed, err := syncOIDCAuthority(r, tx, userID, mappedRole(c, verified.email, verified.groups))
		if err != nil {
			return httpkit.Reply{}, err
		}
		if !allowed || changed && flow.Kind == "reauthenticate" {
			// Verified external deauthorization is authoritative even for the
			// last owner. Commit it before returning an error; rollback must
			// not preserve old sessions, recent-auth grants or invitations.
			if err = tx.Commit(r.Context()); err != nil {
				return httpkit.Reply{}, err
			}
			if !allowed {
				return httpkit.Reply{}, httpkit.Fail(403, "oidc_provisioning_denied", "No role mapping authorizes this identity.")
			}
			return httpkit.Reply{}, httpkit.Fail(403, "oidc_session_changed", "Your access changed. Sign in again before verifying your identity.")
		}
	}
	response := httpkit.Reply{Status: 303, Location: flow.ReturnTo, Cookies: []*http.Cookie{access.ClearCookie("__Host-olp_oidc_login_" + flow.ID)}}
	if flow.Kind != "login" {
		userID, identityID, err = s.completeOIDCLinkOrReauth(r, tx, flow, c, p, userID, identityID, verified, &response)
	} else {
		userID, identityID, err = s.completeOIDCLogin(r, tx, c, userID, identityID, verified, &response)
	}
	if err != nil {
		return httpkit.Reply{}, err
	}
	if err = access.Audit(r.Context(), tx, r, authz.UserActor(userID), "oidc."+flow.Kind, "oidc_identity", identityID, "success"); err != nil {
		return httpkit.Reply{}, err
	}
	return response, tx.Commit(r.Context())
}

func (s *Server) completeOIDCLinkOrReauth(r *http.Request, tx pgx.Tx, flow oidcFlow, c oidcConfiguration, p authz.Principal, userID, identityID string, verified verifiedOIDCIdentity, response *httpkit.Reply) (string, string, error) {
	if flow.Kind == "link" {
		if userID != "" {
			return "", "", httpkit.Fail(409, "oidc_identity_linked", "This identity is already linked.")
		}
		identityID = httpkit.NewID()
		userID = p.ID
		if _, err := tx.Exec(r.Context(), "INSERT INTO olp.oidc_identities(id,user_id,issuer,subject,email_at_link,role_claims,last_login_at) VALUES($1,$2,$3,$4,$5,$6,now())", identityID, p.ID, c.Issuer, verified.subject, verified.email, verified.roleClaims); err != nil {
			return "", "", err
		}
		rotated, err := s.changeSignInMethod(r, tx, p.ID, sessionAuth{Method: "oidc"})
		if err != nil {
			return "", "", err
		}
		response.Cookies = append(response.Cookies, rotated.Cookies...)
		response.CSRF = rotated.CSRF
		return userID, identityID, nil
	}
	if userID != p.ID {
		return "", "", httpkit.Fail(403, "oidc_identity_mismatch", "Use an identity already linked to this account.")
	}
	grant, err := s.grantRecent(r, tx, p, flow.Purpose, flow.ResourceID, false)
	if err != nil {
		return "", "", err
	}
	response.Cookies = append(response.Cookies, grant.Cookies...)
	response.Location = reauthenticatedAt(flow.Purpose) + "?reauthenticated=" + url.QueryEscape(flow.Purpose)
	if flow.ResourceID != "" {
		response.Location += "&resource_id=" + url.QueryEscape(flow.ResourceID)
	}
	return userID, identityID, nil
}

func (s *Server) completeOIDCLogin(r *http.Request, tx pgx.Tx, c oidcConfiguration, userID, identityID string, verified verifiedOIDCIdentity, response *httpkit.Reply) (string, string, error) {
	if userID == "" {
		var existing bool
		if err := tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM olp.users WHERE email=$1)", verified.email).Scan(&existing); err != nil {
			return "", "", err
		}
		if existing {
			return "", "", httpkit.Fail(409, "oidc_link_required", "Sign in to your existing account and link this identity from your profile.")
		}
		role := mappedRole(c, verified.email, verified.groups)
		if role == "" {
			return "", "", httpkit.Fail(403, "oidc_provisioning_denied", "No role mapping authorizes this identity.")
		}
		var complete bool
		if err := tx.QueryRow(r.Context(), "SELECT setup_complete FROM olp.installation WHERE singleton").Scan(&complete); err != nil {
			return "", "", err
		}
		if !complete {
			return "", "", httpkit.Fail(403, "setup_required", "Complete owner setup before OIDC sign-in.")
		}
		userID = httpkit.NewID()
		identityID = httpkit.NewID()
		var name string
		json.Unmarshal(verified.claims["name"], &name)
		if httpkit.ValidText("display_name", name, 100) != nil {
			name = verified.email
		}
		if _, err := tx.Exec(r.Context(), "INSERT INTO olp.users(id,email,display_name,role,etag,role_management) VALUES($1,$2,$3,$4,$5,'oidc')", userID, verified.email, name, role, httpkit.NewID()); err != nil {
			return "", "", err
		}
		if _, err := tx.Exec(r.Context(), "INSERT INTO olp.oidc_identities(id,user_id,issuer,subject,email_at_link,role_claims,last_login_at) VALUES($1,$2,$3,$4,$5,$6,now())", identityID, userID, c.Issuer, verified.subject, verified.email, verified.roleClaims); err != nil {
			return "", "", err
		}
	}
	var active bool
	if err := tx.QueryRow(r.Context(), "SELECT active FROM olp.users WHERE id=$1", userID).Scan(&active); err != nil {
		return "", "", err
	}
	if !active {
		return "", "", httpkit.Fail(403, "account_disabled", "This account is disabled.")
	}
	session, err := s.newSession(r, tx, userID, sessionAuth{Method: "oidc"})
	if err != nil {
		return "", "", err
	}
	response.Cookies = append(response.Cookies, session.Cookies...)
	response.CSRF = session.CSRF
	return userID, identityID, nil
}
