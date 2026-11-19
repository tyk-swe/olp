package access

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/authz"
	"github.com/tyk-swe/olp/internal/database"
	"github.com/tyk-swe/olp/internal/secrets"
)

func (s *Server) Register(mux *http.ServeMux) {
	s.registerSCIM(mux)
	s.Public(mux, "POST /api/v1/saml/login", s.beginSAMLLogin)
	s.Public(mux, "POST /api/v1/saml/acs", s.samlACS, MaxBody(2<<20))
	s.Public(mux, "GET /api/v1/saml/complete", s.completeSAML)
	s.Route(mux, "GET /api/v1/saml/configuration", s.samlConfiguration)
	s.Route(mux, "PUT /api/v1/saml/configuration", s.putSAMLConfiguration, MaxBody(2<<20))
	s.Route(mux, "POST /api/v1/saml/import", s.importSAMLMetadata)
	s.Stream(mux, "GET /api/v1/saml/metadata", s.samlMetadata)
	s.Route(mux, "GET /api/v1/profile/saml-identities", s.samlIdentities)
	s.Route(mux, "POST /api/v1/profile/saml/link", s.beginSAMLLink)
	s.Route(mux, "POST /api/v1/profile/saml/reauthenticate", s.beginSAMLReauthentication)
	s.Route(mux, "DELETE /api/v1/profile/saml-identities/{identity_id}", s.unlinkSAML)
	s.Route(mux, "GET /api/v1/scim/groups", s.scimGroups)
	s.Route(mux, "PUT /api/v1/scim/groups/{scim_id}/mapping", s.scimGroupMapping)
	public := map[string]PublicHandler{
		"GET /api/v1/setup/status": s.setupStatus, "POST /api/v1/setup": s.setup,
		"GET /api/v1/auth/capabilities":   s.capabilities,
		"POST /api/v1/auth/mfa/verify":    s.mfaVerify,
		"POST /api/v1/auth/mfa/enroll":    s.mfaBootstrap,
		"POST /api/v1/sessions":           s.login,
		"POST /api/v1/invitations/accept": s.acceptInvitation,
		"GET /api/v1/oidc/login":          s.beginOIDCLogin, "POST /api/v1/oidc/login": s.beginOIDCLogin,
		"GET /api/v1/oidc/callback": s.oidcCallback,
	}
	for pattern, fn := range public {
		s.Public(mux, pattern, fn)
	}
	routes := map[string]Handler{
		"GET /api/v1/sessions":         s.sessions,
		"GET /api/v1/sessions/current": s.currentSession, "DELETE /api/v1/sessions/current": s.logout,
		"DELETE /api/v1/sessions/{session_id}": s.revokeSession,
		"GET /api/v1/users":                    s.users, "GET /api/v1/users/{user_id}": s.user, "PATCH /api/v1/users/{user_id}": s.updateUser,
		"GET /api/v1/invitations": s.invitations, "POST /api/v1/invitations": s.createInvitation,
		"DELETE /api/v1/invitations/{invitation_id}":     s.retireInvitation,
		"GET /api/v1/profile/mfa":                        s.mfaStatus,
		"POST /api/v1/profile/mfa/challenge":             s.mfaManageChallenge,
		"POST /api/v1/profile/mfa/enroll":                s.mfaEnroll,
		"POST /api/v1/profile/mfa/recovery-codes":        s.mfaRecovery,
		"DELETE /api/v1/profile/mfa/factors/{factor_id}": s.mfaRemove,
		"GET /api/v1/profile":                            s.profile, "PATCH /api/v1/profile": s.updateProfile,
		"POST /api/v1/profile/password": s.changePassword, "POST /api/v1/profile/password/enroll": s.enrollPassword,
		"POST /api/v1/profile/reauthenticate": s.reauthenticate,

		"GET /api/v1/management-tokens": s.managementTokens, "POST /api/v1/management-tokens": s.createManagementToken,
		"GET /api/v1/management-tokens/{management_token_id}":         s.managementToken,
		"POST /api/v1/management-tokens/{management_token_id}/revoke": s.revokeManagementToken,
		"PUT /api/v1/provisioning/{source}/users/{external_id}":       s.provisionUser,
		"DELETE /api/v1/provisioning/{source}/users/{external_id}":    s.deprovisionUser,
		"GET /api/v1/oidc/configuration":                              s.oidcConfiguration, "PUT /api/v1/oidc/configuration": s.putOIDCConfiguration,
		"POST /api/v1/oidc/link": s.beginOIDCLink, "POST /api/v1/oidc/reauthenticate": s.beginOIDCReauthentication,
		"GET /api/v1/oidc/identities":                  s.oidcIdentities,
		"DELETE /api/v1/oidc/identities/{identity_id}": s.unlinkOIDCIdentity,
	}
	for pattern, fn := range routes {
		s.Route(mux, pattern, fn)
	}
}

func (s *Server) setupStatus(r *http.Request) (httpkit.Reply, error) {
	var complete bool
	err := s.Pool.QueryRow(r.Context(), "SELECT setup_complete FROM olp.installation WHERE singleton").Scan(&complete)
	_, configured := s.Bootstrap()
	return httpkit.OK(map[string]bool{"setup_required": !complete, "bootstrap_token_configured": configured}), err
}
func (s *Server) capabilities(r *http.Request) (httpkit.Reply, error) {
	var local, oidc, samlEnabled, captureEnabled bool
	err := s.Pool.QueryRow(r.Context(), authenticationCapabilitiesSQL).Scan(&local, &oidc, &samlEnabled, &captureEnabled)
	return httpkit.OK(map[string]bool{"local_login_enabled": local && !s.localLoginDisabled, "oidc_login_enabled": oidc, "saml_login_enabled": samlEnabled, "gateway_available": true, "limits_enforced": s.limitsEnforced, "management_network_restricted": s.managementNetworkRestricted, "retention_enforced": s.limitsEnforced, "notifications_active": true, "payload_capture_active": captureEnabled, "response_cache_configured": s.responseCacheConfigured, "semantic_cache_vectors_unsealed": true}), err
}

func (s *Server) passwordWork(r *http.Request, work func()) error {
	select {
	case s.passwordSlots <- struct{}{}:
		defer func() { <-s.passwordSlots }()
		work()
		return r.Context().Err()
	default:
		return httpkit.Fail(429, "authentication_busy", "Authentication is busy. Try again shortly.")
	}
}

// Admission uses its own committed transaction, including denied attempts.
// Digests retain neither submitted emails/tokens nor raw source addresses.
func (s *Server) admit(r *http.Request, action, target string) error {
	tx, err := s.Pool.Begin(r.Context())
	if err != nil {
		return err
	}
	defer tx.Rollback(r.Context())
	source := s.clientIP(r)
	sourceLimit := 60
	if action == "invitation" {
		sourceLimit = 30
	}
	buckets := []struct {
		key   string
		limit int
	}{{"global", 10000}, {"source:" + source, sourceLimit}}
	if target != "" {
		buckets = append(buckets, struct {
			key   string
			limit int
		}{"target:" + source + ":" + target, 5}, struct {
			key   string
			limit int
		}{"account:" + target, 30})
	}
	admitted := true
	for _, bucket := range buckets {
		var count int
		err = tx.QueryRow(r.Context(), `INSERT INTO olp.auth_admission(action,digest,attempts) VALUES($1,$2,1)
            ON CONFLICT(action,digest) DO UPDATE SET attempts=CASE WHEN auth_admission.window_started_at<=now()-interval '1 minute' THEN 1 ELSE LEAST(auth_admission.attempts+1,$3+1) END,
            window_started_at=CASE WHEN auth_admission.window_started_at<=now()-interval '1 minute' THEN now() ELSE auth_admission.window_started_at END RETURNING attempts`, action, s.Auth.Digest(secrets.AdmissionDigest, bucket.key), bucket.limit).Scan(&count)
		if err != nil {
			return err
		}
		if count > bucket.limit {
			admitted = false
			break
		}
	}
	if _, err = tx.Exec(r.Context(), "DELETE FROM olp.auth_admission WHERE ctid IN(SELECT ctid FROM olp.auth_admission WHERE window_started_at<now()-interval '10 minutes' LIMIT 1000)"); err != nil {
		return err
	}
	if err = tx.Commit(r.Context()); err != nil {
		return err
	}
	if !admitted {
		return httpkit.Fail(429, "authentication_rate_limited", "Too many attempts. Try again in a minute.")
	}
	return nil
}

func (s *Server) setup(r *http.Request) (httpkit.Reply, error) {
	if err := s.admit(r, "setup", ""); err != nil {
		return httpkit.Reply{}, err
	}
	token, configured := s.Bootstrap()
	if !configured || !hmac.Equal([]byte(token), []byte(r.Header.Get("X-OLP-Setup-Token"))) {
		return httpkit.Reply{}, httpkit.Fail(403, "setup_token_invalid", "The bootstrap token is invalid.")
	}
	var input struct {
		Email       string `json:"email"`
		Password    string `json:"password"`
		DisplayName string `json:"display_name"`
		Name        string `json:"installation_name"`
	}
	if err := httpkit.Decode(r, &input); err != nil {
		return httpkit.Reply{}, err
	}
	address, err := email(input.Email)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if err = password(input.Password); err != nil {
		return httpkit.Reply{}, err
	}
	if err = httpkit.ValidText("display_name", input.DisplayName, 100); err != nil {
		return httpkit.Reply{}, err
	}
	if input.Name == "" {
		input.Name = "OpenLLMProxy"
	}
	if err = httpkit.ValidText("installation_name", input.Name, 100); err != nil {
		return httpkit.Reply{}, err
	}
	var hash string
	if err = s.passwordWork(r, func() { hash = secrets.HashPassword(input.Password) }); err != nil {
		return httpkit.Reply{}, err
	}
	tx, err := s.Begin(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	var complete bool
	if err = tx.QueryRow(r.Context(), "SELECT setup_complete FROM olp.installation WHERE singleton").Scan(&complete); err != nil {
		return httpkit.Reply{}, err
	}
	if complete {
		return httpkit.Reply{}, httpkit.Fail(409, "setup_complete", "This installation already has an owner.")
	}
	id := httpkit.NewID()
	if _, err = tx.Exec(r.Context(), "INSERT INTO olp.users(id,email,display_name,password_hash,role,etag) VALUES($1,$2,$3,$4,'owner',$5)", id, address, strings.TrimSpace(input.DisplayName), hash, httpkit.NewID()); err != nil {
		return httpkit.Reply{}, err
	}
	if _, err = tx.Exec(r.Context(), "UPDATE olp.installation SET name=$1,setup_complete=true WHERE singleton", strings.TrimSpace(input.Name)); err != nil {
		return httpkit.Reply{}, err
	}
	for key, value := range map[string]string{"retention.requests_days": "30", "retention.usage_days": "90", "retention.audit_days": "365", "limits.valkey_unavailable": "fail_closed", "budgets.time_zone": "UTC", "guardrails.stream_window_bytes": "65536", "auth.mfa_required": "false", "auth.local_login_enabled": "true"} {
		if _, err = tx.Exec(r.Context(), "INSERT INTO olp.settings(key,value,etag,updated_by) VALUES($1,$2,$3,$4)", key, value, httpkit.NewID(), id); err != nil {
			return httpkit.Reply{}, err
		}
	}
	if err = Audit(r.Context(), tx, r, authz.UserActor(id), "installation.setup", "installation", s.Installation, "success"); err != nil {
		return httpkit.Reply{}, err
	}
	response, err := s.newSession(r, tx, id)
	if err != nil {
		return httpkit.Reply{}, err
	}
	return response, tx.Commit(r.Context())
}

func (s *Server) users(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	p, err := httpkit.Page(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	rows, err := s.Pool.Query(r.Context(), "SELECT to_jsonb(u)-'password_hash'-'role_management'-'oidc_authorized' FROM olp.users u WHERE id<$1 ORDER BY id DESC LIMIT $2", p.Before, p.Limit+1)
	if err != nil {
		return httpkit.Reply{}, err
	}
	items, err := httpkit.JSONRows(rows)
	return httpkit.ListReply(items, p), err
}
func (s *Server) user(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "user_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	u, err := scanUser(s.Pool.QueryRow(r.Context(), "SELECT "+userColumns+" FROM olp.users u WHERE id=$1", id))
	return httpkit.Detail(u, u.ETag), err
}
func (s *Server) updateUser(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	var input struct {
		Role        *string `json:"role"`
		Active      *bool   `json:"active"`
		AccessScope *string `json:"access_scope"`
	}
	if err := httpkit.Decode(r, &input); err != nil {
		return httpkit.Reply{}, err
	}
	if input.Role == nil && input.Active == nil && input.AccessScope == nil {
		return httpkit.Reply{}, httpkit.Invalid("user", "Provide a role or active status.")
	}
	if input.Role != nil && !authz.ValidRole(*input.Role) {
		return httpkit.Reply{}, httpkit.Invalid("role", "Use owner, operator, developer, or viewer.")
	}
	if input.AccessScope != nil && *input.AccessScope != "global" && *input.AccessScope != "assigned" {
		return httpkit.Reply{}, httpkit.Invalid("access_scope", "Use global or assigned.")
	}
	id, err := httpkit.IDParam(r, "user_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	tx, p, err := s.BeginMutation(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	u, err := scanUser(tx.QueryRow(r.Context(), "SELECT "+userColumns+" FROM olp.users u WHERE id=$1", id))
	if err != nil {
		return httpkit.Reply{}, err
	}
	if err = httpkit.Match(r, u.ETag); err != nil {
		return httpkit.Reply{}, err
	}
	if input.Role != nil {
		u.Role = *input.Role
	}
	if input.Active != nil {
		u.Active = *input.Active
	}
	if input.AccessScope != nil {
		u.AccessScope = *input.AccessScope
	}
	if id == p.ID && (!u.Active || u.Role != p.Role || u.AccessScope != p.AccessScope) {
		return httpkit.Reply{}, httpkit.Fail(409, "cannot_change_current_user_access", "Ask another owner to change your access.")
	}
	u.ETag = httpkit.NewID()
	// Taking over a provisioned user ends the project grants their SCIM groups
	// gave them, which must not leave a project without a manager.
	managed, err := scimManagedProjects(r, tx)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if _, err = tx.Exec(r.Context(), "UPDATE olp.users SET role=$1,active=$2,access_scope=$3,etag=$4,updated_at=now(),role_management=CASE WHEN role_management='provisioned' THEN 'local' ELSE role_management END WHERE id=$5", u.Role, u.Active, u.AccessScope, u.ETag, id); err != nil {
		return httpkit.Reply{}, err
	}
	if err = s.EnsureUsableOwner(r.Context(), tx); err != nil {
		return httpkit.Reply{}, err
	}
	if err = keepSCIMManagers(r, tx, managed); err != nil {
		return httpkit.Reply{}, err
	}
	if _, err = tx.Exec(r.Context(), "DELETE FROM olp.sessions WHERE user_id=$1", id); err != nil {
		return httpkit.Reply{}, err
	}
	if !u.Active || u.Role != "owner" || u.AccessScope != "global" {
		if err = retireIssuedInvitations(r, tx, id, p.Actor(), p.UserID()); err != nil {
			return httpkit.Reply{}, err
		}
	}
	// Issuance is installation-scoped. Existing keys retain their issuer and
	// policy across account role changes and disabling, as in the reference.
	if _, err = AdvanceAuthority(r.Context(), tx); err != nil {
		return httpkit.Reply{}, err
	}
	if err = Audit(r.Context(), tx, r, p.Actor(), "user.update", "user", id, "success"); err != nil {
		return httpkit.Reply{}, err
	}
	u, err = scanUser(tx.QueryRow(r.Context(), "SELECT "+userColumns+" FROM olp.users u WHERE id=$1", id))
	if err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.Detail(u, u.ETag), tx.Commit(r.Context())
}

func (s *Server) EnsureUsableOwner(ctx context.Context, tx pgx.Tx) error {
	var exists bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.users WHERE active AND oidc_authorized AND role='owner'
        AND access_scope='global' AND password_hash IS NOT NULL AND $1 AND COALESCE((SELECT value='true' FROM olp.settings WHERE key='auth.local_login_enabled'),true))`, !s.localLoginDisabled).Scan(&exists)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	if usable, e := s.usableSAMLOwner(ctx, tx); e != nil {
		return e
	} else if usable {
		return nil
	}
	missing := httpkit.Fail(409, "last_usable_owner", "Keep at least one active owner with a usable sign-in method and an owner role after sign-in.")
	c, err := loadOIDC(ctx, tx)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !c.Enabled {
		return missing
	}
	if err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT u.role_management<>'oidc',i.role_claims
        FROM olp.users u JOIN olp.oidc_identities i ON i.user_id=u.id
        WHERE u.active AND u.oidc_authorized AND u.role='owner' AND u.access_scope='global' AND i.issuer=$1`, c.Issuer)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var local bool
		var data []byte
		if err = rows.Scan(&local, &data); err != nil {
			return err
		}
		role, err := oidcSignInRole(c, local, "owner", data)
		if err != nil {
			return err
		}
		if role == "owner" {
			return nil
		}
	}
	if err = rows.Err(); err != nil {
		return err
	}
	return missing
}

// Invitations are outstanding access grants and cannot outlive the issuer's
// membership-management authority. Call inside the authority change transaction.
func retireIssuedInvitations(r *http.Request, tx pgx.Tx, issuer string, actor authz.Actor, revokedBy string) error {
	result, err := tx.Exec(r.Context(), `UPDATE olp.invitations
        SET revoked_at=now(),revoked_by=NULLIF($2::text,'')::uuid
        WHERE invited_by=$1 AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at>now()`, issuer, revokedBy)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return nil
	}
	return Audit(r.Context(), tx, r, actor, "invitation.revoke_for_access_loss", "user", issuer, "success")
}

const invitationJSON = `jsonb_build_object('id',i.id,'email',i.email,'role',i.role,'invited_by',i.invited_by,'invited_by_email',inviter.email,'accepted_by_email',accepted.email,'revoked_by_email',revoker.email,'accepted_at',i.accepted_at,'revoked_at',i.revoked_at,'expires_at',i.expires_at,'created_at',i.created_at,'status',CASE WHEN i.accepted_at IS NOT NULL THEN 'accepted' WHEN i.revoked_at IS NOT NULL THEN 'revoked' WHEN i.expires_at<=now() THEN 'expired' ELSE 'pending' END)`
const invitationFrom = ` FROM olp.invitations i LEFT JOIN olp.users inviter ON inviter.id=i.invited_by LEFT JOIN olp.users accepted ON accepted.id=i.accepted_by LEFT JOIN olp.users revoker ON revoker.id=i.revoked_by`

func (s *Server) invitations(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	p, err := httpkit.Page(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	rows, err := s.Pool.Query(r.Context(), "SELECT "+invitationJSON+invitationFrom+" WHERE i.id<$1 ORDER BY i.id DESC LIMIT $2", p.Before, p.Limit+1)
	if err != nil {
		return httpkit.Reply{}, err
	}
	items, err := httpkit.JSONRows(rows)
	return httpkit.ListReply(items, p), err
}
func invitation(r *http.Request, tx pgx.Tx, id string) (any, error) {
	var data []byte
	err := tx.QueryRow(r.Context(), "SELECT "+invitationJSON+invitationFrom+" WHERE i.id=$1", id).Scan(&data)
	return json.RawMessage(data), err
}
func (s *Server) createInvitation(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	var input struct {
		Email string `json:"email"`
		Role  string `json:"role"`
		Hours *int   `json:"expires_in_hours"`
	}
	if err := httpkit.Decode(r, &input); err != nil {
		return httpkit.Reply{}, err
	}
	address, err := email(input.Email)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if !authz.ValidRole(input.Role) {
		return httpkit.Reply{}, httpkit.Invalid("role", "Use owner, operator, developer, or viewer.")
	}
	hours := 168
	if input.Hours != nil {
		hours = *input.Hours
	}
	if hours < 1 || hours > 720 {
		return httpkit.Reply{}, httpkit.Invalid("expires_in_hours", "Use 1–720 hours.")
	}
	tx, p, err := s.BeginMutation(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	if err = s.requireInvitationLogin(r, tx); err != nil {
		return httpkit.Reply{}, err
	}
	claim, replayed, err := s.Replay(r, tx, p, input)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if replayed != nil {
		return *replayed, tx.Commit(r.Context())
	}
	var exists bool
	if err = tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM olp.users WHERE email=$1)", address).Scan(&exists); err != nil {
		return httpkit.Reply{}, err
	}
	if exists {
		return httpkit.Reply{}, httpkit.Fail(409, "user_exists", "This email already belongs to a member.")
	}
	id, token := httpkit.NewID(), secrets.Token()
	if _, err = tx.Exec(r.Context(), "UPDATE olp.invitations SET revoked_at=now(),revoked_by=$2 WHERE email=$1 AND accepted_at IS NULL AND revoked_at IS NULL", address, p.UserID()); err != nil {
		return httpkit.Reply{}, err
	}
	if _, err = tx.Exec(r.Context(), "INSERT INTO olp.invitations(id,email,role,digest,invited_by,expires_at) VALUES($1,$2,$3,$4,$5,$6)", id, address, input.Role, s.Auth.Digest(secrets.InvitationDigest, token), p.UserID(), time.Now().Add(time.Duration(hours)*time.Hour)); err != nil {
		return httpkit.Reply{}, err
	}
	body, err := invitation(r, tx, id)
	if err != nil {
		return httpkit.Reply{}, err
	}
	result := httpkit.Reply{Status: 201, Body: map[string]any{"invitation": body, "token": token}, Location: "/api/v1/invitations/" + id}
	if err = Audit(r.Context(), tx, r, p.Actor(), "invitation.create", "invitation", id, "success"); err != nil {
		return httpkit.Reply{}, err
	}
	if err = s.CompleteReplay(r, tx, claim, result); err != nil {
		return httpkit.Reply{}, err
	}
	return result, tx.Commit(r.Context())
}
func (s *Server) retireInvitation(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "invitation_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	tx, p, err := s.BeginMutation(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	claim, replayed, err := s.Replay(r, tx, p, nil)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if replayed != nil {
		return *replayed, tx.Commit(r.Context())
	}
	var accepted, revoked *time.Time
	if err = tx.QueryRow(r.Context(), "SELECT accepted_at,revoked_at FROM olp.invitations WHERE id=$1", id).Scan(&accepted, &revoked); err != nil {
		return httpkit.Reply{}, err
	}
	if accepted != nil {
		return httpkit.Reply{}, httpkit.Fail(409, "invitation_accepted", "An accepted invitation cannot be revoked.")
	}
	if revoked == nil {
		if _, err = tx.Exec(r.Context(), "UPDATE olp.invitations SET revoked_at=now(),revoked_by=$2 WHERE id=$1", id, p.UserID()); err != nil {
			return httpkit.Reply{}, err
		}
		if err = Audit(r.Context(), tx, r, p.Actor(), "invitation.revoke", "invitation", id, "success"); err != nil {
			return httpkit.Reply{}, err
		}
	}
	body, err := invitation(r, tx, id)
	if err != nil {
		return httpkit.Reply{}, err
	}
	result := httpkit.OK(body)
	if err = s.CompleteReplay(r, tx, claim, result); err != nil {
		return httpkit.Reply{}, err
	}
	return result, tx.Commit(r.Context())
}
func (s *Server) acceptInvitation(r *http.Request) (httpkit.Reply, error) {
	var input struct {
		Token       string `json:"token"`
		DisplayName string `json:"display_name"`
		Password    string `json:"password"`
	}
	if err := httpkit.Decode(r, &input); err != nil {
		return httpkit.Reply{}, err
	}
	if err := s.admit(r, "invitation", input.Token); err != nil {
		return httpkit.Reply{}, err
	}
	if err := password(input.Password); err != nil {
		return httpkit.Reply{}, err
	}
	if err := httpkit.ValidText("display_name", input.DisplayName, 100); err != nil {
		return httpkit.Reply{}, err
	}
	var hash string
	if err := s.passwordWork(r, func() { hash = secrets.HashPassword(input.Password) }); err != nil {
		return httpkit.Reply{}, err
	}
	tx, err := s.Begin(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	if err = s.requireInvitationLogin(r, tx); err != nil {
		return httpkit.Reply{}, err
	}
	var id, address, role string
	err = tx.QueryRow(r.Context(), "SELECT id::text,email,role FROM olp.invitations WHERE digest=$1 AND expires_at>now() AND accepted_at IS NULL AND revoked_at IS NULL", s.Auth.Digest(secrets.InvitationDigest, input.Token)).Scan(&id, &address, &role)
	if errors.Is(err, pgx.ErrNoRows) {
		return httpkit.Reply{}, httpkit.Fail(410, "invitation_invalid", "The invitation is expired, retired, or already used.")
	}
	if err != nil {
		return httpkit.Reply{}, err
	}
	userID := httpkit.NewID()
	if _, err = tx.Exec(r.Context(), "INSERT INTO olp.users(id,email,display_name,password_hash,role,etag) VALUES($1,$2,$3,$4,$5,$6)", userID, address, strings.TrimSpace(input.DisplayName), hash, role, httpkit.NewID()); err != nil {
		return httpkit.Reply{}, err
	}
	if _, err = tx.Exec(r.Context(), "UPDATE olp.invitations SET accepted_at=now(),accepted_by=$2 WHERE id=$1", id, userID); err != nil {
		return httpkit.Reply{}, err
	}
	if err = Audit(r.Context(), tx, r, authz.UserActor(userID), "invitation.accept", "invitation", id, "success"); err != nil {
		return httpkit.Reply{}, err
	}
	response, err := s.localSession(r, tx, userID)
	if err != nil {
		return httpkit.Reply{}, err
	}
	return response, tx.Commit(r.Context())
}

// The process switch is immutable after composition; database policy is read
// inside the caller's installation transaction for protected mutations.
func (s *Server) localLoginEnabled(r *http.Request, q database.Queryer) (bool, error) {
	var enabled bool
	err := q.QueryRow(r.Context(), "SELECT COALESCE((SELECT value='true' FROM olp.settings WHERE key='auth.local_login_enabled'),true)").Scan(&enabled)
	return enabled && !s.localLoginDisabled, err
}

func (s *Server) requireInvitationLogin(r *http.Request, q database.Queryer) error {
	enabled, err := s.localLoginEnabled(r, q)
	if err != nil {
		return err
	}
	if !enabled {
		return httpkit.Fail(409, "invitation_local_login_required", "Password invitations require local sign-in. Ask an owner to configure an OIDC role mapping, then sign in with single sign-on, or enable local sign-in before using this invitation.")
	}
	return nil
}
