package configuration

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
)

func applyMFAPolicy(ctx context.Context, tx pgx.Tx, p access.Principal, required *bool) error {
	if required == nil {
		return nil
	}
	var current string
	if err := tx.QueryRow(ctx, "SELECT value FROM olp.settings WHERE key='auth.mfa_required' FOR UPDATE").Scan(&current); err != nil {
		return err
	}
	desired := "false"
	if *required {
		desired = "true"
	}
	if current == desired {
		return nil
	}
	if err := p.Authorize(access.Access); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, "UPDATE olp.settings SET value=$1,etag=$2,updated_by=$3,updated_at=now() WHERE key='auth.mfa_required'", desired, access.NewID(), p.UserID())
	return err
}
