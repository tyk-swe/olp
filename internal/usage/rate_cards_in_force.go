package usage

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
)

type CardInForce struct {
	ID           string
	Organization *string
	Project      *string
	RevisionID   *string
	Markup       *string
}

func ReadCardsInForce(ctx context.Context, q access.Queryer) ([]CardInForce, error) {
	rows, err := q.Query(ctx, `SELECT c.id::text,
 (SELECT name FROM olp.organizations WHERE id=c.organization_id),
 (SELECT name FROM olp.projects WHERE id=c.project_id),
 rev.id::text, rev.markup::text
 FROM olp.rate_cards c
 LEFT JOIN LATERAL (
  SELECT r.id, r.markup FROM olp.rate_card_revisions r
  WHERE r.rate_card_id=c.id AND r.effective_at<=now()
  ORDER BY r.effective_at DESC,r.revision DESC LIMIT 1
 ) rev ON true
 ORDER BY c.id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[CardInForce])
}
