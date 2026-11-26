package identity

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/domain/ids"
	"github.com/tyk-swe/olp/internal/platform/secrets"
	"golang.org/x/oauth2"
)

type oidcFlow struct {
	ID                                                                             string `json:"-"` // Authoritative row ID, used only to address the browser binding cookie.
	Kind, State, Nonce, Verifier, ReturnTo, UserID, SessionID, Purpose, ResourceID string
}

type preparedOIDCBegin struct {
	kind   string
	input  federatedBeginInput
	config oidcConfiguration
	oauth  *oauth2.Config
}

type oidcOutcome struct {
	userID     string
	identityID string
	reply      httpkit.Reply
}

func (s *oidcService) beginOIDCLogin(r *http.Request) (httpkit.Reply, error) {
	prep, err := s.prepareOIDCBegin(r, "login")
	if err != nil {
		return httpkit.Reply{}, err
	}
	return access.Serialize(s.access, r, func(tx pgx.Tx) (httpkit.Reply, bool, error) {
		reply, err := s.storeOIDCFlow(r, tx, prep, nil)
		return reply, err == nil, err
	})
}

func (s *oidcService) beginOIDCLink(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	return s.beginSecuredOIDC(r, "link")
}

func (s *oidcService) beginOIDCReauthentication(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	return s.beginSecuredOIDC(r, "reauthenticate")
}

func (s *oidcService) beginSecuredOIDC(r *http.Request, kind string) (httpkit.Reply, error) {
	prep, err := s.prepareOIDCBegin(r, kind)
	if err != nil {
		return httpkit.Reply{}, err
	}
	return access.Transact(s.access, r, access.Unlocked, func(tx pgx.Tx, p authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		reply, err := s.storeOIDCFlow(r, tx, prep, &p)
		return reply, access.Event{}, err
	})
}

func (s *oidcService) prepareOIDCBegin(r *http.Request, kind string) (preparedOIDCBegin, error) {
	if err := admitAuth(s.access, r, "oidc_begin", ""); err != nil {
		return preparedOIDCBegin{}, err
	}
	input, err := parseFederatedBeginInput(r, kind, true)
	if err != nil {
		return preparedOIDCBegin{}, err
	}
	c, err := loadOIDC(r.Context(), s.access.Pool)
	if err != nil {
		return preparedOIDCBegin{}, err
	}
	if !c.Enabled {
		return preparedOIDCBegin{}, httpkit.Fail(403, "oidc_disabled", "OIDC sign-in is disabled.")
	}
	_, oauth, _, err := s.discover(r.Context(), c)
	if err != nil {
		return preparedOIDCBegin{}, err
	}
	return preparedOIDCBegin{kind: kind, input: input, config: c, oauth: oauth}, nil
}

func (s *oidcService) validateOIDCFlowPrincipal(r *http.Request, tx pgx.Tx, p authz.Principal, kind, issuer string) error {
	if kind == "link" {
		return s.access.ConsumeRecent(r, tx, p, "oidc_link", "")
	}
	if kind != "reauthenticate" {
		return nil
	}
	var linked bool
	if err := tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM olp.oidc_identities WHERE user_id=$1 AND issuer=$2)", p.ID, issuer).Scan(&linked); err != nil {
		return err
	}
	if !linked {
		return httpkit.Fail(403, "oidc_identity_required", "Link an identity before using OIDC reauthentication.")
	}
	return nil
}

func (s *oidcService) storeOIDCFlow(r *http.Request, tx pgx.Tx, prep preparedOIDCBegin, principal *authz.Principal) (httpkit.Reply, error) {
	current, err := loadOIDC(r.Context(), tx)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if current.ETag != prep.config.ETag {
		return httpkit.Reply{}, httpkit.Fail(409, "oidc_configuration_changed", "The OIDC configuration changed. Try again.")
	}
	flow := oidcFlow{
		Kind:       prep.kind,
		State:      secrets.Token(),
		Nonce:      secrets.Token(),
		Verifier:   oauth2.GenerateVerifier(),
		ReturnTo:   prep.input.ReturnTo,
		Purpose:    prep.input.Purpose,
		ResourceID: prep.input.ResourceID,
	}
	if principal != nil {
		if err := s.validateOIDCFlowPrincipal(r, tx, *principal, prep.kind, prep.config.Issuer); err != nil {
			return httpkit.Reply{}, err
		}
		flow.UserID = principal.ID
		flow.SessionID = principal.SessionID
		flow.ReturnTo = "/settings/profile"
	}
	id, cookieToken := ids.New(), secrets.Token()
	expires := time.Now().Add(10 * time.Minute)
	data, err := json.Marshal(flow)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if err = pruneExpiredSecrets(r.Context(), tx); err != nil {
		return httpkit.Reply{}, err
	}
	if err = s.access.Keys.Store(r.Context(), tx, s.access.Installation, id, secrets.OIDCFlow, data, &expires); err != nil {
		return httpkit.Reply{}, err
	}
	if _, err = tx.Exec(r.Context(), "INSERT INTO olp.oidc_flows(id,state_digest,cookie_digest,configuration_etag,expires_at) VALUES($1,$2,$3,$4,$5)", id, s.access.Auth.Digest(secrets.OIDCStateDigest, flow.State), s.access.Auth.Digest(secrets.OIDCCookieDigest, cookieToken), prep.config.ETag, expires); err != nil {
		return httpkit.Reply{}, err
	}
	options := []oauth2.AuthCodeOption{oidc.Nonce(flow.Nonce), oauth2.S256ChallengeOption(flow.Verifier)}
	if prep.kind == "reauthenticate" {
		options = append(options, oauth2.SetAuthURLParam("prompt", "login"), oauth2.SetAuthURLParam("max_age", "0"))
	}
	authorizationURL := prep.oauth.AuthCodeURL(flow.State, options...)
	cookies := []*http.Cookie{access.NewCookie("__Host-olp_oidc_login_"+id, cookieToken, 10*time.Minute, true)}
	if prep.kind == "link" {
		cookies = append(cookies, access.ClearCookie(access.RecentCookie))
	}
	if r.Method == http.MethodGet {
		return httpkit.Reply{Status: 303, Location: authorizationURL, Cookies: cookies}, nil
	}
	return httpkit.Reply{Status: 200, Body: authorizationURLResponse{AuthorizationURL: authorizationURL}, Cookies: cookies}, nil
}

// The flow is consumed and committed before network exchange. Neither a
// repeated callback nor a failed token response can resurrect a login grant.
func (s *oidcService) consumeFlow(r *http.Request) (oidcFlow, oidcConfiguration, error) {
	var flow oidcFlow
	var config oidcConfiguration
	_, err := access.Serialize(s.access, r, func(tx pgx.Tx) (httpkit.Reply, bool, error) {
		state := r.URL.Query().Get("state")
		var id, etag string
		var digest []byte
		err := tx.QueryRow(r.Context(), "SELECT id::text,configuration_etag::text,cookie_digest FROM olp.oidc_flows WHERE state_digest=$1 AND expires_at>now()", s.access.Auth.Digest(secrets.OIDCStateDigest, state)).Scan(&id, &etag, &digest)
		if errors.Is(err, pgx.ErrNoRows) {
			return httpkit.Reply{}, false, httpkit.Fail(403, "oidc_flow_invalid", "This sign-in request expired or was already used.")
		}
		if err != nil {
			return httpkit.Reply{}, false, err
		}
		if !hmac.Equal(digest, s.access.Auth.Digest(secrets.OIDCCookieDigest, access.CookieValue(r, "__Host-olp_oidc_login_"+id))) {
			return httpkit.Reply{}, false, httpkit.Fail(403, "oidc_flow_invalid", "The sign-in request belongs to a different browser.")
		}
		data, err := s.access.Keys.Read(r.Context(), tx, s.access.Installation, id, secrets.OIDCFlow)
		if err != nil {
			return httpkit.Reply{}, false, err
		}
		if err = json.Unmarshal(data, &flow); err != nil {
			return httpkit.Reply{}, false, err
		}
		flow.ID = id
		config, err = loadOIDC(r.Context(), tx)
		if err != nil {
			return httpkit.Reply{}, false, err
		}
		if config.ETag != etag || !config.Enabled {
			return httpkit.Reply{}, false, httpkit.Fail(403, "oidc_flow_invalid", "OIDC configuration changed during sign-in.")
		}
		if _, err = tx.Exec(r.Context(), "DELETE FROM olp.secrets WHERE id=$1", id); err != nil {
			return httpkit.Reply{}, false, err
		}
		return httpkit.Reply{}, true, nil
	})
	return flow, config, err
}

type verifiedOIDCIdentity struct {
	subject    string
	email      string
	groups     []string
	claims     map[string]json.RawMessage
	roleClaims []byte
}

func decodeOIDCVerifiedClaims(claims map[string]json.RawMessage, subject, kind string, c oidcConfiguration, now time.Time) (verifiedOIDCIdentity, error) {
	if len(subject) < 1 || len(subject) > 255 {
		return verifiedOIDCIdentity{}, httpkit.Fail(403, "oidc_claims_invalid", "A bounded identity subject is required.")
	}
	var address string
	var emailVerified bool
	if raw := claims[c.EmailClaim]; len(raw) == 0 || json.Unmarshal(raw, &address) != nil {
		return verifiedOIDCIdentity{}, httpkit.Fail(403, "oidc_email_unverified", "A verified email address is required.")
	}
	if raw := claims["email_verified"]; len(raw) == 0 || json.Unmarshal(raw, &emailVerified) != nil {
		return verifiedOIDCIdentity{}, httpkit.Fail(403, "oidc_email_unverified", "A verified email address is required.")
	}
	normalizedEmail, err := email(address)
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
	roleClaims, err := json.Marshal(oidcRoleClaims{ClientID: c.ClientID, Scopes: c.Scopes, EmailClaim: c.EmailClaim, GroupsClaim: c.GroupsClaim, Email: normalizedEmail, Groups: groups})
	if err != nil {
		return verifiedOIDCIdentity{}, err
	}
	if kind == "reauthenticate" {
		if err := verifyOIDCAuthTime(claims, now); err != nil {
			return verifiedOIDCIdentity{}, err
		}
	}
	return verifiedOIDCIdentity{subject: subject, email: normalizedEmail, groups: groups, claims: claims, roleClaims: roleClaims}, nil
}

func verifyOIDCAuthTime(claims map[string]json.RawMessage, now time.Time) error {
	var authTime int64
	if raw := claims["auth_time"]; len(raw) == 0 || json.Unmarshal(raw, &authTime) != nil {
		return httpkit.Fail(403, "oidc_recent_auth_required", "The provider must confirm a recent authentication.")
	}
	if authTime < now.Add(-5*time.Minute).Unix() || authTime > now.Add(time.Minute).Unix() {
		return httpkit.Fail(403, "oidc_recent_auth_required", "The provider must confirm a recent authentication.")
	}
	return nil
}

func (s *oidcService) exchangeOIDCIdentity(r *http.Request, flow oidcFlow, c oidcConfiguration) (verifiedOIDCIdentity, error) {
	provider, oauth, _, err := s.discover(r.Context(), c)
	if err != nil {
		return verifiedOIDCIdentity{}, err
	}
	if c.HasClientSecret {
		data, err := s.access.Keys.Read(r.Context(), s.access.Pool, s.access.Installation, c.ID, secrets.OIDCClientSecret)
		if err != nil {
			return verifiedOIDCIdentity{}, err
		}
		oauth.ClientSecret = string(data)
		clear(data)
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
	return decodeOIDCVerifiedClaims(claims, verified.Subject, flow.Kind, c, time.Now())
}

func (s *oidcService) oidcCallback(r *http.Request) (reply httpkit.Reply, callbackErr error) {
	var flow oidcFlow
	defer func() {
		if callbackErr != nil && strings.Contains(r.Header.Get("Accept"), "text/html") && !strings.Contains(r.Header.Get("Accept"), "application/json") {
			reply = oidcFailureRedirect(flow, callbackErr)
			callbackErr = nil
		}
	}()
	if err := admitAuth(s.access, r, "oidc_callback", ""); err != nil {
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
	verified, err := s.exchangeOIDCIdentity(r, flow, c)
	if err != nil {
		return httpkit.Reply{}, err
	}
	return access.Serialize(s.access, r, func(tx pgx.Tx) (httpkit.Reply, bool, error) {
		return s.finishOIDCCallback(r, tx, flow, c, verified)
	})
}

func verifyOIDCCallbackConfig(ctx context.Context, tx pgx.Tx, expectedETag string) error {
	current, err := loadOIDC(ctx, tx)
	if err != nil {
		return err
	}
	if current.ETag != expectedETag || !current.Enabled {
		return httpkit.Fail(403, "oidc_flow_invalid", "The OIDC configuration changed during sign-in.")
	}
	return nil
}

func lookupOIDCIdentity(ctx context.Context, tx pgx.Tx, issuer, subject string) (string, string, error) {
	var userID, identityID string
	err := tx.QueryRow(ctx, "SELECT user_id::text,id::text FROM olp.oidc_identities WHERE issuer=$1 AND subject=$2", issuer, subject).Scan(&userID, &identityID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", nil
	}
	return userID, identityID, err
}

func (s *oidcService) authorizeOIDCCallback(r *http.Request, tx pgx.Tx, flow oidcFlow, linkedUserID string) (authz.Principal, error) {
	if flow.Kind == "login" {
		return authz.Principal{}, nil
	}
	p, err := authenticateFlowSession(s.access, r, tx, flow.UserID, flow.SessionID, "oidc_session_changed", "The browser identity changed during authorization.")
	if err != nil {
		return authz.Principal{}, err
	}
	if flow.Kind == "reauthenticate" && linkedUserID != p.ID {
		return authz.Principal{}, httpkit.Fail(403, "oidc_identity_mismatch", "Use an identity already linked to this account.")
	}
	return p, nil
}

func syncOIDCCallbackIdentity(r *http.Request, tx pgx.Tx, flow oidcFlow, c oidcConfiguration, linkedUserID, linkedIdentityID string, verified verifiedOIDCIdentity) (bool, error) {
	if linkedIdentityID == "" {
		return false, nil
	}
	if _, err := tx.Exec(r.Context(), "UPDATE olp.oidc_identities SET role_claims=$2,last_login_at=now() WHERE id=$1", linkedIdentityID, verified.roleClaims); err != nil {
		return false, err
	}
	if flow.Kind == "link" {
		return false, nil
	}
	changed, allowed, err := syncOIDCAuthority(r, tx, linkedUserID, mappedRole(c, verified.email, verified.groups))
	if err != nil {
		return false, err
	}
	if !allowed {
		return true, httpkit.Fail(403, "oidc_provisioning_denied", "No role mapping authorizes this identity.")
	}
	if changed && flow.Kind == "reauthenticate" {
		return true, httpkit.Fail(403, "oidc_session_changed", "Your access changed. Sign in again before verifying your identity.")
	}
	return false, nil
}

func (s *oidcService) finishOIDCCallback(r *http.Request, tx pgx.Tx, flow oidcFlow, c oidcConfiguration, verified verifiedOIDCIdentity) (httpkit.Reply, bool, error) {
	if err := verifyOIDCCallbackConfig(r.Context(), tx, c.ETag); err != nil {
		return httpkit.Reply{}, false, err
	}
	linkedUserID, linkedIdentityID, err := lookupOIDCIdentity(r.Context(), tx, c.Issuer, verified.subject)
	if err != nil {
		return httpkit.Reply{}, false, err
	}
	p, err := s.authorizeOIDCCallback(r, tx, flow, linkedUserID)
	if err != nil {
		return httpkit.Reply{}, false, err
	}
	if commit, err := syncOIDCCallbackIdentity(r, tx, flow, c, linkedUserID, linkedIdentityID, verified); err != nil {
		return httpkit.Reply{}, commit, err
	}
	outcome, err := s.dispatchOIDCOutcome(r, tx, flow, c, p, linkedUserID, linkedIdentityID, verified)
	if err != nil {
		return httpkit.Reply{}, false, err
	}
	if err := access.Audit(r.Context(), tx, r, authz.UserActor(outcome.userID), "oidc."+flow.Kind, "oidc_identity", outcome.identityID, "success"); err != nil {
		return httpkit.Reply{}, false, err
	}
	reply := outcome.reply
	reply.Cookies = append([]*http.Cookie{access.ClearCookie("__Host-olp_oidc_login_" + flow.ID)}, reply.Cookies...)
	return reply, true, nil
}

func (s *oidcService) dispatchOIDCOutcome(r *http.Request, tx pgx.Tx, flow oidcFlow, c oidcConfiguration, p authz.Principal, linkedUserID, linkedIdentityID string, verified verifiedOIDCIdentity) (oidcOutcome, error) {
	switch flow.Kind {
	case "link":
		return s.completeOIDCLink(r, tx, flow, c, p, linkedUserID, verified)
	case "reauthenticate":
		return s.completeOIDCReauth(r, tx, flow, p, linkedUserID, linkedIdentityID)
	default:
		return s.completeOIDCLogin(r, tx, flow, c, linkedUserID, linkedIdentityID, verified)
	}
}

func (s *oidcService) completeOIDCLink(r *http.Request, tx pgx.Tx, flow oidcFlow, c oidcConfiguration, p authz.Principal, existingUserID string, verified verifiedOIDCIdentity) (oidcOutcome, error) {
	if existingUserID != "" {
		return oidcOutcome{}, httpkit.Fail(409, "oidc_identity_linked", "This identity is already linked.")
	}
	identityID := ids.New()
	if _, err := tx.Exec(r.Context(), "INSERT INTO olp.oidc_identities(id,user_id,issuer,subject,email_at_link,role_claims,last_login_at) VALUES($1,$2,$3,$4,$5,$6,now())", identityID, p.ID, c.Issuer, verified.subject, verified.email, verified.roleClaims); err != nil {
		return oidcOutcome{}, err
	}
	rotated, err := s.sessions.changeSignInMethod(r, tx, p.ID, sessionAuth{Method: "oidc"})
	if err != nil {
		return oidcOutcome{}, err
	}
	return oidcOutcome{
		userID:     p.ID,
		identityID: identityID,
		reply:      httpkit.Reply{Status: 303, Location: flow.ReturnTo, Cookies: rotated.Cookies, CSRF: rotated.CSRF},
	}, nil
}

func (s *oidcService) completeOIDCReauth(r *http.Request, tx pgx.Tx, flow oidcFlow, p authz.Principal, userID, identityID string) (oidcOutcome, error) {
	if userID != p.ID {
		return oidcOutcome{}, httpkit.Fail(403, "oidc_identity_mismatch", "Use an identity already linked to this account.")
	}
	grant, err := s.sessions.grantRecent(r, tx, p, flow.Purpose, flow.ResourceID, false)
	if err != nil {
		return oidcOutcome{}, err
	}
	return oidcOutcome{
		userID:     userID,
		identityID: identityID,
		reply:      httpkit.Reply{Status: 303, Location: reauthenticatedLocation(flow.Purpose, flow.ResourceID), Cookies: grant.Cookies},
	}, nil
}

func provisionOIDCUser(ctx context.Context, tx pgx.Tx, c oidcConfiguration, verified verifiedOIDCIdentity) (string, string, error) {
	var existing bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM olp.users WHERE email=$1)", verified.email).Scan(&existing); err != nil {
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
	if err := tx.QueryRow(ctx, "SELECT setup_complete FROM olp.installation WHERE singleton").Scan(&complete); err != nil {
		return "", "", err
	}
	if !complete {
		return "", "", httpkit.Fail(403, "setup_required", "Complete owner setup before OIDC sign-in.")
	}
	userID := ids.New()
	identityID := ids.New()
	var name string
	if raw := verified.claims["name"]; len(raw) == 0 || json.Unmarshal(raw, &name) != nil || httpkit.ValidText("display_name", name, 100) != nil {
		name = verified.email
	}
	if _, err := tx.Exec(ctx, "INSERT INTO olp.users(id,email,display_name,role,etag,role_management) VALUES($1,$2,$3,$4,$5,'oidc')", userID, verified.email, name, role, ids.New()); err != nil {
		return "", "", err
	}
	if _, err := tx.Exec(ctx, "INSERT INTO olp.oidc_identities(id,user_id,issuer,subject,email_at_link,role_claims,last_login_at) VALUES($1,$2,$3,$4,$5,$6,now())", identityID, userID, c.Issuer, verified.subject, verified.email, verified.roleClaims); err != nil {
		return "", "", err
	}
	return userID, identityID, nil
}

func (s *oidcService) completeOIDCLogin(r *http.Request, tx pgx.Tx, flow oidcFlow, c oidcConfiguration, existingUserID, existingIdentityID string, verified verifiedOIDCIdentity) (oidcOutcome, error) {
	userID, identityID := existingUserID, existingIdentityID
	if userID == "" {
		var err error
		userID, identityID, err = provisionOIDCUser(r.Context(), tx, c, verified)
		if err != nil {
			return oidcOutcome{}, err
		}
	}
	var active bool
	if err := tx.QueryRow(r.Context(), "SELECT active FROM olp.users WHERE id=$1", userID).Scan(&active); err != nil {
		return oidcOutcome{}, err
	}
	if !active {
		return oidcOutcome{}, httpkit.Fail(403, "account_disabled", "This account is disabled.")
	}
	session, err := s.sessions.newSession(r, tx, userID, sessionAuth{Method: "oidc"})
	if err != nil {
		return oidcOutcome{}, err
	}
	return oidcOutcome{
		userID:     userID,
		identityID: identityID,
		reply:      httpkit.Reply{Status: 303, Location: flow.ReturnTo, Cookies: session.Cookies, CSRF: session.CSRF},
	}, nil
}
