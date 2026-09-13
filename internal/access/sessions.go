package access

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/secrets"
)

func rawJSON(data []byte) any { return json.RawMessage(data) }
func (s *Server) sessionBody(r *http.Request, q queryer, u User, token string) (any, error) {
	var name string
	err := q.QueryRow(r.Context(), "SELECT name FROM olp_go.installation WHERE singleton").Scan(&name)
	return map[string]any{"user": map[string]any{"id": u.ID, "email": u.Email, "display_name": u.DisplayName, "role": u.Role}, "installation_name": name, "csrf_token": s.csrf(token)}, err
}
func (s *Server) newSession(r *http.Request, tx pgx.Tx, userID string) (reply, error) {
	token := secrets.Token()
	id := newID()
	if _, err := tx.Exec(r.Context(), "DELETE FROM olp_go.sessions WHERE id IN(SELECT id FROM olp_go.sessions WHERE expires_at<=now() LIMIT 100)"); err != nil {
		return reply{}, err
	}
	// Bound active sessions per account without retaining expired credentials.
	if _, err := tx.Exec(r.Context(), "DELETE FROM olp_go.sessions WHERE id IN(SELECT id FROM olp_go.sessions WHERE user_id=$1 ORDER BY created_at DESC OFFSET 19)", userID); err != nil {
		return reply{}, err
	}
	if _, err := tx.Exec(r.Context(), "INSERT INTO olp_go.sessions(id,user_id,digest,expires_at) VALUES($1,$2,$3,$4)", id, userID, s.Auth.Digest("session", token), time.Now().Add(sessionTTL)); err != nil {
		return reply{}, err
	}
	u, err := scanUser(tx.QueryRow(r.Context(), "SELECT "+userColumns+" FROM olp_go.users u WHERE id=$1", userID))
	if err != nil {
		return reply{}, err
	}
	body, err := s.sessionBody(r, tx, u, token)
	return reply{Status: 201, Body: body, CSRF: s.csrf(token), Cookies: []*http.Cookie{cookie(sessionCookie, token, sessionTTL, true), cookie(csrfCookie, s.csrf(token), sessionTTL, false), clearCookie(recentCookie)}}, err
}
func (s *Server) login(r *http.Request) (reply, error) {
	var input struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := decode(r, &input); err != nil {
		return reply{}, err
	}
	input.Email = strings.ToLower(strings.TrimSpace(input.Email))
	if err := s.admit(r, "login", input.Email); err != nil {
		return reply{}, err
	}
	var id string
	var hash *string
	err := s.Pool.QueryRow(r.Context(), "SELECT id::text,password_hash FROM olp_go.users WHERE email=$1 AND active", input.Email).Scan(&id, &hash)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return reply{}, err
	}
	encoded := s.dummyPassword
	if hash != nil {
		encoded = *hash
	}
	var valid bool
	if err = s.passwordWork(r, func() { valid = secrets.VerifyPassword(input.Password, encoded) }); err != nil {
		return reply{}, err
	}
	tx, err := s.begin(r)
	if err != nil {
		return reply{}, err
	}
	defer tx.Rollback(r.Context())
	var local bool
	if err = tx.QueryRow(r.Context(), "SELECT COALESCE((SELECT value='true' FROM olp_go.settings WHERE key='auth.local_login_enabled'),true)").Scan(&local); err != nil {
		return reply{}, err
	}
	var current bool
	if id != "" {
		if err = tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM olp_go.users WHERE id=$1 AND active AND password_hash=$2)", id, encoded).Scan(&current); err != nil {
			return reply{}, err
		}
	}
	if !valid || !current || !local {
		if err = audit(r.Context(), tx, r, "", "session.login", "session", "", "failure"); err != nil {
			return reply{}, err
		}
		if err = tx.Commit(r.Context()); err != nil {
			return reply{}, err
		}
		return reply{}, fail(401, "invalid_credentials", "Email or password is invalid, or local sign-in is unavailable.")
	}
	response, err := s.newSession(r, tx, id)
	if err != nil {
		return reply{}, err
	}
	if err = audit(r.Context(), tx, r, id, "session.login", "user", id, "success"); err != nil {
		return reply{}, err
	}
	return commit(r, tx, response)
}
func (s *Server) currentSession(r *http.Request) (reply, error) {
	p, err := s.principal(r, s.Pool, "read")
	if err != nil {
		return reply{}, err
	}
	if _, err = s.Pool.Exec(r.Context(), "UPDATE olp_go.sessions SET last_seen_at=now() WHERE id=$1 AND last_seen_at<now()-interval '1 minute'", p.SessionID); err != nil {
		return reply{}, err
	}
	body, err := s.sessionBody(r, s.Pool, p.User, p.Token)
	return ok(body), err
}
func (s *Server) sessions(r *http.Request) (reply, error) {
	p, err := s.principal(r, s.Pool, "read")
	if err != nil {
		return reply{}, err
	}
	pagination, err := page(r)
	if err != nil {
		return reply{}, err
	}
	userID := r.URL.Query().Get("user_id")
	if userID == "" {
		userID = p.ID
	}
	if userID != p.ID && p.Role != "owner" {
		return reply{}, forbidden()
	}
	if _, err := parseUUID(userID); err != nil {
		return reply{}, err
	}
	rows, err := s.Pool.Query(r.Context(), "SELECT jsonb_build_object('id',id,'user_id',user_id,'current',id=$1,'expires_at',expires_at,'last_seen_at',last_seen_at,'created_at',created_at) FROM olp_go.sessions WHERE user_id=$2 AND expires_at>now() AND id<$3 ORDER BY id DESC LIMIT $4", p.SessionID, userID, pagination.Before, pagination.Limit+1)
	if err != nil {
		return reply{}, err
	}
	items, err := jsonRows(rows)
	return listReply(items, pagination), err
}
func (s *Server) logout(r *http.Request) (reply, error)        { return s.deleteSession(r, true) }
func (s *Server) revokeSession(r *http.Request) (reply, error) { return s.deleteSession(r, false) }
func (s *Server) deleteSession(r *http.Request, current bool) (reply, error) {
	tx, err := s.begin(r)
	if err != nil {
		return reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.principal(r, tx, "read")
	if err != nil {
		return reply{}, err
	}
	id := p.SessionID
	if !current {
		id, err = idParam(r, "session_id")
		if err != nil {
			return reply{}, err
		}
	}
	var userID string
	if err = tx.QueryRow(r.Context(), "SELECT user_id::text FROM olp_go.sessions WHERE id=$1", id).Scan(&userID); err != nil {
		return reply{}, err
	}
	if userID != p.ID && p.Role != "owner" {
		return reply{}, forbidden()
	}
	if _, err = tx.Exec(r.Context(), "DELETE FROM olp_go.sessions WHERE id=$1", id); err != nil {
		return reply{}, err
	}
	if err = audit(r.Context(), tx, r, p.ID, "session.revoke", "session", id, "success"); err != nil {
		return reply{}, err
	}
	response := reply{Status: 204}
	if id == p.SessionID {
		response.Cookies = []*http.Cookie{clearCookie(sessionCookie), clearCookie(csrfCookie), clearCookie(recentCookie)}
	}
	return commit(r, tx, response)
}

func (s *Server) profile(r *http.Request) (reply, error) {
	p, err := s.principal(r, s.Pool, "read")
	return detail(p.User, p.ETag), err
}
func (s *Server) updateProfile(r *http.Request) (reply, error) {
	var input struct {
		Name string `json:"display_name"`
	}
	if err := decode(r, &input); err != nil {
		return reply{}, err
	}
	if err := validText("display_name", input.Name, 100); err != nil {
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
	if err = match(r, p.ETag); err != nil {
		return reply{}, err
	}
	if _, err = tx.Exec(r.Context(), "UPDATE olp_go.users SET display_name=$1,etag=$2,updated_at=now() WHERE id=$3", strings.TrimSpace(input.Name), newID(), p.ID); err != nil {
		return reply{}, err
	}
	if err = audit(r.Context(), tx, r, p.ID, "profile.update", "user", p.ID, "success"); err != nil {
		return reply{}, err
	}
	u, err := scanUser(tx.QueryRow(r.Context(), "SELECT "+userColumns+" FROM olp_go.users u WHERE id=$1", p.ID))
	if err != nil {
		return reply{}, err
	}
	return commit(r, tx, detail(u, u.ETag))
}
func (s *Server) changePassword(r *http.Request) (reply, error) { return s.writePassword(r, false) }
func (s *Server) enrollPassword(r *http.Request) (reply, error) { return s.writePassword(r, true) }
func (s *Server) writePassword(r *http.Request, enroll bool) (reply, error) {
	var input struct {
		Current string `json:"current_password"`
		New     string `json:"new_password"`
	}
	if err := decode(r, &input); err != nil {
		return reply{}, err
	}
	if err := password(input.New); err != nil {
		return reply{}, err
	}
	p, err := s.principal(r, s.Pool, "read")
	if err != nil {
		return reply{}, err
	}
	if err = s.admit(r, "password", p.ID); err != nil {
		return reply{}, err
	}
	var stored *string
	if err = s.Pool.QueryRow(r.Context(), "SELECT password_hash FROM olp_go.users WHERE id=$1", p.ID).Scan(&stored); err != nil {
		return reply{}, err
	}
	if enroll && stored != nil {
		return reply{}, fail(409, "password_already_enrolled", "A local password is already configured.")
	}
	if !enroll && stored == nil {
		return reply{}, fail(403, "local_password_unavailable", "This profile has no local password.")
	}
	var hash string
	valid := enroll
	if err = s.passwordWork(r, func() {
		if !enroll {
			valid = secrets.VerifyPassword(input.Current, *stored)
		}
		if valid {
			hash = secrets.HashPassword(input.New)
		}
	}); err != nil {
		return reply{}, err
	}
	if !valid {
		return reply{}, fail(403, "current_password_invalid", "The current password is invalid.")
	}
	tx, err := s.begin(r)
	if err != nil {
		return reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err = s.principal(r, tx, "read")
	if err != nil {
		return reply{}, err
	}
	if err = match(r, p.ETag); err != nil {
		return reply{}, err
	}
	if enroll {
		if err = s.consumeRecent(r, tx, p, "password_enrollment", ""); err != nil {
			return reply{}, err
		}
	}
	if _, err = tx.Exec(r.Context(), "UPDATE olp_go.users SET password_hash=$1,etag=$2,updated_at=now() WHERE id=$3", hash, newID(), p.ID); err != nil {
		return reply{}, err
	}
	if _, err = tx.Exec(r.Context(), "DELETE FROM olp_go.sessions WHERE user_id=$1", p.ID); err != nil {
		return reply{}, err
	}
	response, err := s.newSession(r, tx, p.ID)
	if err != nil {
		return reply{}, err
	}
	u, err := scanUser(tx.QueryRow(r.Context(), "SELECT "+userColumns+" FROM olp_go.users u WHERE id=$1", p.ID))
	if err != nil {
		return reply{}, err
	}
	response.Status = 200
	response.Body = u
	response.ETag = u.ETag
	if err = audit(r.Context(), tx, r, p.ID, "profile.password.update", "user", p.ID, "success"); err != nil {
		return reply{}, err
	}
	return commit(r, tx, response)
}
func validatePurpose(purpose, resource string) error {
	switch purpose {
	case "password_enrollment", "oidc_link":
		if resource == "" {
			return nil
		}
	case "oidc_unlink":
		_, err := parseUUID(resource)
		return err
	}
	return invalid("purpose", "Use password_enrollment, oidc_link, or oidc_unlink with its identity ID.")
}
func (s *Server) reauthenticate(r *http.Request) (reply, error) {
	var input struct {
		Password string `json:"current_password"`
		Purpose  string `json:"purpose"`
		Resource string `json:"resource_id"`
	}
	if err := decode(r, &input); err != nil {
		return reply{}, err
	}
	if err := validatePurpose(input.Purpose, input.Resource); err != nil {
		return reply{}, err
	}
	p, err := s.principal(r, s.Pool, "read")
	if err != nil {
		return reply{}, err
	}
	if err = s.admit(r, "reauthentication", p.ID); err != nil {
		return reply{}, err
	}
	var hash *string
	if err = s.Pool.QueryRow(r.Context(), "SELECT password_hash FROM olp_go.users WHERE id=$1", p.ID).Scan(&hash); err != nil {
		return reply{}, err
	}
	if hash == nil {
		return reply{}, fail(403, "local_password_unavailable", "Use your linked identity to authenticate.")
	}
	var valid bool
	if err = s.passwordWork(r, func() { valid = secrets.VerifyPassword(input.Password, *hash) }); err != nil {
		return reply{}, err
	}
	if !valid {
		return reply{}, fail(403, "current_password_invalid", "The current password is invalid.")
	}
	tx, err := s.begin(r)
	if err != nil {
		return reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err = s.principal(r, tx, "read")
	if err != nil {
		return reply{}, err
	}
	response, err := s.grantRecent(r, tx, p, input.Purpose, input.Resource)
	if err != nil {
		return reply{}, err
	}
	return commit(r, tx, response)
}
func (s *Server) grantRecent(r *http.Request, tx pgx.Tx, p principal, purpose, resource string) (reply, error) {
	token := secrets.Token()
	var target any
	if resource != "" {
		target = resource
	}
	if _, err := tx.Exec(r.Context(), "DELETE FROM olp_go.recent_auth WHERE session_id=$1 OR expires_at<=now()", p.SessionID); err != nil {
		return reply{}, err
	}
	_, err := tx.Exec(r.Context(), "INSERT INTO olp_go.recent_auth(digest,session_id,purpose,resource_id,expires_at) VALUES($1,$2,$3,$4,now()+interval '5 minutes')", s.Auth.Digest("recent_auth", token), p.SessionID, purpose, target)
	return reply{Status: 204, Cookies: []*http.Cookie{cookie(recentCookie, token, 5*time.Minute, true)}}, err
}
func (s *Server) consumeRecent(r *http.Request, tx pgx.Tx, p principal, purpose, resource string) error {
	var target any
	if resource != "" {
		target = resource
	}
	tag, err := tx.Exec(r.Context(), "DELETE FROM olp_go.recent_auth WHERE digest=$1 AND session_id=$2 AND purpose=$3 AND resource_id IS NOT DISTINCT FROM $4::uuid AND expires_at>now()", s.Auth.Digest("recent_auth", cookieValue(r, recentCookie)), p.SessionID, purpose, target)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fail(428, "reauthentication_required", "Confirm your identity again before this operation.")
	}
	return nil
}
