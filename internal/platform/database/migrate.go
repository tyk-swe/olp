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
	applied, latest, err := appliedMigrations(ctx, tx)
	if err != nil {
		return err
	}
	_, googleBillingApplied := applied["0125_google_billing.sql"]
	for _, entry := range entries {
		name := entry.Name()
		if _, ok := applied[name]; ok {
			continue
		}
		superseded := name == "0124_aws_cur.sql" && googleBillingApplied
		if name < latest && !superseded && !canBackfillDelegation(name, applied) {
			return fmt.Errorf("olp migration history is not a sequential prefix: missing %s before %s", name, latest)
		}
		sql, err := migrations.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		checksum := sha256.Sum256(sql)
		if !superseded {
			if _, err := tx.Exec(ctx, string(sql)); err != nil {
				return fmt.Errorf("migration %s failed, transaction rolled back: %w", name, err)
			}
		}
		if _, err = tx.Exec(ctx, "INSERT INTO olp.migrations(version,checksum) VALUES($1,$2)", name, checksum[:]); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(ctx, "INSERT INTO olp.installation(singleton,id,authority_id) VALUES(true,$1,$2) ON CONFLICT DO NOTHING", uuid.NewString(), uuid.NewString()); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func appliedMigrations(ctx context.Context, tx pgx.Tx) (map[string]struct{}, string, error) {
	rows, err := tx.Query(ctx, "SELECT version,checksum FROM olp.migrations")
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	applied := make(map[string]struct{})
	var latest string
	for rows.Next() {
		var name string
		var stored []byte
		if err := rows.Scan(&name, &stored); err != nil {
			return nil, "", err
		}
		sql, err := migrations.ReadFile("migrations/" + name)
		if err != nil {
			return nil, "", errors.New("database requires a newer olp binary")
		}
		checksum := sha256.Sum256(sql)
		if !slices.Equal(stored, checksum[:]) {
			return nil, "", fmt.Errorf("migration checksum mismatch: %s", name)
		}
		applied[name] = struct{}{}
		latest = max(latest, name)
	}
	return applied, latest, rows.Err()
}

func canBackfillDelegation(name string, applied map[string]struct{}) bool {
	switch name {
	case "0104_agent_delegation.sql", "0105_a2a_tasks.sql", "0106_delegation_consent.sql", "0107_agent_route_runs.sql",
		"0108_trace_spans.sql", "0109_a2a_task_bounds.sql", "0110_delegation_retention.sql", "0111_agent_delegated_tools.sql",
		"0112_mcp_delegation_attribution.sql", "0113_agent_oauth_grants.sql":
	default:
		return false
	}
	for later := range applied {
		if later <= name {
			continue
		}
		switch later {
		case "0104_code_pool_weights.sql", "0105_code_health_observations.sql", "0106_code_initial_recovery.sql",
			"0107_code_alias_pins.sql", "0108_code_reference_owners.sql", "0109_code_portable_recovery.sql",
			"0110_code_model_pools.sql", "0111_code_paid_targets.sql", "0112_code_paid_fallback.sql", "0113_code_definite_availability.sql":
		default:
			return false
		}
	}
	return true
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
		data, err := migrations.ReadFile("migrations/" + version)
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
