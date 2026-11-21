// Package access owns installation identity and management transactions.
package access

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/authz"
	"github.com/tyk-swe/olp/internal/platform/egress"
	"github.com/tyk-swe/olp/internal/platform/secrets"
)

const SessionCookie = "__Host-olp_session"
const CSRFCookie = "__Host-olp_csrf"
const RecentCookie = "__Host-olp_recent_auth"
const csrfHeader = "X-CSRF-Token"
const SessionTTL = 24 * time.Hour

type Config struct {
	RuntimePublisher     func(context.Context, pgx.Tx, string) error
	Pool                 *pgxpool.Pool
	Installation, Origin string
	Auth                 *secrets.AuthKey
	Keys                 *secrets.KeyRing
	Egress               *egress.Policy
	Log                  *slog.Logger

	OIDCClient         *http.Client
	ClientIP           func(*http.Request) string
	LimitsEnforced     bool
	LocalLoginDisabled bool
}

type Server struct {
	RuntimePublisher     func(context.Context, pgx.Tx, string) error
	Pool                 *pgxpool.Pool
	Installation, Origin string
	Auth                 *secrets.AuthKey
	Keys                 *secrets.KeyRing
	Egress               *egress.Policy

	log                *slog.Logger
	oidcClient         *http.Client
	resolveClientIP    func(*http.Request) string
	limitsEnforced     bool
	localLoginDisabled bool
}

func New(cfg Config) *Server {
	return &Server{
		RuntimePublisher: cfg.RuntimePublisher, Pool: cfg.Pool, Installation: cfg.Installation,
		Origin: strings.TrimSuffix(cfg.Origin, "/"), Auth: cfg.Auth, Keys: cfg.Keys,
		Egress: cfg.Egress, log: cfg.Log, oidcClient: oidcHTTPClient(cfg.OIDCClient), resolveClientIP: cfg.ClientIP,
		limitsEnforced: cfg.LimitsEnforced, localLoginDisabled: cfg.LocalLoginDisabled,
	}
}

func PrepareInstallation(ctx context.Context, pool *pgxpool.Pool, installation string, auth *secrets.AuthKey, keys *secrets.KeyRing) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var fingerprint []byte
	var active *int
	if err = tx.QueryRow(ctx, "SELECT auth_fingerprint,active_key_version FROM olp.installation WHERE singleton FOR UPDATE").Scan(&fingerprint, &active); err != nil {
		return err
	}
	expected := auth.Digest(secrets.InstallationDigest, "identity")
	if fingerprint != nil && !hmac.Equal(fingerprint, expected) {
		return errors.New("authentication key does not match the installation")
	}
	if active != nil && *active != keys.Active {
		return errors.New("master key active version differs; run master-key reencrypt or reload the current ring")
	}
	// Authenticate every record, including expired replays and old key versions,
	// before admitting writes.
	if _, err = keys.VerifyAll(ctx, tx, installation); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "UPDATE olp.installation SET auth_fingerprint=$1,active_key_version=$2 WHERE singleton", expected, keys.Active); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	return nil
}

func (s *Server) LimitsEnforced() bool { return s.limitsEnforced }

func (s *Server) LocalLoginDisabled() bool { return s.localLoginDisabled }

func (s *Server) IdentityClient() *http.Client { return s.oidcClient }

func (s *Server) guard(w http.ResponseWriter, r *http.Request, o routeOptions) (*http.Request, context.CancelFunc, error) {
	// Management responses carry credentials and member data; the public
	// perimeter adds the rest of the API's security headers.
	w.Header().Set("Cache-Control", "no-store")
	ctx, cancel := context.WithTimeout(r.Context(), o.timeout)
	deadline, _ := ctx.Deadline()
	if err := http.NewResponseController(w).SetReadDeadline(deadline); err != nil {
		cancel()
		return r, cancel, err
	}
	// Resolve the client once so audit rows written by any package attribute
	// the same trusted-proxy client address that admission uses.
	r = r.WithContext(context.WithValue(ctx, clientIPKey{}, s.ClientIP(r)))
	r.Body = http.MaxBytesReader(w, r.Body, o.maxBody)
	if _, machine := managementBearer(r); machine || o.browser == anyOrigin {
		return r, cancel, nil
	}
	return r, cancel, s.checkBrowser(r, o.browser)
}

func (s *Server) checkBrowser(r *http.Request, policy browserPolicy) error {
	if err := checkCookies(r); err != nil {
		return err
	}
	if r.Method != "GET" && r.Method != "HEAD" && r.Header.Get("Origin") != s.Origin {
		return httpkit.Fail(403, "origin_denied", "Use the configured console origin.")
	}
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" && policy != crossSiteNavigation {
		return httpkit.Fail(403, "origin_denied", "Cross-site access is not allowed.")
	}
	return nil
}

// serve applies the management transport guards, runs fn, and writes its
// Reply or problem.
func (s *Server) serve(o routeOptions, fn func(*http.Request) (httpkit.Reply, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r, cancel, err := s.guard(w, r, o)
		defer cancel()
		var result httpkit.Reply
		if err == nil {
			result, err = fn(r)
		}
		if err != nil {
			s.writeProblem(w, err)
			return
		}
		if result.ETag != "" {
			w.Header().Set("ETag", `"`+result.ETag+`"`)
		}
		if result.ParentMutation != nil {
			w.Header().Set("OLP-Previous-Parent-ETag", `"`+result.ParentMutation.Previous+`"`)
			w.Header().Set("OLP-Parent-ETag", `"`+result.ParentMutation.Current+`"`)
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
			encoder := json.NewEncoder(w)
			encoder.SetEscapeHTML(!result.DisableHTMLEscaping)
			encoder.Encode(result.Body)
		}
	}
}

// serveStream is serve for a handler that writes its own response; a problem
// is written only while nothing has been committed.
func (s *Server) serveStream(o routeOptions, fn func(http.ResponseWriter, *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r, cancel, err := s.guard(w, r, o)
		defer cancel()
		tracker := &streamWriter{ResponseWriter: w}
		if err == nil {
			err = fn(tracker, r)
		}
		if err != nil && !tracker.committed {
			s.writeProblem(w, err)
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

func (s *Server) writeProblem(w http.ResponseWriter, err error) {
	p, known := httpkit.ProblemFor(err)
	if !known {
		s.LogFailure("management request failed", err)
	}
	httpkit.RenderProblem(w, p)
}

// LogFailure logs an unexpected error by category only, because driver and
// wrapped errors can contain SQL values or credentials.
func (s *Server) LogFailure(message string, err error) {
	if pg, ok := errors.AsType[*pgconn.PgError](err); ok {
		s.log.Error(message, "error_type", fmt.Sprintf("%T", err), "sqlstate", pg.Code)
		return
	}
	s.log.Error(message, "error_type", fmt.Sprintf("%T", err))
}

func checkCookies(r *http.Request) error {
	seen := map[string]string{}
	for _, c := range r.Cookies() {
		if strings.HasPrefix(c.Name, "__Host-olp_") {
			if value, ok := seen[c.Name]; ok && value != c.Value {
				return httpkit.Fail(400, "conflicting_cookie_values", "Conflicting authentication cookies were supplied.")
			}
			seen[c.Name] = c.Value
		}
	}
	return nil
}
func CookieValue(r *http.Request, name string) string {
	c, err := r.Cookie(name)
	if err != nil {
		return ""
	}
	return c.Value
}
func NewCookie(name, value string, ttl time.Duration, httpOnly bool) *http.Cookie {
	return &http.Cookie{Name: name, Value: value, Path: "/", Secure: true, HttpOnly: httpOnly, SameSite: http.SameSiteLaxMode, MaxAge: int(ttl.Seconds())}
}
func ClearCookie(name string) *http.Cookie {
	c := NewCookie(name, "", 0, true)
	c.MaxAge = -1
	return c
}
func (s *Server) Begin(r *http.Request) (pgx.Tx, error) {
	tx, err := s.Pool.Begin(r.Context())
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(r.Context(), "SELECT id FROM olp.installation WHERE singleton FOR UPDATE"); err != nil {
		tx.Rollback(r.Context())
		return nil, err
	}
	return tx, nil
}

// clientIPKey carries the client address guard resolved for a request.
type clientIPKey struct{}

// clientIP resolves the request's client address through the trusted-proxy
// resolver when one is configured.
func (s *Server) ClientIP(r *http.Request) string {
	if s.resolveClientIP != nil {
		return s.resolveClientIP(r)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// auditSource returns the client address guard resolved for r, falling back
// to the peer address for requests that did not pass through guard.
func auditSource(r *http.Request) any {
	source, ok := r.Context().Value(clientIPKey{}).(string)
	if !ok {
		source, _, _ = net.SplitHostPort(r.RemoteAddr)
	}
	if source == "" {
		return nil
	}
	return source
}

// Audit records a metadata-only audit event inside the mutation's
// transaction, attributed to actor.
func Audit(ctx context.Context, tx pgx.Tx, r *http.Request, actor authz.Actor, action, resource, id, outcome string) error {
	return AuditForProject(ctx, tx, r, actor, action, resource, id, outcome, nil)
}

func AuditForProject(ctx context.Context, tx pgx.Tx, r *http.Request, actor authz.Actor, action, resource, id, outcome string, projectID *string) error {
	source := auditSource(r)
	family := "other"
	agent := r.UserAgent()
	for _, known := range []string{"Firefox", "Chrome", "Safari", "curl"} {
		if strings.Contains(agent, known) {
			family = known
			break
		}
	}
	var userID, tokenID any
	if user, token := actor.Subject(); user != "" {
		userID = user
	} else if token != "" {
		tokenID = token
	}
	_, err := tx.Exec(ctx, auditInsertSQL, httpkit.NewID(), userID, tokenID, action, resource, id, outcome, source, family, projectID)
	return err
}
