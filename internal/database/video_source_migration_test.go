//go:build integration

package database

import (
	"io/fs"
	"testing"

	"github.com/google/uuid"
)

func TestIntegrationVideoSourceCleanupPreservesLifecycleAndOtherSecrets(t *testing.T) {
	pool := scratchPool(t)
	if _, err := pool.Exec(t.Context(), `CREATE SCHEMA olp;
CREATE TABLE olp.media_jobs (
 id uuid PRIMARY KEY, state text, strict_contract boolean, lifecycle_state text, native_source_id uuid,
 CONSTRAINT media_jobs_strict_source CHECK (NOT strict_contract OR lifecycle_state <> 'active' OR native_source_id IS NOT NULL)
);
CREATE TABLE olp.secrets (id uuid PRIMARY KEY, purpose text, ciphertext bytea);`); err != nil {
		t.Fatal(err)
	}
	id, other := uuid.NewString(), uuid.NewString()
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.media_jobs VALUES($1,'queued',true,'active',$1)`, id); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct{ id, purpose string }{{id, "media_job_source"}, {other, "provider_credential"}} {
		if _, err := pool.Exec(t.Context(), `INSERT INTO olp.secrets VALUES($1,$2,'provider content'::bytea)`, row.id, row.purpose); err != nil {
			t.Fatal(err)
		}
	}
	migration, err := fs.ReadFile(migrations, "migrations/0045_transient_video_sources.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), string(migration)); err != nil {
		t.Fatal(err)
	}
	var state, lifecycle string
	var strict bool
	var source *string
	if err := pool.QueryRow(t.Context(), `SELECT state,lifecycle_state,strict_contract,native_source_id::text FROM olp.media_jobs WHERE id=$1`, id).Scan(&state, &lifecycle, &strict, &source); err != nil || state != "queued" || lifecycle != "active" || !strict || source != nil {
		t.Fatalf("lifecycle changed or content pointer survived: %s %s %t %v %v", state, lifecycle, strict, source, err)
	}
	var retained, credentials int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FILTER (WHERE purpose='media_job_source'),count(*) FILTER (WHERE purpose='provider_credential') FROM olp.secrets`).Scan(&retained, &credentials); err != nil || retained != 0 || credentials != 1 {
		t.Fatalf("wrong secret cleanup: media=%d credentials=%d err=%v", retained, credentials, err)
	}
}
