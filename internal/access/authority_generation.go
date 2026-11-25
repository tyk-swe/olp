package access

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/domain/ids"
)

type AuthorityGeneration struct {
	ID       string `json:"id"`
	Sequence int64  `json:"sequence"`
}

func AdvanceAuthority(ctx context.Context, tx pgx.Tx) (AuthorityGeneration, error) {
	var id string
	var sequence int64
	err := tx.QueryRow(ctx, "UPDATE olp.installation SET authority_id=$1,authority_sequence=authority_sequence+1 WHERE singleton RETURNING authority_id::text,authority_sequence", ids.New()).Scan(&id, &sequence)
	return AuthorityGeneration{ID: id, Sequence: sequence}, err
}
