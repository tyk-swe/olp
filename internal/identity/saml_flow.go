package identity

import (
	"context"
	"crypto/hmac"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/crewjam/saml"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/domain/ids"
	"github.com/tyk-swe/olp/internal/domain/problem"
	"github.com/tyk-swe/olp/internal/domain/textfield"
	"github.com/tyk-swe/olp/internal/identity/samlidentity"
	"github.com/tyk-swe/olp/internal/platform/database"
	"github.com/tyk-swe/olp/internal/platform/secrets"
)

const samlFlowTTL = 5 * time.Minute

type samlFacts struct {
	Email           string   `json:"email"`
	Groups          []string `json:"groups"`
	EmailAttribute  string   `json:"email_attribute"`
	GroupsAttribute string   `json:"groups_attribute"`
}

type samlFlow struct {
	ID        string     `json:"id"`
	Kind      string     `json:"kind"`
	ReturnTo  string     `json:"return_to"`
	Purpose   string     `json:"purpose"`
	Resource  string     `json:"resource"`
	UserID    string     `json:"user_id"`
	SessionID string     `json:"session_id"`
	RequestID string     `json:"request_id"`
	ETag      string     `json:"configuration_etag"`
	Expires   time.Time  `json:"expires_at"`
	Subject   string     `json:"subject,omitempty"`
	Name      string     `json:"name,omitempty"`
	Facts     *samlFacts `json:"facts,omitempty"`
}

type preparedSAMLBegin struct {
	kind        string
	input       federatedBeginInput
	config      samlConfiguration
	requestID   string
	state       string
	browser     string
	destination string
}

func (s *samlService) beginSAMLLogin(r *http.Request) (httpkit.Reply, error) {
	prep, err := s.prepareSAMLBegin(r, "login")
	if err != nil {
		return httpkit.Reply{}, err
	}
	return access.Serialize(s.access, r, func(tx pgx.Tx) (httpkit.Reply, bool, error) {
		reply, err := s.storeSAMLFlow(r, tx, prep, nil)
		return reply, err == nil, err
	})
}

func (s *samlService) beginSAMLLink(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	return s.beginSecuredSAML(r, "link")
}

func (s *samlService) beginSAMLReauthentication(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	return s.beginSecuredSAML(r, "reauthenticate")
}

func (s *samlService) beginSecuredSAML(r *http.Request, kind string) (httpkit.Reply, error) {
	prep, err := s.prepareSAMLBegin(r, kind)
	if err != nil {
		return httpkit.Reply{}, err
	}
	return access.Transact(s.access, r, access.Unlocked, func(tx pgx.Tx, p authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		reply, err := s.storeSAMLFlow(r, tx, prep, &p)
		return reply, access.Event{}, err
	})
}

func (s *samlService) prepareSAMLBegin(r *http.Request, kind string) (preparedSAMLBegin, error) {
	if err := admitAuth(s.access, r, "saml_begin", ""); err != nil {
		return preparedSAMLBegin{}, err
	}
	input, err := parseFederatedBeginInput(r, kind, false)
	if err != nil {
		return preparedSAMLBegin{}, err
	}
	c, err := loadSAML(r.Context(), s.access.Pool)
	if err != nil {
		return preparedSAMLBegin{}, err
	}
	if !c.Enabled {
		return preparedSAMLBegin{}, problem.Fail(403, "saml_disabled", "SAML sign-in is disabled.")
	}
	sp, m, err := s.samlProvider(r, s.access.Pool, c)
	if err != nil {
		return preparedSAMLBegin{}, err
	}
	force := kind != "login"
	sp.ForceAuthn = &force
	request, err := sp.MakeAuthenticationRequest(m.RedirectURL, saml.HTTPRedirectBinding, saml.HTTPPostBinding)
	if err != nil {
		return preparedSAMLBegin{}, problem.Fail(503, "saml_unavailable", "SAML sign-in could not be started.")
	}
	state, browser := secrets.Token(), secrets.Token()
	destination, err := request.Redirect(state, sp)
	if err != nil {
		return preparedSAMLBegin{}, problem.Fail(503, "saml_unavailable", "SAML sign-in could not be started.")
	}
	return preparedSAMLBegin{
		kind:        kind,
		input:       input,
		config:      c,
		requestID:   request.ID,
		state:       state,
		browser:     browser,
		destination: destination.String(),
	}, nil
}

func (s *samlService) storeSAMLFlow(r *http.Request, tx pgx.Tx, prep preparedSAMLBegin, principal *authz.Principal) (httpkit.Reply, error) {
	current, err := loadSAML(r.Context(), tx)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if current.ETag != prep.config.ETag || !current.Enabled {
		return httpkit.Reply{}, problem.Fail(409, "saml_configuration_changed", "SAML configuration changed. Start again.")
	}
	flow := samlFlow{
		ID:        ids.New(),
		Kind:      prep.kind,
		ReturnTo:  prep.input.ReturnTo,
		Purpose:   prep.input.Purpose,
		Resource:  prep.input.ResourceID,
		RequestID: prep.requestID,
		ETag:      prep.config.ETag,
		Expires:   time.Now().Add(samlFlowTTL),
	}
	if principal != nil {
		flow.UserID = principal.ID
		flow.SessionID = principal.SessionID
		if prep.kind == "link" {
			if err := s.access.ConsumeRecent(r, tx, *principal, "saml_link", ""); err != nil {
				return httpkit.Reply{}, err
			}
		}
	}
	if err := pruneExpiredSecrets(r.Context(), tx); err != nil {
		return httpkit.Reply{}, err
	}
	raw, err := json.Marshal(flow)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if err := s.access.Keys.Store(r.Context(), tx, s.access.Installation, flow.ID, secrets.SAMLFlow, raw, &flow.Expires); err != nil {
		return httpkit.Reply{}, err
	}
	if _, err := tx.Exec(r.Context(), "INSERT INTO olp.saml_flows(id,state_digest,cookie_digest,configuration_etag,request_id,expires_at) VALUES($1,$2,$3,$4,$5,$6)", flow.ID, s.access.Auth.Digest(secrets.SAMLStateDigest, prep.state), s.access.Auth.Digest(secrets.SAMLCookieDigest, prep.browser), prep.config.ETag, prep.requestID, flow.Expires); err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.Reply{
		Status:  200,
		Body:    authorizationURLResponse{AuthorizationURL: prep.destination},
		Cookies: []*http.Cookie{access.NewCookie("__Host-olp_saml_"+flow.ID, prep.browser, samlFlowTTL, true)},
	}, nil
}

func (s *samlService) readSAMLFlow(r *http.Request, q database.Queryer, state string) (samlFlow, []byte, *time.Time, error) {
	var f samlFlow
	var digest []byte
	var received *time.Time
	if state == "" || len(state) > 256 {
		return f, nil, nil, problem.Fail(403, "saml_flow_invalid", "Start SAML sign-in again.")
	}
	err := q.QueryRow(r.Context(), "SELECT id::text,cookie_digest,received_at FROM olp.saml_flows WHERE state_digest=$1 AND expires_at>clock_timestamp()", s.access.Auth.Digest(secrets.SAMLStateDigest, state)).Scan(&f.ID, &digest, &received)
	if errors.Is(err, pgx.ErrNoRows) {
		return f, nil, nil, problem.Fail(403, "saml_flow_invalid", "Start SAML sign-in again.")
	}
	if err != nil {
		return f, nil, nil, err
	}
	raw, err := s.access.Keys.Read(r.Context(), q, s.access.Installation, f.ID, secrets.SAMLFlow)
	if err != nil {
		return f, nil, nil, err
	}
	defer clear(raw)
	err = json.Unmarshal(raw, &f)
	return f, digest, received, err
}

func (s *samlService) samlACS(r *http.Request) (httpkit.Reply, error) {
	if err := admitAuth(s.access, r, "saml_callback", ""); err != nil {
		return httpkit.Reply{}, err
	}
	if r.URL.RawQuery != "" || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") || r.ParseForm() != nil || len(r.PostForm) != 2 || len(r.PostForm["SAMLResponse"]) != 1 || len(r.PostForm["RelayState"]) != 1 {
		return httpkit.Reply{}, problem.Fail(400, "saml_response_invalid", "Use the configured SAML POST response binding.")
	}
	state := r.PostForm.Get("RelayState")
	flow, _, received, err := s.readSAMLFlow(r, s.access.Pool, state)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if received != nil {
		return httpkit.Reply{}, problem.Fail(403, "saml_flow_invalid", "This SAML response was already received.")
	}
	c, err := loadSAML(r.Context(), s.access.Pool)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if !c.Enabled || c.ETag != flow.ETag {
		return httpkit.Reply{}, problem.Fail(403, "saml_flow_invalid", "The SAML configuration changed.")
	}
	assertion, verifiedFlow, err := s.verifySAMLResponse(r, c, flow)
	if err != nil {
		return httpkit.Reply{}, err
	}
	return s.recordSAMLAssertion(r, c, verifiedFlow, assertion, state)
}

func (s *samlService) verifySAMLResponse(r *http.Request, c samlConfiguration, flow samlFlow) (*saml.Assertion, samlFlow, error) {
	sp, m, err := s.samlProvider(r, s.access.Pool, c)
	if err != nil {
		return nil, samlFlow{}, err
	}
	raw, err := base64.StdEncoding.DecodeString(r.PostForm.Get("SAMLResponse"))
	if err != nil {
		return nil, samlFlow{}, problem.Fail(403, "saml_assertion_invalid", "The SAML response could not be verified.")
	}
	assertion, err := samlidentity.Verify(sp, m, raw, flow.RequestID, time.Now())
	clear(raw)
	if err != nil {
		return nil, samlFlow{}, problem.Fail(403, "saml_assertion_invalid", "The SAML response could not be verified.")
	}
	if flow.Kind != "login" && (assertion.AuthnStatements[0].AuthnInstant.Before(flow.Expires.Add(-samlFlowTTL-samlidentity.Skew)) || assertion.AuthnStatements[0].AuthnInstant.After(time.Now().Add(samlidentity.Skew))) {
		return nil, samlFlow{}, problem.Fail(403, "saml_authentication_not_recent", "Authenticate again at the identity provider.")
	}
	attrs, err := samlidentity.Attributes(assertion)
	if err != nil || len(attrs[c.EmailAttribute]) != 1 {
		return nil, samlFlow{}, problem.Fail(403, "saml_claims_invalid", "The SAML identity lacks the configured email attribute.")
	}
	address, err := email(attrs[c.EmailAttribute][0])
	if err != nil {
		return nil, samlFlow{}, problem.Fail(403, "saml_claims_invalid", "The SAML email attribute is invalid.")
	}
	groups := attrs[c.GroupsAttribute]
	if groups == nil {
		groups = []string{}
	}
	name := address
	if names := attrs[c.NameAttribute]; len(names) == 1 && textfield.Validate("display_name", names[0], 100) == nil {
		name = names[0]
	}
	verifiedFlow := flow
	verifiedFlow.Facts = &samlFacts{Email: address, Groups: groups, EmailAttribute: c.EmailAttribute, GroupsAttribute: c.GroupsAttribute}
	verifiedFlow.Subject = assertion.Subject.NameID.Value
	verifiedFlow.Name = name
	return assertion, verifiedFlow, nil
}

func clampSAMLExpiry(initial time.Time, assertion *saml.Assertion) time.Time {
	expires := initial
	for _, end := range []time.Time{assertion.Conditions.NotOnOrAfter, assertion.Subject.SubjectConfirmations[0].SubjectConfirmationData.NotOnOrAfter} {
		if deadline := end.Add(samlidentity.Skew); deadline.Before(expires) {
			expires = deadline
		}
	}
	if end := assertion.AuthnStatements[0].SessionNotOnOrAfter; end != nil && end.Add(samlidentity.Skew).Before(expires) {
		expires = end.Add(samlidentity.Skew)
	}
	return expires
}

func (s *samlService) recordSAMLAssertion(r *http.Request, c samlConfiguration, flow samlFlow, assertion *saml.Assertion, state string) (httpkit.Reply, error) {
	return access.Serialize(s.access, r, func(tx pgx.Tx) (httpkit.Reply, bool, error) {
		current, err := loadSAML(r.Context(), tx)
		if err != nil {
			return httpkit.Reply{}, false, err
		}
		if !current.Enabled || current.ETag != flow.ETag {
			return httpkit.Reply{}, false, problem.Fail(403, "saml_flow_invalid", "The SAML configuration changed.")
		}
		if _, err = tx.Exec(r.Context(), "DELETE FROM olp.saml_assertion_replays WHERE digest IN (SELECT digest FROM olp.saml_assertion_replays WHERE expires_at<=now() LIMIT $1)", expiredCleanupLimit); err != nil {
			return httpkit.Reply{}, false, err
		}
		replayInput, err := json.Marshal([]string{c.EntityID, assertion.ID})
		if err != nil {
			return httpkit.Reply{}, false, err
		}
		result, err := tx.Exec(r.Context(), "INSERT INTO olp.saml_assertion_replays(digest,expires_at) VALUES($1,$2) ON CONFLICT DO NOTHING", s.access.Auth.Digest(secrets.SAMLAssertionDigest, string(replayInput)), assertion.Conditions.NotOnOrAfter.Add(samlidentity.Skew))
		if err != nil {
			return httpkit.Reply{}, false, err
		}
		if result.RowsAffected() != 1 {
			return httpkit.Reply{}, false, problem.Fail(403, "saml_assertion_replayed", "Start SAML sign-in again.")
		}
		updated := flow
		updated.Expires = clampSAMLExpiry(flow.Expires, assertion)
		result, err = tx.Exec(r.Context(), "UPDATE olp.saml_flows SET received_at=now(),expires_at=$2 WHERE id=$1 AND received_at IS NULL AND expires_at>clock_timestamp()", updated.ID, updated.Expires)
		if err != nil {
			return httpkit.Reply{}, false, err
		}
		if result.RowsAffected() != 1 {
			return httpkit.Reply{}, false, problem.Fail(403, "saml_flow_invalid", "Start SAML sign-in again.")
		}
		data, err := json.Marshal(updated)
		if err != nil {
			return httpkit.Reply{}, false, err
		}
		defer clear(data)
		if err = s.access.Keys.Store(r.Context(), tx, s.access.Installation, updated.ID, secrets.SAMLFlow, data, &updated.Expires); err != nil {
			return httpkit.Reply{}, false, err
		}
		return httpkit.Reply{Status: 303, Location: "/api/v1/saml/complete?state=" + url.QueryEscape(state)}, true, nil
	})
}

func (s *samlService) completeSAML(r *http.Request) (httpkit.Reply, error) {
	if len(r.URL.Query()["state"]) != 1 {
		return httpkit.Reply{}, problem.Fail(403, "saml_flow_invalid", "Start SAML sign-in again.")
	}
	return access.Serialize(s.access, r, func(tx pgx.Tx) (httpkit.Reply, bool, error) {
		flow, digest, received, err := s.readSAMLFlow(r, tx, r.URL.Query().Get("state"))
		if err != nil {
			return httpkit.Reply{}, false, err
		}
		if received == nil || flow.Facts == nil || !hmac.Equal(digest, s.access.Auth.Digest(secrets.SAMLCookieDigest, access.CookieValue(r, "__Host-olp_saml_"+flow.ID))) {
			return httpkit.Reply{}, false, problem.Fail(403, "saml_flow_invalid", "Use the browser that started SAML sign-in.")
		}
		c, err := loadSAML(r.Context(), tx)
		if err != nil {
			return httpkit.Reply{}, false, err
		}
		if !c.Enabled || c.ETag != flow.ETag {
			return httpkit.Reply{}, false, problem.Fail(403, "saml_flow_invalid", "The SAML configuration changed.")
		}
		consumed, err := tx.Exec(r.Context(), "DELETE FROM olp.secrets WHERE id=$1", flow.ID)
		if err != nil {
			return httpkit.Reply{}, false, err
		}
		if consumed.RowsAffected() != 1 {
			return httpkit.Reply{}, false, problem.Fail(403, "saml_flow_invalid", "Start SAML sign-in again.")
		}
		response, err := s.acceptSAML(r, tx, c, flow)
		if err != nil {
			var problem *problem.Problem
			commitDeauth := errors.As(err, &problem) && problem.Code == "saml_provisioning_denied"
			return httpkit.Reply{}, commitDeauth, err
		}
		response.Cookies = append(response.Cookies, access.ClearCookie("__Host-olp_saml_"+flow.ID))
		return response, true, nil
	})
}

func (s *samlService) acceptSAML(r *http.Request, tx pgx.Tx, c samlConfiguration, flow samlFlow) (httpkit.Reply, error) {
	userID, identityID, p, err := s.resolveSAMLIdentity(r, tx, c, flow)
	if err != nil {
		return httpkit.Reply{}, err
	}
	changed, allowed, err := syncFederatedAuthority(r, tx, userID, samlMappedRole(c, flow.Facts.Email, flow.Facts.Groups), "saml")
	if err != nil {
		return httpkit.Reply{}, err
	}
	if !allowed {
		return httpkit.Reply{}, problem.Fail(403, "saml_provisioning_denied", "No role mapping authorizes this identity.")
	}
	if changed && flow.Kind == "reauthenticate" {
		return httpkit.Reply{}, problem.Fail(403, "saml_provisioning_denied", "Your access changed; sign in again.")
	}
	var active bool
	if err = tx.QueryRow(r.Context(), "SELECT active AND oidc_authorized FROM olp.users WHERE id=$1", userID).Scan(&active); err != nil {
		return httpkit.Reply{}, err
	}
	if !active {
		return httpkit.Reply{}, problem.Fail(403, "account_disabled", "This account is disabled.")
	}
	response, err := s.finishSAMLOutcome(r, tx, flow, p, userID)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if err = access.Audit(r.Context(), tx, r, authz.UserActor(userID), "saml."+flow.Kind, "saml_identity", identityID, "success"); err != nil {
		return httpkit.Reply{}, err
	}
	return response, nil
}

func provisionSAMLUser(ctx context.Context, tx pgx.Tx, c samlConfiguration, flow samlFlow) (string, error) {
	var complete, existing bool
	if err := tx.QueryRow(ctx, "SELECT setup_complete,EXISTS(SELECT 1 FROM olp.users WHERE email=$1) FROM olp.installation WHERE singleton", flow.Facts.Email).Scan(&complete, &existing); err != nil {
		return "", err
	}
	if !complete {
		return "", problem.Fail(403, "setup_required", "Complete owner setup before SAML sign-in.")
	}
	if existing {
		return "", problem.Fail(409, "saml_link_required", "Sign in to the existing account and explicitly link this identity.")
	}
	role := samlMappedRole(c, flow.Facts.Email, flow.Facts.Groups)
	if role == "" {
		return "", problem.Fail(403, "saml_provisioning_denied", "No role mapping authorizes this identity.")
	}
	userID := ids.New()
	if _, err := tx.Exec(ctx, "INSERT INTO olp.users(id,email,display_name,role,etag,role_management) VALUES($1,$2,$3,$4,$5,'saml')", userID, flow.Facts.Email, flow.Name, role, ids.New()); err != nil {
		return "", err
	}
	return userID, nil
}

func upsertSAMLIdentity(ctx context.Context, tx pgx.Tx, c samlConfiguration, flow samlFlow, userID, existingIdentityID string) (string, error) {
	facts, err := json.Marshal(flow.Facts)
	if err != nil {
		return "", err
	}
	if existingIdentityID != "" {
		if _, err := tx.Exec(ctx, "UPDATE olp.saml_identities SET role_claims=$2,last_login_at=now() WHERE id=$1", existingIdentityID, facts); err != nil {
			return "", err
		}
		return existingIdentityID, nil
	}
	identityID := ids.New()
	if _, err := tx.Exec(ctx, "INSERT INTO olp.saml_identities(id,user_id,issuer,subject,email_at_link,role_claims,last_login_at) VALUES($1,$2,$3,$4,$5,$6,now())", identityID, userID, c.EntityID, flow.Subject, flow.Facts.Email, facts); err != nil {
		return "", err
	}
	return identityID, nil
}

func (s *samlService) resolveSAMLIdentity(r *http.Request, tx pgx.Tx, c samlConfiguration, flow samlFlow) (string, string, authz.Principal, error) {
	var userID, identityID string
	err := tx.QueryRow(r.Context(), "SELECT user_id::text,id::text FROM olp.saml_identities WHERE issuer=$1 AND subject=$2", c.EntityID, flow.Subject).Scan(&userID, &identityID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", "", authz.Principal{}, err
	}
	var p authz.Principal
	if flow.Kind != "login" {
		p, err = authenticateFlowSession(s.access, r, tx, flow.UserID, flow.SessionID, "saml_session_changed", "Your browser session changed during sign-in.")
		if err != nil {
			return "", "", authz.Principal{}, err
		}
		if flow.Kind == "reauthenticate" && p.ID != userID {
			return "", "", authz.Principal{}, problem.Fail(403, "saml_identity_mismatch", "Use an identity linked to your current account.")
		}
	}
	if flow.Kind == "link" {
		if identityID != "" {
			return "", "", authz.Principal{}, problem.Fail(409, "saml_identity_linked", "The SAML identity is already linked.")
		}
		userID = p.ID
	}
	if userID == "" {
		userID, err = provisionSAMLUser(r.Context(), tx, c, flow)
		if err != nil {
			return "", "", authz.Principal{}, err
		}
	}
	resolvedIdentityID, err := upsertSAMLIdentity(r.Context(), tx, c, flow, userID, identityID)
	if err != nil {
		return "", "", authz.Principal{}, err
	}
	return userID, resolvedIdentityID, p, nil
}

func (s *samlService) finishSAMLOutcome(r *http.Request, tx pgx.Tx, flow samlFlow, p authz.Principal, userID string) (httpkit.Reply, error) {
	if flow.Kind == "reauthenticate" {
		grant, err := s.sessions.grantRecent(r, tx, p, flow.Purpose, flow.Resource, false)
		if err != nil {
			return httpkit.Reply{}, err
		}
		return httpkit.Reply{Status: 303, Location: reauthenticatedLocation(flow.Purpose, flow.Resource), Cookies: grant.Cookies}, nil
	}
	var session httpkit.Reply
	var err error
	if flow.Kind == "link" {
		session, err = s.rotateUserSession(r, tx, userID, sessionAuth{Method: "saml"})
	} else {
		session, err = s.sessions.newSession(r, tx, userID, sessionAuth{Method: "saml"})
	}
	if err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.Reply{Status: 303, Location: flow.ReturnTo, Cookies: session.Cookies, CSRF: session.CSRF}, nil
}
