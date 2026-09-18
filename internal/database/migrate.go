package database

import (
	"context"
	"crypto/sha256"
	"embed"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Migrate takes a transaction-scoped installation lock. PostgreSQL rolls back
// both DDL and history on failure; rerunning the same migration is the recovery
// path. Rust's migration files and schema are never written by this runner.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(726419823071)"); err != nil {
		return errors.New("migration lock unavailable")
	}
	if err = rejectReference(ctx, tx); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS olp_go;
        REVOKE ALL ON SCHEMA olp_go FROM PUBLIC;
        CREATE TABLE IF NOT EXISTS olp_go.migrations (
            version text PRIMARY KEY, checksum bytea NOT NULL, applied_at timestamptz NOT NULL DEFAULT now()
        )`); err != nil {
		return errors.New("migration role cannot initialize Go schema")
	}
	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		return err
	}
	known := make([]string, 0, len(entries))
	missingSeen := false
	for _, entry := range entries {
		name := entry.Name()
		known = append(known, name)
		sql, err := migrations.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		checksum := sha256.Sum256(sql)
		var stored []byte
		err = tx.QueryRow(ctx, "SELECT checksum FROM olp_go.migrations WHERE version=$1", name).Scan(&stored)
		if err == nil {
			if missingSeen {
				return errors.New("Go migration history is not a sequential prefix")
			}
			if !slices.Equal(stored, checksum[:]) {
				return fmt.Errorf("migration checksum mismatch: %s", name)
			}
			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		missingSeen = true
		if _, err = tx.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("migration %s failed; transaction rolled back", name)
		}
		if _, err = tx.Exec(ctx, "INSERT INTO olp_go.migrations(version,checksum) VALUES($1,$2)", name, checksum[:]); err != nil {
			return err
		}
	}
	var unknown bool
	if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM olp_go.migrations WHERE NOT(version=ANY($1)))", known).Scan(&unknown); err != nil {
		return err
	}
	if unknown {
		return errors.New("database requires a newer Go binary")
	}
	if _, err = tx.Exec(ctx, "INSERT INTO olp_go.installation(singleton,id,authority_id) VALUES(true,$1,$2) ON CONFLICT DO NOTHING", uuid.NewString(), uuid.NewString()); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type queryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func rejectReference(ctx context.Context, q queryer) error {
	var foreign bool
	err := q.QueryRow(ctx, `SELECT EXISTS (
        SELECT 1 FROM pg_namespace WHERE nspname IN ('olp_v3','olp_v2')
        UNION ALL SELECT 1 FROM pg_class c JOIN pg_namespace n ON c.relnamespace=n.oid
        WHERE n.nspname='public' AND c.relkind IN ('r','p')
    )`).Scan(&foreign)
	if err != nil {
		return errors.New("cannot inspect installation storage")
	}
	if foreign {
		return errors.New("reference or foreign database detected; use a fresh, separate Go database")
	}
	return nil
}

func Installation(ctx context.Context, pool *pgxpool.Pool) (string, error) {
	if err := rejectReference(ctx, pool); err != nil {
		return "", err
	}
	var id string
	if err := pool.QueryRow(ctx, "SELECT id::text FROM olp_go.installation WHERE singleton").Scan(&id); err != nil {
		return "", errors.New("Go installation is not initialized; run olp migrate using the migration role")
	}
	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		return "", err
	}
	rows, err := pool.Query(ctx, "SELECT version,checksum FROM olp_go.migrations")
	if err != nil {
		return "", errors.New("cannot read Go migration history")
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var version string
		var stored []byte
		if err = rows.Scan(&version, &stored); err != nil {
			return "", errors.New("cannot read Go migration history")
		}
		data, err := migrations.ReadFile("migrations/" + version)
		if err != nil {
			return "", errors.New("database schema is newer than this Go binary")
		}
		checksum := sha256.Sum256(data)
		if !slices.Equal(stored, checksum[:]) {
			return "", errors.New("Go migration checksum mismatch")
		}
		count++
	}
	if err = rows.Err(); err != nil {
		return "", errors.New("cannot read Go migration history")
	}
	if count != len(entries) {
		return "", errors.New("Go migrations are pending; run olp migrate using the migration role")
	}
	return id, nil
}

func ValkeyNamespace(installation string) string { return "olp:go:v1:" + installation + ":" }
