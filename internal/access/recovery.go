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

func RecoverPassword(ctx context.Context, pool *pgxpool.Pool, address, secret string) (map[string]any, error) {
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
	if _, err = tx.Exec(ctx, "SELECT id FROM olp_go.installation WHERE singleton FOR UPDATE"); err != nil {
		return nil, err
	}
	var id string
	var active bool
	err = tx.QueryRow(ctx, "SELECT id::text,active FROM olp_go.users WHERE email=$1 FOR UPDATE", address).Scan(&id, &active)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !active {
		return nil, errRecovery
	}
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, "UPDATE olp_go.users SET password_hash=$1,etag=$2,updated_at=now() WHERE id=$3", hash, NewID(), id); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, "DELETE FROM olp_go.recent_auth WHERE session_id IN (SELECT id FROM olp_go.sessions WHERE user_id=$1)", id); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, "DELETE FROM olp_go.sessions WHERE user_id=$1", id); err != nil {
		return nil, err
	}
	if err = Audit(ctx, tx, &http.Request{}, "", "user.password_recover", "user", id, "success"); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return map[string]any{"id": id, "email": address, "password_recovered": true}, nil
}
