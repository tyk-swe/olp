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

const baseline = "0001_schema.sql"

var errPreBaseline = errors.New("olp database predates the " + baseline + " baseline and cannot be upgraded; migrate a new empty database")

type migrationRecord struct {
	Version  string
	Checksum []byte
}

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Migrate takes a transaction-scoped installation lock. PostgreSQL rolls back
// both DDL and history on failure; rerunning the same migration is the recovery
// path.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SET LOCAL statement_timeout = 0; SET LOCAL lock_timeout = 0"); err != nil {
		return err
	}
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
	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		return err
	}
	applied, err := verifiedHistory(ctx, tx)
	if err != nil {
		return err
	}
	latest := ""
	if len(applied) > 0 {
		latest = slices.Max(applied)
	}
	for _, entry := range entries {
		name := entry.Name()
		if slices.Contains(applied, name) {
			continue
		}
		if name < latest {
			return fmt.Errorf("olp migration history is not a sequential prefix: missing %s before %s", name, latest)
		}
		sql, err := migrations.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("migration %s failed, transaction rolled back: %w", name, err)
		}
		checksum := sha256.Sum256(sql)
		if _, err = tx.Exec(ctx, "INSERT INTO olp.migrations(version,checksum) VALUES($1,$2)", name, checksum[:]); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(ctx, "INSERT INTO olp.installation(singleton,id,authority_id) VALUES(true,$1,$2) ON CONFLICT DO NOTHING", uuid.NewString(), uuid.NewString()); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func verifiedHistory(ctx context.Context, q querier) ([]string, error) {
	rows, err := q.Query(ctx, "SELECT version,checksum FROM olp.migrations")
	if err != nil {
		return nil, errors.New("cannot read olp migration history")
	}
	records, err := pgx.CollectRows(rows, pgx.RowToStructByPos[migrationRecord])
	if err != nil {
		return nil, errors.New("cannot read olp migration history")
	}
	if len(records) > 0 && !slices.ContainsFunc(records, func(r migrationRecord) bool { return r.Version == baseline }) {
		return nil, errPreBaseline
	}
	versions := make([]string, 0, len(records))
	for _, record := range records {
		sql, err := migrations.ReadFile("migrations/" + record.Version)
		if err != nil {
			return nil, errors.New("database schema is newer than this olp binary")
		}
		checksum := sha256.Sum256(sql)
		if !slices.Equal(record.Checksum, checksum[:]) {
			return nil, fmt.Errorf("olp migration checksum mismatch: %s", record.Version)
		}
		versions = append(versions, record.Version)
	}
	return versions, nil
}

// Installation returns the installation identity once the database holds
// exactly this binary's migration history.
func Installation(ctx context.Context, pool *pgxpool.Pool) (string, error) {
	var id string
	if err := pool.QueryRow(ctx, "SELECT id::text FROM olp.installation WHERE singleton").Scan(&id); err != nil {
		return "", errors.New("olp installation is not initialized; run olp migrate using the migration role")
	}
	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		return "", err
	}
	applied, err := verifiedHistory(ctx, pool)
	if err != nil {
		return "", err
	}
	if len(applied) != len(entries) {
		return "", errors.New("olp migrations are pending; run olp migrate using the migration role")
	}
	return id, nil
}
