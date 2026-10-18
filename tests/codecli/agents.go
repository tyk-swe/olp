package codecli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// NewClaudeCode returns the pinned [CC]. Run sources the generated
// shell configuration, which must replace the decoy Anthropic key.
func NewClaudeCode(t testing.TB, key string) *Agent {
	a := agent(t, "OLP_CLAUDE_CODE_BINARY", codeadapter.ClaudeCodeVersion, key, func(out string) string {
		return strings.TrimSuffix(out, " ([CC])")
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
	a.env = append(a.env, "OPENCODE_DISABLE_AUTOUPDATE=1", "OPENCODE_DISABLE_MODELS_FET", "OPENCODE_DISABLE_LSP_DOWNLOAD=1", "OPENCODE_DISABLE_DEFAULT_PLUGINS=1",
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
