// Package access owns installation identity and management transactions.
package access

import (
	"bytes"
	"context"
	"crypto/hmac"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/mail"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/secrets"
)

const sessionCookie = "__Host-olp_session"
const csrfCookie = "__Host-olp_csrf"
const recentCookie = "__Host-olp_recent_auth"
const csrfHeader = "X-CSRF-Token"
const sessionTTL = 24 * time.Hour

type Server struct {
	Pool                 *pgxpool.Pool
	Installation, Origin string
	Auth                 *secrets.AuthKey
	Keys                 *secrets.KeyRing
	Bootstrap            string
	OIDCClient           *http.Client
	// LimitsEnforced is true where the process has the shared state that
	// admission needs, so stored request, token, concurrency and cost limits
	// are actually applied. The console shows stored amounts either way and
	// uses this to say whether they bind. It is set during composition,
	// before the first request is served, and never changes afterwards.
	LimitsEnforced     bool
	LocalLoginDisabled bool
	// ClientIP is wired to the shared trusted-proxy resolver by the process.
	ClientIP func(*http.Request) string
	// RetentionEnforced is true where this installation is configured with the
	// shared coordination state (OLP_VALKEY_URL) the worker plane requires, so
	// retention and aggregation run in its worker and all processes. A control
	// process cannot observe a separate worker replica, so this reports the
	// installation's configuration rather than a worker's liveness: the console
	// uses it to say whether stored retention policies are applied at all, not
	// whether a pass ran. It is set during composition, before the first
	// request is served, and never changes afterwards.
	RetentionEnforced   bool
	NotificationsActive bool

	Egress        *egress.Policy
	passwordSlots chan struct{}
	dummyPassword string
}

func New(ctx context.Context, pool *pgxpool.Pool, installation, origin string, auth *secrets.AuthKey, keys *secrets.KeyRing, bootstrap string) (*Server, error) {
	s := &Server{Pool: pool, Installation: installation, Origin: strings.TrimSuffix(origin, "/"), Auth: auth, Keys: keys, Bootstrap: bootstrap, passwordSlots: make(chan struct{}, 4), dummyPassword: secrets.HashPassword(secrets.Token()), OIDCClient: oidcHTTPClient()}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var fingerprint []byte
	var active *int
	if err = tx.QueryRow(ctx, "SELECT auth_fingerprint,active_key_version FROM olp_go.installation WHERE singleton FOR UPDATE").Scan(&fingerprint, &active); err != nil {
		return nil, err
	}
	expected := auth.Digest("installation", "identity")
	if fingerprint != nil && !hmac.Equal(fingerprint, expected) {
		return nil, errors.New("authentication key does not match the Go installation")
	}
	if active != nil && *active != keys.Active {
		return nil, errors.New("master key active version differs; run master-key reencrypt or reload the current ring")
	}
	// Authenticate every record, including expired replays and old key versions,
	// before admitting writes. A version number alone does not identify a key.
	rows, err := tx.Query(ctx, "SELECT id::text,purpose,key_version,ciphertext FROM olp_go.secrets")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, purpose string
		var version int
		var ciphertext []byte
		if err = rows.Scan(&id, &purpose, &version, &ciphertext); err != nil {
			rows.Close()
			return nil, err
		}
		if !keys.Has(version) {
			rows.Close()
			return nil, errors.New("master key ring is missing a stored version")
		}
		if _, err = keys.Open(installation, purpose, id, version, ciphertext); err != nil {
			rows.Close()
			return nil, err
		}
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	if _, err = tx.Exec(ctx, "UPDATE olp_go.installation SET auth_fingerprint=$1,active_key_version=$2 WHERE singleton", expected, keys.Active); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

type Problem struct {
	Status              int
	Code, Detail, Field string
}

func (p *Problem) Error() string { return p.Code }
func Fail(status int, code, detail string) error {
	return &Problem{Status: status, Code: code, Detail: detail}
}
func Invalid(field, detail string) error {
	return &Problem{Status: 422, Code: "validation_failed", Detail: detail, Field: field}
}
func Forbidden() error {
	return Fail(403, "permission_denied", "The current role cannot perform this operation.")
}

type Reply struct {
	Status   int            `json:"status"`
	Body     any            `json:"body"`
	ETag     string         `json:"etag,omitempty"`
	Location string         `json:"location,omitempty"`
	Cookies  []*http.Cookie `json:"-"`
	CSRF     string         `json:"-"`
}

func OK(body any) Reply                  { return Reply{Status: 200, Body: body} }
func Detail(body any, etag string) Reply { return Reply{Status: 200, Body: body, ETag: etag} }

func (s *Server) Handle(fn func(*http.Request) (Reply, error)) http.HandlerFunc {
	return s.HandleWith(65536, fn)
}

// HandleWith is Handle with an explicit request-body limit for the few
// operations whose documented payloads exceed the default.
func (s *Server) HandleWith(maxBody int64, fn func(*http.Request) (Reply, error)) http.HandlerFunc {
	return s.HandleTimeout(maxBody, 15*time.Second, fn)
}

func (s *Server) guard(w http.ResponseWriter, r *http.Request, maxBody int64, timeout time.Duration) (*http.Request, context.CancelFunc, error) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	deadline, _ := ctx.Deadline()
	if err := http.NewResponseController(w).SetReadDeadline(deadline); err != nil {
		cancel()
		return r, cancel, err
	}
	r = r.WithContext(ctx)
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	_, machine := managementBearer(r)
	if machine {
		return r, cancel, nil
	}
	if err := checkCookies(r); err != nil {
		return r, cancel, err
	}
	if r.Method != "GET" && r.Method != "HEAD" && r.Header.Get("Origin") != s.Origin {
		return r, cancel, Fail(403, "origin_denied", "Use the configured console origin.")
	}
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" && r.URL.Path != "/api/v3/oidc/callback" {
		return r, cancel, Fail(403, "origin_denied", "Cross-site access is not allowed.")
	}
	return r, cancel, nil
}

// HandleTimeout is HandleWith with an explicit request deadline for
// operations that legitimately outlive the default management budget.
func (s *Server) HandleTimeout(maxBody int64, timeout time.Duration, fn func(*http.Request) (Reply, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r, cancel, err := s.guard(w, r, maxBody, timeout)
		defer cancel()
		var result Reply
		if err == nil {
			result, err = fn(r)
		}
		if err != nil {
			WriteProblem(w, err)
			return
		}
		if result.ETag != "" {
			w.Header().Set("ETag", `"`+result.ETag+`"`)
		}
		if result.Location != "" {
			w.Header().Set("Location", result.Location)
		}
		if result.CSRF != "" {
			w.Header().Set(csrfHeader, result.CSRF)
		}
		for _, cookie := range result.Cookies {
			http.SetCookie(w, cookie)
		}
		if result.Status == 0 {
			result.Status = 200
		}
		if result.Body != nil {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(result.Status)
		if result.Body != nil {
			json.NewEncoder(w).Encode(result.Body)
		}
	}
}

func (s *Server) HandleStream(maxBody int64, timeout time.Duration, fn func(http.ResponseWriter, *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r, cancel, err := s.guard(w, r, maxBody, timeout)
		defer cancel()
		tracker := &streamWriter{ResponseWriter: w}
		if err == nil {
			err = fn(tracker, r)
		}
		if err != nil && !tracker.committed {
			WriteProblem(w, err)
		}
	}
}

type streamWriter struct {
	http.ResponseWriter
	committed bool
}

func (w *streamWriter) WriteHeader(status int) {
	w.committed = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *streamWriter) Write(data []byte) (int, error) {
	w.committed = true
	return w.ResponseWriter.Write(data)
}

func (w *streamWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func WriteProblem(w http.ResponseWriter, err error) {
	p := &Problem{Status: 503, Code: "service_unavailable", Detail: "The operation could not be completed. Retry shortly."}
	var pg *pgconn.PgError
	if known, ok := errors.AsType[*Problem](err); ok {
		p = known
	} else if errors.Is(err, pgx.ErrNoRows) {
		p = &Problem{Status: 404, Code: "not_found", Detail: "The resource was not found."}
	} else if errors.As(err, &pg) && pg.Code == "23505" {
		p = &Problem{Status: 409, Code: "already_exists", Detail: "This record already exists."}
	} else {
		// Driver and wrapped errors can contain SQL values or credentials.
		// Log the category, never the error text.
		args := []any{"error_type", fmt.Sprintf("%T", err)}
		if pg != nil {
			args = append(args, "sqlstate", pg.Code)
		}
		slog.Error("management request failed", args...)
	}
	body := map[string]any{"type": "https://openllmproxy.dev/problems/" + p.Code, "title": http.StatusText(p.Status), "status": p.Status, "detail": p.Detail}
	if p.Field != "" {
		body["errors"] = map[string]any{p.Field: []any{map[string]string{"code": p.Code, "message": p.Detail}}}
	}
	w.Header().Set("Content-Type", "application/problem+json")
	if p.Status == 429 {
		w.Header().Set("Retry-After", "60")
	}
	w.WriteHeader(p.Status)
	json.NewEncoder(w).Encode(body)
}

func Decode(r *http.Request, v any) error {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return Fail(415, "unsupported_media_type", "Send an application/json request.")
	}
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return Fail(400, "invalid_json", "The request body is not valid for this operation.")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return Fail(400, "invalid_json", "Send one JSON document.")
	}
	return nil
}
func checkCookies(r *http.Request) error {
	seen := map[string]string{}
	for _, c := range r.Cookies() {
		if strings.HasPrefix(c.Name, "__Host-olp_") {
			if value, ok := seen[c.Name]; ok && value != c.Value {
				return Fail(400, "conflicting_cookie_values", "Conflicting authentication cookies were supplied.")
			}
			seen[c.Name] = c.Value
		}
	}
	return nil
}
func cookieValue(r *http.Request, name string) string {
	c, err := r.Cookie(name)
	if err != nil {
		return ""
	}
	return c.Value
}
func cookie(name, value string, ttl time.Duration, httpOnly bool) *http.Cookie {
	return &http.Cookie{Name: name, Value: value, Path: "/", Secure: true, HttpOnly: httpOnly, SameSite: http.SameSiteLaxMode, MaxAge: int(ttl.Seconds())}
}
func clearCookie(name string) *http.Cookie { c := cookie(name, "", 0, true); c.MaxAge = -1; return c }
func Match(r *http.Request, etag string) error {
	value := r.Header.Get("If-Match")
	if value == "" {
		return Fail(428, "precondition_required", "Reload the record and send its ETag in If-Match.")
	}
	if value != `"`+etag+`"` {
		return Fail(412, "etag_mismatch", "This record changed. Reload it before saving.")
	}
	return nil
}
func IDParam(r *http.Request, name string) (string, error) {
	v := r.PathValue(name)
	id, err := uuid.Parse(v)
	if err != nil {
		return "", Fail(400, "invalid_identifier", "Use a valid UUID.")
	}
	return id.String(), nil
}
func ValidText(field, value string, max int) error {
	if strings.TrimSpace(value) == "" || utf8.RuneCountInString(value) > max {
		return Invalid(field, "Use 1–"+strconv.Itoa(max)+" characters.")
	}
	return nil
}
func email(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	a, err := mail.ParseAddress(value)
	if err != nil || a.Address != value || len(value) > 254 {
		return "", Invalid("email", "Enter a valid email address.")
	}
	return value, nil
}
func password(value string) error {
	n := utf8.RuneCountInString(value)
	if n < 12 || n > 1024 {
		return Invalid("password", "Use 12–1024 characters.")
	}
	return nil
}
func validRole(role string) bool {
	return role == "owner" || role == "operator" || role == "developer" || role == "viewer"
}
func Permission(role, operation string) bool {
	if !validRole(role) {
		return false
	}
	if role == "owner" || operation == "read" || operation == "usage" {
		return true
	}
	switch operation {
	case "access_read", "settings", "configure":
		return role == "operator"
	case "keys", "playground":
		return role == "operator" || role == "developer"
	}
	return false
}

type Queryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}
type User struct {
	ID          string    `json:"id"`
	Email       string    `json:"email"`
	DisplayName string    `json:"display_name"`
	Role        string    `json:"role"`
	Active      bool      `json:"active"`
	AccessScope string    `json:"access_scope"`
	ETag        string    `json:"etag"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}
type Principal struct {
	User
	Kind             string
	Creator          string
	AllProjects      bool
	Projects         map[string]string
	SessionID, Token string
}

func (p Principal) UserID() string {
	if p.Kind == "machine" {
		return p.Creator
	}
	return p.ID
}

const userColumns = "u.id::text,u.email,u.display_name,u.role,u.active,u.access_scope,u.etag::text,u.created_at,u.updated_at"

func scanUser(row pgx.Row) (User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Email, &u.DisplayName, &u.Role, &u.Active, &u.AccessScope, &u.ETag, &u.CreatedAt, &u.UpdatedAt)
	return u, err
}
func (s *Server) Principal(r *http.Request, q Queryer, operation string) (Principal, error) {
	if secret, ok := managementBearer(r); ok {
		return s.machinePrincipal(r, q, secret, operation)
	}
	var p Principal
	p.Kind = "user"
	p.Token = cookieValue(r, sessionCookie)
	err := q.QueryRow(r.Context(), "SELECT "+userColumns+",s.id::text FROM olp_go.sessions s JOIN olp_go.users u ON u.id=s.user_id WHERE s.digest=$1 AND s.expires_at>now() AND u.active AND u.oidc_authorized", s.Auth.Digest("session", p.Token)).Scan(&p.ID, &p.Email, &p.DisplayName, &p.Role, &p.Active, &p.AccessScope, &p.ETag, &p.CreatedAt, &p.UpdatedAt, &p.SessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, Fail(401, "authentication_required", "Sign in to continue.")
	}
	if err != nil {
		return p, err
	}
	p.AllProjects = p.AccessScope == "global"
	if err = p.loadProjects(r.Context(), q); err != nil {
		return p, err
	}
	if !Permission(p.Role, operation) {
		return p, Forbidden()
	}
	if err = restrictInstallation(p, operation); err != nil {
		return p, err
	}
	if r.Method != "GET" && !hmac.Equal([]byte(r.Header.Get(csrfHeader)), []byte(s.csrf(p.Token))) {
		return p, Fail(403, "csrf_invalid", "Reload the console before trying again.")
	}
	return p, nil
}
func (p *Principal) loadProjects(ctx context.Context, q Queryer) error {
	rows, err := q.Query(ctx, "SELECT project_id::text,role FROM olp_go.project_members WHERE user_id=$1", p.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, role string
		if err = rows.Scan(&id, &role); err != nil {
			return err
		}
		if p.Projects == nil {
			p.Projects = map[string]string{}
		}
		p.Projects[id] = role
	}
	return rows.Err()
}
func restrictInstallation(p Principal, operation string) error {
	if !p.AllProjects && (operation == "access" || operation == "access_read" || operation == "settings") {
		return Forbidden()
	}
	return nil
}
func (p Principal) CanProject(projectID *string, write bool) bool {
	if p.AllProjects {
		return true
	}
	if projectID == nil {
		return false
	}
	role, ok := p.Projects[*projectID]
	return ok && (!write || role == "manager")
}
func ProjectAccess(p Principal, projectID *string, write bool) error {
	if !p.CanProject(projectID, false) {
		return pgx.ErrNoRows
	}
	if write && !p.CanProject(projectID, true) {
		return Forbidden()
	}
	return nil
}
func (p Principal) ProjectIDs() []string {
	ids := make([]string, 0, len(p.Projects))
	for id := range p.Projects {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}
func (s *Server) RequireProject(ctx context.Context, q Queryer, p Principal, projectID *string, write bool) error {
	if projectID == nil {
		if p.AllProjects {
			return nil
		}
		return Forbidden()
	}
	// Callers compare and persist the identifier after this check.
	parsed, err := uuid.Parse(*projectID)
	if err != nil {
		return Invalid("project_id", "Use a valid project UUID.")
	}
	*projectID = parsed.String()
	var exists bool
	if err = q.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM olp_go.projects WHERE id=$1)", *projectID).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return Fail(404, "not_found", "The resource was not found.")
	}
	if !p.CanProject(projectID, write) {
		return Fail(403, "project_scope_denied", "This project is outside the caller's scope.")
	}
	return nil
}
func managementBearer(r *http.Request) (string, bool) {
	const prefix = "Bearer olpm_"
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, prefix) {
		return "", false
	}
	return strings.TrimPrefix(header, "Bearer "), true
}
func (s *Server) machinePrincipal(r *http.Request, q Queryer, secret, operation string) (Principal, error) {
	var p Principal
	p.Kind = "machine"
	parts := strings.Split(secret, "_")
	if len(parts) != 3 {
		return p, Fail(401, "authentication_required", "Sign in to continue.")
	}
	var digest, data, projectData []byte
	var live bool
	err := q.QueryRow(r.Context(), "SELECT id::text,name,scopes,digest,created_by::text,expires_at>now() AND revoked_at IS NULL,all_projects,project_ids FROM olp_go.management_tokens WHERE lookup_id=$1", parts[1]).Scan(&p.ID, &p.DisplayName, &data, &digest, &p.Creator, &live, &p.AllProjects, &projectData)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && (!live || !hmac.Equal(digest, s.Auth.Digest("management_token", secret))) {
		return p, Fail(401, "authentication_required", "Sign in to continue.")
	}
	if err != nil {
		return p, err
	}
	if p.AllProjects {
		p.AccessScope = "global"
	} else {
		p.AccessScope = "assigned"
		var projectIDs []string
		if err = json.Unmarshal(projectData, &projectIDs); err != nil {
			return p, err
		}
		p.Projects = map[string]string{}
		for _, id := range projectIDs {
			p.Projects[id] = "manager"
		}
	}
	var scopes []string
	if err = json.Unmarshal(data, &scopes); err != nil {
		return p, err
	}
	if !slices.Contains(scopes, operation) {
		return p, Forbidden()
	}
	return p, restrictInstallation(p, operation)
}
func (s *Server) sessionPrincipal(r *http.Request, q Queryer, operation string) (Principal, error) {
	p, err := s.Principal(r, q, operation)
	if err == nil && p.Kind != "user" {
		err = Forbidden()
	}
	return p, err
}
func (s *Server) csrf(token string) string {
	return base64.RawURLEncoding.EncodeToString(s.Auth.Digest("csrf", token))
}
func (s *Server) Begin(r *http.Request) (pgx.Tx, error) {
	tx, err := s.Pool.Begin(r.Context())
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(r.Context(), "SELECT id FROM olp_go.installation WHERE singleton FOR UPDATE"); err != nil {
		tx.Rollback(r.Context())
		return nil, err
	}
	return tx, nil
}
func Audit(ctx context.Context, tx pgx.Tx, r *http.Request, actor, action, resource, id, outcome string) error {
	source, _, _ := net.SplitHostPort(r.RemoteAddr)
	family := "other"
	agent := r.UserAgent()
	for _, known := range []string{"Firefox", "Chrome", "Safari", "curl"} {
		if strings.Contains(agent, known) {
			family = known
			break
		}
	}
	var userID, tokenID any
	if actor != "" {
		var kind string
		err := tx.QueryRow(ctx, `SELECT CASE WHEN EXISTS(SELECT 1 FROM olp_go.users WHERE id=$1) THEN 'user' WHEN EXISTS(SELECT 1 FROM olp_go.management_tokens WHERE id=$1) THEN 'management_token' ELSE '' END`, actor).Scan(&kind)
		if err != nil {
			return err
		}
		switch kind {
		case "user":
			userID = actor
		case "management_token":
			tokenID = actor
		default:
			return errors.New("audit actor is not a known principal")
		}
	}
	_, err := tx.Exec(ctx, "INSERT INTO olp_go.Audit(id,actor_user_id,actor_management_token_id,action,resource_type,resource_id,outcome,source_ip,user_agent_family) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)", NewID(), userID, tokenID, action, resource, id, outcome, source, family)
	return err
}
func Commit(r *http.Request, tx pgx.Tx, result Reply) (Reply, error) {
	return result, tx.Commit(r.Context())
}

type Pagination struct {
	Limit  int
	Before string
}

func Page(r *http.Request) (Pagination, error) {
	p := Pagination{Limit: 50, Before: "ffffffff-ffff-ffff-ffff-ffffffffffff"}
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 200 {
			return p, Invalid("limit", "Use a page size from 1 to 200.")
		}
		p.Limit = n
	}
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		b, err := base64.RawURLEncoding.DecodeString(raw)
		if err != nil {
			return p, Invalid("cursor", "Invalid pagination cursor.")
		}
		v, err := uuid.Parse(string(b))
		if err != nil {
			return p, Invalid("cursor", "Invalid pagination cursor.")
		}
		p.Before = v.String()
	}
	return p, nil
}
func ListReply(items []map[string]any, p Pagination) Reply {
	return ListReplyBy(items, p, func(item map[string]any) string { return item["id"].(string) })
}

// ListReplyBy paginates records using the identifier ordered by their query.
func ListReplyBy[T any](items []T, p Pagination, id func(T) string) Reply {
	var next any
	if len(items) > p.Limit {
		items = items[:p.Limit]
		next = base64.RawURLEncoding.EncodeToString([]byte(id(items[len(items)-1])))
	}
	if items == nil {
		items = []T{}
	}
	return OK(map[string]any{"items": items, "next_cursor": next})
}
func JSONRows(rows pgx.Rows) ([]map[string]any, error) {
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var b []byte
		var item map[string]any
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		decoder := json.NewDecoder(bytes.NewReader(b))
		decoder.UseNumber()
		if err := decoder.Decode(&item); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}
