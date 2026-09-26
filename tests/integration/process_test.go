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
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tyk-swe/olp/internal/testutil"
	"github.com/tyk-swe/olp/openapi"
)

func TestProcessModesPrivateProbesAndShutdown(t *testing.T) {
	binary := required(t, "OLP_TEST_BINARY")
	assets := t.TempDir()
	if err := os.WriteFile(filepath.Join(assets, "index.html"), []byte("<html>console shell</html>"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"all", "gateway", "control", "worker"} {
		t.Run(mode, func(t *testing.T) {
			env := map[string]string{"OLP_DATABASE_URL": required(t, "OLP_TEST_DATABASE_URL"),
				"OLP_AUTH_HMAC_KEY_FILE": required(t, "OLP_AUTH_HMAC_KEY_FILE"),
				"OLP_MASTER_KEY_FILE":    required(t, "OLP_MASTER_KEY_FILE"), "OLP_VALKEY_URL": required(t, "OLP_TEST_VALKEY_URL"), "OLP_LISTEN_ADDR": "127.0.0.1:0", "OLP_OBSERVABILITY_LISTEN_ADDR": "127.0.0.1:0", "OLP_CONSOLE_DIR": assets, "OLP_SHUTDOWN_TIMEOUT": "1s"}
			started := time.Now().UTC()
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
			get(p.PrivateOrigin, "/api/v1/openapi.json", 404)
			// This installation has no published runtime. Only modes serving
			// inference must stay unready; management and workers can operate.
			readyStatus := http.StatusOK
			if mode == "all" || mode == "gateway" {
				readyStatus = http.StatusServiceUnavailable
			}
			deadline := time.Now().Add(10 * time.Second)
			for {
				resp, err := httpClient.Get(p.PrivateOrigin + "/health/ready")
				if err == nil {
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
					if resp.StatusCode == readyStatus {
						break
					}
				}
				if time.Now().After(deadline) {
					t.Fatalf("readiness did not reach HTTP %d: %v\n%s", readyStatus, err, p.Log())
				}
				time.Sleep(20 * time.Millisecond)
			}
			env["OLP_OBSERVABILITY_LISTEN_ADDR"] = strings.TrimPrefix(p.PrivateOrigin, "http://")
			probe := exec.Command(binary, "health-probe")
			probe.Env = testutil.Environment(env)
			if output, err := probe.CombinedOutput(); (err == nil) != (readyStatus == http.StatusOK) {
				t.Fatalf("health-probe for readiness HTTP %d: %v %s", readyStatus, err, output)
			}
			if mode == "worker" {
				// Serving nothing is the point of this mode: what it owes the
				// installation is a recovery plane that checks in.
				awaitWorkerPlane(t, started)
			}
			if mode != "worker" {
				for _, path := range []string{"/health/live", "/health/ready", "/metrics"} {
					get(p.PublicOrigin, path, 404)
				}
				management := mode == "all" || mode == "control"
				if management {
					body := get(p.PublicOrigin, "/api/v1/openapi.json", 200)
					if string(body) != string(openapi.Document) {
						t.Fatal("served contract differs")
					}
					get(p.PublicOrigin, "/api/v1/bootstrap", 404)
					assertConcreteManagementHandlers(t, p.PublicOrigin)
					// Usage, pricing and request history must claim their own
					// paths ahead of the catch-all that answers 404 for every
					// management path no surface owns.
					get(p.PublicOrigin, "/api/v1/usage/summary", 401)
					get(p.PublicOrigin, "/health", 200)
					get(p.PublicOrigin, "/providers", 200)
				} else {
					get(p.PublicOrigin, "/api/v1/openapi.json", 404)
					get(p.PublicOrigin, "/", 404)
				}
				if mode == "control" {
					get(p.PublicOrigin, "/v1/models", 404)
				} else {
					// The gateway owns /v1 and answers with native OpenAI errors;
					// native model reads enforce their own authentication envelopes.
					if body := get(p.PublicOrigin, "/v1/models", 401); !strings.Contains(string(body), `"invalid_api_key"`) {
						t.Fatalf("unauthenticated model listing: %s", body)
					}
					get(p.PublicOrigin, "/anthropic/v1/models", 401)
					get(p.PublicOrigin, "/gemini/v1beta/models", 401)
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

// A fresh installation keeps its PostgreSQL state in schema olp and its Valkey
// state under olp:<installation UUID>:, exactly as the released binary writes them.
func TestFreshInstallationUsesOlpStorageNames(t *testing.T) {
	binary := required(t, "OLP_TEST_BINARY")
	pool, url := accessDatabase(t)
	migrate := exec.CommandContext(t.Context(), binary, "migrate")
	migrate.Env = testutil.Environment(map[string]string{"OLP_DATABASE_URL": url})
	if output, err := migrate.CombinedOutput(); err != nil {
		t.Fatalf("migrate: %v: %s", err, output)
	}
	rows, err := pool.Query(t.Context(), "SELECT nspname FROM pg_namespace WHERE nspname LIKE 'olp%' ORDER BY nspname")
	if err != nil {
		t.Fatal(err)
	}
	schemas, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || !slices.Equal(schemas, []string{"olp"}) {
		t.Fatalf("installation schemas = %v, want [olp]: %v", schemas, err)
	}
	var installation string
	var applied int
	if err := pool.QueryRow(t.Context(), "SELECT (SELECT id::text FROM olp.installation WHERE singleton), (SELECT count(*) FROM olp.migrations)").Scan(&installation, &applied); err != nil {
		t.Fatalf("read installation row and migration ledger: %v", err)
	}
	embedded, err := os.ReadDir("../../internal/database/migrations")
	if err != nil || applied == 0 || applied != len(embedded) {
		t.Fatalf("migration ledger holds %d of %d migrations: %v", applied, len(embedded), err)
	}

	valkey := client(t, required(t, "OLP_TEST_VALKEY_URL"), 5*time.Second)
	prefix := "olp:" + installation + ":"
	owned := func() []string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		reply, err := valkey.Do(ctx, "KEYS", "*"+installation+"*")
		if err != nil {
			t.Fatalf("list installation keys: %v", err)
		}
		items, _ := reply.([]any)
		keys := make([]string, 0, len(items))
		for _, item := range items {
			key, ok := item.(string)
			if !ok {
				t.Fatalf("KEYS returned %#v", item)
			}
			keys = append(keys, key)
		}
		return keys
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, key := range owned() {
			if _, err := valkey.Do(ctx, "DEL", key); err != nil {
				t.Errorf("remove %s: %v", key, err)
			}
		}
	})
	worker := testutil.StartProcess(t, binary, "worker", map[string]string{"OLP_DATABASE_URL": url,
		"OLP_AUTH_HMAC_KEY_FILE": required(t, "OLP_AUTH_HMAC_KEY_FILE"), "OLP_MASTER_KEY_FILE": required(t, "OLP_MASTER_KEY_FILE"),
		"OLP_VALKEY_URL": required(t, "OLP_TEST_VALKEY_URL"), "OLP_LISTEN_ADDR": "127.0.0.1:0",
		"OLP_OBSERVABILITY_LISTEN_ADDR": "127.0.0.1:0", "OLP_SHUTDOWN_TIMEOUT": "1s"})
	deadline := time.Now().Add(20 * time.Second)
	for keys := owned(); !slices.Contains(keys, prefix+"request-metadata"); keys = owned() {
		if time.Now().After(deadline) {
			t.Fatalf("worker keys for installation %s = %v, want %srequest-metadata\n%s", installation, keys, prefix, worker.Log())
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := worker.Stop(3 * time.Second); err != nil {
		t.Fatal(err)
	}
	for _, key := range owned() {
		if !strings.HasPrefix(key, prefix) {
			t.Fatalf("installation key %q is outside namespace %q", key, prefix)
		}
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

// awaitWorkerPlane waits until every worker task has recorded liveness against
// this run. The health rows outlive the processes that wrote them, so only a
// checkpoint written since this process started proves its plane is running.
func awaitWorkerPlane(t *testing.T, since time.Time) {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), required(t, "OLP_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	wanted := []string{"budget_alert_delivery", "cost_reconciliation", "maintenance", "media_reconciliation", "request_metadata_consumer",
		"request_metadata_gateway_epoch_detection"}
	deadline := time.Now().Add(20 * time.Second)
	for {
		rows, err := pool.Query(t.Context(),
			"SELECT task FROM olp.worker_task_health WHERE checked_at>=$1 ORDER BY task", since)
		if err != nil {
			t.Fatal(err)
		}
		reported, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		if slices.Equal(reported, wanted) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("worker tasks that checkpointed since startup: %v, want %v", reported, wanted)
		}
		select {
		case <-t.Context().Done():
			t.Fatal(t.Context().Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
}
