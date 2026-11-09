package guardrails

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
)

type DraftAttachment struct {
	Slug             string
	PolicyRevisionID string
	Position         int
}

func ReadDraftAttachments(ctx context.Context, q access.Queryer) ([]DraftAttachment, error) {
	rows, err := q.Query(ctx, "SELECT d.slug,a.policy_revision_id::text,a.position FROM olp.guardrail_draft_attachments a JOIN olp.route_drafts d ON d.id=a.draft_id ORDER BY d.updated_at,a.position")
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[DraftAttachment])
}
