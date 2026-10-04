package codecli

import (
	"context"
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

	"github.com/tyk-swe/olp/internal/codeadapter"
)

// Agent drives a pinned, unmodified coding agent with the configuration OLP
// generated for it, in a private home and working directory. Its environment
// is an allowlist, so a workstation login, vendor key or the harness's own
// configuration cannot make an OLP-key-only test pass, and every request it
// sends outside the loopback gateway meets Trap.
type Agent struct {
	Binary string
	Home   string
	Work   string
	Key    string
	Trap   *Trap
	// Registry names the package registry the agent asks for at startup to
	// install its own dependencies, which the trap refuses; no model traffic
	// goes there.
	Registry string
	env      []string
	args     func(configuration string, args []string) (string, []string)
}

// Decoy is a workstation credential every agent environment carries, which
// the generated configuration must keep from reaching OLP or the upstream.
const Decoy = "SEEDED_WORKSTATION_SECRET"

func agent(t testing.TB, variable, version, key string, versionOf func(string) string) *Agent {
	t.Helper()
	binary := os.Getenv(variable)
	if binary == "" {
		t.Fatalf("%s is required; run scripts/code-mode-qualification.sh install", variable)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, binary, "--version").CombinedOutput()
	if err != nil || versionOf(strings.TrimSpace(string(output))) != version {
		t.Fatalf("expected the pinned release %s: %s (%v)", version, output, err)
	}
	home := t.TempDir()
	return &Agent{Binary: binary, Home: home, Work: t.TempDir(), Key: key, Trap: NewTrap(t), env: []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + home, "TERM=dumb", "NO_COLOR=1", "OLP_API_KEY=" + key,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"), "XDG_DATA_HOME=" + filepath.Join(home, ".local", "share"),
		"XDG_CACHE_HOME=" + filepath.Join(home, ".cache"), "XDG_STATE_HOME=" + filepath.Join(home, ".local", "state"),
	}}
}

// NewClaudeCode returns the pinned Claude Code. Run sources the generated
// shell configuration, which must replace the decoy Anthropic key.
func NewClaudeCode(t testing.TB, key string) *Agent {
	a := agent(t, "OLP_CLAUDE_CODE_BINARY", codeadapter.ClaudeCodeVersion, key, func(out string) string {
		return strings.TrimSuffix(out, " (Claude Code)")
	})
	a.env = append(a.env, "ANTHROPIC_API_KEY="+Decoy)
	a.args = func(configuration string, args []string) (string, []string) {
		write(t, filepath.Join(a.Work, "olp.sh"), configuration)
		return "bash", append([]string{"-c", `set -eu; . ./olp.sh; exec "$0" "$@"`, a.Binary}, args...)
	}
	return a
}

// NewOpenCode returns the pinned OpenCode. Run saves the generated
// configuration as the project's opencode.json, unchanged.
func NewOpenCode(t testing.TB, key string) *Agent {
	a := agent(t, "OLP_OPENCODE_BINARY", codeadapter.OpenCodeVersion, key, func(out string) string { return out })
	a.env = append(a.env, "OPENCODE_DISABLE_AUTOUPDATE=1", "OPENCODE_DISABLE_MODELS_FETCH=1", "OPENCODE_DISABLE_LSP_DOWNLOAD=1", "OPENCODE_DISABLE_DEFAULT_PLUGINS=1",
		"OPENCODE_API_KEY="+Decoy, "ZHIPU_API_KEY="+Decoy)
	a.Registry = "registry.npmjs.org"
	a.args = func(configuration string, args []string) (string, []string) {
		write(t, filepath.Join(a.Work, "opencode.json"), configuration)
		return a.Binary, args
	}
	return a
}

func write(t testing.TB, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

// Command prepares the agent with configuration, its stdin closed.
func (a *Agent) Command(ctx context.Context, configuration string, args ...string) *exec.Cmd {
	name, argv := a.args(configuration, args)
	cmd := exec.CommandContext(ctx, name, argv...)
	cmd.Dir = a.Work
	cmd.Env = append(append([]string(nil), a.env...), a.Trap.Environment()...)
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

// Run runs the agent to completion and returns its output.
func (a *Agent) Run(t testing.TB, configuration string, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	out, err := a.Command(ctx, configuration, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", filepath.Base(a.Binary), err, out)
	}
	return out
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

// Escaped lists the hosts the agent asked the trap for, other than loopback
// and its package registry.
func (a *Agent) Escaped() []string {
	var hosts []string
	for _, host := range a.Trap.Hosts() {
		if host != a.Registry {
			hosts = append(hosts, host)
		}
	}
	return hosts
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
