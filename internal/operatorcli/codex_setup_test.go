package operatorcli_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/pelletier/go-toml/v2"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codemode/codeclient"
	"github.com/tyk-swe/olp/internal/operatorcli"
)

const testGateway = "https://gateway.example.test/prefix"
const testModel = "published-model"
const extraServer = "\n[mcp_servers.from_publication]\nurl = \"https://mcp.example.test\"\nbearer_token_env_var = \"OLP_API_KEY\"\n"

func setupCommand(t *testing.T) (func(string, ...string) (string, error), string, codemode.ClientConfiguration) {
	t.Helper()
	root := t.TempDir()
	dir, token := filepath.Join(root, "codex"), filepath.Join(root, "token")
	if err := os.WriteFile(token, []byte("management-test-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	routeID := uuid.NewString()
	route := codemode.Route{Enabled: true, RevisionID: uuid.NewString(), Revision: 3, Slug: "team", Models: []string{testModel}}
	offers := codeclient.Offers{{Adapter: codemode.AdapterCodex, Models: route.Models}}
	published, err := codeclient.Generate(route, offers, codeclient.Request{GatewayURL: testGateway, Client: "codex", Model: testModel})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/code/routes/"+routeID+"/client-config" ||
			q.Get("client") != "codex" || q.Get("gateway_url") != testGateway || q.Get("model") != testModel ||
			r.Header.Get("Authorization") != "Bearer management-test-token" {
			t.Error("CLI did not request the selected published Codex configuration")
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		result := published
		result.Configuration += extraServer
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(result); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	return func(action string, extra ...string) (string, error) {
		var out bytes.Buffer
		env := map[string]string{"CODEX_HOME": dir, "OLP_API_KEY": "inference-test-token"}
		args := []string{"code-setup", action}
		if action == "preview" || action == "apply" {
			env["OLP_MANAGEMENT_URL"], env["OLP_MANAGEMENT_TOKEN_FILE"] = server.URL, token
			args = append(args, "--route", routeID, "--gateway-url", testGateway, "--model", testModel)
		}
		runner := operatorcli.Runner{Getenv: func(key string) string { return env[key] }, Out: &out, Err: &out}
		err := runner.Run(t.Context(), append(args, extra...))
		return out.String(), err
	}, filepath.Join(dir, "config.toml"), published
}

func TestCodexSetupRoundTrip(t *testing.T) {
	t.Parallel()
	run, path, publication := setupCommand(t)
	existing := `# workstation configuration
model = 'previous'  # keep this comment
model_provider = "custom"
approval_policy = "on-request"

[mcp_servers.local]
command = "local-tool"

[model_providers.custom]
name = "Other provider"

[model_providers.olp]
name = "Previous OLP"
base_url = "https://previous.example.test/code/blue"
env_key = "MY_EXISTING_KEY"
wire_api = "responses"

[profiles.keep]
model = "other-model"
`
	writeConfig(t, path, existing)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	output, err := run("preview")
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{"Would configure", "config.toml", publication.BaseURL, publication.Model, publication.RevisionID} {
		if !strings.Contains(output, part) {
			t.Errorf("preview missing %q in %q", part, output)
		}
	}
	if got := readConfig(t, path); got != existing {
		t.Fatal("preview changed the config")
	}
	if _, err := run("apply", "--revision", uuid.NewString()); err == nil || readConfig(t, path) != existing {
		t.Fatalf("apply accepted a different published revision or changed the config: %v", err)
	}
	if _, err := run("apply", "--revision", publication.RevisionID); err != nil {
		t.Fatal(err)
	}
	installed := readConfig(t, path)
	var parsed struct {
		Model         string `toml:"model"`
		ModelProvider string `toml:"model_provider"`
		Providers     map[string]struct {
			BaseURL string `toml:"base_url"`
			EnvKey  string `toml:"env_key"`
		} `toml:"model_providers"`
	}
	if err := toml.Unmarshal([]byte(installed), &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Model != publication.Model || parsed.ModelProvider != "olp" ||
		parsed.Providers["olp"].EnvKey != "OLP_API_KEY" || parsed.Providers["olp"].BaseURL != publication.BaseURL {
		t.Fatalf("wrong installed settings: %+v", parsed)
	}
	for _, absent := range []string{"management-test-token", "inference-test-token", "from_publication"} {
		if strings.Contains(installed, absent) {
			t.Errorf("setup wrote %q to the config", absent)
		}
	}
	for _, intact := range []string{"# workstation configuration", "# keep this comment", "approval_policy = \"on-request\"", "[mcp_servers.local]\ncommand = \"local-tool\"", "[model_providers.custom]\nname = \"Other provider\""} {
		if !strings.Contains(installed, intact) {
			t.Errorf("apply changed unrelated config %q", intact)
		}
	}
	info, err := os.Stat(path)
	if err != nil || before.Mode().Perm() != info.Mode().Perm() {
		t.Fatalf("apply did not preserve file permissions: %v", err)
	}
	if output, err := run("apply"); err != nil || !strings.Contains(output, "Already configured") || readConfig(t, path) != installed {
		t.Fatalf("second apply was not a no-op: %q, %v", output, err)
	}
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(info, after) {
		t.Fatalf("no-op apply replaced the file: %v", err)
	}
	const userComment = "# my new comment\n"
	const userEdit = "\n[mcp_servers.added]\ncommand = \"another-tool\"\n"
	writeConfig(t, path, userComment+installed+userEdit)
	if output, err := run("status"); err != nil || !strings.Contains(output, publication.RevisionID) {
		t.Fatalf("offline status = %q, %v", output, err)
	}
	if _, err := run("detach"); err != nil {
		t.Fatal(err)
	}
	if got := readConfig(t, path); got != userComment+existing+userEdit {
		t.Fatalf("detach did not restore the prior configuration and retain unrelated edits:\n%s", got)
	}
}

func TestCodexSetupRefusesDrift(t *testing.T) {
	t.Parallel()
	run, path, _ := setupCommand(t)
	writeConfig(t, path, "model = \"previous\"\n")
	if _, err := run("apply"); err != nil {
		t.Fatal(err)
	}
	drifted := strings.Replace(readConfig(t, path), "\nmodel = \""+testModel+"\"", "\nmodel = \"my-choice\"", 1)
	writeConfig(t, path, drifted)
	for _, action := range []string{"status", "preview", "apply", "detach"} {
		_, err := run(action)
		if err == nil || !strings.Contains(err.Error(), "conflict in model") || readConfig(t, path) != drifted {
			t.Errorf("%s did not report drift without writing: %v", action, err)
		}
	}
}

func TestCodexSetupRefusesUnsupportedConfig(t *testing.T) {
	t.Parallel()
	for name, config := range map[string]string{
		"duplicate root setting": "model = \"one\"\nmodel = \"two\"\n",
		"wrong model type":       "model = true\n",
		"inline OLP provider":    "model_providers = { olp = { name = \"existing\" } }\n",
		"inline provider parent": "model_providers = { custom = { name = \"existing\" } }\n",
		"nested OLP provider":    "[model_providers.olp.auth]\ncommand = \"custom-command\"\n",
		"custom OLP provider":    "[model_providers.olp]\nhttp_headers = { Authorization = \"existing\" }\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			run, path, _ := setupCommand(t)
			writeConfig(t, path, config)
			if _, err := run("apply"); err == nil || readConfig(t, path) != config {
				t.Fatalf("apply accepted unsupported config or changed bytes: %v", err)
			}
		})
	}
}

func TestCodexSetupMissingAndUnterminatedConfigs(t *testing.T) {
	t.Parallel()
	for _, existing := range []string{"", "# existing comment without a newline", "model = 'old'\nmodel_provider = 'custom'", "[mcp_servers.local]\r\ncommand = \"run\""} {
		t.Run(existing, func(t *testing.T) {
			t.Parallel()
			run, path, _ := setupCommand(t)
			if existing != "" {
				writeConfig(t, path, existing)
			}
			if _, err := run("preview"); err != nil {
				t.Fatal(err)
			}
			if existing == "" {
				if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
					t.Fatalf("preview created the Codex directory: %v", err)
				}
			}
			if _, err := run("apply"); err != nil {
				t.Fatal(err)
			}
			if _, err := run("detach"); err != nil {
				t.Fatal(err)
			}
			if existing == "" {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("detach did not remove the file created by apply: %v", err)
				}
			} else if got := readConfig(t, path); got != existing {
				t.Fatalf("detach changed original file %q to %q", existing, got)
			}
		})
	}
}

func TestCodexSetupRefusesSymlink(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks requires privileges on Windows")
	}
	run, path, _ := setupCommand(t)
	target := filepath.Join(filepath.Dir(path), "real.toml")
	const existing = "model = \"previous\"\n"
	writeConfig(t, target, existing)
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := run("apply"); err == nil || readConfig(t, target) != existing {
		t.Fatalf("apply followed the symlink or changed the target: %v", err)
	}
}

func writeConfig(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0640); err != nil {
		t.Fatal(err)
	}
}

func readConfig(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
