//go:build integration

package database

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// scratchPool returns a pool on a new, empty database that is dropped after
// the test. The integration suite provisions the admin connection.
func scratchPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	admin := os.Getenv("OLP_TEST_DATABASE_ADMIN_URL")
	if admin == "" {
		t.Fatal("OLP_TEST_DATABASE_ADMIN_URL is required; run make integration")
	}
	cfg, err := pgxpool.ParseConfig(admin)
	if err != nil {
		t.Fatal(err)
	}
	adminPool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	name := pgx.Identifier{"migrate_" + strings.ReplaceAll(uuid.NewString(), "-", "")}
	if _, err = adminPool.Exec(t.Context(), "CREATE DATABASE "+name.Sanitize()); err != nil {
		adminPool.Close()
		t.Fatal(err)
	}
	cfg.ConnConfig.Database = name[0]
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		adminPool.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		if _, err := adminPool.Exec(context.Background(), "DROP DATABASE "+name.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Logf("drop scratch database: %v", err)
		}
		adminPool.Close()
	})
	return pool
}

// laterHistory is this binary's migrations followed by two later ones.
func laterHistory(t *testing.T) fstest.MapFS {
	t.Helper()
	entries, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	history := fstest.MapFS{}
	for _, entry := range entries {
		data, err := fs.ReadFile(migrations, "migrations/"+entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		history["migrations/"+entry.Name()] = &fstest.MapFile{Data: data}
	}
	history["migrations/9998_first_later.sql"] = &fstest.MapFile{Data: []byte("CREATE TABLE olp.first_later (id integer)")}
	history["migrations/9999_second_later.sql"] = &fstest.MapFile{Data: []byte("CREATE TABLE olp.second_later (id integer)")}
	return history
}

func ledger(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), "SELECT version FROM olp.migrations ORDER BY version")
	if err != nil {
		t.Fatal(err)
	}
	versions, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return versions
}

func TestMigrationRefusesHistoryWithAHole(t *testing.T) {
	pool := scratchPool(t)
	history := laterHistory(t)
	if err := migrate(t.Context(), pool, history); err != nil {
		t.Fatal(err)
	}
	// The missing step would apply cleanly, so only the history check can
	// refuse the later step that is already recorded.
	if _, err := pool.Exec(t.Context(), "DELETE FROM olp.migrations WHERE version='9998_first_later.sql'; DROP TABLE olp.first_later"); err != nil {
		t.Fatal(err)
	}
	before := ledger(t, pool)
	err := migrate(t.Context(), pool, history)
	if err == nil || err.Error() != "olp migration history is not a sequential prefix" {
		t.Fatalf("migrate = %v; want the sequential history refusal", err)
	}
	if after := ledger(t, pool); !slices.Equal(before, after) {
		t.Fatalf("refused migration changed history from %v to %v", before, after)
	}
	var applied bool
	if err = pool.QueryRow(t.Context(), "SELECT to_regclass('olp.first_later') IS NOT NULL").Scan(&applied); err != nil || applied {
		t.Fatal("refused migration left the missing step applied", err)
	}
}

func TestMigrationAndStartupRefuseANewerSchema(t *testing.T) {
	pool := scratchPool(t)
	if err := migrate(t.Context(), pool, laterHistory(t)); err != nil {
		t.Fatal(err)
	}
	before := ledger(t, pool)
	if err := Migrate(t.Context(), pool); err == nil || err.Error() != "database requires a newer olp binary" {
		t.Fatalf("Migrate = %v; want the newer schema refusal", err)
	}
	if after := ledger(t, pool); !slices.Equal(before, after) {
		t.Fatalf("refused migration changed history from %v to %v", before, after)
	}
	if _, err := Installation(t.Context(), pool); err == nil || err.Error() != "database schema is newer than this olp binary" {
		t.Fatalf("Installation = %v; want the newer schema refusal", err)
	}
}

func TestStoredRouteFidelityStatesStrictOrTransformed(t *testing.T) {
	pool := scratchPool(t)
	if err := Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	user, route := uuid.NewString(), uuid.NewString()
	if _, err := pool.Exec(t.Context(), "INSERT INTO olp.users(id,email,display_name,role,etag) VALUES($1,'owner@example.com','Owner','owner',$2)", user, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), "INSERT INTO olp.routes(id,slug,created_by,latest_revision,latest_revision_id,etag) VALUES($1,'route',$2,1,$3,$4)", route, user, uuid.NewString(), uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	revision := 0
	store := map[string]func(fidelity string) error{
		"route_drafts": func(fidelity string) error {
			_, err := pool.Exec(t.Context(), `INSERT INTO olp.route_drafts(id,slug,state,operations,overall_timeout_ms,max_attempts,targets,etag,created_by,fidelity)
                VALUES($1,'route','draft','[]',1,1,'[]',$2,$3,$4::jsonb)`, uuid.NewString(), uuid.NewString(), user, fidelity)
			return err
		},
		"route_revisions": func(fidelity string) error {
			revision++
			_, err := pool.Exec(t.Context(), `INSERT INTO olp.route_revisions(id,route_id,revision,slug,operations,overall_timeout_ms,max_attempts,targets,source_draft_id,activated_by,routing_policy,fidelity)
                VALUES($1,$2,$3,'route','[]',1,1,'[]',$4,$5,'{}',$6::jsonb)`, uuid.NewString(), route, revision, uuid.NewString(), user, fidelity)
			return err
		},
	}
	for table, insert := range store {
		for _, fidelity := range []string{`{"mode":"strict"}`, `{"mode":"transformed"}`} {
			if err := insert(fidelity); err != nil {
				t.Fatalf("%s refused %s: %v", table, fidelity, err)
			}
		}
		for _, fidelity := range []string{`{}`, `{"mode":null}`, `{"mode":""}`, `{"mode":"legacy"}`, `{"mode":["strict"]}`, `{"mode":"strict","other":true}`, `[]`, `"strict"`, `null`} {
			var refused *pgconn.PgError
			if err := insert(fidelity); !errors.As(err, &refused) || refused.Code != "23514" {
				t.Fatalf("%s stored fidelity %s without an explicit strict or transformed mode: %v", table, fidelity, err)
			}
		}
	}
}
