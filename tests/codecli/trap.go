package codecli

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Decoy is a workstation credential every agent environment carries, which
// the generated configuration must keep from reaching OLP or the upstream.
const Decoy = "SEEDED_WORKSTATION_SECRET"

func resolvedBinary(t testing.TB, variable, format string) (string, string) {
	t.Helper()
	binary := os.Getenv(variable)
	version := os.Getenv(strings.TrimSuffix(variable, "_BINARY") + "_VERSION")
	if binary == "" || version == "" {
		t.Fatalf("%s and its resolved version are required; source the environment from scripts/code-mode-qualification.sh resolve", variable)
	}
	home := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "--version")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "CODEX_HOME=" + home,
		"XDG_CONFIG_HOME=" + home, "XDG_DATA_HOME=" + home, "XDG_CACHE_HOME=" + home, "XDG_STATE_HOME=" + home,
		"DISABLE_AUTOUPDATER=1", "OPENCODE_DISABLE_AUTOUPDATE=1", "NO_COLOR=1"}
	output, err := cmd.CombinedOutput()
	if err != nil || strings.TrimSpace(string(output)) != fmt.Sprintf(format, version) {
		t.Fatalf("expected resolved release %s: %s (%v)", version, output, err)
	}
	t.Logf("official client: %s", strings.TrimSpace(string(output)))
	return binary, version
}

func agent(t testing.TB, variable, key, format string) *Agent {
	t.Helper()
	binary, version := resolvedBinary(t, variable, format)
	home := t.TempDir()
	return &Agent{Binary: binary, Version: version, Home: home, Work: t.TempDir(), Key: key, Trap: NewTrap(t), env: []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + home, "TERM=dumb", "NO_COLOR=1", "OLP_API_KEY=" + key,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"), "XDG_DATA_HOME=" + filepath.Join(home, ".local", "share"),
		"XDG_CACHE_HOME=" + filepath.Join(home, ".cache"), "XDG_STATE_HOME=" + filepath.Join(home, ".local", "state"),
	}}
}

// Trap is a forward proxy that refuses every request and records the host
// each asked for, so a request that would leave the machine instead of
// reaching OLP is visible.
type Trap struct {
	server *httptest.Server
	mu     sync.Mutex
	hosts  []string
}

func NewTrap(t testing.TB) *Trap {
	p := &Trap{}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if r.Method != http.MethodConnect && r.URL.Host != "" {
			host = r.URL.Host
		}
		p.mu.Lock()
		p.hosts = append(p.hosts, host)
		p.mu.Unlock()
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(p.server.Close)
	return p
}

// Environment points an agent's proxy variables at the trap, sparing only
// loopback, where the gateway under test listens.
func (p *Trap) Environment() []string {
	return []string{"HTTPS_PROXY=" + p.server.URL, "HTTP_PROXY=" + p.server.URL, "https_proxy=" + p.server.URL, "http_proxy=" + p.server.URL,
		"NO_PROXY=127.0.0.1,localhost", "no_proxy=127.0.0.1,localhost"}
}

// Hosts lists the hosts requested through the trap, without loopback.
func (p *Trap) Hosts() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var hosts []string
	for _, host := range p.hosts {
		if name, _, err := net.SplitHostPort(host); err == nil {
			host = name
		}
		if host != "127.0.0.1" && host != "localhost" {
			hosts = append(hosts, host)
		}
	}
	return hosts
}
