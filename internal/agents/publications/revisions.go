package publications

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/agentcontract"
)

func RevisionDefinitions(ctx context.Context, q access.Queryer, publicationID string) ([]agentcontract.Draft, error) {
	rows, err := q.Query(ctx, `SELECT definition FROM olp.agent_publication_revisions WHERE publication_id=$1`, publicationID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[agentcontract.Draft])
}

func RevisionIDByDigest(ctx context.Context, q access.Queryer, publicationID, digest string) (string, bool, error) {
	var id string
	err := q.QueryRow(ctx, `SELECT id::text FROM olp.agent_publication_revisions WHERE publication_id=$1 AND digest=$2`, publicationID, digest).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return id, err == nil, err
}
