package guardrails

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
)

func createAttachment(ctx context.Context, tx pgx.Tx, actor string, a Attachment) (Attachment, error) {
	created := Attachment{ID: access.NewID(), Scope: a.Scope, ScopeID: a.ScopeID, PolicyRevisionID: a.PolicyRevisionID, Order: a.Order}
	_, err := tx.Exec(ctx, "INSERT INTO olp.guardrail_attachments(id,scope,scope_id,policy_revision_id,position,etag,created_by) VALUES($1,$2,$3,$4,$5,$6,$7)", created.ID, created.Scope, ProjectPointer(created.ScopeID), created.PolicyRevisionID, created.Order, access.NewID(), actor)
	return created, err
}

func repointAttachment(ctx context.Context, tx pgx.Tx, id, policyRevisionID string) (string, error) {
	etag := access.NewID()
	_, err := tx.Exec(ctx, "UPDATE olp.guardrail_attachments SET policy_revision_id=$2,etag=$3 WHERE id=$1", id, policyRevisionID, etag)
	return etag, err
}

func PlaceAttachment(ctx context.Context, tx pgx.Tx, actor string, a Attachment) error {
	var id, current string
	err := tx.QueryRow(ctx, "SELECT id::text,policy_revision_id::text FROM olp.guardrail_attachments WHERE scope=$1 AND scope_id IS NOT DISTINCT FROM $2::uuid AND position=$3", a.Scope, ProjectPointer(a.ScopeID), a.Order).Scan(&id, &current)
	if errors.Is(err, pgx.ErrNoRows) {
		_, err := createAttachment(ctx, tx, actor, a)
		return err
	}
	if err != nil || current == a.PolicyRevisionID {
		return err
	}
	if err := requireSamePolicy(ctx, tx, current, a.PolicyRevisionID, "An additive attachment cannot replace a different policy at this position."); err != nil {
		return err
	}
	_, err = repointAttachment(ctx, tx, id, a.PolicyRevisionID)
	return err
}

func StageDraftAttachment(ctx context.Context, tx pgx.Tx, actor, draftID, revisionID string, position int) error {
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

func requireSamePolicy(ctx context.Context, tx pgx.Tx, current, next, refusal string) error {
	var same bool
	if err := tx.QueryRow(ctx, "SELECT a.policy_id=b.policy_id FROM olp.guardrail_policy_revisions a,olp.guardrail_policy_revisions b WHERE a.id=$1 AND b.id=$2", current, next).Scan(&same); err != nil {
		return err
	}
	if !same {
		return access.Invalid("guardrail_attachments", refusal)
	}
	return nil
}
