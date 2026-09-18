//go:build integration

package gateway

import (
	"encoding/hex"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/database"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/testutil"
)

func TestReplacementRestoreRetainsMediaJobsAndRotatedCredentials(t *testing.T) {
	if os.Getenv("OLP_TEST_RESTORE_VALKEY_URL") == "" {
		t.Skip("requires isolated recovery Valkey")
	}
	f := seedMediaFixture(t, "api_key", true)
	resp := f.call(t, http.MethodPost, "/v1/videos", videoCreateContentType, strings.NewReader(videoCreateBody))
	created := decodeJSON(t, resp)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create %d: %v", resp.StatusCode, created)
	}
	id := created["id"].(string)
	dir := t.TempDir()
	ringJSON := `{"active_version":2,"keys":[{"version":1,"key":"` + hexKey(23) + `"},{"version":2,"key":"` + hexKey(31) + `"}]}`
	for name, value := range map[string]string{"master.json": ringJSON, "auth.key": hex.EncodeToString(bytes32(19)), "bootstrap.token": strings.Repeat("b", 48), "index.html": "<!doctype html><title>Recovery</title>"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	root, _ := filepath.Abs("../..")
	sourceURL, err := url.Parse(os.Getenv(mediaSystemEnv))
	if err != nil {
		t.Fatal(err)
	}
	sourceURL.Path = "/" + f.pool.Config().ConnConfig.Database
	env := map[string]string{
		"OLP_DATABASE_URL":            sourceURL.String(),
		"OLP_VALKEY_URL":              os.Getenv("OLP_TEST_RESTORE_VALKEY_URL"),
		"OLP_RESTORE_VALKEY_ISOLATED": "true", "OLP_BACKUP_TRAFFIC_QUIESCED": "true",
		"OLP_MASTER_KEY_FILE":      filepath.Join(dir, "master.json"),
		"OLP_AUTH_HMAC_KEY_FILE":   filepath.Join(dir, "auth.key"),
		"OLP_BOOTSTRAP_TOKEN_FILE": filepath.Join(dir, "bootstrap.token"),
		"OLP_CONSOLE_DIR":          dir, "OLP_MEDIA_SPOOL_DIR": filepath.Join(dir, "doctor-spool"),
		"OLP_MAINTENANCE_BIN": os.Getenv("OLP_TEST_BINARY"),
	}
	run := func(program string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), program, args...)
		cmd.Dir = root
		cmd.Env = testutil.Environment(env)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", filepath.Base(program), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run(env["OLP_MAINTENANCE_BIN"], "master-key", "reencrypt")
	run(env["OLP_MAINTENANCE_BIN"], "master-key", "verify-retirement", "1")
	if _, err := f.pool.Exec(t.Context(), "INSERT INTO olp_go.request_metadata_consumer_health(singleton,pending_events,lag_events,checked_at) VALUES(true,0,0,clock_timestamp())"); err != nil {
		t.Fatal(err)
	}
	backup := run("./scripts/backup.sh", filepath.Join(dir, "backup"))
	replacement := systemPool(t)
	if _, err := replacement.Exec(t.Context(), "DROP SCHEMA olp_go CASCADE"); err != nil {
		t.Fatal(err)
	}
	sourceURL.Path = "/" + replacement.Config().ConnConfig.Database
	env["OLP_RESTORE_DATABASE_URL"] = sourceURL.String()
	run("./scripts/restore.sh", backup)
	installation, err := database.Installation(t.Context(), replacement)
	if err != nil || installation != f.installation {
		t.Fatalf("identity changed: %q %v", installation, err)
	}
	keys, err := secrets.ParseRing([]byte(ringJSON))
	if err != nil {
		t.Fatal(err)
	}
	// The retained job resolves its immutable provider, owner, release and
	// credential references exclusively from the restored database.
	f.service.Pool, f.service.Keys = replacement, keys
	resp = f.call(t, http.MethodGet, "/v1/videos/"+id, "", nil)
	job := decodeJSON(t, resp)
	if resp.StatusCode != http.StatusOK || job["status"] != "completed" {
		t.Fatalf("restored refresh %d: %v", resp.StatusCode, job)
	}
	resp = f.call(t, http.MethodGet, "/v1/videos/"+id+"/content", "", nil)
	content, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || string(content) != "video-content" {
		t.Fatalf("restored content %d: %v", resp.StatusCode, err)
	}
	resp = f.call(t, http.MethodDelete, "/v1/videos/"+id, "", nil)
	deleted := decodeJSON(t, resp)
	if resp.StatusCode != http.StatusOK || deleted["deleted"] != true {
		t.Fatalf("restored delete %d: %v", resp.StatusCode, deleted)
	}
}
