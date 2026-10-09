//go:build integration

package integration_test

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/database"
	"github.com/tyk-swe/olp/internal/testutil"
)

func TestManagementNetworksProtectProcessWithoutBlockingInferenceOrProbes(t *testing.T) {
	for _, mode := range []string{"all", "control"} {
		for _, trusted := range []bool{false, true} {
			name := mode + "/direct"
			if trusted {
				name = mode + "/trusted-proxy"
			}
			t.Run(name, func(t *testing.T) {
				pool, dbURL := accessDatabase(t)
				if err := database.Migrate(t.Context(), pool); err != nil {
					t.Fatal(err)
				}
				assets := t.TempDir()
				if err := os.WriteFile(filepath.Join(assets, "index.html"), []byte("<html>network test console</html>"), 0600); err != nil {
					t.Fatal(err)
				}
				env := map[string]string{
					"OLP_DATABASE_URL": dbURL, "OLP_AUTH_HMAC_KEY_FILE": required(t, "OLP_AUTH_HMAC_KEY_FILE"),
					"OLP_MASTER_KEY_FILE": required(t, "OLP_MASTER_KEY_FILE"), "OLP_BOOTSTRAP_TOKEN_FILE": required(t, "OLP_BOOTSTRAP_TOKEN_FILE"),
					"OLP_LISTEN_ADDR": "127.0.0.1:0", "OLP_OBSERVABILITY_LISTEN_ADDR": "127.0.0.1:0", "OLP_CONSOLE_DIR": assets,
					"OLP_SHUTDOWN_TIMEOUT": "1s", "OLP_MANAGEMENT_ALLOWED_CIDRS": "192.0.2.0/24",
				}
				if trusted {
					env["OLP_TRUSTED_PROXY_CIDRS"] = "127.0.0.0/8"
				}
				p := testutil.StartProcess(t, required(t, "OLP_TEST_BINARY"), mode, env)
				client := &http.Client{Timeout: 3 * time.Second}
				request := func(origin, path, forwarded string) (int, []byte) {
					t.Helper()
					r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, origin+path, nil)
					if err != nil {
						t.Fatal(err)
					}
					r.Header.Set("X-Forwarded-For", forwarded)
					resp, err := client.Do(r)
					if err != nil {
						t.Fatal(err)
					}
					defer resp.Body.Close()
					body, err := io.ReadAll(resp.Body)
					if err != nil {
						t.Fatal(err)
					}
					return resp.StatusCode, body
				}
				for _, path := range []string{"/", "/api/v1/auth/capabilities", "/api/v1/users", "/api/v1/openapi.json", "/api/v1/unknown"} {
					status, body := request(p.PublicOrigin, path, "")
					if status != 403 || !strings.Contains(string(body), "management_ip_not_allowed") {
						t.Fatalf("%s: %d %s", path, status, body)
					}
				}
				status, body := request(p.PublicOrigin, "/api/v1/auth/capabilities", "192.0.2.1")
				if trusted {
					var capabilities map[string]bool
					if status != 200 || json.Unmarshal(body, &capabilities) != nil || !capabilities["management_network_restricted"] {
						t.Fatalf("trusted request: %d %s", status, body)
					}
					status, _ = request(p.PublicOrigin, "/api/v1/users", "192.0.2.1")
					if status != 401 {
						t.Fatalf("network policy bypassed authentication: %d", status)
					}
					status, _ = request(p.PublicOrigin, "/api/v1/auth/capabilities", "192.0.2.1, 203.0.113.1")
					if status != 403 {
						t.Fatalf("untrusted intermediate hop granted access: %d", status)
					}
				} else if status != 403 {
					t.Fatalf("spoofed client granted access: %d", status)
				}
				status, body = request(p.PublicOrigin, "/v1/models", "")
				if strings.Contains(string(body), "management_ip_not_allowed") || status == 200 {
					t.Fatalf("inference unexpectedly admitted or network restricted: %d %s", status, body)
				}
				status, _ = request(p.PrivateOrigin, "/health/live", "")
				if status != 200 {
					t.Fatalf("private probe restricted: %d", status)
				}
			})
		}
	}
}
