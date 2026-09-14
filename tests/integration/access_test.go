//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/database"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/gateway"
	"github.com/tyk-swe/olp/internal/management"
	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/routes"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/testutil"
)

func accessDatabase(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	raw := required(t, "OLP_TEST_DATABASE_URL")
	admin, err := pgx.Connect(t.Context(), raw)
	if err != nil {
		t.Fatal(err)
	}
	name := "access_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = admin.Exec(t.Context(), "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	config, err := database.Configuration(u.String(), 20, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := database.Open(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		admin.Exec(ctx, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
		admin.Close(ctx)
	})
	return pool, u.String()
}

type browser struct {
	Cookies map[string]*http.Cookie
	CSRF    string
}
type accessHarness struct {
	t         *testing.T
	Pool      *pgxpool.Pool
	DBURL     string
	Server    *access.Server
	HTTP      *httptest.Server
	Bootstrap string
	Ring      string
	AuthHex   string
	Runtime   *runtime.Manager
	Gateway   *gateway.Server
}

func newAccessHarness(t *testing.T) *accessHarness {
	t.Helper()
	pool, dbURL := accessDatabase(t)
	if err := database.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	installation, err := database.Installation(t.Context(), pool)
	if err != nil {
		t.Fatal(err)
	}
	key := strings.Repeat("ab", 32)
	ringJSON := `{"active_version":1,"keys":[{"version":1,"key":"` + key + `"}]}`
	ring, err := secrets.ParseRing([]byte(ringJSON))
	if err != nil {
		t.Fatal(err)
	}
	authHex := strings.Repeat("cd", 32)
	auth, _ := secrets.DecodeKey(authHex)
	bootstrap := secrets.Token()
	server, err := access.New(t.Context(), pool, installation, "https://console.test", secrets.NewAuthKey(auth, installation), ring, bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	// The gateway and the catalogue share one process here, exactly as the
	// "all" mode does; provider egress is opened to loopback for the mock vendor.
	policy := egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	rt := runtime.NewManager(pool, installation, secrets.NewAuthKey(auth, installation), ring, log)
	gw := gateway.New(rt, &policy, gateway.Config{MaxInFlight: 16, MaxBodyBytes: 1 << 20, MaxResponseBytes: 1 << 20, MaxEventBytes: 1 << 16}, log)
	catalogue := providers.New(server, &policy)
	catalogue.Health = gw.Health()
	mux := http.NewServeMux()
	management.Register(mux)
	server.Register(mux)
	catalogue.Register(mux)
	routes.New(server).Register(mux)
	(&gateway.Playground{Access: server, Gateway: gw}).Register(mux)
	gw.Register(mux)
	httpServer := httptest.NewServer(mux)
	t.Cleanup(httpServer.Close)
	return &accessHarness{t, pool, dbURL, server, httpServer, bootstrap, ringJSON, authHex, rt, gw}
}

// do performs one management request as the browser and returns the response
// with its fully read body.
func (h *accessHarness) do(b *browser, method, path string, body any, headers map[string]string) (*http.Response, []byte) {
	h.t.Helper()
	var data []byte
	if body != nil {
		var err error
		data, err = json.Marshal(body)
		if err != nil {
			h.t.Fatal(err)
		}
	}
	r, err := http.NewRequest(method, h.HTTP.URL+path, bytes.NewReader(data))
	if err != nil {
		h.t.Fatal(err)
	}
	r.Header.Set("Origin", h.Server.Origin)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("User-Agent", "Chrome test-private-agent")
	if b != nil {
		for _, c := range b.Cookies {
			r.AddCookie(c)
		}
		r.Header.Set("X-CSRF-Token", b.CSRF)
	}
	for key, value := range headers {
		r.Header.Set(key, value)
	}
	client := &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(r)
	if err != nil {
		h.t.Fatal(err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		h.t.Fatal(err)
	}
	if response.Header.Get("Cache-Control") != "no-store" {
		h.t.Fatal("missing no-store")
	}
	return response, raw
}

// list performs a request whose successful body is a bare JSON array.
func (h *accessHarness) list(b *browser, method, path string, body any, headers map[string]string, status int) []any {
	h.t.Helper()
	response, raw := h.do(b, method, path, body, headers)
	if response.StatusCode != status {
		h.t.Fatalf("%s %s: status %d, want %d; body %s", method, path, response.StatusCode, status, raw)
	}
	var out []any
	if err := json.Unmarshal(raw, &out); err != nil {
		h.t.Fatal("invalid JSON list response", err)
	}
	return out
}

func (h *accessHarness) request(b *browser, method, path string, body any, headers map[string]string) (int, map[string]any, http.Header) {
	h.t.Helper()
	response, raw := h.do(b, method, path, body, headers)
	out := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			h.t.Fatal("invalid JSON response", err)
		}
		validateManagementResponse(h.t, method, response.Request.URL.Path, response.StatusCode, out)
	}
	if response.StatusCode == http.StatusSeeOther && response.Header.Get("Location") == "" {
		h.t.Fatal("redirect is missing its destination")
	}
	if b != nil {
		if b.Cookies == nil {
			b.Cookies = map[string]*http.Cookie{}
		}
		for _, c := range response.Cookies() {
			if !c.Secure || c.Path != "/" || c.SameSite != http.SameSiteLaxMode {
				h.t.Fatal("unsafe cookie attributes")
			}
			if c.MaxAge > 0 && c.HttpOnly != (c.Name != "__Host-olp_csrf") {
				h.t.Fatal("authentication cookie has incorrect script visibility")
			}
			if c.MaxAge < 0 {
				delete(b.Cookies, c.Name)
			} else {
				b.Cookies[c.Name] = c
			}
		}
		if token, ok := out["csrf_token"].(string); ok {
			b.CSRF = token
		}
		if token := response.Header.Get("X-CSRF-Token"); token != "" {
			b.CSRF = token
		}
	}
	return response.StatusCode, out, response.Header
}
func (h *accessHarness) want(b *browser, method, path string, body any, headers map[string]string, want int) map[string]any {
	h.t.Helper()
	status, out, _ := h.request(b, method, path, body, headers)
	if status != want {
		h.t.Fatalf("%s %s: status %d, want %d; problem: %v", method, strings.Split(path, "?")[0], status, want, out["detail"])
	}
	return out
}

const accessPassword = "a long integration password"

func (h *accessHarness) owner() *browser {
	b := &browser{}
	h.want(b, "POST", "/api/v3/setup", map[string]any{"email": "owner@example.com", "display_name": "Owner", "password": accessPassword, "installation_name": "Go control"}, map[string]string{"X-OLP-Setup-Token": h.Bootstrap}, 201)
	return b
}
func (h *accessHarness) invite(owner *browser, address, role string) *browser {
	result := h.want(owner, "POST", "/api/v3/invitations", map[string]any{"email": address, "role": role}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	b := &browser{}
	h.want(b, "POST", "/api/v3/invitations/accept", map[string]any{"token": result["token"], "display_name": role, "password": accessPassword}, nil, 201)
	return b
}
func etagHeader(record map[string]any) map[string]string {
	return map[string]string{"If-Match": `"` + record["etag"].(string) + `"`}
}

func TestAPIKeyRouteAllowlistsRejectNull(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	h.want(owner, "POST", "/api/v3/api-keys", map[string]any{"name": "null routes", "allowed_routes": nil}, map[string]string{"Idempotency-Key": "routes-omitted"}, 422)
	if items := h.want(owner, "GET", "/api/v3/api-keys", nil, nil, 200)["items"].([]any); len(items) != 0 {
		t.Fatal("a rejected null allowlist persisted a key")
	}
	for _, tc := range []struct {
		name   string
		routes []string
	}{
		{"omitted", nil},
		{"empty", []string{}},
		{"restricted", []string{"private"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := map[string]any{"name": tc.name}
			if tc.routes != nil {
				input["allowed_routes"] = tc.routes
			}
			created := h.want(owner, "POST", "/api/v3/api-keys", input, map[string]string{"Idempotency-Key": "routes-" + tc.name}, 201)
			path := "/api/v3/api-keys/" + created["id"].(string)
			record := h.want(owner, "GET", path, nil, nil, 200)
			routes, ok := record["allowed_routes"].([]any)
			if !ok || len(routes) != len(tc.routes) {
				t.Fatal("the route allowlist must remain an array with the supplied restrictions")
			}
			for i, route := range routes {
				if route != tc.routes[i] {
					t.Fatal("the route allowlist changed")
				}
			}
			h.want(owner, "PATCH", path, map[string]any{"allowed_routes": nil}, etagHeader(record), 422)
			if current := h.want(owner, "GET", path, nil, nil, 200); current["etag"] != record["etag"] {
				t.Fatal("a rejected null allowlist changed the key")
			}
		})
	}
	if items := h.want(owner, "GET", "/api/v3/api-keys", nil, nil, 200)["items"].([]any); len(items) != 3 {
		t.Fatal("the inventory must contain only the valid keys")
	}
}

func TestAccessTransactionsReplayAndSecretSafety(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	developer := h.invite(owner, "dev@example.com", "developer")
	viewer := h.invite(owner, "viewer@example.com", "viewer")
	h.want(nil, "GET", "/api/v3/users", nil, nil, 401)
	h.want(viewer, "GET", "/api/v3/users", nil, nil, 403)
	h.want(owner, "POST", "/api/v3/api-keys", map[string]any{"name": "denied"}, map[string]string{"Origin": "https://evil.test", "Idempotency-Key": "bad"}, 403)
	h.want(owner, "POST", "/api/v3/api-keys", map[string]any{"name": "denied"}, map[string]string{"X-CSRF-Token": "bad", "Idempotency-Key": "bad"}, 403)
	headers := map[string]string{"Idempotency-Key": "create-one"}
	input := map[string]any{"name": "application", "scopes": []string{"models_read"}, "allowed_routes": []string{"private"}, "requests_per_minute": 12, "daily_cost_limit": "10.25000000"}
	first := h.want(developer, "POST", "/api/v3/api-keys", input, headers, 201)
	second := h.want(developer, "POST", "/api/v3/api-keys", input, headers, 201)
	if first["secret"] != second["secret"] || first["id"] != second["id"] {
		t.Fatal("replay changed secret or side effect")
	}
	h.want(developer, "POST", "/api/v3/api-keys", map[string]any{"name": "different"}, headers, 409)
	path := "/api/v3/api-keys/" + first["id"].(string)
	record := h.want(owner, "GET", path, nil, nil, 200)
	if _, ok := record["secret"]; ok {
		t.Fatal("read exposed secret")
	}
	h.want(viewer, "PATCH", path, map[string]any{"name": "unauthorized"}, etagHeader(record), 403)
	h.want(owner, "PATCH", path, map[string]any{"name": "updated", "requests_per_minute": nil}, etagHeader(record), 200)
	h.want(owner, "PATCH", path, map[string]any{"name": "stale"}, etagHeader(record), 412)
	record = h.want(owner, "GET", path, nil, nil, 200)
	if record["requests_per_minute"] != nil || record["allowed_routes"].([]any)[0] != "private" {
		t.Fatal("patch did not preserve omitted values and clear explicit null")
	}
	authority, err := h.Server.LookupAuthority(t.Context(), first["secret"].(string))
	if err != nil || !authority.Allows("models_read", "private", time.Now()) || authority.Allows("inference", "private", time.Now()) {
		t.Fatal("invalid authority", err)
	}
	rotateHeaders := etagHeader(record)
	rotateHeaders["Idempotency-Key"] = "rotate-one"
	rotated := h.want(owner, "POST", path+"/rotate", nil, rotateHeaders, 200)
	replayed := h.want(owner, "POST", path+"/rotate", nil, rotateHeaders, 200)
	if rotated["secret"] != replayed["secret"] {
		t.Fatal("rotation replay differs")
	}
	if _, err = h.Server.LookupAuthority(t.Context(), first["secret"].(string)); err == nil {
		t.Fatal("old key survived rotation")
	}
	record = h.want(owner, "GET", path, nil, nil, 200)
	revocation := etagHeader(record)
	revocation["Idempotency-Key"] = "revoke-one"
	h.want(owner, "POST", path+"/revoke", nil, revocation, 200)
	authority, err = h.Server.LookupAuthority(t.Context(), rotated["secret"].(string))
	if err != nil || authority.Allows("models_read", "private", time.Now()) {
		t.Fatal("revoked key admitted", err)
	}
	profile := h.want(developer, "GET", "/api/v3/profile", nil, nil, 200)
	h.want(owner, "PATCH", "/api/v3/users/"+profile["id"].(string), map[string]any{"active": false}, etagHeader(profile), 200)
	h.want(developer, "POST", "/api/v3/api-keys", input, headers, 401)
	h.want(&browser{}, "POST", "/api/v3/sessions", map[string]any{"email": "dev@example.com", "password": accessPassword}, nil, 401)
	self := h.want(owner, "GET", "/api/v3/profile", nil, nil, 200)
	h.want(owner, "PATCH", "/api/v3/users/"+self["id"].(string), map[string]any{"role": "viewer"}, etagHeader(self), 409)
	settings := h.want(owner, "GET", "/api/v3/settings/auth.local_login_enabled", nil, nil, 200)
	h.want(owner, "PUT", "/api/v3/settings/auth.local_login_enabled", map[string]any{"value": "false"}, etagHeader(settings), 409)
	h.want(owner, "GET", "/api/v3/users?limit=201", nil, nil, 422)
	h.want(owner, "GET", "/api/v3/users?cursor=malformed", nil, nil, 422)
	events := h.want(owner, "GET", "/api/v3/audit?action=api_key.create", nil, nil, 200)
	if len(events["items"].([]any)) != 1 {
		t.Fatal("idempotency duplicated audit")
	}
	var dump string
	if err = h.Pool.QueryRow(t.Context(), `SELECT jsonb_build_object('users',(SELECT jsonb_agg(u) FROM olp_go.users u),'sessions',(SELECT jsonb_agg(s) FROM olp_go.sessions s),'keys',(SELECT jsonb_agg(k) FROM olp_go.api_keys k),'audit',(SELECT jsonb_agg(a) FROM olp_go.audit a),'secrets',(SELECT jsonb_agg(s) FROM olp_go.secrets s))::text`).Scan(&dump); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{accessPassword, first["secret"].(string), rotated["secret"].(string), owner.Cookies["__Host-olp_session"].Value, "test-private-agent"} {
		if strings.Contains(dump, secret) {
			t.Fatal("secret or raw user agent entered storage")
		}
	}
}

func TestConcurrentSetupInvitationsAndPasswordSessionTransitions(t *testing.T) {
	h := newAccessHarness(t)
	var wg sync.WaitGroup
	statuses := make(chan int, 2)
	for range 2 {
		wg.Go(func() {
			status, _, _ := h.request(nil, "POST", "/api/v3/setup", map[string]any{"email": "owner@example.com", "display_name": "Owner", "password": accessPassword}, map[string]string{"X-OLP-Setup-Token": h.Bootstrap})
			statuses <- status
		})
	}
	wg.Wait()
	close(statuses)
	counts := map[int]int{}
	for status := range statuses {
		counts[status]++
	}
	if counts[201] != 1 || counts[409] != 1 {
		t.Fatal("setup race", counts)
	}
	owner := &browser{}
	h.want(owner, "POST", "/api/v3/sessions", map[string]any{"email": "owner@example.com", "password": accessPassword}, nil, 201)
	invite := h.want(owner, "POST", "/api/v3/invitations", map[string]any{"email": "invite@example.com", "role": "operator"}, map[string]string{"Idempotency-Key": "invite-race"}, 201)
	statuses = make(chan int, 2)
	for range 2 {
		wg.Go(func() {
			status, _, _ := h.request(nil, "POST", "/api/v3/invitations/accept", map[string]any{"token": invite["token"], "display_name": "Member", "password": accessPassword}, nil)
			statuses <- status
		})
	}
	wg.Wait()
	close(statuses)
	counts = map[int]int{}
	for status := range statuses {
		counts[status]++
	}
	if counts[201] != 1 || counts[410] != 1 {
		t.Fatal("invitation race", counts)
	}
	retired := h.want(owner, "POST", "/api/v3/invitations", map[string]any{"email": "retired@example.com", "role": "viewer"}, map[string]string{"Idempotency-Key": "retired"}, 201)
	id := retired["invitation"].(map[string]any)["id"].(string)
	h.want(owner, "DELETE", "/api/v3/invitations/"+id, nil, map[string]string{"Idempotency-Key": "retire"}, 200)
	h.want(nil, "POST", "/api/v3/invitations/accept", map[string]any{"token": retired["token"], "display_name": "Member", "password": accessPassword}, nil, 410)
	expired := h.want(owner, "POST", "/api/v3/invitations", map[string]any{"email": "expired@example.com", "role": "viewer"}, map[string]string{"Idempotency-Key": "expired"}, 201)
	if _, err := h.Pool.Exec(t.Context(), "UPDATE olp_go.invitations SET expires_at=now()-interval '1 second' WHERE id=$1", expired["invitation"].(map[string]any)["id"]); err != nil {
		t.Fatal(err)
	}
	h.want(nil, "POST", "/api/v3/invitations/accept", map[string]any{"token": expired["token"], "display_name": "Member", "password": accessPassword}, nil, 410)
	for _, token := range []string{invite["token"].(string), secrets.Token()} {
		problem := h.want(nil, "POST", "/api/v3/invitations/accept", map[string]any{"token": token, "display_name": "Member", "password": accessPassword}, nil, 410)
		if problem["status"] != float64(410) {
			t.Fatal("unavailable invitation problem must expose the terminal status")
		}
	}
	previous := &browser{Cookies: map[string]*http.Cookie{}, CSRF: owner.CSRF}
	for name, c := range owner.Cookies {
		previous.Cookies[name] = c
	}
	profile := h.want(owner, "GET", "/api/v3/profile", nil, nil, 200)
	h.want(owner, "POST", "/api/v3/profile/password", map[string]any{"current_password": accessPassword, "new_password": "new long integration password"}, etagHeader(profile), 200)
	h.want(previous, "GET", "/api/v3/sessions/current", nil, nil, 401)
	h.want(owner, "GET", "/api/v3/sessions/current", nil, nil, 200)
	h.want(owner, "DELETE", "/api/v3/sessions/current", nil, nil, 204)
	h.want(owner, "GET", "/api/v3/sessions/current", nil, nil, 401)
	for range 5 {
		h.want(nil, "POST", "/api/v3/sessions", map[string]any{"email": "nobody@example.com", "password": "wrong"}, nil, 401)
	}
	h.want(nil, "POST", "/api/v3/sessions", map[string]any{"email": "nobody@example.com", "password": "wrong"}, nil, 429)
}

func TestFreshMigrationsIsolationPrivilegesAndRotationCLI(t *testing.T) {
	pool, _ := accessDatabase(t)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Go(func() { errs <- database.Migrate(t.Context(), pool) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	first, err := database.Installation(t.Context(), pool)
	if err != nil {
		t.Fatal(err)
	}
	if err = database.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	second, err := database.Installation(t.Context(), pool)
	if err != nil || first != second {
		t.Fatal("installation identity changed")
	}
	if _, err = pool.Exec(t.Context(), "UPDATE olp_go.migrations SET checksum='broken'"); err != nil {
		t.Fatal(err)
	}
	if database.Migrate(t.Context(), pool) == nil {
		t.Fatal("accepted changed checksum")
	}
	foreign, _ := accessDatabase(t)
	if _, err = foreign.Exec(t.Context(), "CREATE TABLE public._sqlx_migrations(version bigint)"); err != nil {
		t.Fatal(err)
	}
	if database.Migrate(t.Context(), foreign) == nil {
		t.Fatal("accepted reference installation")
	}
	var wrote bool
	if err = foreign.QueryRow(t.Context(), "SELECT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname='olp_go')").Scan(&wrote); err != nil || wrote {
		t.Fatal("wrote before rejection", err)
	}
	h := newAccessHarness(t)
	owner := h.owner()
	created := h.want(owner, "POST", "/api/v3/api-keys", map[string]any{"name": "survives rotation"}, map[string]string{"Idempotency-Key": "retained"}, 201)
	dir := t.TempDir()
	write := func(name, value string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	ring2 := `{"active_version":2,"keys":[{"version":1,"key":"` + strings.Repeat("ab", 32) + `"},{"version":2,"key":"` + strings.Repeat("ef", 32) + `"}]}`
	env := map[string]string{"OLP_DATABASE_URL": h.DBURL, "OLP_AUTH_HMAC_KEY_FILE": write("auth", h.AuthHex), "OLP_MASTER_KEY_FILE": write("ring", ring2)}
	binary := required(t, "OLP_TEST_BINARY")
	for _, args := range [][]string{{"master-key", "reencrypt"}, {"master-key", "reencrypt"}, {"doctor"}} {
		cmd := exec.CommandContext(t.Context(), binary, args...)
		cmd.Env = testutil.Environment(env)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("maintenance %v: %v %s", args, err, output)
		}
		if bytes.Contains(output, []byte(created["secret"].(string))) {
			t.Fatal("CLI exposed secret")
		}
	}
	newRing, err := secrets.ParseRing([]byte(ring2))
	if err != nil {
		t.Fatal(err)
	}
	h.Server.Keys = newRing
	replayed := h.want(owner, "POST", "/api/v3/api-keys", map[string]any{"name": "survives rotation"}, map[string]string{"Idempotency-Key": "retained"}, 201)
	if created["secret"] != replayed["secret"] {
		t.Fatal("rotation lost encrypted replay")
	}
	if err = h.Pool.QueryRow(t.Context(), "SELECT EXISTS(SELECT 1 FROM olp_go.secrets WHERE key_version<>2)").Scan(&wrote); err != nil || wrote {
		t.Fatal("rotation left old records", err)
	}
	// Runtime privileges allow feature transactions but cannot alter migrations.
	role := "runtime_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = h.Pool.Exec(t.Context(), "CREATE ROLE "+pgx.Identifier{role}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		h.Pool.Exec(ctx, "DROP OWNED BY "+pgx.Identifier{role}.Sanitize())
		h.Pool.Exec(ctx, "DROP ROLE "+pgx.Identifier{role}.Sanitize())
	})
	cmd := exec.CommandContext(t.Context(), binary, "migrate", "--runtime-role", role)
	cmd.Env = testutil.Environment(env)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("grant runtime: %v %s", err, out)
	}
	tx, err := h.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err = tx.Exec(t.Context(), "SET LOCAL ROLE "+pgx.Identifier{role}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(t.Context(), "SELECT id FROM olp_go.users"); err != nil {
		t.Fatal("runtime cannot read", err)
	}
	if _, err = tx.Exec(t.Context(), "DELETE FROM olp_go.migrations"); err == nil {
		t.Fatal("runtime can mutate migration history")
	}
	_ = fmt.Sprintf("%s", database.ValkeyNamespace(first))
}
