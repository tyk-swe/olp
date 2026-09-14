//go:build integration

package integration_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/testutil"
	"github.com/tyk-swe/olp/openapi"
)

func TestProcessModesPrivateProbesAndShutdown(t *testing.T) {
	binary := required(t, "OLP_TEST_BINARY")
	assets := t.TempDir()
	if err := os.WriteFile(filepath.Join(assets, "index.html"), []byte("<html>foundation shell</html>"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"all", "gateway", "control", "worker"} {
		t.Run(mode, func(t *testing.T) {
			env := map[string]string{"OLP_DATABASE_URL": required(t, "OLP_TEST_DATABASE_URL"),
				"OLP_AUTH_HMAC_KEY_FILE": required(t, "OLP_AUTH_HMAC_KEY_FILE"),
				"OLP_MASTER_KEY_FILE":    required(t, "OLP_MASTER_KEY_FILE"), "OLP_VALKEY_URL": required(t, "OLP_TEST_VALKEY_URL"), "OLP_LISTEN_ADDR": "127.0.0.1:0", "OLP_OBSERVABILITY_LISTEN_ADDR": "127.0.0.1:0", "OLP_CONSOLE_DIR": assets, "OLP_SHUTDOWN_TIMEOUT": "1s"}
			p := testutil.StartProcess(t, binary, mode, env)
			httpClient := &http.Client{Timeout: 3 * time.Second}
			get := func(origin, path string, status int) []byte {
				t.Helper()
				resp, err := httpClient.Get(origin + path)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				body, _ := io.ReadAll(resp.Body)
				if resp.StatusCode != status {
					t.Fatalf("%s: got %d want %d: %s", path, resp.StatusCode, status, body)
				}
				return body
			}
			get(p.PrivateOrigin, "/health/live", 200)
			get(p.PrivateOrigin, "/api/v3/openapi.json", 404)
			env["OLP_OBSERVABILITY_LISTEN_ADDR"] = strings.TrimPrefix(p.PrivateOrigin, "http://")
			probe := exec.Command(binary, "health-probe")
			probe.Env = testutil.Environment(env)
			if output, err := probe.CombinedOutput(); err != nil {
				t.Fatalf("health-probe: %v %s", err, output)
			}
			if mode != "worker" {
				for _, path := range []string{"/health/live", "/health/ready", "/metrics"} {
					get(p.PublicOrigin, path, 404)
				}
				management := mode == "all" || mode == "control"
				if management {
					body := get(p.PublicOrigin, "/api/v3/openapi.json", 200)
					if string(body) != string(openapi.Document) {
						t.Fatal("served contract differs")
					}
					get(p.PublicOrigin, "/api/v3/bootstrap", 501)
					get(p.PublicOrigin, "/health", 200)
					get(p.PublicOrigin, "/providers", 200)
				} else {
					get(p.PublicOrigin, "/api/v3/openapi.json", 404)
					get(p.PublicOrigin, "/", 404)
				}
				if mode == "control" {
					get(p.PublicOrigin, "/v1/models", 404)
				} else {
					// The gateway owns /v1 and answers with native OpenAI errors;
					// the other native surfaces stay explicit placeholders.
					if body := get(p.PublicOrigin, "/v1/models", 401); !strings.Contains(string(body), `"invalid_api_key"`) {
						t.Fatalf("unauthenticated model listing: %s", body)
					}
					get(p.PublicOrigin, "/anthropic/v1/messages", 501)
				}
				// An idle connection cannot prevent the shared shutdown deadline.
				idle, err := net.Dial("tcp", strings.TrimPrefix(p.PublicOrigin, "http://"))
				if err != nil {
					t.Fatal(err)
				}
				defer idle.Close()
			}
			if err := p.Stop(3 * time.Second); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestInvalidConfigurationFailsBeforeBinding(t *testing.T) {
	binary := required(t, "OLP_TEST_BINARY")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	for _, args := range [][]string{{"all"}, {"worker"}, {"migrate"}, {"doctor"}, {"master-key", "status"}} {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Env = testutil.Environment(map[string]string{"OLP_DATABASE_URL": "postgres://secret%broken", "OLP_LISTEN_ADDR": listener.Addr().String()})
		output, err := cmd.CombinedOutput()
		cancel()
		if err == nil || strings.Contains(string(output), "secret") || strings.Contains(string(output), "bind") {
			t.Fatalf("invalid configuration: %v %s", err, output)
		}
	}
}
