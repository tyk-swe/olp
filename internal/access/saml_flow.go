package access

import (
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
	"github.com/tyk-swe/olp/internal/samlidentity"
	"github.com/tyk-swe/olp/internal/secrets"
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

func (s *Server) beginSAMLLogin(r *http.Request) (Reply, error) { return s.beginSAML(r, "login") }

func (s *Server) beginSAMLLink(r *http.Request, _ Principal) (Reply, error) {
	return s.beginSAML(r, "link")
}

func (s *Server) beginSAMLReauthentication(r *http.Request, _ Principal) (Reply, error) {
	return s.beginSAML(r, "reauthenticate")
}

func (s *Server) beginSAML(r *http.Request, kind string) (Reply, error) {
	if err := s.admit(r, "saml_begin", ""); err != nil {
		return Reply{}, err
	}
	var input struct {
		ReturnTo string `json:"return_to"`
		Purpose  string `json:"purpose"`
		Resource string `json:"resource_id"`
	}
	if r.ContentLength != 0 {
		if err := Decode(r, &input); err != nil {
			return Reply{}, err
		}
	}
	if !safeReturn(input.ReturnTo) || len(input.ReturnTo) > 2048 {
		return Reply{}, Invalid("return_to", "Use a local console destination.")
	}
	if input.ReturnTo == "" {
		input.ReturnTo = "/"
		if kind == "link" {
			input.ReturnTo = "/settings/profile"
		}
	}
	if kind == "reauthenticate" {
		if err := validatePurpose(input.Purpose, input.Resource); err != nil {
			return Reply{}, err
		}
	} else if input.Purpose != "" || input.Resource != "" {
		return Reply{}, Invalid("purpose", "This flow has no recent-authentication purpose.")
	}
	c, err := loadSAML(r, s.Pool)
	if err != nil {
		return Reply{}, err
	}
	if !c.Enabled {
		return Reply{}, Fail(403, "saml_disabled", "SAML sign-in is disabled.")
	}
	sp, m, err := s.samlProvider(r, s.Pool, c)
	if err != nil {
		return Reply{}, err
	}
	force := kind != "login"
	sp.ForceAuthn = &force
	request, err := sp.MakeAuthenticationRequest(m.RedirectURL, saml.HTTPRedirectBinding, saml.HTTPPostBinding)
	if err != nil {
		return Reply{}, Fail(503, "saml_unavailable", "SAML sign-in could not be started.")
	}
	state, browser := secrets.Token(), secrets.Token()
	destination, err := request.Redirect(state, sp)
	if err != nil {
		return Reply{}, Fail(503, "saml_unavailable", "SAML sign-in could not be started.")
	}
	tx, err := s.Begin(r)
	if err != nil {
		return Reply{}, err
	}
	defer tx.Rollback(r.Context())
	current, err := loadSAML(r, tx)
	if err != nil {
		return Reply{}, err
	}
	if current.ETag != c.ETag || !current.Enabled {
		return Reply{}, Fail(409, "saml_configuration_changed", "SAML configuration changed. Start again.")
	}
	flow := samlFlow{ID: NewID(), Kind: kind, ReturnTo: input.ReturnTo, Purpose: input.Purpose, Resource: input.Resource, RequestID: request.ID, ETag: c.ETag, Expires: time.Now().Add(samlFlowTTL)}
	if kind != "login" {
		p, e := s.Reauthorize(r, tx)
		if e != nil {
			return Reply{}, e
		}
		flow.UserID = p.ID
		flow.SessionID = p.SessionID
		if kind == "link" {
			if e = s.ConsumeRecent(r, tx, p, "saml_link", ""); e != nil {
				return Reply{}, e
			}
		}
	}
	if _, err = tx.Exec(r.Context(), "DELETE FROM olp.secrets WHERE expires_at<=now()"); err != nil {
		return Reply{}, err
	}
	raw, err := json.Marshal(flow)
	if err != nil {
		return Reply{}, err
	}
	if err = s.Keys.Store(r.Context(), tx, s.Installation, flow.ID, secrets.SAMLFlow, raw, &flow.Expires); err != nil {
		return Reply{}, err
	}
	if _, err = tx.Exec(r.Context(), "INSERT INTO olp.saml_flows(id,state_digest,cookie_digest,configuration_etag,request_id,expires_at) VALUES($1,$2,$3,$4,$5,$6)", flow.ID, s.Auth.Digest(secrets.SAMLStateDigest, state), s.Auth.Digest(secrets.SAMLCookieDigest, browser), c.ETag, request.ID, flow.Expires); err != nil {
		return Reply{}, err
	}
	return Commit(r, tx, Reply{Status: 200, Body: map[string]any{"authorization_url": destination.String()}, Cookies: []*http.Cookie{cookie("__Host-olp_saml_"+flow.ID, browser, samlFlowTTL, true)}})
}

func (s *Server) readSAMLFlow(r *http.Request, q Queryer, state string) (samlFlow, []byte, *time.Time, error) {
	var f samlFlow
	var digest []byte
	var received *time.Time
	if state == "" || len(state) > 256 {
		return f, nil, nil, Fail(403, "saml_flow_invalid", "Start SAML sign-in again.")
	}
	err := q.QueryRow(r.Context(), "SELECT id::text,cookie_digest,received_at FROM olp.saml_flows WHERE state_digest=$1 AND expires_at>clock_timestamp()", s.Auth.Digest(secrets.SAMLStateDigest, state)).Scan(&f.ID, &digest, &received)
	if errors.Is(err, pgx.ErrNoRows) {
		return f, nil, nil, Fail(403, "saml_flow_invalid", "Start SAML sign-in again.")
	}
	if err != nil {
		return f, nil, nil, err
	}
	raw, err := s.Keys.Read(r.Context(), q, s.Installation, f.ID, secrets.SAMLFlow)
	if err != nil {
		return f, nil, nil, err
	}
	defer clear(raw)
	err = json.Unmarshal(raw, &f)
	return f, digest, received, err
}

func (s *Server) samlACS(r *http.Request) (Reply, error) {
	if err := s.admit(r, "saml_callback", ""); err != nil {
		return Reply{}, err
	}
	if r.URL.RawQuery != "" || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") || r.ParseForm() != nil || len(r.PostForm) != 2 || len(r.PostForm["SAMLResponse"]) != 1 || len(r.PostForm["RelayState"]) != 1 {
		return Reply{}, Fail(400, "saml_response_invalid", "Use the configured SAML POST response binding.")
	}
	state := r.PostForm.Get("RelayState")
	flow, _, received, err := s.readSAMLFlow(r, s.Pool, state)
	if err != nil {
		return Reply{}, err
	}
	if received != nil {
		return Reply{}, Fail(403, "saml_flow_invalid", "This SAML response was already received.")
	}
	c, err := loadSAML(r, s.Pool)
	if err != nil {
		return Reply{}, err
	}
	if !c.Enabled || c.ETag != flow.ETag {
		return Reply{}, Fail(403, "saml_flow_invalid", "The SAML configuration changed.")
	}
	sp, m, err := s.samlProvider(r, s.Pool, c)
	if err != nil {
		return Reply{}, err
	}
	raw, err := base64.StdEncoding.DecodeString(r.PostForm.Get("SAMLResponse"))
	if err != nil {
		return Reply{}, Fail(403, "saml_assertion_invalid", "The SAML response could not be verified.")
	}
	assertion, err := samlidentity.Verify(sp, m, raw, flow.RequestID, time.Now())
	clear(raw)
	if err != nil {
		return Reply{}, Fail(403, "saml_assertion_invalid", "The SAML response could not be verified.")
	}
	if flow.Kind != "login" && (assertion.AuthnStatements[0].AuthnInstant.Before(flow.Expires.Add(-samlFlowTTL-samlidentity.Skew)) || assertion.AuthnStatements[0].AuthnInstant.After(time.Now().Add(samlidentity.Skew))) {
		return Reply{}, Fail(403, "saml_authentication_not_recent", "Authenticate again at the identity provider.")
	}
	attrs, err := samlidentity.Attributes(assertion)
	if err != nil || len(attrs[c.EmailAttribute]) != 1 {
		return Reply{}, Fail(403, "saml_claims_invalid", "The SAML identity lacks the configured email attribute.")
	}
	email, err := email(attrs[c.EmailAttribute][0])
	if err != nil {
		return Reply{}, Fail(403, "saml_claims_invalid", "The SAML email attribute is invalid.")
	}
	groups := attrs[c.GroupsAttribute]
	if groups == nil {
		groups = []string{}
	}
	flow.Facts = &samlFacts{Email: email, Groups: groups, EmailAttribute: c.EmailAttribute, GroupsAttribute: c.GroupsAttribute}
	flow.Subject = assertion.Subject.NameID.Value
	flow.Name = email
	if names := attrs[c.NameAttribute]; len(names) == 1 && ValidText("display_name", names[0], 100) == nil {
		flow.Name = names[0]
	}
	tx, err := s.Begin(r)
	if err != nil {
		return Reply{}, err
	}
	defer tx.Rollback(r.Context())
	current, err := loadSAML(r, tx)
	if err != nil {
		return Reply{}, err
	}
	if !current.Enabled || current.ETag != flow.ETag {
		return Reply{}, Fail(403, "saml_flow_invalid", "The SAML configuration changed.")
	}
	if _, err = tx.Exec(r.Context(), "DELETE FROM olp.saml_assertion_replays WHERE expires_at<=now()"); err != nil {
		return Reply{}, err
	}
	replayInput, _ := json.Marshal([]string{c.EntityID, assertion.ID})
	result, err := tx.Exec(r.Context(), "INSERT INTO olp.saml_assertion_replays(digest,expires_at) VALUES($1,$2) ON CONFLICT DO NOTHING", s.Auth.Digest(secrets.SAMLAssertionDigest, string(replayInput)), assertion.Conditions.NotOnOrAfter.Add(samlidentity.Skew))
	if err != nil {
		return Reply{}, err
	}
	if result.RowsAffected() != 1 {
		return Reply{}, Fail(403, "saml_assertion_replayed", "Start SAML sign-in again.")
	}
	// The browser-bound completion cannot extend the verified assertion's life.
	for _, end := range []time.Time{assertion.Conditions.NotOnOrAfter, assertion.Subject.SubjectConfirmations[0].SubjectConfirmationData.NotOnOrAfter} {
		if deadline := end.Add(samlidentity.Skew); deadline.Before(flow.Expires) {
			flow.Expires = deadline
		}
	}
	if end := assertion.AuthnStatements[0].SessionNotOnOrAfter; end != nil && end.Add(samlidentity.Skew).Before(flow.Expires) {
		flow.Expires = end.Add(samlidentity.Skew)
	}
	result, err = tx.Exec(r.Context(), "UPDATE olp.saml_flows SET received_at=now(),expires_at=$2 WHERE id=$1 AND received_at IS NULL AND expires_at>clock_timestamp()", flow.ID, flow.Expires)
	if err != nil {
		return Reply{}, err
	}
	if result.RowsAffected() != 1 {
		return Reply{}, Fail(403, "saml_flow_invalid", "Start SAML sign-in again.")
	}
	data, err := json.Marshal(flow)
	if err != nil {
		return Reply{}, err
	}
	defer clear(data)
	if err = s.Keys.Store(r.Context(), tx, s.Installation, flow.ID, secrets.SAMLFlow, data, &flow.Expires); err != nil {
		return Reply{}, err
	}
	return Commit(r, tx, Reply{Status: 303, Location: "/api/v1/saml/complete?state=" + url.QueryEscape(state)})
}

func (s *Server) completeSAML(r *http.Request) (Reply, error) {
	if len(r.URL.Query()["state"]) != 1 {
		return Reply{}, Fail(403, "saml_flow_invalid", "Start SAML sign-in again.")
	}
	tx, err := s.Begin(r)
	if err != nil {
		return Reply{}, err
	}
	defer tx.Rollback(r.Context())
	flow, digest, received, err := s.readSAMLFlow(r, tx, r.URL.Query().Get("state"))
	if err != nil {
		return Reply{}, err
	}
	if received == nil || flow.Facts == nil || !hmac.Equal(digest, s.Auth.Digest(secrets.SAMLCookieDigest, cookieValue(r, "__Host-olp_saml_"+flow.ID))) {
		return Reply{}, Fail(403, "saml_flow_invalid", "Use the browser that started SAML sign-in.")
	}
	c, err := loadSAML(r, tx)
	if err != nil {
		return Reply{}, err
	}
	if !c.Enabled || c.ETag != flow.ETag {
		return Reply{}, Fail(403, "saml_flow_invalid", "The SAML configuration changed.")
	}
	if _, err = tx.Exec(r.Context(), "DELETE FROM olp.secrets WHERE id=$1", flow.ID); err != nil {
		return Reply{}, err
	}
	response, err := s.acceptSAML(r, tx, c, flow)
	if err != nil {
		var problem *Problem
		if errors.As(err, &problem) && problem.Code == "saml_provisioning_denied" {
			if e := tx.Commit(r.Context()); e != nil {
				return Reply{}, e
			}
		}
		return Reply{}, err
	}
	response.Cookies = append(response.Cookies, clearCookie("__Host-olp_saml_"+flow.ID))
	return Commit(r, tx, response)
}

func (s *Server) acceptSAML(r *http.Request, tx pgx.Tx, c samlConfiguration, flow samlFlow) (Reply, error) {
	var userID, identityID string
	err := tx.QueryRow(r.Context(), "SELECT user_id::text,id::text FROM olp.saml_identities WHERE issuer=$1 AND subject=$2", c.EntityID, flow.Subject).Scan(&userID, &identityID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Reply{}, err
	}
	var p Principal
	if flow.Kind != "login" {
		p, err = s.Authenticate(r, tx)
		if err != nil {
			return Reply{}, err
		}
		if err = p.Authorize(Self); err != nil {
			return Reply{}, err
		}
		if p.ID != flow.UserID || p.SessionID != flow.SessionID {
			return Reply{}, Fail(403, "saml_session_changed", "Your browser session changed during sign-in.")
		}
		if flow.Kind == "reauthenticate" && p.ID != userID {
			return Reply{}, Fail(403, "saml_identity_mismatch", "Use an identity linked to your current account.")
		}
	}
	response := Reply{Status: 303, Location: flow.ReturnTo}
	if flow.Kind == "link" {
		if identityID != "" {
			return Reply{}, Fail(409, "saml_identity_linked", "The SAML identity is already linked.")
		}
		userID = p.ID
	}
	if userID == "" {
		var complete, existing bool
		if err = tx.QueryRow(r.Context(), "SELECT setup_complete,EXISTS(SELECT 1 FROM olp.users WHERE email=$1) FROM olp.installation WHERE singleton", flow.Facts.Email).Scan(&complete, &existing); err != nil {
			return Reply{}, err
		}
		if !complete {
			return Reply{}, Fail(403, "setup_required", "Complete owner setup before SAML sign-in.")
		}
		if existing {
			return Reply{}, Fail(409, "saml_link_required", "Sign in to the existing account and explicitly link this identity.")
		}
		role := samlMappedRole(c, flow.Facts.Email, flow.Facts.Groups)
		if role == "" {
			return Reply{}, Fail(403, "saml_provisioning_denied", "No role mapping authorizes this identity.")
		}
		userID = NewID()
		if _, err = tx.Exec(r.Context(), "INSERT INTO olp.users(id,email,display_name,role,etag,role_management) VALUES($1,$2,$3,$4,$5,'saml')", userID, flow.Facts.Email, flow.Name, role, NewID()); err != nil {
			return Reply{}, err
		}
	}
	facts, err := json.Marshal(flow.Facts)
	if err != nil {
		return Reply{}, err
	}
	if identityID == "" {
		identityID = NewID()
		if _, err = tx.Exec(r.Context(), "INSERT INTO olp.saml_identities(id,user_id,issuer,subject,email_at_link,role_claims,last_login_at) VALUES($1,$2,$3,$4,$5,$6,now())", identityID, userID, c.EntityID, flow.Subject, flow.Facts.Email, facts); err != nil {
			return Reply{}, err
		}
	} else {
		if _, err = tx.Exec(r.Context(), "UPDATE olp.saml_identities SET role_claims=$2,last_login_at=now() WHERE id=$1", identityID, facts); err != nil {
			return Reply{}, err
		}
	}
	changed, allowed, err := syncFederatedAuthority(r, tx, userID, samlMappedRole(c, flow.Facts.Email, flow.Facts.Groups), "saml")
	if err != nil {
		return Reply{}, err
	}
	if !allowed {
		return Reply{}, Fail(403, "saml_provisioning_denied", "No role mapping authorizes this identity.")
	}
	if changed && flow.Kind == "reauthenticate" {
		return Reply{}, Fail(403, "saml_provisioning_denied", "Your access changed; sign in again.")
	}
	var active bool
	if err = tx.QueryRow(r.Context(), "SELECT active AND oidc_authorized FROM olp.users WHERE id=$1", userID).Scan(&active); err != nil {
		return Reply{}, err
	}
	if !active {
		return Reply{}, Fail(403, "account_disabled", "This account is disabled.")
	}
	if flow.Kind == "reauthenticate" {
		grant, e := s.grantRecent(r, tx, p, flow.Purpose, flow.Resource, false)
		if e != nil {
			return Reply{}, e
		}
		response.Cookies = grant.Cookies
		response.Location = reauthenticatedAt(flow.Purpose) + "?reauthenticated=" + url.QueryEscape(flow.Purpose)
		if flow.Resource != "" {
			response.Location += "&resource_id=" + url.QueryEscape(flow.Resource)
		}
	} else {
		if flow.Kind == "link" {
			if _, err = tx.Exec(r.Context(), "UPDATE olp.users SET etag=$2,updated_at=now() WHERE id=$1", userID, NewID()); err != nil {
				return Reply{}, err
			}
			if _, err = tx.Exec(r.Context(), "DELETE FROM olp.sessions WHERE user_id=$1", userID); err != nil {
				return Reply{}, err
			}
		}
		session, e := s.newSession(r, tx, userID, sessionAuth{Method: "saml"})
		if e != nil {
			return Reply{}, e
		}
		response.Cookies = session.Cookies
		response.CSRF = session.CSRF
	}
	if err = Audit(r.Context(), tx, r, UserActor(userID), "saml."+flow.Kind, "saml_identity", identityID, "success"); err != nil {
		return Reply{}, err
	}
	return response, nil
}
