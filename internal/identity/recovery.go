package identity

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/domain/ids"
	"github.com/tyk-swe/olp/internal/platform/secrets"
)

type RecoveryResult struct {
	ID                string `json:"id"`
	Email             string `json:"email"`
	PasswordRecovered bool   `json:"password_recovered"`
	MFAReset          bool   `json:"mfa_reset"`
}

var errRecovery = errors.New("account recovery failed")

func RecoverPassword(ctx context.Context, pool *pgxpool.Pool, address, secret string, resetMFA ...bool) (RecoveryResult, error) {
	normalizedEmail, err := email(address)
	if err != nil {
		return RecoveryResult{}, err
	}
	if err := password(secret); err != nil {
		return RecoveryResult{}, err
	}
	hash := secrets.HashPassword(secret)
	clearedMFA := len(resetMFA) > 0 && resetMFA[0]
	var userID string
	err = withPoolTx(ctx, pool, func(tx pgx.Tx) error {
		id, err := recoverUserPassword(ctx, tx, normalizedEmail, hash, clearedMFA)
		if err != nil {
			return err
		}
		userID = id
		return nil
	})
	if err != nil {
		return RecoveryResult{}, err
	}
	return RecoveryResult{
		ID:                userID,
		Email:             normalizedEmail,
		PasswordRecovered: true,
		MFAReset:          clearedMFA,
	}, nil
}

func recoverUserPassword(ctx context.Context, tx pgx.Tx, address, hash string, clearMFA bool) (string, error) {
	if _, err := tx.Exec(ctx, "SELECT id FROM olp.installation WHERE singleton FOR UPDATE"); err != nil {
		return "", err
	}
	var id string
	var active bool
	err := tx.QueryRow(ctx, "SELECT id::text,active FROM olp.users WHERE email=$1 FOR UPDATE", address).Scan(&id, &active)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !active) {
		return "", errRecovery
	}
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, "UPDATE olp.users SET password_hash=$1,etag=$2,updated_at=now() WHERE id=$3", hash, ids.New(), id); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, "DELETE FROM olp.recent_auth WHERE session_id IN (SELECT id FROM olp.sessions WHERE user_id=$1)", id); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, "DELETE FROM olp.sessions WHERE user_id=$1", id); err != nil {
		return "", err
	}
	if clearMFA {
		if err := clearRecoveredMFA(ctx, tx, id); err != nil {
			return "", err
		}
	}
	if err := access.Audit(ctx, tx, &http.Request{}, authz.System, "user.password_recover", "user", id, "success"); err != nil {
		return "", err
	}
	return id, nil
}

func clearRecoveredMFA(ctx context.Context, tx pgx.Tx, userID string) error {
	secretIDs, err := collectMFASecretIDs(ctx, tx, userID)
	if err != nil {
		return err
	}
	for _, query := range []string{
		"DELETE FROM olp.mfa_challenges WHERE user_id=$1",
		"DELETE FROM olp.mfa_factors WHERE user_id=$1",
		"DELETE FROM olp.mfa_recovery_codes WHERE user_id=$1",
		"UPDATE olp.users SET mfa_revision=uuidv7() WHERE id=$1",
	} {
		if _, err := tx.Exec(ctx, query, userID); err != nil {
			return err
		}
	}
	if _, err := secrets.DeleteByID(ctx, tx, secretIDs...); err != nil {
		return err
	}
	return access.Audit(ctx, tx, &http.Request{}, authz.System, "user.mfa_recover", "user", userID, "success")
}

func collectMFASecretIDs(ctx context.Context, tx pgx.Tx, userID string) ([]string, error) {
	rows, err := tx.Query(ctx, "SELECT id::text FROM olp.mfa_factors WHERE user_id=$1 UNION SELECT secret_id::text FROM olp.mfa_challenges WHERE user_id=$1 AND secret_id IS NOT NULL", userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var secretIDs []string
	for rows.Next() {
		var secretID string
		if err := rows.Scan(&secretID); err != nil {
			return nil, err
		}
		secretIDs = append(secretIDs, secretID)
	}
	return secretIDs, rows.Err()
}
