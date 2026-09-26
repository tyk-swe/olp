package database

import (
	"context"
	"crypto/sha256"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Migrate takes a transaction-scoped installation lock. PostgreSQL rolls back
// both DDL and history on failure; rerunning the same migration is the recovery
// path.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	return migrate(ctx, pool, migrations)
}

func migrate(ctx context.Context, pool *pgxpool.Pool, history fs.FS) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(726419823071)"); err != nil {
		return errors.New("migration lock unavailable")
	}
	if _, err = tx.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS olp;
        REVOKE ALL ON SCHEMA olp FROM PUBLIC;
        CREATE TABLE IF NOT EXISTS olp.migrations (
            version text PRIMARY KEY, checksum bytea NOT NULL, applied_at timestamptz NOT NULL DEFAULT now()
        )`); err != nil {
		return errors.New("migration role cannot initialize the olp schema")
	}
	entries, err := fs.ReadDir(history, "migrations")
	if err != nil {
		return err
	}
	known := make([]string, 0, len(entries))
	missingSeen := false
	for _, entry := range entries {
		name := entry.Name()
		known = append(known, name)
		sql, err := fs.ReadFile(history, "migrations/"+name)
		if err != nil {
			return err
		}
		checksum := sha256.Sum256(sql)
		var stored []byte
		err = tx.QueryRow(ctx, "SELECT checksum FROM olp.migrations WHERE version=$1", name).Scan(&stored)
		if err == nil {
			if missingSeen {
				return errors.New("olp migration history is not a sequential prefix")
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
		if _, err = tx.Exec(ctx, "INSERT INTO olp.migrations(version,checksum) VALUES($1,$2)", name, checksum[:]); err != nil {
			return err
		}
	}
	var unknown bool
	if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM olp.migrations WHERE NOT(version=ANY($1)))", known).Scan(&unknown); err != nil {
		return err
	}
	if unknown {
		return errors.New("database requires a newer olp binary")
	}
	if _, err = tx.Exec(ctx, "INSERT INTO olp.installation(singleton,id,authority_id) VALUES(true,$1,$2) ON CONFLICT DO NOTHING", uuid.NewString(), uuid.NewString()); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Installation returns the installation identity once the database holds
// exactly this binary's migration history.
func Installation(ctx context.Context, pool *pgxpool.Pool) (string, error) {
	return installation(ctx, pool, migrations)
}

func installation(ctx context.Context, pool *pgxpool.Pool, history fs.FS) (string, error) {
	var id string
	if err := pool.QueryRow(ctx, "SELECT id::text FROM olp.installation WHERE singleton").Scan(&id); err != nil {
		return "", errors.New("olp installation is not initialized; run olp migrate using the migration role")
	}
	entries, err := fs.ReadDir(history, "migrations")
	if err != nil {
		return "", err
	}
	rows, err := pool.Query(ctx, "SELECT version,checksum FROM olp.migrations")
	if err != nil {
		return "", errors.New("cannot read olp migration history")
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var version string
		var stored []byte
		if err = rows.Scan(&version, &stored); err != nil {
			return "", errors.New("cannot read olp migration history")
		}
		data, err := fs.ReadFile(history, "migrations/"+version)
		if err != nil {
			return "", errors.New("database schema is newer than this olp binary")
		}
		checksum := sha256.Sum256(data)
		if !slices.Equal(stored, checksum[:]) {
			return "", errors.New("olp migration checksum mismatch")
		}
		count++
	}
	if err = rows.Err(); err != nil {
		return "", errors.New("cannot read olp migration history")
	}
	if count != len(entries) {
		return "", errors.New("olp migrations are pending; run olp migrate using the migration role")
	}
	return id, nil
}

func ValkeyNamespace(installation string) string { return "olp:" + installation + ":" }
