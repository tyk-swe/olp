//go:build integration

package database

import (
	"context"
	"io/fs"
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestIntegrationUpgradeFromSecurityAndObservabilityMigrations(t *testing.T) {
	for _, prefix := range []string{"0047", "0050"} {
		t.Run(prefix, func(t *testing.T) {
			pool := scratchPool(t)
			entries, err := fs.ReadDir(migrations, "migrations")
			if err != nil {
				t.Fatal(err)
			}
			previous := fstest.MapFS{}
			for _, entry := range entries {
				if entry.Name()[:4] > prefix {
					break
				}
				path := "migrations/" + entry.Name()
				data, err := fs.ReadFile(migrations, path)
				if err != nil {
					t.Fatal(err)
				}
				previous[path] = &fstest.MapFile{Data: data}
			}
			if err := migrate(t.Context(), pool, previous); err != nil {
				t.Fatal(err)
			}
			if err := Migrate(t.Context(), pool); err != nil {
				t.Fatal(err)
			}
			if _, err := Installation(t.Context(), pool); err != nil {
				t.Fatal(err)
			}
		})
	}
}

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

// The missing step would apply cleanly, so only the history check can
// refuse the later step that is already recorded.
