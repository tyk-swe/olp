//go:build e2e

package e2e_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/tests/harness"
)

const (
	accessPassword = "a long service test password"
	startupLimit   = 30 * time.Second
)

type environment struct {
	values    map[string]string
	bootstrap string
}

func newEnvironment(t *testing.T, databaseURL string) environment {
	t.Helper()
	dir := t.TempDir()
	write := func(name, value string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	write("index.html", "<!doctype html><title>Service test console</title>")
	secret := func() string {
		id := uuid.New()
		other := uuid.New()
		return hex.EncodeToString(append(id[:], other[:]...))
	}
	bootstrap := secret()
	return environment{
		bootstrap: bootstrap,
		values: map[string]string{
			"OLP_DATABASE_URL":                     databaseURL,
			"OLP_VALKEY_URL":                       harness.Required(t, "OLP_TEST_VALKEY_URL"),
			"OLP_AUTH_HMAC_KEY_FILE":               write("auth.key", secret()),
			"OLP_BOOTSTRAP_TOKEN_FILE":             write("bootstrap.token", bootstrap),
			"OLP_MASTER_KEY_FILE":                  write("master.json", `{"active_version":1,"keys":[{"version":1,"key":"`+secret()+`"}]}`),
			"OLP_CONSOLE_DIR":                      dir,
			"OLP_MEDIA_SPOOL_DIR":                  filepath.Join(dir, "spool"),
			"OLP_PROVIDER_EGRESS_ALLOW_CIDRS":      "127.0.0.0/8",
			"OLP_PROVIDER_EGRESS_ALLOW_HTTP_HOSTS": "127.0.0.1",
			"OLP_SHUTDOWN_TIMEOUT":                 "2s",
			"OLP_LOG_LEVEL":                        "warn",
		},
	}
}

func (e environment) list(overrides map[string]string) []string {
	kept := make([]string, 0, len(e.values)+len(overrides)+8)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "OLP_") {
			kept = append(kept, entry)
		}
	}
	for name, value := range e.values {
		if _, overridden := overrides[name]; !overridden {
			kept = append(kept, name+"="+value)
		}
	}
	for name, value := range overrides {
		kept = append(kept, name+"="+value)
	}
	return kept
}

func runBinary(t *testing.T, env environment, args ...string) {
	t.Helper()
	command := exec.CommandContext(t.Context(), harness.Required(t, "OLP_TEST_BINARY"), args...)
	command.Env = env.list(nil)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("olp %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}

func freeAddresses(t *testing.T) (string, string) {
	t.Helper()
	public, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer public.Close()
	private, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer private.Close()
	return public.Addr().String(), private.Addr().String()
}

type service struct {
	t      *testing.T
	pool   *pgxpool.Pool
	origin string
	env    environment
}

func startService(t *testing.T) *service {
	t.Helper()
	pool, databaseURL := harness.Database(t)
	env := newEnvironment(t, databaseURL)
	runBinary(t, env, "migrate")
	return startProcess(t, pool, env)
}

func TestStartupFailureCleanup(t *testing.T) {
	t.Parallel()
	if os.Getenv("OLP_TEST_STARTUP_FAILURE") == "1" {
		startProcess(t, nil, environment{})
		t.Fatal("process unexpectedly started")
	}
	binary := filepath.Join(t.TempDir(), "exit-early")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 23\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "-test.run=^TestStartupFailureCleanup$")
	command.Env = append(os.Environ(), "OLP_TEST_STARTUP_FAILURE=1", "OLP_TEST_BINARY="+binary)
	output, err := command.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("startup failure cleanup did not finish: %v\n%s", ctx.Err(), output)
	}
	if err == nil || !bytes.Contains(output, []byte("olp exited during startup: exit status 23")) {
		t.Fatalf("expected the startup failure: %v\n%s", err, output)
	}
}

func TestStartupPortCollision(t *testing.T) {
	t.Parallel()
	pool, databaseURL := harness.Database(t)
	env := newEnvironment(t, databaseURL)
	runBinary(t, env, "migrate")
	occupied := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer occupied.Close()
	attempts := 0
	addresses := func(t *testing.T) (string, string) {
		attempts++
		public, private := freeAddresses(t)
		if attempts == 1 {
			private = strings.TrimPrefix(occupied.URL, "http://")
		}
		return public, private
	}
	s := startProcessAttempt(t, pool, env, addresses, time.Now().Add(startupLimit), 3)
	if attempts != 2 {
		t.Fatalf("startup attempts = %d, want 2", attempts)
	}
	// Exercise the child rather than trusting a foreign health endpoint.
	s.owner()
}

func startProcess(t *testing.T, pool *pgxpool.Pool, env environment) *service {
	t.Helper()
	return startProcessAttempt(t, pool, env, freeAddresses, time.Now().Add(startupLimit), 3)
}

func startProcessAttempt(t *testing.T, pool *pgxpool.Pool, env environment, addresses func(*testing.T) (string, string), deadline time.Time, remaining int) *service {
	t.Helper()
	public, private := addresses(t)
	origin := "http://" + public
	logPath := filepath.Join(t.TempDir(), "olp.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(harness.Required(t, "OLP_TEST_BINARY"), "all")
	command.Env = env.list(map[string]string{"OLP_LISTEN_ADDR": public, "OLP_OBSERVABILITY_LISTEN_ADDR": private, "OLP_PUBLIC_ORIGIN": origin, "OLP_LOG_LEVEL": "info"})
	command.Stdout, command.Stderr = logFile, logFile
	if err = command.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() {
		exited <- command.Wait()
		close(exited)
	}()
	dumpLog := func() string {
		content, _ := os.ReadFile(logPath)
		return string(content)
	}
	t.Cleanup(func() {
		defer logFile.Close()
		if t.Failed() {
			t.Logf("olp log:\n%s", dumpLog())
		}
		_ = command.Process.Signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			_ = command.Process.Kill()
			<-exited
			t.Errorf("olp did not stop within its shutdown limit")
		}
	})
	client := http.Client{Timeout: time.Second}
	for time.Now().Before(deadline) {
		select {
		case err := <-exited:
			log := dumpLog()
			// Only address collisions are retried. Authentication, migration,
			// configuration and other startup failures must remain visible.
			if remaining > 1 && strings.Contains(log, "bind ") && strings.Contains(log, "listener:") && strings.Contains(log, "address already in use") {
				return startProcessAttempt(t, pool, env, addresses, deadline, remaining-1)
			}
			t.Fatalf("olp exited during startup: %v\n%s", err, dumpLog())
		default:
		}
		// A different process may have acquired the released private address.
		// Its health response cannot establish that our child is ready.
		if !strings.Contains(dumpLog(), `"msg":"listener started"`) {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		response, err := client.Get("http://" + private + "/health/live")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return &service{t: t, pool: pool, origin: origin, env: env}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("olp was not live after %s\n%s", startupLimit, dumpLog())
	return nil
}

// caller is one authenticated management identity: a browser session, or a
// management token when token is set.
type caller struct {
	name    string
	cookies map[string]*http.Cookie
	csrf    string
	token   string
}

var client = &http.Client{
	Timeout:       20 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func (s *service) do(c *caller, method, path string, body any, headers map[string]string) (int, []byte) {
	s.t.Helper()
	var payload []byte
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			s.t.Fatal(err)
		}
		payload = encoded
	}
	request, err := http.NewRequest(method, s.origin+path, bytes.NewReader(payload))
	if err != nil {
		s.t.Fatal(err)
	}
	request.Header.Set("Origin", s.origin)
	request.Header.Set("Content-Type", "application/json")
	if c != nil && c.token != "" {
		request.Header.Set("Authorization", "Bearer "+c.token)
	}
	if c != nil && c.token == "" {
		for _, cookie := range c.cookies {
			request.AddCookie(cookie)
		}
		request.Header.Set("X-CSRF-Token", c.csrf)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := client.Do(request)
	if err != nil {
		s.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		s.t.Fatalf("%s %s: %v", method, path, err)
	}
	if c != nil && c.token == "" {
		c.remember(response, raw)
	}
	return response.StatusCode, raw
}

func (c *caller) remember(response *http.Response, raw []byte) {
	if c.cookies == nil {
		c.cookies = map[string]*http.Cookie{}
	}
	for _, cookie := range response.Cookies() {
		if cookie.MaxAge < 0 {
			delete(c.cookies, cookie.Name)
			continue
		}
		c.cookies[cookie.Name] = cookie
	}
	var body struct {
		CSRF string `json:"csrf_token"`
	}
	if json.Unmarshal(raw, &body) == nil && body.CSRF != "" {
		c.csrf = body.CSRF
	}
	if token := response.Header.Get("X-CSRF-Token"); token != "" {
		c.csrf = token
	}
}

func (s *service) want(c *caller, method, path string, body any, headers map[string]string, status int) map[string]any {
	s.t.Helper()
	got, raw := s.do(c, method, path, body, headers)
	if got != status {
		s.t.Fatalf("%s %s: status %d, want %d: %s", method, path, got, status, raw)
	}
	out := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			s.t.Fatalf("%s %s: invalid JSON: %v", method, path, err)
		}
	}
	return out
}

func idempotent(headers ...map[string]string) map[string]string {
	merged := map[string]string{"Idempotency-Key": uuid.NewString()}
	for _, extra := range headers {
		for name, value := range extra {
			merged[name] = value
		}
	}
	return merged
}

func etag(record map[string]any) map[string]string {
	return map[string]string{"If-Match": `"` + record["etag"].(string) + `"`}
}

func (s *service) owner() *caller {
	s.t.Helper()
	owner := &caller{name: "owner"}
	s.want(owner, "POST", "/api/v1/setup", map[string]any{"email": "owner@example.com", "display_name": "Owner", "password": accessPassword, "installation_name": "Service test"}, map[string]string{"X-OLP-Setup-Token": s.env.bootstrap}, http.StatusCreated)
	return owner
}

func (s *service) login(email string) *caller {
	s.t.Helper()
	member := &caller{name: email}
	s.want(member, "POST", "/api/v1/sessions", map[string]any{"email": email, "password": accessPassword}, nil, http.StatusCreated)
	return member
}

func (s *service) invite(owner *caller, email, role string) *caller {
	s.t.Helper()
	invitation := s.want(owner, "POST", "/api/v1/invitations", map[string]any{"email": email, "role": role}, idempotent(), http.StatusCreated)
	member := &caller{name: email}
	s.want(member, "POST", "/api/v1/invitations/accept", map[string]any{"token": invitation["token"], "display_name": role, "password": accessPassword}, nil, http.StatusCreated)
	return member
}

func (s *service) eventually(what string, limit time.Duration, check func() (bool, string)) {
	s.t.Helper()
	harness.Eventually(s.t, what, limit, check)
}

func describe(status int, raw []byte) string { return fmt.Sprintf("status %d: %s", status, raw) }
