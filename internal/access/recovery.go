package access

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/secrets"
)

var errRecovery = errors.New("account recovery failed")

func RecoverPassword(ctx context.Context, pool *pgxpool.Pool, address, secret string, resetMFA ...bool) (map[string]any, error) {
	address, err := email(address)
	if err != nil {
		return nil, err
	}
	if err = password(secret); err != nil {
		return nil, err
	}
	hash := secrets.HashPassword(secret)
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT id FROM olp.installation WHERE singleton FOR UPDATE"); err != nil {
		return nil, err
	}
	var id string
	var active bool
	err = tx.QueryRow(ctx, "SELECT id::text,active FROM olp.users WHERE email=$1 FOR UPDATE", address).Scan(&id, &active)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !active {
		return nil, errRecovery
	}
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, "UPDATE olp.users SET password_hash=$1,etag=$2,updated_at=now() WHERE id=$3", hash, NewID(), id); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, "DELETE FROM olp.recent_auth WHERE session_id IN (SELECT id FROM olp.sessions WHERE user_id=$1)", id); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, "DELETE FROM olp.sessions WHERE user_id=$1", id); err != nil {
		return nil, err
	}
	clearedMFA := len(resetMFA) > 0 && resetMFA[0]
	if clearedMFA {
		rows, e := tx.Query(ctx, "SELECT id::text FROM olp.mfa_factors WHERE user_id=$1 UNION SELECT secret_id::text FROM olp.mfa_challenges WHERE user_id=$1 AND secret_id IS NOT NULL", id)
		if e != nil {
			return nil, e
		}
		var secretIDs []string
		for rows.Next() {
			var secretID string
			if e = rows.Scan(&secretID); e != nil {
				rows.Close()
				return nil, e
			}
			secretIDs = append(secretIDs, secretID)
		}
		rows.Close()
		if e = rows.Err(); e != nil {
			return nil, e
		}
		for _, query := range []string{"DELETE FROM olp.mfa_challenges WHERE user_id=$1", "DELETE FROM olp.mfa_factors WHERE user_id=$1", "DELETE FROM olp.mfa_recovery_codes WHERE user_id=$1", "UPDATE olp.users SET mfa_revision=uuidv7() WHERE id=$1"} {
			if _, e = tx.Exec(ctx, query, id); e != nil {
				return nil, e
			}
		}
		if _, e = tx.Exec(ctx, "DELETE FROM olp.secrets WHERE id=ANY($1::uuid[])", secretIDs); e != nil {
			return nil, e
		}
		if e = Audit(ctx, tx, &http.Request{}, System, "user.mfa_recover", "user", id, "success"); e != nil {
			return nil, e
		}
	}
	if err = Audit(ctx, tx, &http.Request{}, System, "user.password_recover", "user", id, "success"); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return map[string]any{"id": id, "email": address, "password_recovered": true, "mfa_reset": clearedMFA}, nil
}
