package harness

import (
	"context"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/database"
)

const (
	templateName = "olp_test_template"
	templateLock = 726419823072
)

func Required(t testing.TB, name string) string {
	t.Helper()
	value := os.Getenv(name)
	if value == "" {
		t.Fatalf("%s is not set. Run make test-integration or make test-e2e", name)
	}
	return value
}

func EmptyDatabase(t testing.TB) (*pgxpool.Pool, string) {
	t.Helper()
	return create(t, "CREATE DATABASE %s")
}

func Database(t testing.TB) (*pgxpool.Pool, string) {
	t.Helper()
	if err := template(); err != nil {
		t.Fatalf("migrated template database: %v", err)
	}
	pool, address := create(t, "CREATE DATABASE %s TEMPLATE "+templateName)
	if err := database.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	return pool, address
}

func create(t testing.TB, statement string) (*pgxpool.Pool, string) {
	t.Helper()
	raw := Required(t, "OLP_TEST_DATABASE_URL")
	admin, err := pgx.Connect(t.Context(), raw)
	if err != nil {
		t.Fatal(err)
	}
	name := "test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	identifier := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(t.Context(), strings.Replace(statement, "%s", identifier, 1)); err != nil {
		t.Fatal(err)
	}
	pool, address, err := open(t.Context(), raw, name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.Exec(ctx, "DROP DATABASE "+identifier+" WITH (FORCE)"); err != nil {
			t.Errorf("drop database: %v", err)
		}
		admin.Close(ctx)
	})
	return pool, address
}

func open(ctx context.Context, raw, name string) (*pgxpool.Pool, string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, "", err
	}
	u.Path = "/" + name
	config, err := database.Configuration(u.String(), 10, 10*time.Second)
	if err != nil {
		return nil, "", err
	}
	pool, err := database.Open(ctx, config)
	return pool, u.String(), err
}

// The template keeps no installation row, so database.Migrate gives every
// clone its own identity and Valkey namespace.
var template = sync.OnceValue(func() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	raw := os.Getenv("OLP_TEST_DATABASE_URL")
	admin, err := pgx.Connect(ctx, raw)
	if err != nil {
		return err
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, "SELECT pg_advisory_lock($1)", templateLock); err != nil {
		return err
	}
	if migrateTemplate(ctx, admin, raw) == nil {
		return nil
	}
	if _, err := admin.Exec(ctx, "DROP DATABASE IF EXISTS "+templateName+" WITH (FORCE)"); err != nil {
		return err
	}
	return migrateTemplate(ctx, admin, raw)
})

func migrateTemplate(ctx context.Context, admin *pgx.Conn, raw string) error {
	var exists bool
	if err := admin.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname=$1)", templateName).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		if _, err := admin.Exec(ctx, "CREATE DATABASE "+templateName); err != nil {
			return err
		}
	}
	pool, _, err := open(ctx, raw, templateName)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := database.Migrate(ctx, pool); err != nil {
		return err
	}
	_, err = pool.Exec(ctx, "DELETE FROM olp.installation")
	return err
}
