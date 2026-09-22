package secrets

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func (k *KeyRing) Store(ctx context.Context, tx pgx.Tx, installation, id, purpose string, data []byte, expires *time.Time) error {
	var active int
	// Shared writers may seal independent records concurrently. Rotation takes
	// FOR UPDATE on this row, so it still waits for every in-flight write and
	// fences the active version before changing key material.
	if err := tx.QueryRow(ctx, "SELECT active_key_version FROM olp_go.installation WHERE singleton FOR SHARE").Scan(&active); err != nil {
		return err
	}
	if active != k.Active {
		return errors.New("reload the active master key before writing secrets")
	}
	ciphertext, err := k.Seal(installation, purpose, id, data)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO olp_go.secrets(id,purpose,key_version,ciphertext,expires_at) VALUES($1,$2,$3,$4,$5)
        ON CONFLICT(id) DO UPDATE SET key_version=excluded.key_version,ciphertext=excluded.ciphertext,expires_at=excluded.expires_at`, id, purpose, k.Active, ciphertext, expires)
	return err
}
func (k *KeyRing) Read(ctx context.Context, tx pgx.Tx, installation, id, purpose string) ([]byte, error) {
	var version int
	var encrypted []byte
	err := tx.QueryRow(ctx, "SELECT key_version,ciphertext FROM olp_go.secrets WHERE id=$1 AND purpose=$2 AND (expires_at IS NULL OR expires_at>now())", id, purpose).Scan(&version, &encrypted)
	if err != nil {
		return nil, err
	}
	return k.Open(installation, purpose, id, version, encrypted)
}

// Rotate commits one batch at a time. An interrupted run can resume with the
// same ring; old keys are removable only when Status reports no remaining rows.
func (k *KeyRing) Rotate(ctx context.Context, pool *pgxpool.Pool, installation string) (int, error) {
	total := 0
	for {
		n, err := k.rotateBatch(ctx, pool, installation, total == 0)
		total += n
		if err != nil || n == 0 {
			return total, err
		}
	}
}
func (k *KeyRing) rotateBatch(ctx context.Context, pool *pgxpool.Pool, installation string, validateDestination bool) (int, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	var active int
	if err = tx.QueryRow(ctx, "SELECT active_key_version FROM olp_go.installation WHERE singleton FOR UPDATE").Scan(&active); err != nil {
		return 0, err
	}
	if k.Active < active || !k.Has(active) {
		return 0, errors.New("rotation requires the current key and a nondecreasing active version")
	}
	if validateDestination {
		// Before the first write of each run, authenticate every record that
		// batches will skip, including expired records. A resumed version must
		// use the same key material as the batches already committed.
		rows, err := tx.Query(ctx, "SELECT id::text,purpose,ciphertext FROM olp_go.secrets WHERE key_version=$1", k.Active)
		if err != nil {
			return 0, err
		}
		defer rows.Close()
		for rows.Next() {
			var id, purpose string
			var ciphertext []byte
			if err = rows.Scan(&id, &purpose, &ciphertext); err != nil {
				return 0, err
			}
			if _, err = k.Open(installation, purpose, id, k.Active, ciphertext); err != nil {
				return 0, err
			}
		}
		if err = rows.Err(); err != nil {
			return 0, err
		}
		rows.Close()
	}
	rows, err := tx.Query(ctx, "SELECT id::text,purpose,key_version,ciphertext FROM olp_go.secrets WHERE key_version<>$1 ORDER BY id LIMIT 100 FOR UPDATE", k.Active)
	if err != nil {
		return 0, err
	}
	type record struct {
		id, purpose string
		version     int
		data        []byte
	}
	var records []record
	for rows.Next() {
		var r record
		if err = rows.Scan(&r.id, &r.purpose, &r.version, &r.data); err != nil {
			rows.Close()
			return 0, err
		}
		records = append(records, r)
	}
	if err = rows.Err(); err != nil {
		return 0, err
	}
	rows.Close()
	for _, r := range records {
		plain, err := k.Open(installation, r.purpose, r.id, r.version, r.data)
		if err != nil {
			return 0, err
		}
		encrypted, err := k.Seal(installation, r.purpose, r.id, plain)
		if err != nil {
			return 0, err
		}
		if _, err = tx.Exec(ctx, "UPDATE olp_go.secrets SET key_version=$1,ciphertext=$2 WHERE id=$3", k.Active, encrypted, r.id); err != nil {
			return 0, err
		}
	}
	if _, err = tx.Exec(ctx, "UPDATE olp_go.installation SET active_key_version=$1 WHERE singleton", k.Active); err != nil {
		return 0, err
	}
	if active != k.Active {
		if _, err = tx.Exec(ctx, "INSERT INTO olp_go.audit(id,action,resource_type,resource_id,outcome,user_agent_family) VALUES($1,'master_key.rotate','installation',$2,'success','olp-cli')", uuid.Must(uuid.NewV7()).String(), installation); err != nil {
			return 0, err
		}
	}
	return len(records), tx.Commit(ctx)
}
