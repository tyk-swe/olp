package identity

import (
	"context"
	"crypto/hmac"
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/domain/ids"
	"github.com/tyk-swe/olp/internal/platform/database"
	"github.com/tyk-swe/olp/internal/platform/secrets"
)

func (s *accountService) register(mux *http.ServeMux) {
	public := map[string]access.PublicHandler{
		"GET /api/v1/setup/status":        s.setupStatus,
		"POST /api/v1/setup":              s.setup,
		"GET /api/v1/auth/capabilities":   s.capabilities,
		"POST /api/v1/sessions":           s.login,
		"POST /api/v1/invitations/accept": s.acceptInvitation,
	}
	for pattern, fn := range public {
		s.access.Public(mux, pattern, fn)
	}
	routes := map[string]access.Handler{
		"GET /api/v1/users":                                           s.users,
		"GET /api/v1/users/{user_id}":                                 s.user,
		"PATCH /api/v1/users/{user_id}":                               s.updateUser,
		"GET /api/v1/invitations":                                     s.invitations,
		"POST /api/v1/invitations":                                    s.createInvitation,
		"DELETE /api/v1/invitations/{invitation_id}":                  s.retireInvitation,
		"POST /api/v1/profile/reauthenticate":                         s.reauthenticate,
		"GET /api/v1/management-tokens":                               s.managementTokens,
		"POST /api/v1/management-tokens":                              s.createManagementToken,
		"GET /api/v1/management-tokens/{management_token_id}":         s.managementToken,
		"POST /api/v1/management-tokens/{management_token_id}/revoke": s.revokeManagementToken,
		"PUT /api/v1/provisioning/{source}/users/{external_id}":       s.provisionUser,
		"DELETE /api/v1/provisioning/{source}/users/{external_id}":    s.deprovisionUser,
	}
	for pattern, fn := range routes {
		s.access.Route(mux, pattern, fn)
	}
}

func (s *accountService) setupStatus(r *http.Request) (httpkit.Reply, error) {
	var complete bool
	err := s.access.Pool.QueryRow(r.Context(), "SELECT setup_complete FROM olp.installation WHERE singleton").Scan(&complete)
	_, configured := s.bootstrap()
	return httpkit.OK(map[string]bool{"setup_required": !complete, "bootstrap_token_configured": configured}), err
}
func (s *accountService) capabilities(r *http.Request) (httpkit.Reply, error) {
	var local, oidc, samlEnabled, captureEnabled bool
	err := s.access.Pool.QueryRow(r.Context(), authenticationCapabilitiesSQL).Scan(&local, &oidc, &samlEnabled, &captureEnabled)
	return httpkit.OK(map[string]bool{"local_login_enabled": local && !s.access.LocalLoginDisabled(), "oidc_login_enabled": oidc, "saml_login_enabled": samlEnabled, "gateway_available": true, "limits_enforced": s.access.LimitsEnforced(), "management_network_restricted": s.managementNetworkRestricted, "retention_enforced": s.access.LimitsEnforced(), "notifications_active": true, "payload_capture_active": captureEnabled, "response_cache_configured": s.responseCacheConfigured, "semantic_cache_vectors_unsealed": true}), err
}

func (s *accountService) setup(r *http.Request) (httpkit.Reply, error) {
	if err := admitAuth(s.access, r, "setup", ""); err != nil {
		return httpkit.Reply{}, err
	}
	token, configured := s.bootstrap()
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
	if err = s.passwords.run(r, func() { hash = secrets.HashPassword(input.Password) }); err != nil {
		return httpkit.Reply{}, err
	}
	tx, err := s.access.Begin(r)
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
	id := ids.New()
	if _, err = tx.Exec(r.Context(), "INSERT INTO olp.users(id,email,display_name,password_hash,role,etag) VALUES($1,$2,$3,$4,'owner',$5)", id, address, strings.TrimSpace(input.DisplayName), hash, ids.New()); err != nil {
		return httpkit.Reply{}, err
	}
	if _, err = tx.Exec(r.Context(), "UPDATE olp.installation SET name=$1,setup_complete=true WHERE singleton", strings.TrimSpace(input.Name)); err != nil {
		return httpkit.Reply{}, err
	}
	for key, value := range map[string]string{"retention.requests_days": "30", "retention.usage_days": "90", "retention.audit_days": "365", "limits.valkey_unavailable": "fail_closed", "budgets.time_zone": "UTC", "guardrails.stream_window_bytes": "65536", "auth.mfa_required": "false", "auth.local_login_enabled": "true"} {
		if _, err = tx.Exec(r.Context(), "INSERT INTO olp.settings(key,value,etag,updated_by) VALUES($1,$2,$3,$4)", key, value, ids.New(), id); err != nil {
			return httpkit.Reply{}, err
		}
	}
	if err = access.Audit(r.Context(), tx, r, authz.UserActor(id), "installation.setup", "installation", s.access.Installation, "success"); err != nil {
		return httpkit.Reply{}, err
	}
	response, err := s.sessions.newSession(r, tx, id)
	if err != nil {
		return httpkit.Reply{}, err
	}
	return response, tx.Commit(r.Context())
}

func (s *accountService) users(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	p, err := httpkit.Page(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	rows, err := s.access.Pool.Query(r.Context(), "SELECT to_jsonb(u)-'password_hash'-'role_management'-'oidc_authorized' FROM olp.users u WHERE id<$1 ORDER BY id DESC LIMIT $2", p.Before, p.Limit+1)
	if err != nil {
		return httpkit.Reply{}, err
	}
	items, err := httpkit.JSONRows(rows)
	return httpkit.ListReply(items, p), err
}
func (s *accountService) user(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "user_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	u, err := access.ScanUser(s.access.Pool.QueryRow(r.Context(), "SELECT "+access.UserColumns+" FROM olp.users u WHERE id=$1", id))
	return httpkit.Detail(u, u.ETag), err
}
func (s *accountService) updateUser(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
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
	return access.Transact(s.access, r, access.Unlocked, func(tx pgx.Tx, p authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		u, err := access.ScanUser(tx.QueryRow(r.Context(), "SELECT "+access.UserColumns+" FROM olp.users u WHERE id=$1", id))
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if err = httpkit.Match(r, u.ETag); err != nil {
			return httpkit.Reply{}, access.Event{}, err
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
			return httpkit.Reply{}, access.Event{}, httpkit.Fail(409, "cannot_change_current_user_access", "Ask another owner to change your access.")
		}
		u.ETag = ids.New()
		// Taking over a provisioned user ends the project grants their SCIM groups
		// gave them, which must not leave a project without a manager.
		managed, err := scimManagedProjects(r.Context(), tx)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if _, err = tx.Exec(r.Context(), "UPDATE olp.users SET role=$1,active=$2,access_scope=$3,etag=$4,updated_at=now(),role_management=CASE WHEN role_management='provisioned' THEN 'local' ELSE role_management END WHERE id=$5", u.Role, u.Active, u.AccessScope, u.ETag, id); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if err = EnsureUsableOwner(r.Context(), tx, s.access.LocalLoginDisabled()); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if err = keepSCIMManagers(r.Context(), tx, managed); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if _, err = tx.Exec(r.Context(), "DELETE FROM olp.sessions WHERE user_id=$1", id); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if !u.Active || u.Role != "owner" || u.AccessScope != "global" {
			if err = retireIssuedInvitations(r.Context(), tx, r, id, p.Actor(), p.UserID()); err != nil {
				return httpkit.Reply{}, access.Event{}, err
			}
		}
		// Issuance is installation-scoped. Existing keys retain their issuer and
		// policy across account role changes and disabling, as in the reference.
		if _, err = access.AdvanceAuthority(r.Context(), tx); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		u, err = access.ScanUser(tx.QueryRow(r.Context(), "SELECT "+access.UserColumns+" FROM olp.users u WHERE id=$1", id))
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		return httpkit.Detail(u, u.ETag), access.Event{Action: "user.update", Resource: "user", ID: id}, nil
	})
}

// EnsureUsableOwner refuses a change that would leave the installation
// without an active owner who can still sign in.
func EnsureUsableOwner(ctx context.Context, tx pgx.Tx, localLoginDisabled bool) error {
	var exists bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.users WHERE active AND oidc_authorized AND role='owner'
        AND access_scope='global' AND password_hash IS NOT NULL AND $1 AND COALESCE((SELECT value='true' FROM olp.settings WHERE key='auth.local_login_enabled'),true))`, !localLoginDisabled).Scan(&exists)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	if usable, e := usableSAMLOwner(ctx, tx); e != nil {
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

func (s *accountService) requireInvitationLogin(r *http.Request, q database.Queryer) error {
	enabled, err := isLocalLoginEnabled(r.Context(), s.access, q)
	if err != nil {
		return err
	}
	if !enabled {
		return httpkit.Fail(409, "invitation_local_login_required", "Password invitations require local sign-in. Ask an owner to configure an OIDC role mapping, then sign in with single sign-on, or enable local sign-in before using this invitation.")
	}
	return nil
}
