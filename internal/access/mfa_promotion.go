package access

import (
	"context"

	"github.com/jackc/pgx/v5"
)

func ApplyMFAPolicy(ctx context.Context, tx pgx.Tx, p Principal, required *bool) error {
	if required == nil {
		return nil
	}
	return SetLocalMFARequired(ctx, tx, p, *required)
}
