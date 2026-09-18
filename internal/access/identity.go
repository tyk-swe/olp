package access

import (
	"crypto/hmac"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/secrets"
)

func NewID() string { return uuid.Must(uuid.NewV7()).String() }

func (s *Server) Register(mux *http.ServeMux) {
	routes := map[string]func(*http.Request) (Reply, error){
		"GET /api/v3/setup/status": s.setupStatus, "POST /api/v3/setup": s.setup,
		"GET /api/v3/auth/capabilities": s.capabilities,
		"POST /api/v3/sessions":         s.login, "GET /api/v3/sessions": s.sessions,
		"GET /api/v3/sessions/current": s.currentSession, "DELETE /api/v3/sessions/current": s.logout,
		"DELETE /api/v3/sessions/{session_id}": s.revokeSession,
		"GET /api/v3/users":                    s.users, "GET /api/v3/users/{user_id}": s.user, "PATCH /api/v3/users/{user_id}": s.updateUser,
		"GET /api/v3/invitations": s.invitations, "POST /api/v3/invitations": s.createInvitation,
		"DELETE /api/v3/invitations/{invitation_id}": s.retireInvitation, "POST /api/v3/invitations/accept": s.acceptInvitation,
		"GET /api/v3/profile": s.profile, "PATCH /api/v3/profile": s.updateProfile,
		"POST /api/v3/profile/password": s.changePassword, "POST /api/v3/profile/password/enroll": s.enrollPassword,
		"POST /api/v3/profile/reauthenticate": s.reauthenticate,
		"GET /api/v3/api-keys":                s.apiKeys, "POST /api/v3/api-keys": s.createAPIKey,
		"GET /api/v3/api-keys/{api_key_id}": s.apiKey, "PATCH /api/v3/api-keys/{api_key_id}": s.updateAPIKey,
		"POST /api/v3/api-keys/{api_key_id}/revoke": s.revokeAPIKey, "POST /api/v3/api-keys/{api_key_id}/rotate": s.rotateAPIKey,
		"GET /api/v3/settings": s.settings, "GET /api/v3/settings/{key}": s.setting, "PUT /api/v3/settings/{key}": s.updateSetting,
		"GET /api/v3/audit":              s.auditEvents,
		"GET /api/v3/oidc/configuration": s.oidcConfiguration, "PUT /api/v3/oidc/configuration": s.putOIDCConfiguration,
		"GET /api/v3/oidc/login": s.beginOIDCLogin, "POST /api/v3/oidc/login": s.beginOIDCLogin,
		"POST /api/v3/oidc/link": s.beginOIDCLink, "POST /api/v3/oidc/reauthenticate": s.beginOIDCReauthentication,
		"GET /api/v3/oidc/callback": s.oidcCallback, "GET /api/v3/oidc/identities": s.oidcIdentities,
		"DELETE /api/v3/oidc/identities/{identity_id}": s.unlinkOIDCIdentity,
	}
	for pattern, fn := range routes {
		mux.HandleFunc(pattern, s.Handle(fn))
	}
}

func (s *Server) setupStatus(r *http.Request) (Reply, error) {
	var complete bool
	err := s.Pool.QueryRow(r.Context(), "SELECT setup_complete FROM olp_go.installation WHERE singleton").Scan(&complete)
	return OK(map[string]bool{"setup_required": !complete}), err
}
func (s *Server) capabilities(r *http.Request) (Reply, error) {
	var local, oidc bool
	err := s.Pool.QueryRow(r.Context(), `SELECT COALESCE((SELECT value='true' FROM olp_go.settings WHERE key='auth.local_login_enabled'),true),COALESCE((SELECT (document->>'enabled')::boolean FROM olp_go.oidc_configuration WHERE singleton),false)`).Scan(&local, &oidc)
	return OK(map[string]bool{"local_login_enabled": local && !s.LocalLoginDisabled, "oidc_login_enabled": oidc, "gateway_available": true, "limits_enforced": s.LimitsEnforced, "retention_enforced": s.RetentionEnforced}), err
}

func (s *Server) passwordWork(r *http.Request, work func()) error {
	select {
	case s.passwordSlots <- struct{}{}:
		defer func() { <-s.passwordSlots }()
		work()
		return r.Context().Err()
	default:
		return Fail(429, "authentication_busy", "Authentication is busy. Try again shortly.")
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
	source, _, _ := net.SplitHostPort(r.RemoteAddr)
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
		}{"target:" + source + ":" + target, 5})
	}
	admitted := true
	for _, bucket := range buckets {
		var count int
		err = tx.QueryRow(r.Context(), `INSERT INTO olp_go.auth_admission(action,digest,attempts) VALUES($1,$2,1)
            ON CONFLICT(action,digest) DO UPDATE SET attempts=CASE WHEN auth_admission.window_started_at<=now()-interval '1 minute' THEN 1 ELSE LEAST(auth_admission.attempts+1,$3+1) END,
            window_started_at=CASE WHEN auth_admission.window_started_at<=now()-interval '1 minute' THEN now() ELSE auth_admission.window_started_at END RETURNING attempts`, action, s.Auth.Digest("admission", bucket.key), bucket.limit).Scan(&count)
		if err != nil {
			return err
		}
		if count > bucket.limit {
			admitted = false
			break
		}
	}
	if _, err = tx.Exec(r.Context(), "DELETE FROM olp_go.auth_admission WHERE ctid IN(SELECT ctid FROM olp_go.auth_admission WHERE window_started_at<now()-interval '10 minutes' LIMIT 1000)"); err != nil {
		return err
	}
	if err = tx.Commit(r.Context()); err != nil {
		return err
	}
	if !admitted {
		return Fail(429, "authentication_rate_limited", "Too many attempts. Try again in a minute.")
	}
	return nil
}

func (s *Server) setup(r *http.Request) (Reply, error) {
	if err := s.admit(r, "setup", ""); err != nil {
		return Reply{}, err
	}
	if s.Bootstrap == "" || !hmac.Equal([]byte(s.Bootstrap), []byte(r.Header.Get("X-OLP-Setup-Token"))) {
		return Reply{}, Fail(403, "setup_token_invalid", "The bootstrap token is invalid.")
	}
	var input struct {
		Email       string `json:"email"`
		Password    string `json:"password"`
		DisplayName string `json:"display_name"`
		Name        string `json:"installation_name"`
	}
	if err := Decode(r, &input); err != nil {
		return Reply{}, err
	}
	address, err := email(input.Email)
	if err != nil {
		return Reply{}, err
	}
	if err = password(input.Password); err != nil {
		return Reply{}, err
	}
	if err = ValidText("display_name", input.DisplayName, 100); err != nil {
		return Reply{}, err
	}
	if input.Name == "" {
		input.Name = "OpenLLMProxy"
	}
	if err = ValidText("installation_name", input.Name, 100); err != nil {
		return Reply{}, err
	}
	var hash string
	if err = s.passwordWork(r, func() { hash = secrets.HashPassword(input.Password) }); err != nil {
		return Reply{}, err
	}
	tx, err := s.Begin(r)
	if err != nil {
		return Reply{}, err
	}
	defer tx.Rollback(r.Context())
	var complete bool
	if err = tx.QueryRow(r.Context(), "SELECT setup_complete FROM olp_go.installation WHERE singleton").Scan(&complete); err != nil {
		return Reply{}, err
	}
	if complete {
		return Reply{}, Fail(409, "setup_complete", "This installation already has an owner.")
	}
	id := NewID()
	if _, err = tx.Exec(r.Context(), "INSERT INTO olp_go.users(id,email,display_name,password_hash,role,etag) VALUES($1,$2,$3,$4,'owner',$5)", id, address, strings.TrimSpace(input.DisplayName), hash, NewID()); err != nil {
		return Reply{}, err
	}
	if _, err = tx.Exec(r.Context(), "UPDATE olp_go.installation SET name=$1,setup_complete=true WHERE singleton", strings.TrimSpace(input.Name)); err != nil {
		return Reply{}, err
	}
	for key, value := range map[string]string{"retention.requests_days": "30", "retention.usage_days": "90", "retention.audit_days": "365", "limits.valkey_unavailable": "fail_closed", "auth.local_login_enabled": "true"} {
		if _, err = tx.Exec(r.Context(), "INSERT INTO olp_go.settings(key,value,etag,updated_by) VALUES($1,$2,$3,$4)", key, value, NewID(), id); err != nil {
			return Reply{}, err
		}
	}
	if err = Audit(r.Context(), tx, r, id, "installation.setup", "installation", s.Installation, "success"); err != nil {
		return Reply{}, err
	}
	response, err := s.newSession(r, tx, id)
	if err != nil {
		return Reply{}, err
	}
	return Commit(r, tx, response)
}

func (s *Server) users(r *http.Request) (Reply, error) {
	if _, err := s.Principal(r, s.Pool, "access_read"); err != nil {
		return Reply{}, err
	}
	p, err := Page(r)
	if err != nil {
		return Reply{}, err
	}
	rows, err := s.Pool.Query(r.Context(), "SELECT to_jsonb(u)-'password_hash' FROM olp_go.users u WHERE id<$1 ORDER BY id DESC LIMIT $2", p.Before, p.Limit+1)
	if err != nil {
		return Reply{}, err
	}
	items, err := JSONRows(rows)
	return ListReply(items, p), err
}
func (s *Server) user(r *http.Request) (Reply, error) {
	if _, err := s.Principal(r, s.Pool, "access_read"); err != nil {
		return Reply{}, err
	}
	id, err := IDParam(r, "user_id")
	if err != nil {
		return Reply{}, err
	}
	u, err := scanUser(s.Pool.QueryRow(r.Context(), "SELECT "+userColumns+" FROM olp_go.users u WHERE id=$1", id))
	return Detail(u, u.ETag), err
}
func (s *Server) updateUser(r *http.Request) (Reply, error) {
	var input struct {
		Role   *string `json:"role"`
		Active *bool   `json:"active"`
	}
	if err := Decode(r, &input); err != nil {
		return Reply{}, err
	}
	if input.Role == nil && input.Active == nil {
		return Reply{}, Invalid("user", "Provide a role or active status.")
	}
	if input.Role != nil && !validRole(*input.Role) {
		return Reply{}, Invalid("role", "Use owner, operator, developer, or viewer.")
	}
	id, err := IDParam(r, "user_id")
	if err != nil {
		return Reply{}, err
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
	u, err := scanUser(tx.QueryRow(r.Context(), "SELECT "+userColumns+" FROM olp_go.users u WHERE id=$1", id))
	if err != nil {
		return Reply{}, err
	}
	if err = Match(r, u.ETag); err != nil {
		return Reply{}, err
	}
	if input.Role != nil {
		u.Role = *input.Role
	}
	if input.Active != nil {
		u.Active = *input.Active
	}
	if id == p.ID && (!u.Active || u.Role != p.Role) {
		return Reply{}, Fail(409, "cannot_change_current_user_access", "Ask another owner to change your access.")
	}
	u.ETag = NewID()
	if _, err = tx.Exec(r.Context(), "UPDATE olp_go.users SET role=$1,active=$2,etag=$3,updated_at=now() WHERE id=$4", u.Role, u.Active, u.ETag, id); err != nil {
		return Reply{}, err
	}
	if err = usableOwner(r, tx); err != nil {
		return Reply{}, err
	}
	if _, err = tx.Exec(r.Context(), "DELETE FROM olp_go.sessions WHERE user_id=$1", id); err != nil {
		return Reply{}, err
	}
	if !u.Active || u.Role != "owner" {
		if err = retireIssuedInvitations(r, tx, id, p.ID); err != nil {
			return Reply{}, err
		}
	}
	// Issuance is installation-scoped. Existing keys retain their issuer and
	// policy across account role changes and disabling, as in the reference.
	if _, err = AdvanceAuthority(r, tx); err != nil {
		return Reply{}, err
	}
	if err = Audit(r.Context(), tx, r, p.ID, "user.update", "user", id, "success"); err != nil {
		return Reply{}, err
	}
	u, err = scanUser(tx.QueryRow(r.Context(), "SELECT "+userColumns+" FROM olp_go.users u WHERE id=$1", id))
	if err != nil {
		return Reply{}, err
	}
	return Commit(r, tx, Detail(u, u.ETag))
}

func usableOwner(r *http.Request, tx pgx.Tx) error {
	var exists bool
	err := tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM olp_go.users WHERE active AND role='owner'
        AND password_hash IS NOT NULL AND COALESCE((SELECT value='true' FROM olp_go.settings WHERE key='auth.local_login_enabled'),true))`).Scan(&exists)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	missing := Fail(409, "last_usable_owner", "Keep at least one active owner with a usable sign-in method and an owner role after sign-in.")
	c, err := loadOIDC(r, tx)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !c.Enabled {
		return missing
	}
	if err != nil {
		return err
	}
	rows, err := tx.Query(r.Context(), `SELECT u.password_hash IS NOT NULL,i.role_claims
        FROM olp_go.users u JOIN olp_go.oidc_identities i ON i.user_id=u.id
        WHERE u.active AND u.role='owner' AND i.issuer=$1`, c.Issuer)
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
func retireIssuedInvitations(r *http.Request, tx pgx.Tx, issuer, actor string) error {
	result, err := tx.Exec(r.Context(), `UPDATE olp_go.invitations
        SET revoked_at=now(),revoked_by=NULLIF($2::text,'')::uuid
        WHERE invited_by=$1 AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at>now()`, issuer, actor)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return nil
	}
	return Audit(r.Context(), tx, r, actor, "invitation.revoke_for_access_loss", "user", issuer, "success")
}

const invitationJSON = `jsonb_build_object('id',i.id,'email',i.email,'role',i.role,'invited_by',i.invited_by,'invited_by_email',inviter.email,'accepted_by_email',accepted.email,'revoked_by_email',revoker.email,'accepted_at',i.accepted_at,'revoked_at',i.revoked_at,'expires_at',i.expires_at,'created_at',i.created_at,'status',CASE WHEN i.accepted_at IS NOT NULL THEN 'accepted' WHEN i.revoked_at IS NOT NULL THEN 'revoked' WHEN i.expires_at<=now() THEN 'expired' ELSE 'pending' END)`
const invitationFrom = ` FROM olp_go.invitations i LEFT JOIN olp_go.users inviter ON inviter.id=i.invited_by LEFT JOIN olp_go.users accepted ON accepted.id=i.accepted_by LEFT JOIN olp_go.users revoker ON revoker.id=i.revoked_by`

func (s *Server) invitations(r *http.Request) (Reply, error) {
	if _, err := s.Principal(r, s.Pool, "access_read"); err != nil {
		return Reply{}, err
	}
	p, err := Page(r)
	if err != nil {
		return Reply{}, err
	}
	rows, err := s.Pool.Query(r.Context(), "SELECT "+invitationJSON+invitationFrom+" WHERE i.id<$1 ORDER BY i.id DESC LIMIT $2", p.Before, p.Limit+1)
	if err != nil {
		return Reply{}, err
	}
	items, err := JSONRows(rows)
	return ListReply(items, p), err
}
func invitation(r *http.Request, tx pgx.Tx, id string) (any, error) {
	var data []byte
	err := tx.QueryRow(r.Context(), "SELECT "+invitationJSON+invitationFrom+" WHERE i.id=$1", id).Scan(&data)
	return RawJSON(data), err
}
func (s *Server) createInvitation(r *http.Request) (Reply, error) {
	var input struct {
		Email string `json:"email"`
		Role  string `json:"role"`
		Hours *int   `json:"expires_in_hours"`
	}
	if err := Decode(r, &input); err != nil {
		return Reply{}, err
	}
	address, err := email(input.Email)
	if err != nil {
		return Reply{}, err
	}
	if !validRole(input.Role) {
		return Reply{}, Invalid("role", "Use owner, operator, developer, or viewer.")
	}
	hours := 168
	if input.Hours != nil {
		hours = *input.Hours
	}
	if hours < 1 || hours > 720 {
		return Reply{}, Invalid("expires_in_hours", "Use 1–720 hours.")
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
	claim, replayed, err := s.Replay(r, tx, p, input)
	if err != nil {
		return Reply{}, err
	}
	if replayed != nil {
		return Commit(r, tx, *replayed)
	}
	var exists bool
	if err = tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM olp_go.users WHERE email=$1)", address).Scan(&exists); err != nil {
		return Reply{}, err
	}
	if exists {
		return Reply{}, Fail(409, "user_exists", "This email already belongs to a member.")
	}
	id, token := NewID(), secrets.Token()
	if _, err = tx.Exec(r.Context(), "UPDATE olp_go.invitations SET revoked_at=now(),revoked_by=$2 WHERE email=$1 AND accepted_at IS NULL AND revoked_at IS NULL", address, p.ID); err != nil {
		return Reply{}, err
	}
	if _, err = tx.Exec(r.Context(), "INSERT INTO olp_go.invitations(id,email,role,digest,invited_by,expires_at) VALUES($1,$2,$3,$4,$5,$6)", id, address, input.Role, s.Auth.Digest("invitation", token), p.ID, time.Now().Add(time.Duration(hours)*time.Hour)); err != nil {
		return Reply{}, err
	}
	body, err := invitation(r, tx, id)
	if err != nil {
		return Reply{}, err
	}
	result := Reply{Status: 201, Body: map[string]any{"invitation": body, "token": token}, Location: "/api/v3/invitations/" + id}
	if err = Audit(r.Context(), tx, r, p.ID, "invitation.create", "invitation", id, "success"); err != nil {
		return Reply{}, err
	}
	if err = s.CompleteReplay(r, tx, claim, result); err != nil {
		return Reply{}, err
	}
	return Commit(r, tx, result)
}
func (s *Server) retireInvitation(r *http.Request) (Reply, error) {
	id, err := IDParam(r, "invitation_id")
	if err != nil {
		return Reply{}, err
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
	claim, replayed, err := s.Replay(r, tx, p, nil)
	if err != nil {
		return Reply{}, err
	}
	if replayed != nil {
		return Commit(r, tx, *replayed)
	}
	var accepted, revoked *time.Time
	if err = tx.QueryRow(r.Context(), "SELECT accepted_at,revoked_at FROM olp_go.invitations WHERE id=$1", id).Scan(&accepted, &revoked); err != nil {
		return Reply{}, err
	}
	if accepted != nil {
		return Reply{}, Fail(409, "invitation_accepted", "An accepted invitation cannot be revoked.")
	}
	if revoked == nil {
		if _, err = tx.Exec(r.Context(), "UPDATE olp_go.invitations SET revoked_at=now(),revoked_by=$2 WHERE id=$1", id, p.ID); err != nil {
			return Reply{}, err
		}
		if err = Audit(r.Context(), tx, r, p.ID, "invitation.revoke", "invitation", id, "success"); err != nil {
			return Reply{}, err
		}
	}
	body, err := invitation(r, tx, id)
	if err != nil {
		return Reply{}, err
	}
	result := OK(body)
	if err = s.CompleteReplay(r, tx, claim, result); err != nil {
		return Reply{}, err
	}
	return Commit(r, tx, result)
}
func (s *Server) acceptInvitation(r *http.Request) (Reply, error) {
	var input struct {
		Token       string `json:"token"`
		DisplayName string `json:"display_name"`
		Password    string `json:"password"`
	}
	if err := Decode(r, &input); err != nil {
		return Reply{}, err
	}
	if err := s.admit(r, "invitation", input.Token); err != nil {
		return Reply{}, err
	}
	if err := password(input.Password); err != nil {
		return Reply{}, err
	}
	if err := ValidText("display_name", input.DisplayName, 100); err != nil {
		return Reply{}, err
	}
	var hash string
	if err := s.passwordWork(r, func() { hash = secrets.HashPassword(input.Password) }); err != nil {
		return Reply{}, err
	}
	tx, err := s.Begin(r)
	if err != nil {
		return Reply{}, err
	}
	defer tx.Rollback(r.Context())
	var id, address, role string
	err = tx.QueryRow(r.Context(), "SELECT id::text,email,role FROM olp_go.invitations WHERE digest=$1 AND expires_at>now() AND accepted_at IS NULL AND revoked_at IS NULL", s.Auth.Digest("invitation", input.Token)).Scan(&id, &address, &role)
	if errors.Is(err, pgx.ErrNoRows) {
		return Reply{}, Fail(410, "invitation_invalid", "The invitation is expired, retired, or already used.")
	}
	if err != nil {
		return Reply{}, err
	}
	userID := NewID()
	if _, err = tx.Exec(r.Context(), "INSERT INTO olp_go.users(id,email,display_name,password_hash,role,etag) VALUES($1,$2,$3,$4,$5,$6)", userID, address, strings.TrimSpace(input.DisplayName), hash, role, NewID()); err != nil {
		return Reply{}, err
	}
	if _, err = tx.Exec(r.Context(), "UPDATE olp_go.invitations SET accepted_at=now(),accepted_by=$2 WHERE id=$1", id, userID); err != nil {
		return Reply{}, err
	}
	if err = Audit(r.Context(), tx, r, userID, "invitation.accept", "invitation", id, "success"); err != nil {
		return Reply{}, err
	}
	response, err := s.newSession(r, tx, userID)
	if err != nil {
		return Reply{}, err
	}
	return Commit(r, tx, response)
}
