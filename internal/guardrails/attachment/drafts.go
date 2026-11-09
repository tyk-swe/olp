package attachment

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/guardrails"
)

type Draft struct {
	Slug             string
	PolicyRevisionID string
	Position         int
}

func Stage(ctx context.Context, tx pgx.Tx, actor, draftID, revisionID string, position int) error {
	var current string
	err := tx.QueryRow(ctx, "SELECT policy_revision_id::text FROM olp.guardrail_draft_attachments WHERE draft_id=$1 AND position=$2", draftID, position).Scan(&current)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if current != "" && current != revisionID {
		if err := requireSamePolicy(ctx, tx, current, revisionID, "A draft attachment cannot remove a different policy at this position."); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, "INSERT INTO olp.guardrail_draft_attachments(draft_id,policy_revision_id,position,created_by) VALUES($1,$2,$3,$4) ON CONFLICT(draft_id,position) DO UPDATE SET policy_revision_id=excluded.policy_revision_id", draftID, revisionID, position, actor)
	return err
}

func Drafts(ctx context.Context, q access.Queryer) ([]Draft, error) {
	rows, err := q.Query(ctx, "SELECT d.slug,a.policy_revision_id::text,a.position FROM olp.guardrail_draft_attachments a JOIN olp.route_drafts d ON d.id=a.draft_id ORDER BY d.updated_at,a.position")
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Draft])
}

func Staged(ctx context.Context, q access.Queryer, draftID, routeID string) ([]guardrails.Attachment, error) {
	rows, err := q.Query(ctx, "SELECT policy_revision_id::text,$2,$3::text,policy_revision_id::text,position FROM olp.guardrail_draft_attachments WHERE draft_id=$1 ORDER BY position", draftID, guardrails.RouteScope, routeID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[guardrails.Attachment])
}

func Promote(ctx context.Context, tx pgx.Tx, actor, draftID, routeID string) error {
	var displaces bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.guardrail_draft_attachments d
 JOIN olp.guardrail_attachments a ON a.scope='route' AND a.scope_id=$2 AND a.position=d.position
 JOIN olp.guardrail_policy_revisions old ON old.id=a.policy_revision_id
 JOIN olp.guardrail_policy_revisions next ON next.id=d.policy_revision_id
 WHERE d.draft_id=$1 AND old.policy_id<>next.policy_id)`, draftID, routeID).Scan(&displaces); err != nil {
		return err
	}
	if displaces {
		return access.Invalid("guardrail_attachments", "A promoted attachment cannot remove an existing policy.")
	}
	_, err := tx.Exec(ctx, `INSERT INTO olp.guardrail_attachments(id,scope,scope_id,policy_revision_id,position,etag,created_by)
 SELECT gen_random_uuid(),'route',$2,policy_revision_id,position,gen_random_uuid(),$3 FROM olp.guardrail_draft_attachments WHERE draft_id=$1
 ON CONFLICT(scope,scope_id,position) DO UPDATE SET policy_revision_id=excluded.policy_revision_id,etag=excluded.etag
 WHERE olp.guardrail_attachments.policy_revision_id<>excluded.policy_revision_id`, draftID, routeID, actor)
	return err
}
