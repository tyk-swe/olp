package harness

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	Password     = "a long service test password"
	StartupLimit = 30 * time.Second
)

type Environment struct {
	Values    map[string]string
	Bootstrap string
	Master    string
	label     string
}

func NewEnvironment(t *testing.T, databaseURL, label string) Environment {
	t.Helper()
	dir := t.TempDir()
	write := func(name, value string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	write("index.html", "<!doctype html><title>"+label+" console</title>")
	secret := func() string {
		id := uuid.New()
		other := uuid.New()
		return hex.EncodeToString(append(id[:], other[:]...))
	}
	bootstrap := secret()
	master := `{"active_version":1,"keys":[{"version":1,"key":"` + secret() + `"}]}`
	return Environment{
		Bootstrap: bootstrap,
		Master:    master,
		label:     label,
		Values: map[string]string{
			"OLP_DATABASE_URL":                     databaseURL,
			"OLP_VALKEY_URL":                       Required(t, "OLP_TEST_VALKEY_URL"),
			"OLP_AUTH_HMAC_KEY_FILE":               write("auth.key", secret()),
			"OLP_BOOTSTRAP_TOKEN_FILE":             write("bootstrap.token", bootstrap),
			"OLP_MASTER_KEY_FILE":                  write("master.json", master),
			"OLP_CONSOLE_DIR":                      dir,
			"OLP_MEDIA_SPOOL_DIR":                  filepath.Join(dir, "spool"),
			"OLP_PROVIDER_EGRESS_ALLOW_CIDRS":      "127.0.0.0/8",
			"OLP_PROVIDER_EGRESS_ALLOW_HTTP_HOSTS": "127.0.0.1",
			"OLP_SHUTDOWN_TIMEOUT":                 "2s",
			"OLP_LOG_LEVEL":                        "warn",
		},
	}
}

func (e Environment) List(overrides map[string]string) []string {
	kept := make([]string, 0, len(e.Values)+len(overrides)+8)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "OLP_") {
			kept = append(kept, entry)
		}
	}
	for name, value := range e.Values {
		if _, overridden := overrides[name]; !overridden {
			kept = append(kept, name+"="+value)
		}
	}
	for name, value := range overrides {
		kept = append(kept, name+"="+value)
	}
	return kept
}

func RunBinary(t *testing.T, env Environment, args ...string) {
	t.Helper()
	command := exec.CommandContext(t.Context(), Required(t, "OLP_TEST_BINARY"), args...)
	command.Env = env.List(nil)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("olp %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}

func FreeAddresses(t *testing.T) (string, string) {
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

type Service struct {
	T      *testing.T
	Pool   *pgxpool.Pool
	Origin string
	Env    Environment
}

func StartProcess(t *testing.T, pool *pgxpool.Pool, env Environment) *Service {
	t.Helper()
	return StartProcessAttempt(t, pool, env, FreeAddresses, time.Now().Add(StartupLimit), 3)
}

func StartProcessAttempt(t *testing.T, pool *pgxpool.Pool, env Environment, addresses func(*testing.T) (string, string), deadline time.Time, remaining int) *Service {
	t.Helper()
	public, private := addresses(t)
	origin := "http://" + public
	logPath := filepath.Join(t.TempDir(), "olp.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(Required(t, "OLP_TEST_BINARY"), "all")
	command.Env = env.List(map[string]string{"OLP_LISTEN_ADDR": public, "OLP_OBSERVABILITY_LISTEN_ADDR": private, "OLP_PUBLIC_ORIGIN": origin, "OLP_LOG_LEVEL": "info"})
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
				return StartProcessAttempt(t, pool, env, addresses, deadline, remaining-1)
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
				return &Service{T: t, Pool: pool, Origin: origin, Env: env}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("olp was not live after %s\n%s", StartupLimit, dumpLog())
	return nil
}

// Caller is one authenticated management identity: a browser session, or a
// management token when Token is set.
type Caller struct {
	Name    string
	Cookies map[string]*http.Cookie
	CSRF    string
	Token   string
}

var client = &http.Client{
	Timeout:       20 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func (s *Service) Do(c *Caller, method, path string, body any, headers map[string]string) (int, []byte) {
	s.T.Helper()
	var payload []byte
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			s.T.Fatal(err)
		}
		payload = encoded
	}
	request, err := http.NewRequest(method, s.Origin+path, bytes.NewReader(payload))
	if err != nil {
		s.T.Fatal(err)
	}
	request.Header.Set("Origin", s.Origin)
	request.Header.Set("Content-Type", "application/json")
	if c != nil && c.Token != "" {
		request.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if c != nil && c.Token == "" {
		for _, cookie := range c.Cookies {
			request.AddCookie(cookie)
		}
		request.Header.Set("X-CSRF-Token", c.CSRF)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := client.Do(request)
	if err != nil {
		s.T.Fatalf("%s %s: %v", method, path, err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		s.T.Fatalf("%s %s: %v", method, path, err)
	}
	if c != nil && c.Token == "" {
		c.remember(response, raw)
	}
	return response.StatusCode, raw
}

func (c *Caller) remember(response *http.Response, raw []byte) {
	if c.Cookies == nil {
		c.Cookies = map[string]*http.Cookie{}
	}
	for _, cookie := range response.Cookies() {
		if cookie.MaxAge < 0 {
			delete(c.Cookies, cookie.Name)
			continue
		}
		c.Cookies[cookie.Name] = cookie
	}
	var body struct {
		CSRF string `json:"csrf_token"`
	}
	if json.Unmarshal(raw, &body) == nil && body.CSRF != "" {
		c.CSRF = body.CSRF
	}
	if token := response.Header.Get("X-CSRF-Token"); token != "" {
		c.CSRF = token
	}
}

func (s *Service) Want(c *Caller, method, path string, body any, headers map[string]string, status int) map[string]any {
	s.T.Helper()
	got, raw := s.Do(c, method, path, body, headers)
	if got != status {
		s.T.Fatalf("%s %s: status %d, want %d: %s", method, path, got, status, raw)
	}
	out := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			s.T.Fatalf("%s %s: invalid JSON: %v", method, path, err)
		}
	}
	return out
}

func Idempotent(headers ...map[string]string) map[string]string {
	merged := map[string]string{"Idempotency-Key": uuid.NewString()}
	for _, extra := range headers {
		for name, value := range extra {
			merged[name] = value
		}
	}
	return merged
}

func ETag(record map[string]any) map[string]string {
	return map[string]string{"If-Match": `"` + record["etag"].(string) + `"`}
}

func (s *Service) Owner() *Caller {
	s.T.Helper()
	owner := &Caller{Name: "owner"}
	s.Want(owner, "POST", "/api/v1/setup", map[string]any{"email": "owner@example.com", "display_name": "Owner", "password": Password, "installation_name": s.Env.label}, map[string]string{"X-OLP-Setup-Token": s.Env.Bootstrap}, http.StatusCreated)
	return owner
}

func (s *Service) Login(email string) *Caller {
	s.T.Helper()
	member := &Caller{Name: email}
	s.Want(member, "POST", "/api/v1/sessions", map[string]any{"email": email, "password": Password}, nil, http.StatusCreated)
	return member
}

func (s *Service) Invite(owner *Caller, email, role string) *Caller {
	s.T.Helper()
	invitation := s.Want(owner, "POST", "/api/v1/invitations", map[string]any{"email": email, "role": role}, Idempotent(), http.StatusCreated)
	member := &Caller{Name: email}
	s.Want(member, "POST", "/api/v1/invitations/accept", map[string]any{"token": invitation["token"], "display_name": role, "password": Password}, nil, http.StatusCreated)
	return member
}

func (s *Service) Eventually(what string, limit time.Duration, check func() (bool, string)) {
	s.T.Helper()
	Eventually(s.T, what, limit, check)
}
