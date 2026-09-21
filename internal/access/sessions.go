package access

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/secrets"
)

func (s *Server) sessionBody(r *http.Request, q Queryer, u User, token string) (any, error) {
	var name string
	err := q.QueryRow(r.Context(), "SELECT name FROM olp_go.installation WHERE singleton").Scan(&name)
	return map[string]any{"user": map[string]any{"id": u.ID, "email": u.Email, "display_name": u.DisplayName, "role": u.Role, "access_scope": u.AccessScope}, "installation_name": name, "csrf_token": s.csrf(token)}, err
}
func (s *Server) newSession(r *http.Request, tx pgx.Tx, userID string) (Reply, error) {
	token := secrets.Token()
	id := NewID()
	if _, err := tx.Exec(r.Context(), "DELETE FROM olp_go.sessions WHERE id IN(SELECT id FROM olp_go.sessions WHERE expires_at<=now() LIMIT 100)"); err != nil {
		return Reply{}, err
	}
	// Bound active sessions per account without retaining expired credentials.
	if _, err := tx.Exec(r.Context(), "DELETE FROM olp_go.sessions WHERE id IN(SELECT id FROM olp_go.sessions WHERE user_id=$1 ORDER BY created_at DESC OFFSET 19)", userID); err != nil {
		return Reply{}, err
	}
	if _, err := tx.Exec(r.Context(), "INSERT INTO olp_go.sessions(id,user_id,digest,expires_at,browser_hint) VALUES($1,$2,$3,$4,$5)", id, userID, s.Auth.Digest("session", token), time.Now().Add(sessionTTL), browserHint(r.UserAgent())); err != nil {
		return Reply{}, err
	}
	u, err := scanUser(tx.QueryRow(r.Context(), "SELECT "+userColumns+" FROM olp_go.users u WHERE id=$1", userID))
	if err != nil {
		return Reply{}, err
	}
	body, err := s.sessionBody(r, tx, u, token)
	return Reply{Status: 201, Body: body, CSRF: s.csrf(token), Cookies: []*http.Cookie{cookie(sessionCookie, token, sessionTTL, true), cookie(csrfCookie, s.csrf(token), sessionTTL, false), clearCookie(recentCookie)}}, err
}
func (s *Server) login(r *http.Request) (Reply, error) {
	if s.LocalLoginDisabled {
		return Reply{}, Fail(404, "local_login_disabled", "Password-based local sign-in is disabled for this installation.")
	}
	var input struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := Decode(r, &input); err != nil {
		return Reply{}, err
	}
	input.Email = strings.ToLower(strings.TrimSpace(input.Email))
	if err := s.admit(r, "login", input.Email); err != nil {
		return Reply{}, err
	}
	var id string
	var hash *string
	err := s.Pool.QueryRow(r.Context(), "SELECT id::text,password_hash FROM olp_go.users WHERE email=$1 AND active AND oidc_authorized", input.Email).Scan(&id, &hash)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Reply{}, err
	}
	encoded := s.dummyPassword
	if hash != nil {
		encoded = *hash
	}
	var valid bool
	if err = s.passwordWork(r, func() { valid = secrets.VerifyPassword(input.Password, encoded) }); err != nil {
		return Reply{}, err
	}
	tx, err := s.Begin(r)
	if err != nil {
		return Reply{}, err
	}
	defer tx.Rollback(r.Context())
	local, err := s.localLoginEnabled(r, tx)
	if err != nil {
		return Reply{}, err
	}
	var current bool
	if id != "" {
		if err = tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM olp_go.users WHERE id=$1 AND active AND oidc_authorized AND password_hash=$2)", id, encoded).Scan(&current); err != nil {
			return Reply{}, err
		}
	}
	if !valid || !current || !local {
		if err = Audit(r.Context(), tx, r, "", "session.login", "session", "", "failure"); err != nil {
			return Reply{}, err
		}
		if err = tx.Commit(r.Context()); err != nil {
			return Reply{}, err
		}
		return Reply{}, Fail(401, "invalid_credentials", "Email or password is invalid, or local sign-in is unavailable.")
	}
	response, err := s.newSession(r, tx, id)
	if err != nil {
		return Reply{}, err
	}
	if err = Audit(r.Context(), tx, r, id, "session.login", "user", id, "success"); err != nil {
		return Reply{}, err
	}
	return Commit(r, tx, response)
}
func (s *Server) currentSession(r *http.Request) (Reply, error) {
	p, err := s.sessionPrincipal(r, s.Pool, "read")
	if err != nil {
		return Reply{}, err
	}
	if p.SessionID != "" {
		if _, err = s.Pool.Exec(r.Context(), "UPDATE olp_go.sessions SET last_seen_at=now() WHERE id=$1 AND last_seen_at<now()-interval '1 minute'", p.SessionID); err != nil {
			return Reply{}, err
		}
	}
	body, err := s.sessionBody(r, s.Pool, p.User, p.Token)
	return OK(body), err
}
func (s *Server) sessions(r *http.Request) (Reply, error) {
	p, err := s.sessionPrincipal(r, s.Pool, "read")
	if err != nil {
		return Reply{}, err
	}
	pagination, err := Page(r)
	if err != nil {
		return Reply{}, err
	}
	userID := r.URL.Query().Get("user_id")
	if userID == "" {
		userID = p.ID
	}
	if userID != p.ID && p.Role != "owner" {
		return Reply{}, Forbidden()
	}
	if _, err := ParseUUID(userID); err != nil {
		return Reply{}, err
	}
	var sessionID any
	if p.SessionID != "" {
		sessionID = p.SessionID
	}
	rows, err := s.Pool.Query(r.Context(), "SELECT jsonb_build_object('id',id,'user_id',user_id,'current',id=$1,'expires_at',expires_at,'last_seen_at',last_seen_at,'browser_hint',browser_hint,'created_at',created_at) FROM olp_go.sessions WHERE user_id=$2 AND expires_at>now() AND id<$3 ORDER BY id DESC LIMIT $4", sessionID, userID, pagination.Before, pagination.Limit+1)
	if err != nil {
		return Reply{}, err
	}
	items, err := JSONRows(rows)
	return ListReply(items, pagination), err
}
func (s *Server) logout(r *http.Request) (Reply, error)        { return s.deleteSession(r, true) }
func (s *Server) revokeSession(r *http.Request) (Reply, error) { return s.deleteSession(r, false) }
func (s *Server) deleteSession(r *http.Request, current bool) (Reply, error) {
	tx, err := s.Begin(r)
	if err != nil {
		return Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.sessionPrincipal(r, tx, "read")
	if err != nil {
		return Reply{}, err
	}
	session := p.SessionID
	if !current {
		session, err = IDParam(r, "session_id")
		if err != nil {
			return Reply{}, err
		}
	}
	var id any = session
	if session == "" {
		id = nil
	}
	var userID string
	if err = tx.QueryRow(r.Context(), "SELECT user_id::text FROM olp_go.sessions WHERE id=$1", id).Scan(&userID); err != nil {
		return Reply{}, err
	}
	if userID != p.ID && p.Role != "owner" {
		return Reply{}, Forbidden()
	}
	if _, err = tx.Exec(r.Context(), "DELETE FROM olp_go.sessions WHERE id=$1", id); err != nil {
		return Reply{}, err
	}
	if err = Audit(r.Context(), tx, r, p.ID, "session.revoke", "session", session, "success"); err != nil {
		return Reply{}, err
	}
	response := Reply{Status: 204}
	if session == p.SessionID {
		response.Cookies = []*http.Cookie{clearCookie(sessionCookie), clearCookie(csrfCookie), clearCookie(recentCookie)}
	}
	return Commit(r, tx, response)
}

func (s *Server) profile(r *http.Request) (Reply, error) {
	p, err := s.sessionPrincipal(r, s.Pool, "read")
	if err != nil {
		return Reply{}, err
	}
	return s.profileBody(r, s.Pool, p)
}

type projectOption struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Role string `json:"role"`
}

func (s *Server) profileBody(r *http.Request, q Queryer, p Principal) (Reply, error) {
	var rows pgx.Rows
	var err error
	if p.AllProjects {
		rows, err = q.Query(r.Context(), "SELECT id::text,name,'manager' FROM olp_go.projects ORDER BY lower(name)")
	} else {
		rows, err = q.Query(r.Context(), "SELECT p.id::text,p.name,m.role FROM olp_go.project_members m JOIN olp_go.projects p ON p.id=m.project_id WHERE m.user_id=$1 ORDER BY lower(p.name)", p.ID)
	}
	if err != nil {
		return Reply{}, err
	}
	projects := []projectOption{}
	for rows.Next() {
		var option projectOption
		if err = rows.Scan(&option.ID, &option.Name, &option.Role); err != nil {
			rows.Close()
			return Reply{}, err
		}
		projects = append(projects, option)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return Reply{}, err
	}
	return Detail(map[string]any{"id": p.ID, "email": p.Email, "display_name": p.DisplayName, "role": p.Role, "active": p.Active, "access_scope": p.AccessScope, "etag": p.ETag, "created_at": p.CreatedAt, "updated_at": p.UpdatedAt, "projects": projects}, p.ETag), nil
}
func (s *Server) updateProfile(r *http.Request) (Reply, error) {
	var input struct {
		Name string `json:"display_name"`
	}
	if err := Decode(r, &input); err != nil {
		return Reply{}, err
	}
	if err := ValidText("display_name", input.Name, 100); err != nil {
		return Reply{}, err
	}
	tx, err := s.Begin(r)
	if err != nil {
		return Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.sessionPrincipal(r, tx, "read")
	if err != nil {
		return Reply{}, err
	}
	if err = Match(r, p.ETag); err != nil {
		return Reply{}, err
	}
	if _, err = tx.Exec(r.Context(), "UPDATE olp_go.users SET display_name=$1,etag=$2,updated_at=now() WHERE id=$3", strings.TrimSpace(input.Name), NewID(), p.ID); err != nil {
		return Reply{}, err
	}
	if err = Audit(r.Context(), tx, r, p.ID, "profile.update", "user", p.ID, "success"); err != nil {
		return Reply{}, err
	}
	u, err := scanUser(tx.QueryRow(r.Context(), "SELECT "+userColumns+" FROM olp_go.users u WHERE id=$1", p.ID))
	if err != nil {
		return Reply{}, err
	}
	p.User = u
	reply, err := s.profileBody(r, tx, p)
	if err != nil {
		return Reply{}, err
	}
	return Commit(r, tx, reply)
}
func (s *Server) changePassword(r *http.Request) (Reply, error) { return s.writePassword(r, false) }
func (s *Server) enrollPassword(r *http.Request) (Reply, error) { return s.writePassword(r, true) }
func (s *Server) writePassword(r *http.Request, enroll bool) (Reply, error) {
	var input struct {
		Current string `json:"current_password"`
		New     string `json:"new_password"`
	}
	if err := Decode(r, &input); err != nil {
		return Reply{}, err
	}
	if err := password(input.New); err != nil {
		return Reply{}, err
	}
	p, err := s.sessionPrincipal(r, s.Pool, "read")
	if err != nil {
		return Reply{}, err
	}
	if err = s.admit(r, "password", p.ID); err != nil {
		return Reply{}, err
	}
	var stored *string
	if err = s.Pool.QueryRow(r.Context(), "SELECT password_hash FROM olp_go.users WHERE id=$1", p.ID).Scan(&stored); err != nil {
		return Reply{}, err
	}
	if enroll && stored != nil {
		return Reply{}, Fail(409, "password_already_enrolled", "A local password is already configured.")
	}
	if !enroll && stored == nil {
		return Reply{}, Fail(403, "local_password_unavailable", "This profile has no local password.")
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
		return Reply{}, err
	}
	if !valid {
		return Reply{}, Fail(403, "current_password_invalid", "The current password is invalid.")
	}
	tx, err := s.Begin(r)
	if err != nil {
		return Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err = s.sessionPrincipal(r, tx, "read")
	if err != nil {
		return Reply{}, err
	}
	if err = Match(r, p.ETag); err != nil {
		return Reply{}, err
	}
	if enroll {
		if err = s.consumeRecent(r, tx, p, "password_enrollment", ""); err != nil {
			return Reply{}, err
		}
	}
	if _, err = tx.Exec(r.Context(), "UPDATE olp_go.users SET password_hash=$1,etag=$2,updated_at=now() WHERE id=$3", hash, NewID(), p.ID); err != nil {
		return Reply{}, err
	}
	if _, err = tx.Exec(r.Context(), "DELETE FROM olp_go.sessions WHERE user_id=$1", p.ID); err != nil {
		return Reply{}, err
	}
	response, err := s.newSession(r, tx, p.ID)
	if err != nil {
		return Reply{}, err
	}
	u, err := scanUser(tx.QueryRow(r.Context(), "SELECT "+userColumns+" FROM olp_go.users u WHERE id=$1", p.ID))
	if err != nil {
		return Reply{}, err
	}
	response.Status = 200
	response.Body = u
	response.ETag = u.ETag
	if err = Audit(r.Context(), tx, r, p.ID, "profile.password.update", "user", p.ID, "success"); err != nil {
		return Reply{}, err
	}
	return Commit(r, tx, response)
}
func validatePurpose(purpose, resource string) error {
	switch purpose {
	case "password_enrollment", "oidc_link":
		if resource == "" {
			return nil
		}
	case "oidc_unlink":
		_, err := ParseUUID(resource)
		return err
	}
	return Invalid("purpose", "Use password_enrollment, oidc_link, or oidc_unlink with its identity ID.")
}
func (s *Server) reauthenticate(r *http.Request) (Reply, error) {
	var input struct {
		Password string `json:"current_password"`
		Purpose  string `json:"purpose"`
		Resource string `json:"resource_id"`
	}
	if err := Decode(r, &input); err != nil {
		return Reply{}, err
	}
	if err := validatePurpose(input.Purpose, input.Resource); err != nil {
		return Reply{}, err
	}
	p, err := s.sessionPrincipal(r, s.Pool, "read")
	if err != nil {
		return Reply{}, err
	}
	if err = s.admit(r, "reauthentication", p.ID); err != nil {
		return Reply{}, err
	}
	var hash *string
	if err = s.Pool.QueryRow(r.Context(), "SELECT password_hash FROM olp_go.users WHERE id=$1", p.ID).Scan(&hash); err != nil {
		return Reply{}, err
	}
	if hash == nil {
		return Reply{}, Fail(403, "local_password_unavailable", "Use your linked identity to authenticate.")
	}
	var valid bool
	if err = s.passwordWork(r, func() { valid = secrets.VerifyPassword(input.Password, *hash) }); err != nil {
		return Reply{}, err
	}
	if !valid {
		return Reply{}, Fail(403, "current_password_invalid", "The current password is invalid.")
	}
	tx, err := s.Begin(r)
	if err != nil {
		return Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err = s.sessionPrincipal(r, tx, "read")
	if err != nil {
		return Reply{}, err
	}
	response, err := s.grantRecent(r, tx, p, input.Purpose, input.Resource)
	if err != nil {
		return Reply{}, err
	}
	return Commit(r, tx, response)
}
func (s *Server) grantRecent(r *http.Request, tx pgx.Tx, p Principal, purpose, resource string) (Reply, error) {
	token := secrets.Token()
	var target any
	if resource != "" {
		target = resource
	}
	if _, err := tx.Exec(r.Context(), "DELETE FROM olp_go.recent_auth WHERE session_id=$1 OR expires_at<=now()", p.SessionID); err != nil {
		return Reply{}, err
	}
	_, err := tx.Exec(r.Context(), "INSERT INTO olp_go.recent_auth(digest,session_id,purpose,resource_id,expires_at) VALUES($1,$2,$3,$4,now()+interval '5 minutes')", s.Auth.Digest("recent_auth", token), p.SessionID, purpose, target)
	return Reply{Status: 204, Cookies: []*http.Cookie{cookie(recentCookie, token, 5*time.Minute, true)}}, err
}
func (s *Server) consumeRecent(r *http.Request, tx pgx.Tx, p Principal, purpose, resource string) error {
	var target any
	if resource != "" {
		target = resource
	}
	tag, err := tx.Exec(r.Context(), "DELETE FROM olp_go.recent_auth WHERE digest=$1 AND session_id=$2 AND purpose=$3 AND resource_id IS NOT DISTINCT FROM $4::uuid AND expires_at>now()", s.Auth.Digest("recent_auth", cookieValue(r, recentCookie)), p.SessionID, purpose, target)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return Fail(428, "reauthentication_required", "Confirm your identity again before this operation.")
	}
	return nil
}

func browserHint(agent string) string {
	browser := "Unknown browser"
	for _, entry := range []struct{ match, label string }{
		{"Edg/", "Edge"}, {"Firefox/", "Firefox"}, {"Chrome/", "Chrome"}, {"Safari/", "Safari"}, {"curl/", "curl"},
	} {
		if strings.Contains(agent, entry.match) {
			browser = entry.label
			break
		}
	}
	for _, entry := range []struct{ match, label string }{
		{"Android", "Android"}, {"iPhone", "iPhone"}, {"iPad", "iPad"}, {"Windows", "Windows"}, {"Macintosh", "macOS"}, {"Linux", "Linux"},
	} {
		if strings.Contains(agent, entry.match) {
			return browser + " on " + entry.label
		}
	}
	return browser
}
