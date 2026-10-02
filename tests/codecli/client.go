// Package codecli drives the pinned, unmodified official Codex executable.
package codecli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const Version = "0.153.2"
const Model = "gpt-5.4"

type Client struct {
	Binary string
	Home   string
	Work   string
	Key    string
}

func New(t testing.TB, baseURL, key string, websocket bool) *Client {
	t.Helper()
	binary := os.Getenv("OLP_CODEX_BINARY")
	if binary == "" {
		t.Fatal("OLP_CODEX_BINARY is required; run scripts/code-mode-qualification.sh install")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, binary, "--version").CombinedOutput()
	if err != nil || strings.TrimSpace(string(output)) != "codex-cli "+Version {
		t.Fatalf("expected official Codex %s: %s (%v)", Version, output, err)
	}
	c := &Client{Binary: binary, Home: t.TempDir(), Work: t.TempDir(), Key: key}
	config := fmt.Sprintf(`model = %q
model_provider = "olp"
approval_policy = "never"
sandbox_mode = "read-only"
web_search = "disabled"

[model_providers.olp]
name = "OLP controlled qualification"
base_url = %q
env_key = "OLP_API_KEY"
wire_api = "responses"
requires_openai_auth = false
supports_websockets = %t
request_max_retries = 0
stream_max_retries = 0
stream_idle_timeout_ms = 5000
`, Model, baseURL, websocket)
	if err := os.WriteFile(filepath.Join(c.Home, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	return c
}

func (c *Client) Command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, c.Binary, args...)
	cmd.Dir = c.Work
	// An allowlist prevents a workstation login, provider key or injected Codex
	// configuration from silently making an OLP-key-only test pass.
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + c.Home, "CODEX_HOME=" + c.Home,
		"OLP_API_KEY=" + c.Key, "TERM=dumb", "NO_COLOR=1"}
	return cmd
}

func (c *Client) Exec(t testing.TB, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	command := append([]string{"exec", "--skip-git-repo-check", "--json"}, args...)
	out, err := c.Command(ctx, command...).CombinedOutput()
	if err != nil {
		t.Fatalf("official Codex exec: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(c.Home, "auth.json")); !os.IsNotExist(err) {
		t.Fatalf("OLP-key-only workflow unexpectedly created auth.json: %v", err)
	}
	return out
}
