package process

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/config"
	"github.com/tyk-swe/olp/internal/database"
)

func Maintenance(ctx context.Context, c config.Config, command string, output io.Writer) error {
	timeout := c.StartupTimeout
	if command == "reencrypt" {
		timeout = 30 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	connection, err := database.Configuration(c.DatabaseURL, c.DatabaseMaxConnections, c.RequestTimeout)
	if err != nil {
		return err
	}
	pool, err := database.Open(ctx, connection)
	if err != nil {
		return err
	}
	defer pool.Close()
	if command == "migrate" {
		if err = database.Migrate(ctx, pool); err != nil {
			return err
		}
		if c.RuntimeDatabaseRole != "" {
			role := pgx.Identifier{c.RuntimeDatabaseRole}.Sanitize()
			tx, err := pool.Begin(ctx)
			if err != nil {
				return errors.New("cannot grant runtime privileges")
			}
			defer tx.Rollback(ctx)
			for _, statement := range []string{"GRANT USAGE ON SCHEMA olp_go TO " + role, "GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA olp_go TO " + role, "REVOKE INSERT,UPDATE,DELETE ON olp_go.migrations FROM " + role} {
				if _, err = tx.Exec(ctx, statement); err != nil {
					return errors.New("cannot grant runtime privileges; the runtime role must already exist")
				}
			}
			if err = tx.Commit(ctx); err != nil {
				return errors.New("cannot commit runtime privileges")
			}
		}
		return json.NewEncoder(output).Encode(map[string]any{"migration": "complete"})
	}
	installation, err := database.Installation(ctx, pool)
	if err != nil {
		return err
	}
	auth, keys, _, err := loadSecrets(c, installation)
	if err != nil {
		return err
	}
	var fingerprint []byte
	var active *int
	if err = pool.QueryRow(ctx, "SELECT auth_fingerprint,active_key_version FROM olp_go.installation WHERE singleton").Scan(&fingerprint, &active); err != nil {
		return errors.New("cannot inspect installation key state")
	}
	if fingerprint != nil && !hmac.Equal(fingerprint, auth.Digest("installation", "identity")) {
		return errors.New("authentication key does not match the Go installation")
	}
	rotated := 0
	if command == "reencrypt" {
		if active == nil {
			return errors.New("start the installation before rotating master keys")
		}
		rotated, err = keys.Rotate(ctx, pool, installation)
		if err != nil {
			return err
		}
	}
	rows, err := pool.Query(ctx, "SELECT id::text,purpose,key_version,ciphertext FROM olp_go.secrets ORDER BY id")
	if err != nil {
		return errors.New("cannot inspect encrypted records")
	}
	defer rows.Close()
	versions := map[int]int{}
	for rows.Next() {
		var id, purpose string
		var version int
		var data []byte
		if err = rows.Scan(&id, &purpose, &version, &data); err != nil {
			return errors.New("cannot read encrypted record")
		}
		if _, err = keys.Open(installation, purpose, id, version, data); err != nil {
			return err
		}
		versions[version]++
	}
	if err = rows.Err(); err != nil {
		return errors.New("cannot inspect encrypted records")
	}
	return json.NewEncoder(output).Encode(map[string]any{"installation_id": installation, "valkey_namespace": database.ValkeyNamespace(installation), "active_version": keys.Active, "stored_versions": versions, "reencrypted": rotated})
}
