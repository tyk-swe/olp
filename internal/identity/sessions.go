package identity

import (
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/domain/ids"
	"github.com/tyk-swe/olp/internal/domain/problem"
	"github.com/tyk-swe/olp/internal/domain/textfield"
	"github.com/tyk-swe/olp/internal/platform/database"
	"github.com/tyk-swe/olp/internal/platform/secrets"
)

type sessionUser struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
	AccessScope string `json:"access_scope"`
}

type sessionResponse struct {
	User       sessionUser `json:"user"`
	SessionID  string      `json:"session_id"`
	CSRFToken  string      `json:"csrf_token"`
	Operations []string    `json:"operations"`
}

type sessionSummary struct {
	ID          string    `json:"id"`
	UserID      string    `json:"user_id"`
	Current     bool      `json:"current"`
	ExpiresAt   time.Time `json:"expires_at"`
	LastSeenAt  time.Time `json:"last_seen_at"`
	BrowserHint string    `json:"browser_hint"`
	CreatedAt   time.Time `json:"created_at"`
}

type projectOption struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Role string `json:"role"`
}

type profileResponse struct {
	ID          string          `json:"id"`
	Email       string          `json:"email"`
	DisplayName string          `json:"display_name"`
	Role        string          `json:"role"`
	Active      bool            `json:"active"`
	AccessScope string          `json:"access_scope"`
	ETag        string          `json:"etag"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
	Projects    []projectOption `json:"projects"`
}

func (s *sessionService) register(mux *http.ServeMux) {
	routes := map[string]access.Handler{
		"GET /api/v1/sessions":                 s.sessions,
		"GET /api/v1/sessions/current":         s.currentSession,
		"DELETE /api/v1/sessions/current":      s.logout,
		"DELETE /api/v1/sessions/{session_id}": s.revokeSession,
		"GET /api/v1/profile":                  s.profile,
		"PATCH /api/v1/profile":                s.updateProfile,
		"POST /api/v1/profile/password":        s.changePassword,
		"POST /api/v1/profile/password/enroll": s.enrollPassword,
	}
	for pattern, fn := range routes {
		s.access.Route(mux, pattern, fn)
	}
}

func (s *sessionService) sessionBody(r *http.Request, q database.Queryer, u authz.User, token string) (sessionResponse, error) {
	var sessionID string
	err := q.QueryRow(r.Context(), "SELECT COALESCE((SELECT id::text FROM olp.sessions WHERE digest=$1),'')", s.access.Auth.Digest(secrets.SessionDigest, token)).Scan(&sessionID)
	ops := (authz.Principal{User: u, Kind: "user", AllProjects: u.AccessScope == "global"}).Operations()
	operations := make([]string, 0, len(ops))
	for _, op := range ops {
		operations = append(operations, op.String())
	}
	return sessionResponse{
		User: sessionUser{
			ID:          u.ID,
			Email:       u.Email,
			DisplayName: u.DisplayName,
			Role:        u.Role,
			AccessScope: u.AccessScope,
		},
		SessionID:  sessionID,
		CSRFToken:  s.access.CSRF(token),
		Operations: operations,
	}, err
}

func (s *sessionService) newSession(r *http.Request, tx pgx.Tx, userID string, auth ...sessionAuth) (httpkit.Reply, error) {
	strength := sessionAuth{Method: "local"}
	if len(auth) > 0 {
		strength = auth[0]
	}
	token := secrets.Token()
	id := ids.New()
	if _, err := tx.Exec(r.Context(), "DELETE FROM olp.sessions WHERE id IN(SELECT id FROM olp.sessions WHERE expires_at<=now() LIMIT 100)"); err != nil {
		return httpkit.Reply{}, err
	}
	if _, err := tx.Exec(r.Context(), "DELETE FROM olp.sessions WHERE id IN(SELECT id FROM olp.sessions WHERE user_id=$1 ORDER BY created_at DESC OFFSET 19)", userID); err != nil {
		return httpkit.Reply{}, err
	}
	if _, err := tx.Exec(r.Context(), "INSERT INTO olp.sessions(id,user_id,digest,expires_at,browser_hint,auth_method,mfa_verified) VALUES($1,$2,$3,$4,$5,$6,$7)", id, userID, s.access.Auth.Digest(secrets.SessionDigest, token), time.Now().Add(access.SessionTTL), browserHint(r.UserAgent()), strength.Method, strength.MFA); err != nil {
		return httpkit.Reply{}, err
	}
	u, err := access.ScanUser(tx.QueryRow(r.Context(), "SELECT "+access.UserColumns+" FROM olp.users u WHERE id=$1", userID))
	if err != nil {
		return httpkit.Reply{}, err
	}
	body, err := s.sessionBody(r, tx, u, token)
	return httpkit.Reply{
		Status: 201,
		Body:   body,
		CSRF:   s.access.CSRF(token),
		Cookies: []*http.Cookie{
			access.NewCookie(access.SessionCookie, token, access.SessionTTL, true),
			access.NewCookie(access.CSRFCookie, s.access.CSRF(token), access.SessionTTL, false),
			access.ClearCookie(access.RecentCookie),
		},
	}, err
}

func (s *sessionService) changeSignInMethod(r *http.Request, tx pgx.Tx, userID string, strength sessionAuth) (httpkit.Reply, error) {
	if _, err := tx.Exec(r.Context(), "UPDATE olp.users SET etag=$2,updated_at=now() WHERE id=$1", userID, ids.New()); err != nil {
		return httpkit.Reply{}, err
	}
	if _, err := tx.Exec(r.Context(), "DELETE FROM olp.sessions WHERE user_id=$1", userID); err != nil {
		return httpkit.Reply{}, err
	}
	if err := access.Audit(r.Context(), tx, r, authz.UserActor(userID), "user.authentication_method_change", "user", userID, "success"); err != nil {
		return httpkit.Reply{}, err
	}
	return s.newSession(r, tx, userID, strength)
}

func (s *sessionService) currentSession(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	if p.SessionID != "" {
		if _, err := s.access.Pool.Exec(r.Context(), "UPDATE olp.sessions SET last_seen_at=now() WHERE id=$1 AND last_seen_at<now()-interval '1 minute'", p.SessionID); err != nil {
			return httpkit.Reply{}, err
		}
	}
	body, err := s.sessionBody(r, s.access.Pool, p.User, p.Token)
	return httpkit.OK(body), err
}

func (s *sessionService) sessions(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	pagination, err := httpkit.Page(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	userID := r.URL.Query().Get("user_id")
	if userID == "" {
		userID = p.ID
	}
	if userID != p.ID {
		if err := p.Authorize(authz.ManageSessions); err != nil {
			return httpkit.Reply{}, err
		}
	}
	if _, err := ids.Parse(userID); err != nil {
		return httpkit.Reply{}, err
	}
	var currentSession *string
	if p.SessionID != "" {
		id := p.SessionID
		currentSession = &id
	}
	rows, err := s.access.Pool.Query(
		r.Context(),
		"SELECT id::text,user_id::text,COALESCE(id=$1::uuid,false),expires_at,last_seen_at,browser_hint,created_at FROM olp.sessions WHERE user_id=$2 AND expires_at>now() AND id<$3 ORDER BY id DESC LIMIT $4",
		currentSession, userID, pagination.Before, pagination.Limit+1,
	)
	if err != nil {
		return httpkit.Reply{}, err
	}
	defer rows.Close()
	items := []sessionSummary{}
	for rows.Next() {
		var item sessionSummary
		if err := rows.Scan(&item.ID, &item.UserID, &item.Current, &item.ExpiresAt, &item.LastSeenAt, &item.BrowserHint, &item.CreatedAt); err != nil {
			return httpkit.Reply{}, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.ListReplyBy(items, pagination, func(item sessionSummary) string { return item.ID }), nil
}

func (s *sessionService) logout(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	return s.deleteSession(r, true)
}

func (s *sessionService) revokeSession(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	return s.deleteSession(r, false)
}

func revokedSession(r *http.Request, p authz.Principal, current bool) (string, error) {
	if current {
		return p.SessionID, nil
	}
	return httpkit.IDParam(r, "session_id")
}

func (s *sessionService) deleteSession(r *http.Request, current bool) (httpkit.Reply, error) {
	return access.Transact(s.access, r, access.Unlocked, func(tx pgx.Tx, p authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		session, err := revokedSession(r, p, current)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if session == "" {
			return httpkit.Reply{}, access.Event{}, pgx.ErrNoRows
		}
		var userID string
		if err := tx.QueryRow(r.Context(), "SELECT user_id::text FROM olp.sessions WHERE id=$1", session).Scan(&userID); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		// Another member's session is invisible to a caller who cannot manage it.
		if userID != p.ID && p.Authorize(authz.ManageSessions) != nil {
			return httpkit.Reply{}, access.Event{}, pgx.ErrNoRows
		}
		if _, err := tx.Exec(r.Context(), "DELETE FROM olp.sessions WHERE id=$1", session); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		var cookies []*http.Cookie
		if session == p.SessionID {
			cookies = []*http.Cookie{
				access.ClearCookie(access.SessionCookie),
				access.ClearCookie(access.CSRFCookie),
				access.ClearCookie(access.RecentCookie),
			}
		}
		return httpkit.Reply{Status: 204, Cookies: cookies}, access.Event{Action: "session.revoke", Resource: "session", ID: session}, nil
	})
}

func (s *sessionService) profile(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	return s.profileBody(r, s.access.Pool, p)
}

func (s *sessionService) profileBody(r *http.Request, q database.Queryer, p authz.Principal) (httpkit.Reply, error) {
	var rows pgx.Rows
	var err error
	if p.AllProjects {
		rows, err = q.Query(r.Context(), "SELECT id::text,name,'manager' FROM olp.projects ORDER BY lower(name)")
	} else {
		rows, err = q.Query(r.Context(), "SELECT p.id::text,p.name,m.role FROM olp.effective_project_members m JOIN olp.projects p ON p.id=m.project_id WHERE m.user_id=$1 ORDER BY lower(p.name)", p.ID)
	}
	if err != nil {
		return httpkit.Reply{}, err
	}
	defer rows.Close()
	projects := []projectOption{}
	for rows.Next() {
		var option projectOption
		if err := rows.Scan(&option.ID, &option.Name, &option.Role); err != nil {
			return httpkit.Reply{}, err
		}
		projects = append(projects, option)
	}
	if err := rows.Err(); err != nil {
		return httpkit.Reply{}, err
	}
	body := profileResponse{
		ID:          p.ID,
		Email:       p.Email,
		DisplayName: p.DisplayName,
		Role:        p.Role,
		Active:      p.Active,
		AccessScope: p.AccessScope,
		ETag:        p.ETag,
		CreatedAt:   p.CreatedAt,
		UpdatedAt:   p.UpdatedAt,
		Projects:    projects,
	}
	return httpkit.Detail(body, p.ETag), nil
}

func (s *sessionService) updateProfile(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	var input struct {
		Name string `json:"display_name"`
	}
	if err := httpkit.Decode(r, &input); err != nil {
		return httpkit.Reply{}, err
	}
	if err := textfield.Validate("display_name", input.Name, 100); err != nil {
		return httpkit.Reply{}, err
	}
	return access.Transact(s.access, r, access.Unlocked, func(tx pgx.Tx, p authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		if err := httpkit.Match(r, p.ETag); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if _, err := tx.Exec(r.Context(), "UPDATE olp.users SET display_name=$1,etag=$2,updated_at=now() WHERE id=$3", strings.TrimSpace(input.Name), ids.New(), p.ID); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		u, err := access.ScanUser(tx.QueryRow(r.Context(), "SELECT "+access.UserColumns+" FROM olp.users u WHERE id=$1", p.ID))
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		updated := p
		updated.User = u
		reply, err := s.profileBody(r, tx, updated)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		return reply, access.Event{Action: "profile.update", Resource: "user", ID: p.ID}, nil
	})
}

func (s *sessionService) changePassword(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	return s.writePassword(r, p, false)
}

func (s *sessionService) enrollPassword(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	return s.writePassword(r, p, true)
}

func (s *sessionService) preparePasswordWrite(r *http.Request, p authz.Principal, enroll bool) (string, error) {
	var input struct {
		Current string `json:"current_password"`
		New     string `json:"new_password"`
	}
	if err := httpkit.Decode(r, &input); err != nil {
		return "", err
	}
	if err := password(input.New); err != nil {
		return "", err
	}
	if err := admitAuth(s.access, r, "password", p.ID); err != nil {
		return "", err
	}
	var stored *string
	if err := s.access.Pool.QueryRow(r.Context(), "SELECT password_hash FROM olp.users WHERE id=$1", p.ID).Scan(&stored); err != nil {
		return "", err
	}
	if enroll && stored != nil {
		return "", problem.Fail(409, "password_already_enrolled", "A local password is already configured.")
	}
	if !enroll && stored == nil {
		return "", problem.Fail(403, "local_password_unavailable", "This profile has no local password.")
	}
	var hash string
	valid := enroll
	err := s.passwords.run(r, func() {
		if !enroll {
			valid = secrets.VerifyPassword(input.Current, *stored)
		}
		if valid {
			hash = secrets.HashPassword(input.New)
		}
	})
	if err != nil {
		return "", err
	}
	if !valid {
		return "", problem.Fail(403, "current_password_invalid", "The current password is invalid.")
	}
	return hash, nil
}

func (s *sessionService) commitPasswordWrite(r *http.Request, tx pgx.Tx, p authz.Principal, hash string, enroll bool) (httpkit.Reply, access.Event, error) {
	if err := httpkit.Match(r, p.ETag); err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	if enroll {
		if err := s.access.ConsumeRecent(r, tx, p, "password_enrollment", ""); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
	}
	if _, err := tx.Exec(r.Context(), "UPDATE olp.users SET password_hash=$1,etag=$2,updated_at=now() WHERE id=$3", hash, ids.New(), p.ID); err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	strength, err := sessionStrength(r.Context(), tx, p.SessionID)
	if err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	if _, err := tx.Exec(r.Context(), "DELETE FROM olp.sessions WHERE user_id=$1", p.ID); err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	sessionReply, err := s.newSession(r, tx, p.ID, strength)
	if err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	u, err := access.ScanUser(tx.QueryRow(r.Context(), "SELECT "+access.UserColumns+" FROM olp.users u WHERE id=$1", p.ID))
	if err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	reply := httpkit.Reply{
		Status:  200,
		Body:    u,
		ETag:    u.ETag,
		CSRF:    sessionReply.CSRF,
		Cookies: sessionReply.Cookies,
	}
	return reply, access.Event{Action: "profile.password.update", Resource: "user", ID: p.ID}, nil
}

func (s *sessionService) writePassword(r *http.Request, p authz.Principal, enroll bool) (httpkit.Reply, error) {
	hash, err := s.preparePasswordWrite(r, p, enroll)
	if err != nil {
		return httpkit.Reply{}, err
	}
	return access.Transact(s.access, r, access.Unlocked, func(tx pgx.Tx, p authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		return s.commitPasswordWrite(r, tx, p, hash, enroll)
	})
}

func (s *sessionService) grantRecent(r *http.Request, tx pgx.Tx, p authz.Principal, purpose, resource string, factorVerified bool) (httpkit.Reply, error) {
	if purpose == "mfa_manage" && !factorVerified {
		configured, err := mfaConfigured(r.Context(), tx, p.ID)
		if err != nil {
			return httpkit.Reply{}, err
		}
		if configured {
			return httpkit.Reply{}, problem.Fail(403, "mfa_factor_required", "Verify an enrolled factor or recovery code to manage local MFA.")
		}
	}
	token := secrets.Token()
	var target *string
	if resource != "" {
		res := resource
		target = &res
	}
	if _, err := tx.Exec(r.Context(), "DELETE FROM olp.recent_auth WHERE session_id=$1 OR expires_at<=now()", p.SessionID); err != nil {
		return httpkit.Reply{}, err
	}
	_, err := tx.Exec(r.Context(), "INSERT INTO olp.recent_auth(digest,session_id,purpose,resource_id,expires_at) VALUES($1,$2,$3,$4,now()+interval '5 minutes')", s.access.Auth.Digest(secrets.RecentAuthDigest, token), p.SessionID, purpose, target)
	return httpkit.Reply{Status: 204, Cookies: []*http.Cookie{access.NewCookie(access.RecentCookie, token, 5*time.Minute, true)}}, err
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
