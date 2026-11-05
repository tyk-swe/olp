//go:build qualification

package qualification_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/database"
)

const (
	accessPassword = "a long qualification password"
	startupLimit   = 30 * time.Second
)

func required(t *testing.T, name string) string {
	t.Helper()
	value := os.Getenv(name)
	if value == "" {
		t.Fatalf("%s is not set. Run make qualify-code", name)
	}
	return value
}

func newDatabase(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	raw := required(t, "OLP_TEST_DATABASE_URL")
	admin, err := pgx.Connect(t.Context(), raw)
	if err != nil {
		t.Fatal(err)
	}
	name := "qualify_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	identifier := pgx.Identifier{name}.Sanitize()
	if _, err = admin.Exec(t.Context(), "CREATE DATABASE "+identifier); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	config, err := database.Configuration(u.String(), 10, 10*time.Second)
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
		if _, err := admin.Exec(ctx, "DROP DATABASE "+identifier+" WITH (FORCE)"); err != nil {
			t.Errorf("drop database: %v", err)
		}
		admin.Close(ctx)
	})
	return pool, u.String()
}

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
	write("index.html", "<!doctype html><title>Qualification console</title>")
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
			"OLP_VALKEY_URL":                       required(t, "OLP_TEST_VALKEY_URL"),
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
	command := exec.CommandContext(t.Context(), required(t, "OLP_TEST_BINARY"), args...)
	command.Env = env.list(nil)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("olp %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}

func freeAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().String()
}

type service struct {
	t      *testing.T
	pool   *pgxpool.Pool
	origin string
	env    environment
}

func startService(t *testing.T) *service {
	t.Helper()
	pool, databaseURL := newDatabase(t)
	env := newEnvironment(t, databaseURL)
	runBinary(t, env, "migrate")
	public, private := freeAddress(t), freeAddress(t)
	origin := "http://" + public
	logPath := filepath.Join(t.TempDir(), "olp.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(required(t, "OLP_TEST_BINARY"), "all")
	command.Env = env.list(map[string]string{"OLP_LISTEN_ADDR": public, "OLP_OBSERVABILITY_LISTEN_ADDR": private, "OLP_PUBLIC_ORIGIN": origin})
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
	deadline := time.Now().Add(startupLimit)
	for time.Now().Before(deadline) {
		select {
		case err := <-exited:
			t.Fatalf("olp exited during startup: %v\n%s", err, dumpLog())
		default:
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
	s.want(owner, "POST", "/api/v1/setup", map[string]any{"email": "owner@example.com", "display_name": "Owner", "password": accessPassword, "installation_name": "Qualification"}, map[string]string{"X-OLP-Setup-Token": s.env.bootstrap}, http.StatusCreated)
	return owner
}

func (s *service) eventually(what string, limit time.Duration, check func() (bool, string)) {
	s.t.Helper()
	deadline := time.Now().Add(limit)
	last := ""
	for time.Now().Before(deadline) {
		done, detail := check()
		if done {
			return
		}
		last = detail
		time.Sleep(50 * time.Millisecond)
	}
	s.t.Fatalf("%s did not happen within %s: %s", what, limit, last)
}

func describe(status int, raw []byte) string { return fmt.Sprintf("status %d: %s", status, raw) }

func uniqueName(prefix string) string { return prefix + "-" + uuid.NewString()[:8] }
