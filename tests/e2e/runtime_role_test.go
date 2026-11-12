//go:build e2e

package e2e_test

import (
	"context"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/tests/harness"
)

// TestServiceRunsUnderRuntimeRole follows the documented deployment: the owner
// migrates and grants, and the process connects as the restricted runtime role.
func TestServiceRunsUnderRuntimeRole(t *testing.T) {
	t.Parallel()
	pool, databaseURL := harness.EmptyDatabase(t)
	role := "runtime_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	identifier := pgx.Identifier{role}.Sanitize()
	if _, err := pool.Exec(t.Context(), "CREATE ROLE "+identifier+" LOGIN PASSWORD 'runtime-test'"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := pool.Exec(ctx, "DROP OWNED BY "+identifier); err != nil {
			t.Errorf("drop owned: %v", err)
		}
		if _, err := pool.Exec(ctx, "DROP ROLE "+identifier); err != nil {
			t.Errorf("drop role: %v", err)
		}
	})
	env := newEnvironment(t, databaseURL)
	runBinary(t, env, "migrate", "--runtime-role", role)

	parsed, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	parsed.User = url.UserPassword(role, "runtime-test")
	env.values["OLP_DATABASE_URL"] = parsed.String()
	s := startProcess(t, pool, env)
	owner := s.owner()
	s.want(owner, "POST", "/api/v1/projects", map[string]any{"name": "runtime-role"}, idempotent(), http.StatusCreated)
	s.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "runtime-role", "scopes": []string{"inference"}}, idempotent(), http.StatusCreated)

	config, err := pgx.ParseConfig(parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	restricted, err := pgx.ConnectConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer restricted.Close(context.Background())
	if _, err = restricted.Exec(t.Context(), "SELECT olp.dirty_commitment(date_trunc('day', now()), date_trunc('day', now()) + interval '1 day')"); err != nil {
		t.Fatalf("the runtime role cannot mark commitment usage for export: %v", err)
	}
	var dirtyDays int
	if err = restricted.QueryRow(t.Context(), "SELECT count(*) FROM olp.focus_dirty_days").Scan(&dirtyDays); err != nil || dirtyDays != 1 {
		t.Fatalf("commitment export days = %d, want 1: %v", dirtyDays, err)
	}
	if _, err = restricted.Exec(t.Context(), "DELETE FROM olp.migrations"); err == nil {
		t.Fatal("the runtime role can rewrite migration history")
	}
}

func TestRestoreRejectsQueryDatabaseBeforeAccess(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		address string
		want    string
	}{
		{"path", "postgresql://127.0.0.1/replacement?sslmode=require", "Backup and manifest are required."},
		{"query", "postgresql://127.0.0.1/?dbname=replacement&sslmode=require", "Set the restore destination database in the URL path"},
		{"path_and_query", "postgresql://127.0.0.1/replacement?dbname=replacement", "Set the restore destination database in the URL path"},
		{"encoded_query", "postgresql://127.0.0.1/?%64bname=replacement", "Set the restore destination database in the URL path"},
		{"duplicate_query", "postgresql://127.0.0.1/?dbname=&dbname=replacement", "Set the restore destination database in the URL path"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			command := exec.CommandContext(t.Context(), "bash", "../../scripts/restore.sh", filepath.Join(t.TempDir(), "missing.dump"))
			command.Env = environment{}.list(map[string]string{
				"OLP_RESTORE_DATABASE_URL":    test.address,
				"OLP_RESTORE_VALKEY_ISOLATED": "true",
			})
			output, err := command.CombinedOutput()
			if err == nil || !strings.Contains(string(output), test.want) {
				t.Fatalf("restore preflight = %v, %s; want %s", err, output, test.want)
			}
		})
	}
}

func TestEpochRecoveryWithoutValkey(t *testing.T) {
	t.Parallel()
	pool, databaseURL := harness.Database(t)
	epoch := uuid.NewString()
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.request_metadata_gateway_epochs
 (gateway_instance,process_epoch,started_at,accepted,persisted,dropped,abandoned,retrying,writer_closed,updated_at,stale_candidate_at)
 VALUES('stopped-gateway',$1,now()-interval '3 minutes',1,0,0,0,false,false,now()-interval '2 minutes',now()-interval '30 seconds')`, epoch); err != nil {
		t.Fatal(err)
	}
	env := newEnvironment(t, databaseURL)
	env.values["OLP_VALKEY_URL"] = ""
	s := startProcess(t, pool, env)
	s.eventually("PostgreSQL-only epoch recovery and its health checkpoint", 12*time.Second, func() (bool, string) {
		var recovered, healthy bool
		if err := pool.QueryRow(t.Context(), `SELECT e.stale_detected_at IS NOT NULL AND g.event_count=1,
 EXISTS(SELECT 1 FROM olp.worker_task_health WHERE task='request_metadata_gateway_epoch_detection' AND successes_total>0)
 FROM olp.request_metadata_gateway_epochs e LEFT JOIN olp.request_metadata_ingestion_gaps g ON g.id=e.uncertainty_gap_id
 WHERE e.process_epoch=$1`, epoch).Scan(&recovered, &healthy); err != nil {
			return false, err.Error()
		}
		if recovered && healthy {
			return true, ""
		}
		return false, "the stopped gateway has no recorded gap or successful detector checkpoint"
	})
}
