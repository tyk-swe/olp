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
	if err = pool.QueryRow(t.Context(), "SELECT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname='olp')").Scan(&exists); err != nil {
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
