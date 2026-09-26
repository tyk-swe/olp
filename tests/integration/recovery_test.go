//go:build integration

package integration_test

import (
	"encoding/json"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/database"
	"github.com/tyk-swe/olp/internal/testutil"
)

// The shell entrypoints are the same ones operators use. Failure assertions
// inspect the destination, not just exit codes, so a partial restore cannot pass.
func TestReplacementRestoreIsAtomicAndAuthenticatesKeys(t *testing.T) {
	restoreValkey := required(t, "OLP_TEST_RESTORE_VALKEY_URL")
	binary := required(t, "OLP_TEST_BINARY")
	h := newAccessHarness(t)
	h.owner()
	secretID := uuid.NewString()
	tx, err := h.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if err := h.Server.Keys.Store(t.Context(), tx, h.Server.Installation, secretID, "oidc_client", []byte("retained credential"), nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	// This in-process fixture does not emit asynchronous metadata. Real worker
	// drain and history preservation are exercised by browser-integration.sh.
	if _, err := h.Pool.Exec(t.Context(), `INSERT INTO olp.request_metadata_consumer_health VALUES(true,0,0,NULL,now())`); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	root, _ := filepath.Abs("../..")
	write := func(name, data string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	auth := write("auth.key", h.AuthHex)
	master := write("master.json", `{"active_version":1,"keys":[{"version":1,"key":"`+strings.Repeat("ab", 32)+`"}]}`)
	bootstrap := write("bootstrap.token", strings.Repeat("b", 48))
	// Completed installations retain their long-lived keys after bootstrap retirement.
	if err := os.Remove(bootstrap); err != nil {
		t.Fatal(err)
	}
	assets := filepath.Join(dir, "console")
	if err := os.Mkdir(assets, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(assets, "index.html"), []byte("<html>recovery</html>"), 0600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"OLP_DATABASE_URL": h.DBURL, "OLP_VALKEY_URL": restoreValkey, "OLP_RESTORE_VALKEY_ISOLATED": "true", "OLP_BACKUP_TRAFFIC_QUIESCED": "true", "OLP_AUTH_HMAC_KEY_FILE": auth, "OLP_MASTER_KEY_FILE": master, "OLP_BOOTSTRAP_TOKEN_FILE": bootstrap, "OLP_CONSOLE_DIR": assets, "OLP_MAINTENANCE_BIN": binary}
	run := func(script string, args ...string) ([]byte, error) {
		c := exec.CommandContext(t.Context(), filepath.Join(root, "scripts", script), args...)
		c.Dir = root
		c.Env = testutil.Environment(env)
		return c.CombinedOutput()
	}
	out, err := run("backup.sh", filepath.Join(dir, "backups"))
	if err != nil {
		t.Fatalf("backup: %v: %s", err, out)
	}
	backup := strings.TrimSpace(string(out))
	manifest, err := os.ReadFile(backup + ".manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var markers struct{ Format, Schema string }
	if err := json.Unmarshal(manifest, &markers); err != nil || markers.Format != "olp" || markers.Schema != "olp" {
		t.Fatalf("backup manifest markers = %+v, want format and schema olp: %v", markers, err)
	}
	dump, err := os.ReadFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("fresh_installation_requires_configured_bootstrap", func(t *testing.T) {
		fresh, freshURL := accessDatabase(t)
		if err := database.Migrate(t.Context(), fresh); err != nil {
			t.Fatal(err)
		}
		check := exec.CommandContext(t.Context(), os.Getenv("OLP_TEST_BINARY"), "doctor")
		checkEnv := maps.Clone(env)
		checkEnv["OLP_DATABASE_URL"] = freshURL
		check.Env = testutil.Environment(checkEnv)
		out, err := check.CombinedOutput()
		if err == nil || !strings.Contains(string(out), "bootstrap token file is required") {
			t.Fatalf("missing bootstrap was accepted: %v: %s", err, out)
		}
	})
	for _, failure := range []string{"incomplete", "corrupt", "incompatible", "wrong-format", "wrong-schema", "wrong-key", "initialized", "valid"} {
		t.Run(failure, func(t *testing.T) {
			target, url := accessDatabase(t)
			env["OLP_RESTORE_DATABASE_URL"] = url
			env["OLP_MASTER_KEY_FILE"] = master
			candidate := write(failure+".dump", string(dump))
			write(failure+".dump.manifest.json", string(manifest))
			switch failure {
			case "incomplete":
				os.Remove(candidate + ".manifest.json")
			case "corrupt":
				write(failure+".dump", "corrupt")
			case "incompatible":
				var m map[string]any
				json.Unmarshal(manifest, &m)
				m["migration_history"].([]any)[0].(map[string]any)["checksum"] = strings.Repeat("0", 64)
				b, _ := json.Marshal(m)
				write(failure+".dump.manifest.json", string(b))
			case "wrong-format", "wrong-schema":
				var m map[string]any
				json.Unmarshal(manifest, &m)
				if failure == "wrong-format" {
					m["format"] = "olp-archive"
				} else {
					m["schema"] = "public"
				}
				b, _ := json.Marshal(m)
				write(failure+".dump.manifest.json", string(b))
			case "wrong-key":
				env["OLP_MASTER_KEY_FILE"] = write("wrong-master.json", `{"active_version":1,"keys":[{"version":1,"key":"`+strings.Repeat("ef", 32)+`"}]}`)
			case "initialized":
				if _, err := target.Exec(t.Context(), "CREATE SCHEMA sentinel"); err != nil {
					t.Fatal(err)
				}
			}
			out, err := run("restore.sh", candidate)
			if failure == "valid" {
				if err != nil {
					t.Fatalf("restore: %v: %s", err, out)
				}
				id, err := database.Installation(t.Context(), target)
				if err != nil || id != h.Server.Installation {
					t.Fatalf("identity changed: %q %v", id, err)
				}
				read, err := target.Begin(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer read.Rollback(t.Context())
				plain, err := h.Server.Keys.Read(t.Context(), read, id, secretID, "oidc_client")
				if err != nil || string(plain) != "retained credential" {
					t.Fatalf("credential lost: %v", err)
				}
				restartEnv := maps.Clone(env)
				restartEnv["OLP_DATABASE_URL"] = url
				restartEnv["OLP_LISTEN_ADDR"] = "127.0.0.1:0"
				restartEnv["OLP_OBSERVABILITY_LISTEN_ADDR"] = "127.0.0.1:0"
				restartEnv["OLP_SHUTDOWN_TIMEOUT"] = "2s"
				process := testutil.StartProcess(t, os.Getenv("OLP_TEST_BINARY"), "all", restartEnv)
				if err := process.Stop(5 * time.Second); err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil {
				t.Fatalf("accepted %s backup", failure)
			}
			var exists bool
			if err := target.QueryRow(t.Context(), "SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname='olp')").Scan(&exists); err != nil || exists {
				t.Fatalf("failed restore mutated destination: %v", err)
			}
		})
	}
}
