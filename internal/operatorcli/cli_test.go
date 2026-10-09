package operatorcli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/sdk/management"
)

func TestCommonGeneratedCommandsAreUnambiguous(t *testing.T) {
	for group, commands := range map[string][]string{"api-keys": {"list", "get", "create", "update", "revoke", "rotate"}, "routes": {"list", "get", "create", "update", "activate", "simulate"}, "providers": {"list", "get", "create", "update", "activate"}, "usage": {"summary", "breakdown"}} {
		for _, command := range commands {
			count := 0
			for _, op := range management.Operations() {
				if op.Group == group && commandName(op) == command {
					count++
				}
			}
			if count != 1 {
				t.Errorf("%s %s resolves to %d operations", group, command, count)
			}
		}
	}
}

func testRunner(t *testing.T, server *httptest.Server) (Runner, *bytes.Buffer) {
	t.Helper()
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("management-fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	out := &bytes.Buffer{}
	runner := Runner{Out: out, Err: io.Discard, Getenv: func(name string) string {
		if name == "OLP_MANAGEMENT_URL" {
			return server.URL
		}
		return token
	}}
	return runner, out
}

func TestSavedConfigurationPlanRefusesChangedDestinationAndOmitsSecrets(t *testing.T) {
	changed, applied := false, false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer management-fixture" {
			t.Error("token not sent")
		}
		switch r.URL.Path {
		case "/api/v1/configuration/export":
			w.Write([]byte(`{"digest":"destination-one","document":{}}`))
		case "/api/v1/configuration/plan":
			var input map[string]json.RawMessage
			json.NewDecoder(r.Body).Decode(&input)
			if string(input["expected_digest"]) != `"destination-one"` {
				t.Error("destination digest missing")
			}
			if changed {
				w.Write([]byte(`{"digest":"desired","actions":[],"conflicts":[{"kind":"configuration","key":"destination","action":"conflict","detail":"changed"}],"blockers":[]}`))
			} else {
				w.Write([]byte(`{"digest":"desired","actions":[],"conflicts":[],"blockers":[]}`))
			}
		case "/api/v1/configuration/apply":
			applied = true
			if r.Header.Get("Idempotency-Key") != "apply-key" {
				t.Error("apply key missing")
			}
			w.Write([]byte(`{"digest":"desired","actions":[],"conflicts":[],"blockers":[]}`))
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	}))
	defer server.Close()
	runner, _ := testRunner(t, server)
	dir := t.TempDir()
	doc, bindings, plan := filepath.Join(dir, "document.json"), filepath.Join(dir, "bindings.json"), filepath.Join(dir, "plan.json")
	os.WriteFile(doc, []byte(`{"document":{"format":"olp-configuration"}}`), 0600)
	os.WriteFile(bindings, []byte(`{"provider":"provider-secret"}`), 0600)
	if err := runner.Run(t.Context(), []string{"config", "plan", "--file", doc, "--bindings-file", bindings, "--output", plan}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(plan)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "provider-secret") || strings.Contains(string(data), "secret_bindings") {
		t.Fatal("saved plan contains bindings")
	}
	info, _ := os.Stat(plan)
	if info.Mode().Perm() != 0600 {
		t.Fatal("plan permissions", info.Mode())
	}
	changed = true
	args := []string{"config", "apply", "--plan-file", plan, "--bindings-file", bindings, "--idempotency-key", "apply-key"}
	if err := runner.Run(t.Context(), args); err == nil || applied {
		t.Fatalf("stale plan applied=%v err=%v", applied, err)
	}
	changed = false
	if err := runner.Run(t.Context(), args); err != nil || !applied {
		t.Fatalf("fresh plan applied=%v err=%v", applied, err)
	}
}

func TestGeneratedCommandsSendETagsAndUsageCSVQuotesCells(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.URL.Path != "/api/v1/api-keys/key" || r.Header.Get("If-Match") != `"seen"` {
			t.Errorf("request = %s %v", r.URL, r.Header)
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	runner, _ := testRunner(t, server)
	body := filepath.Join(t.TempDir(), "body")
	os.WriteFile(body, []byte(`{"name":"changed"}`), 0600)
	if err := runner.Run(t.Context(), []string{"keys", "update", "key", "--body-file", body}); err == nil || called {
		t.Fatal("missing ETag was not refused")
	}
	if err := runner.Run(t.Context(), []string{"keys", "update", "key", "--body-file", body, "--if-match", "seen"}); err != nil || !called {
		t.Fatal(err)
	}
	data, err := usageCSV(json.RawMessage(`{"items":[{"route":"=cmd()","cost":"1,2"}]}`))
	if err != nil || !strings.Contains(string(data), "'=cmd()") || !strings.Contains(string(data), `"1,2"`) {
		t.Fatalf("csv=%s err=%v", data, err)
	}
}

func TestClientEnvironmentKeepsKeyValuesOutOfOutputAndQuotesPaths(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key'file")
	os.WriteFile(path, []byte("private-key-value"), 0600)
	for _, client := range []string{"codex", "claude-code", "gemini-cli", "openai"} {
		out := &bytes.Buffer{}
		runner := Runner{Out: out, Err: io.Discard}
		if err := runner.Run(t.Context(), []string{"client-env", client, "--url", "https://olp.example.com", "--key-file", path, "--model", "assistant"}); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.String(), "private-key-value") || !strings.Contains(out.String(), shellQuote(path)) {
			t.Fatalf("unsafe environment: %s", out)
		}
	}
}
