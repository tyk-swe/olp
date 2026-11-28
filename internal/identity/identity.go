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
	"github.com/tyk-swe/olp/internal/domain/problem"
	"github.com/tyk-swe/olp/internal/domain/textfield"
	"github.com/tyk-swe/olp/internal/platform/secrets"
)

type setupStatusResponse struct {
	SetupRequired            bool `json:"setup_required"`
	BootstrapTokenConfigured bool `json:"bootstrap_token_configured"`
}

type setupInput struct {
	email            string
	password         string
	displayName      string
	installationName string
}

type updateUserInput struct {
	Role        *string `json:"role"`
	Active      *bool   `json:"active"`
	AccessScope *string `json:"access_scope"`
}

var defaultInstallationSettings = []struct{ key, value string }{
	{"retention.requests_days", "30"},
	{"retention.usage_days", "90"},
	{"retention.audit_days", "365"},
	{"limits.valkey_unavailable", "fail_closed"},
	{"budgets.time_zone", "UTC"},
	{"guardrails.stream_window_bytes", "65536"},
	{"auth.mfa_required", "false"},
	{"auth.local_login_enabled", "true"},
}

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
	return httpkit.OK(setupStatusResponse{
		SetupRequired:            !complete,
		BootstrapTokenConfigured: configured,
	}), err
}

func parseSetupInput(r *http.Request) (setupInput, error) {
	var raw struct {
		Email       string `json:"email"`
		Password    string `json:"password"`
		DisplayName string `json:"display_name"`
		Name        string `json:"installation_name"`
	}
	if err := httpkit.Decode(r, &raw); err != nil {
		return setupInput{}, err
	}
	address, err := email(raw.Email)
	if err != nil {
		return setupInput{}, err
	}
	if err := password(raw.Password); err != nil {
		return setupInput{}, err
	}
	if err := textfield.Validate("display_name", raw.DisplayName, 100); err != nil {
		return setupInput{}, err
	}
	installationName := raw.Name
	if installationName == "" {
		installationName = "OpenLLMProxy"
	}
	if err := textfield.Validate("installation_name", installationName, 100); err != nil {
		return setupInput{}, err
	}
	return setupInput{
		email:            address,
		password:         raw.Password,
		displayName:      strings.TrimSpace(raw.DisplayName),
		installationName: strings.TrimSpace(installationName),
	}, nil
}

func (s *accountService) setup(r *http.Request) (httpkit.Reply, error) {
	if err := admitAuth(s.access, r, "setup", ""); err != nil {
		return httpkit.Reply{}, err
	}
	token, configured := s.bootstrap()
	if !configured || !hmac.Equal([]byte(token), []byte(r.Header.Get("X-OLP-Setup-Token"))) {
		return httpkit.Reply{}, problem.Fail(403, "setup_token_invalid", "The bootstrap token is invalid.")
	}
	input, err := parseSetupInput(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	var hash string
	if err := s.passwords.run(r, func() { hash = secrets.HashPassword(input.password) }); err != nil {
		return httpkit.Reply{}, err
	}
	return access.Serialize(s.access, r, func(tx pgx.Tx) (httpkit.Reply, bool, error) {
		reply, err := s.completeSetup(r, tx, input, hash)
		return reply, err == nil, err
	})
}

func (s *accountService) completeSetup(r *http.Request, tx pgx.Tx, input setupInput, hash string) (httpkit.Reply, error) {
	var complete bool
	if err := tx.QueryRow(r.Context(), "SELECT setup_complete FROM olp.installation WHERE singleton").Scan(&complete); err != nil {
		return httpkit.Reply{}, err
	}
	if complete {
		return httpkit.Reply{}, problem.Fail(409, "setup_complete", "This installation already has an owner.")
	}
	id := ids.New()
	if _, err := tx.Exec(r.Context(), "INSERT INTO olp.users(id,email,display_name,password_hash,role,etag) VALUES($1,$2,$3,$4,'owner',$5)", id, input.email, input.displayName, hash, ids.New()); err != nil {
		return httpkit.Reply{}, err
	}
	if _, err := tx.Exec(r.Context(), "UPDATE olp.installation SET name=$1,setup_complete=true WHERE singleton", input.installationName); err != nil {
		return httpkit.Reply{}, err
	}
	for _, setting := range defaultInstallationSettings {
		if _, err := tx.Exec(r.Context(), "INSERT INTO olp.settings(key,value,etag,updated_by) VALUES($1,$2,$3,$4)", setting.key, setting.value, ids.New(), id); err != nil {
			return httpkit.Reply{}, err
		}
	}
	if err := access.Audit(r.Context(), tx, r, authz.UserActor(id), "installation.setup", "installation", s.access.Installation, "success"); err != nil {
		return httpkit.Reply{}, err
	}
	return s.sessions.newSession(r, tx, id)
}

func (s *accountService) users(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	p, err := httpkit.Page(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	rows, err := s.access.Pool.Query(r.Context(), "SELECT "+access.UserColumns+" FROM olp.users u WHERE u.id<$1 ORDER BY u.id DESC LIMIT $2", p.Before, p.Limit+1)
	if err != nil {
		return httpkit.Reply{}, err
	}
	defer rows.Close()
	items := []authz.User{}
	for rows.Next() {
		u, err := access.ScanUser(rows)
		if err != nil {
			return httpkit.Reply{}, err
		}
		items = append(items, u)
	}
	if err := rows.Err(); err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.ListReplyBy(items, p, func(u authz.User) string { return u.ID }), nil
}

func (s *accountService) user(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "user_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	u, err := access.ScanUser(s.access.Pool.QueryRow(r.Context(), "SELECT "+access.UserColumns+" FROM olp.users u WHERE id=$1", id))
	return httpkit.Detail(u, u.ETag), err
}

func parseUpdateUserInput(r *http.Request) (updateUserInput, error) {
	var input updateUserInput
	if err := httpkit.Decode(r, &input); err != nil {
		return updateUserInput{}, err
	}
	if input.Role == nil && input.Active == nil && input.AccessScope == nil {
		return updateUserInput{}, problem.Invalid("user", "Provide a role or active status.")
	}
	if input.Role != nil && !authz.ValidRole(*input.Role) {
		return updateUserInput{}, problem.Invalid("role", "Use owner, operator, developer, or viewer.")
	}
	if input.AccessScope != nil && *input.AccessScope != "global" && *input.AccessScope != "assigned" {
		return updateUserInput{}, problem.Invalid("access_scope", "Use global or assigned.")
	}
	return input, nil
}

func nextUserState(current authz.User, input updateUserInput) authz.User {
	role := current.Role
	if input.Role != nil {
		role = *input.Role
	}
	active := current.Active
	if input.Active != nil {
		active = *input.Active
	}
	scope := current.AccessScope
	if input.AccessScope != nil {
		scope = *input.AccessScope
	}
	return authz.User{
		ID:          current.ID,
		Email:       current.Email,
		DisplayName: current.DisplayName,
		Role:        role,
		Active:      active,
		AccessScope: scope,
		ETag:        ids.New(),
		CreatedAt:   current.CreatedAt,
		UpdatedAt:   current.UpdatedAt,
	}
}

func (s *accountService) updateUser(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	input, err := parseUpdateUserInput(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	id, err := httpkit.IDParam(r, "user_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	return access.Transact(s.access, r, access.Unlocked, func(tx pgx.Tx, p authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		return s.applyUserChange(r, tx, p, id, input)
	})
}

func (s *accountService) applyUserChange(r *http.Request, tx pgx.Tx, p authz.Principal, id string, input updateUserInput) (httpkit.Reply, access.Event, error) {
	current, err := access.ScanUser(tx.QueryRow(r.Context(), "SELECT "+access.UserColumns+" FROM olp.users u WHERE id=$1", id))
	if err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	if err := httpkit.Match(r, current.ETag); err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	next := nextUserState(current, input)
	if id == p.ID && (!next.Active || next.Role != p.Role || next.AccessScope != p.AccessScope) {
		return httpkit.Reply{}, access.Event{}, problem.Fail(409, "cannot_change_current_user_access", "Ask another owner to change your access.")
	}
	// Taking over a provisioned user ends the project grants their SCIM groups
	// gave them, which must not leave a project without a manager.
	managed, err := scimManagedProjects(r.Context(), tx)
	if err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	if _, err := tx.Exec(r.Context(), "UPDATE olp.users SET role=$1,active=$2,access_scope=$3,etag=$4,updated_at=now(),role_management=CASE WHEN role_management='provisioned' THEN 'local' ELSE role_management END WHERE id=$5", next.Role, next.Active, next.AccessScope, next.ETag, id); err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	if err := EnsureUsableOwner(r.Context(), tx, s.access.LocalLoginDisabled()); err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	if err := keepSCIMManagers(r.Context(), tx, managed); err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	if _, err := tx.Exec(r.Context(), "DELETE FROM olp.sessions WHERE user_id=$1", id); err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	if !next.Active || next.Role != "owner" || next.AccessScope != "global" {
		if err := retireIssuedInvitations(r.Context(), tx, r, id, p.Actor(), p.UserID()); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
	}
	if _, err := access.AdvanceAuthority(r.Context(), tx); err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	reloaded, err := access.ScanUser(tx.QueryRow(r.Context(), "SELECT "+access.UserColumns+" FROM olp.users u WHERE id=$1", id))
	if err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	return httpkit.Detail(reloaded, reloaded.ETag), access.Event{Action: "user.update", Resource: "user", ID: id}, nil
}

func EnsureUsableOwner(ctx context.Context, tx pgx.Tx, localLoginDisabled bool) error {
	var exists bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.users WHERE active AND oidc_authorized AND role='owner'
        AND access_scope='global' AND password_hash IS NOT NULL AND $1 AND COALESCE((SELECT value='true' FROM olp.settings WHERE key='auth.local_login_enabled'),true))`, !localLoginDisabled).Scan(&exists)
	if err != nil || exists {
		return err
	}
	if usable, err := usableSAMLOwner(ctx, tx); err != nil || usable {
		return err
	}
	if usable, err := usableOIDCOwner(ctx, tx); err != nil || usable {
		return err
	}
	return problem.Fail(409, "last_usable_owner", "Keep at least one active owner with a usable sign-in method and an owner role after sign-in.")
}

func usableOIDCOwner(ctx context.Context, tx pgx.Tx) (bool, error) {
	c, err := loadOIDC(ctx, tx)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !c.Enabled) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	rows, err := tx.Query(ctx, `SELECT u.role_management<>'oidc',i.role_claims
        FROM olp.users u JOIN olp.oidc_identities i ON i.user_id=u.id
        WHERE u.active AND u.oidc_authorized AND u.role='owner' AND u.access_scope='global' AND i.issuer=$1`, c.Issuer)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var local bool
		var data []byte
		if err := rows.Scan(&local, &data); err != nil {
			return false, err
		}
		role, err := oidcSignInRole(c, local, "owner", data)
		if err != nil {
			return false, err
		}
		if role == "owner" {
			return true, nil
		}
	}
	return false, rows.Err()
}

func (s *accountService) verifyLocalLogin(r *http.Request, address, secret string) (string, string, bool, error) {
	var id string
	var hash *string
	err := s.access.Pool.QueryRow(r.Context(), "SELECT id::text,password_hash FROM olp.users WHERE email=$1 AND active AND oidc_authorized", address).Scan(&id, &hash)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", "", false, err
	}
	encoded := s.passwords.dummyHash
	if hash != nil {
		encoded = *hash
	}
	var valid bool
	if err := s.passwords.run(r, func() { valid = secrets.VerifyPassword(secret, encoded) }); err != nil {
		return "", "", false, err
	}
	return id, encoded, valid, nil
}

func (s *accountService) completeLogin(r *http.Request, tx pgx.Tx, userID, encoded string, valid bool) (httpkit.Reply, bool, error) {
	local, err := isLocalLoginEnabled(r.Context(), s.access, tx)
	if err != nil {
		return httpkit.Reply{}, false, err
	}
	var current bool
	if userID != "" {
		if err := tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM olp.users WHERE id=$1 AND active AND oidc_authorized AND password_hash=$2)", userID, encoded).Scan(&current); err != nil {
			return httpkit.Reply{}, false, err
		}
	}
	if !valid || !current || !local {
		if err := access.Audit(r.Context(), tx, r, authz.System, "session.login", "session", "", "failure"); err != nil {
			return httpkit.Reply{}, false, err
		}
		return httpkit.Reply{}, true, problem.Fail(401, "invalid_credentials", "Email or password is invalid, or local sign-in is unavailable.")
	}
	response, err := s.mfa.localSession(r, tx, userID)
	if err != nil {
		return httpkit.Reply{}, false, err
	}
	action := "session.login"
	if response.Status == 202 {
		action = "mfa.challenge"
	}
	if err := access.Audit(r.Context(), tx, r, authz.UserActor(userID), action, "user", userID, "success"); err != nil {
		return httpkit.Reply{}, false, err
	}
	return response, true, nil
}

func (s *accountService) login(r *http.Request) (httpkit.Reply, error) {
	if s.access.LocalLoginDisabled() {
		return httpkit.Reply{}, problem.Fail(404, "local_login_disabled", "Password-based local sign-in is disabled for this installation.")
	}
	var input struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := httpkit.Decode(r, &input); err != nil {
		return httpkit.Reply{}, err
	}
	address := strings.ToLower(strings.TrimSpace(input.Email))
	if err := admitAuth(s.access, r, "login", address); err != nil {
		return httpkit.Reply{}, err
	}
	userID, encoded, valid, err := s.verifyLocalLogin(r, address, input.Password)
	if err != nil {
		return httpkit.Reply{}, err
	}
	return access.Serialize(s.access, r, func(tx pgx.Tx) (httpkit.Reply, bool, error) {
		return s.completeLogin(r, tx, userID, encoded, valid)
	})
}

func (s *accountService) verifyReauthPassword(r *http.Request, userID, secret string) (string, error) {
	var hash *string
	if err := s.access.Pool.QueryRow(r.Context(), "SELECT password_hash FROM olp.users WHERE id=$1", userID).Scan(&hash); err != nil {
		return "", err
	}
	if hash == nil {
		return "", problem.Fail(403, "local_password_unavailable", "Use your linked identity to authenticate.")
	}
	var valid bool
	if err := s.passwords.run(r, func() { valid = secrets.VerifyPassword(secret, *hash) }); err != nil {
		return "", err
	}
	if !valid {
		return "", problem.Fail(403, "current_password_invalid", "The current password is invalid.")
	}
	return *hash, nil
}

func (s *accountService) reauthenticate(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	var input struct {
		Password string `json:"current_password"`
		Purpose  string `json:"purpose"`
		Resource string `json:"resource_id"`
	}
	if err := httpkit.Decode(r, &input); err != nil {
		return httpkit.Reply{}, err
	}
	if err := validatePurpose(input.Purpose, input.Resource); err != nil {
		return httpkit.Reply{}, err
	}
	if err := admitAuth(s.access, r, "reauthentication", p.ID); err != nil {
		return httpkit.Reply{}, err
	}
	hash, err := s.verifyReauthPassword(r, p.ID, input.Password)
	if err != nil {
		return httpkit.Reply{}, err
	}
	return access.Transact(s.access, r, access.Unlocked, func(tx pgx.Tx, p authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		var current bool
		if err := tx.QueryRow(r.Context(), "SELECT COALESCE(password_hash=$2,false) FROM olp.users WHERE id=$1", p.ID, hash).Scan(&current); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if !current {
			return httpkit.Reply{}, access.Event{}, problem.Fail(403, "current_password_invalid", "Authenticate again with the current password.")
		}
		configured, err := mfaConfigured(r.Context(), tx, p.ID)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if configured {
			reply, err := s.mfa.mfaChallenge(r, tx, p.ID, "reauth", &p.SessionID, mfaFlowData{RecentPurpose: input.Purpose, RecentResource: input.Resource})
			return reply, access.Event{}, err
		}
		response, err := s.sessions.grantRecent(r, tx, p, input.Purpose, input.Resource, false)
		return response, access.Event{Action: "session.reauthenticate", Resource: "session", ID: p.SessionID}, err
	})
}
