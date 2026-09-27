package secrets

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RowQuerier is the read-only part of a database handle needed to load a
// secret. It permits callers that do not write to avoid opening a transaction.
type RowQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// RowsQuerier reads many rows, as a pool or a transaction does.
type RowsQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

var errUnknownPurpose = errors.New("an encrypted secret names an unknown purpose")

// VerifyAll authenticates every stored secret, including expired records and
// old key versions, and counts the records under each key version. A version
// number alone does not identify key material, so a process refuses a ring
// that cannot open everything the installation stores.
func (k *KeyRing) VerifyAll(ctx context.Context, q RowsQuerier, installation string) (map[int]int, error) {
	rows, err := q.Query(ctx, "SELECT id::text,purpose,key_version,ciphertext FROM olp.secrets ORDER BY id")
	if err != nil {
		return nil, errors.New("cannot inspect encrypted records")
	}
	defer rows.Close()
	versions := map[int]int{}
	for rows.Next() {
		var id, name string
		var version int
		var ciphertext []byte
		if err = rows.Scan(&id, &name, &version, &ciphertext); err != nil {
			return nil, errors.New("cannot read encrypted record")
		}
		purpose, ok := ParseSealPurpose(name)
		if !ok {
			return nil, errUnknownPurpose
		}
		if !k.Has(version) {
			return nil, errors.New("master key ring is missing a stored version")
		}
		if _, err = k.Open(installation, purpose, id, version, ciphertext); err != nil {
			return nil, err
		}
		versions[version]++
	}
	if err = rows.Err(); err != nil {
		return nil, errors.New("cannot inspect encrypted records")
	}
	return versions, nil
}

func (k *KeyRing) Store(ctx context.Context, tx pgx.Tx, installation, id string, purpose SealPurpose, data []byte, expires *time.Time) error {
	var active int
	// Shared writers may seal independent records concurrently. Rotation takes
	// FOR UPDATE on this row, so it still waits for every in-flight write and
	// fences the active version before changing key material.
	if err := tx.QueryRow(ctx, "SELECT active_key_version FROM olp.installation WHERE singleton FOR SHARE").Scan(&active); err != nil {
		return err
	}
	if active != k.Active {
		return errors.New("reload the active master key before writing secrets")
	}
	ciphertext, err := k.Seal(installation, purpose, id, data)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO olp.secrets(id,purpose,key_version,ciphertext,expires_at) VALUES($1,$2,$3,$4,$5)
        ON CONFLICT(id) DO UPDATE SET key_version=excluded.key_version,ciphertext=excluded.ciphertext,expires_at=excluded.expires_at`, id, purpose, k.Active, ciphertext, expires)
	return err
}
func (k *KeyRing) Read(ctx context.Context, query RowQuerier, installation, id string, purpose SealPurpose) ([]byte, error) {
	var version int
	var encrypted []byte
	err := query.QueryRow(ctx, "SELECT key_version,ciphertext FROM olp.secrets WHERE id=$1 AND purpose=$2 AND (expires_at IS NULL OR expires_at>now())", id, purpose).Scan(&version, &encrypted)
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
	if err = tx.QueryRow(ctx, "SELECT active_key_version FROM olp.installation WHERE singleton FOR UPDATE").Scan(&active); err != nil {
		return 0, err
	}
	if k.Active < active || !k.Has(active) {
		return 0, errors.New("rotation requires the current key and a nondecreasing active version")
	}
	if validateDestination {
		// Before the first write of each run, authenticate every record that
		// batches will skip, including expired records. A resumed version must
		// use the same key material as the batches already committed.
		rows, err := tx.Query(ctx, "SELECT id::text,purpose,ciphertext FROM olp.secrets WHERE key_version=$1", k.Active)
		if err != nil {
			return 0, err
		}
		defer rows.Close()
		for rows.Next() {
			var id, name string
			var ciphertext []byte
			if err = rows.Scan(&id, &name, &ciphertext); err != nil {
				return 0, err
			}
			purpose, ok := ParseSealPurpose(name)
			if !ok {
				return 0, errUnknownPurpose
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
	rows, err := tx.Query(ctx, "SELECT id::text,purpose,key_version,ciphertext FROM olp.secrets WHERE key_version<>$1 ORDER BY id LIMIT 100 FOR UPDATE", k.Active)
	if err != nil {
		return 0, err
	}
	type record struct {
		id      string
		purpose SealPurpose
		version int
		data    []byte
	}
	var records []record
	for rows.Next() {
		var r record
		var name string
		if err = rows.Scan(&r.id, &name, &r.version, &r.data); err != nil {
			rows.Close()
			return 0, err
		}
		var ok bool
		if r.purpose, ok = ParseSealPurpose(name); !ok {
			rows.Close()
			return 0, errUnknownPurpose
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
		if _, err = tx.Exec(ctx, "UPDATE olp.secrets SET key_version=$1,ciphertext=$2 WHERE id=$3", k.Active, encrypted, r.id); err != nil {
			return 0, err
		}
	}
	if _, err = tx.Exec(ctx, "UPDATE olp.installation SET active_key_version=$1 WHERE singleton", k.Active); err != nil {
		return 0, err
	}
	if active != k.Active {
		if _, err = tx.Exec(ctx, "INSERT INTO olp.audit(id,action,resource_type,resource_id,outcome,user_agent_family) VALUES($1,'master_key.rotate','installation',$2,'success','olp-cli')", uuid.Must(uuid.NewV7()).String(), installation); err != nil {
			return 0, err
		}
	}
	return len(records), tx.Commit(ctx)
}
