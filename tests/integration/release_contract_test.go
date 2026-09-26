//go:build integration

package integration_test

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/testutil"
	"github.com/tyk-swe/olp/openapi"
)

// Every operation in the current embedded contract must reach a concrete handler.
// Feature suites exercise the authorized success and failure response schemas.
func assertConcreteManagementHandlers(t *testing.T, origin string) {
	t.Helper()
	var document struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(openapi.Document, &document); err != nil {
		t.Fatal(err)
	}
	parameter := regexp.MustCompile(`\{[^}]+\}`)
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	count := 0
	for path, operations := range document.Paths {
		for method := range operations {
			if !strings.Contains(" get post put patch delete head options ", " "+method+" ") {
				continue
			}
			count++
			url := origin + parameter.ReplaceAllString(path, "00000000-0000-4000-8000-000000000001")
			req, _ := http.NewRequestWithContext(t.Context(), strings.ToUpper(method), url, strings.NewReader(`{}`))
			req.Header.Set("Content-Type", "application/json")
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
			resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode >= 500 || strings.Contains(string(body), "The requested management operation does not exist.") {
				t.Errorf("current contract operation %s %s has no concrete handler (status %d)", method, path, resp.StatusCode)
			}
		}
	}
	t.Logf("validated registration of %d current management operations", count)
}

func TestComposeSecretsSurviveBootstrapRetirement(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "secrets")
	run := func(script string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), filepath.Join(root, "scripts", script))
		cmd.Dir = root
		cmd.Env = testutil.Environment(map[string]string{"OLP_COMPOSE_SECRETS_DIR": dir})
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v: %s", script, err, out)
		}
	}
	run("prepare-compose-production.sh")
	files := []string{"olp_master_key", "olp_auth_hmac_key", "olp_database_password", "production.env"}
	before := map[string]string{}
	for _, name := range files {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		before[name] = string(data)
		info, _ := os.Stat(filepath.Join(dir, name))
		if info.Mode().Perm() != 0600 {
			t.Fatalf("%s is not private", name)
		}
	}
	run("prepare-compose-production.sh")
	run("retire-compose-bootstrap-secret.sh")
	run("prepare-compose-production.sh")
	if _, err := os.Stat(filepath.Join(dir, "olp_bootstrap_token")); !os.IsNotExist(err) {
		t.Fatal("preparation recreated retired bootstrap credential")
	}
	for _, name := range files {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(data) != before[name] {
			t.Fatalf("long-lived %s changed across bootstrap retirement", name)
		}
	}
}
