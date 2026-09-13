//go:build integration

package integration_test

import (
	"testing"

	"github.com/tyk-swe/olp/internal/database"
)

func TestMigrationDDLFailureRollsBackAndRecovers(t *testing.T) {
	pool, _ := accessDatabase(t)
	// Fail after the baseline has created its first tables inside the migration
	// transaction, rather than merely timing out before it can acquire the lock.
	_, err := pool.Exec(t.Context(), `CREATE FUNCTION public.reject_test_index() RETURNS event_trigger
        LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected DDL failure'; END $$;
        CREATE EVENT TRIGGER reject_test_index ON ddl_command_start
        WHEN TAG IN ('CREATE INDEX') EXECUTE FUNCTION public.reject_test_index()`)
	if err != nil {
		t.Fatal(err)
	}
	if database.Migrate(t.Context(), pool) == nil {
		t.Fatal("expected migration failure")
	}
	var exists bool
	if err = pool.QueryRow(t.Context(), "SELECT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname='olp_go')").Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("failed DDL left partial schema or migration history")
	}
	if _, err = pool.Exec(t.Context(), "DROP EVENT TRIGGER reject_test_index; DROP FUNCTION public.reject_test_index()"); err != nil {
		t.Fatal(err)
	}
	if err = database.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	if _, err = database.Installation(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
}

func TestPopulatedInstallationAppliesForwardMigration(t *testing.T) {
	h := newAccessHarness(t)
	// Reconstruct the prior Go baseline in this disposable fixture. Product
	// migrations remain forward-only; no downgrade command is provided.
	_, err := h.Pool.Exec(t.Context(), `DROP INDEX olp_go.sessions_expiry,olp_go.secrets_expiry,
        olp_go.auth_admission_expiry,olp_go.invitations_email;
        ALTER TABLE olp_go.oidc_identities DROP COLUMN role_claims;
        DELETE FROM olp_go.migrations WHERE version IN ('0002_cleanup_indexes.sql','0003_oidc_role_claims.sql')`)
	if err != nil {
		t.Fatal(err)
	}
	owner := h.owner()
	profile := h.want(owner, "GET", "/api/v3/profile", nil, nil, 200)
	if _, err = h.Pool.Exec(t.Context(), `INSERT INTO olp_go.oidc_identities(id,user_id,issuer,subject,email_at_link)
        VALUES(gen_random_uuid(),$1,'https://identity.example','existing-subject','owner@example.com')`, profile["id"]); err != nil {
		t.Fatal(err)
	}
	input := map[string]any{"name": "survives forward migration"}
	headers := map[string]string{"Idempotency-Key": "forward-migration"}
	issued := h.want(owner, "POST", "/api/v3/api-keys", input, headers, 201)
	if _, err = database.Installation(t.Context(), h.Pool); err == nil {
		t.Fatal("startup accepted a pending migration")
	}
	if err = database.Migrate(t.Context(), h.Pool); err != nil {
		t.Fatal(err)
	}
	id, err := database.Installation(t.Context(), h.Pool)
	if err != nil || id != h.Server.Installation {
		t.Fatal("installation identity changed during migration")
	}
	h.want(owner, "GET", "/api/v3/sessions/current", nil, nil, 200)
	replay := h.want(owner, "POST", "/api/v3/api-keys", input, headers, 201)
	if replay["id"] != issued["id"] || replay["secret"] != issued["secret"] {
		t.Fatal("forward migration changed key identity or encrypted replay")
	}
	var retained bool
	if err = h.Pool.QueryRow(t.Context(), `SELECT role_claims IS NULL AND subject='existing-subject' AND email_at_link='owner@example.com'
        FROM olp_go.oidc_identities WHERE user_id=$1`, profile["id"]).Scan(&retained); err != nil || !retained {
		t.Fatal("forward migration changed the identity or invented verified role inputs")
	}
}
