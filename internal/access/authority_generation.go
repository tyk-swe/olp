package access

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/domain/ids"
	"github.com/tyk-swe/olp/internal/platform/database"
)

type AuthorityGeneration struct {
	ID       string `json:"id"`
	Sequence int64  `json:"sequence"`
}

func AdvanceAuthority(ctx context.Context, tx pgx.Tx) (AuthorityGeneration, error) {
	id, sequence, err := database.AdvanceInstallationAuthority(ctx, tx, ids.New())
	return AuthorityGeneration{ID: id, Sequence: sequence}, err
}
