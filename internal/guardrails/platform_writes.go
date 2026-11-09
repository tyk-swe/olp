package guardrails

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
)

func ActivateRevision(ctx context.Context, tx pgx.Tx, actor, guardrailID, revisionID string) error {
	return activate(ctx, tx, "guardrails", "guardrail_revision_activations", actor, guardrailID, revisionID)
}

func ActivatePolicy(ctx context.Context, tx pgx.Tx, actor, policyID, revisionID string) error {
	return activate(ctx, tx, "guardrail_policies", "guardrail_policy_revision_activations", actor, policyID, revisionID)
}

func activate(ctx context.Context, tx pgx.Tx, table, log, actor, id, revisionID string) error {
	if _, err := tx.Exec(ctx, "INSERT INTO olp."+log+"(revision_id,activated_by) VALUES($1,$2) ON CONFLICT DO NOTHING", revisionID, actor); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, "UPDATE olp."+table+" SET active_revision_id=$2,etag=$3 WHERE id=$1", id, revisionID, access.NewID())
	return err
}

func SavePolicy(ctx context.Context, tx pgx.Tx, actor string, current *PolicyRevision, next PolicyRevision) (PolicyRevision, error) {
	entries, err := json.Marshal(next.Entries)
	if err != nil {
		return PolicyRevision{}, err
	}
	saved := followingRevision(current, next)
	if err := writePolicyHead(ctx, tx, actor, current == nil, saved); err != nil {
		return PolicyRevision{}, err
	}
	if _, err := tx.Exec(ctx, "INSERT INTO olp.guardrail_policy_revisions(id,policy_id,revision,entries,created_by) VALUES($1,$2,$3,$4,$5)", saved.ID, saved.PolicyID, saved.Revision, entries, actor); err != nil {
		return PolicyRevision{}, err
	}
	return saved, nil
}

func followingRevision(current *PolicyRevision, next PolicyRevision) PolicyRevision {
	if current == nil {
		return PolicyRevision{ID: access.NewID(), PolicyID: access.NewID(), ProjectID: next.ProjectID, Name: next.Name, Revision: 1, Entries: next.Entries}
	}
	return PolicyRevision{ID: access.NewID(), PolicyID: current.PolicyID, ProjectID: current.ProjectID, Name: next.Name, Revision: current.Revision + 1, Entries: next.Entries}
}

func writePolicyHead(ctx context.Context, tx pgx.Tx, actor string, create bool, saved PolicyRevision) error {
	if create {
		_, err := tx.Exec(ctx, "INSERT INTO olp.guardrail_policies(id,project_id,name,latest_revision_id,etag,created_by) VALUES($1,$2,$3,$4,$5,$6)", saved.PolicyID, projectPointer(saved.ProjectID), saved.Name, saved.ID, access.NewID(), actor)
		return err
	}
	_, err := tx.Exec(ctx, "UPDATE olp.guardrail_policies SET name=$2,latest_revision_id=$3,etag=$4 WHERE id=$1", saved.PolicyID, saved.Name, saved.ID, access.NewID())
	return err
}

func createAttachment(ctx context.Context, tx pgx.Tx, actor string, a Attachment) (Attachment, error) {
	created := Attachment{ID: access.NewID(), Scope: a.Scope, ScopeID: a.ScopeID, PolicyRevisionID: a.PolicyRevisionID, Order: a.Order}
	_, err := tx.Exec(ctx, "INSERT INTO olp.guardrail_attachments(id,scope,scope_id,policy_revision_id,position,etag,created_by) VALUES($1,$2,$3,$4,$5,$6,$7)", created.ID, created.Scope, projectPointer(created.ScopeID), created.PolicyRevisionID, created.Order, access.NewID(), actor)
	return created, err
}

func repointAttachment(ctx context.Context, tx pgx.Tx, id, policyRevisionID string) (string, error) {
	etag := access.NewID()
	_, err := tx.Exec(ctx, "UPDATE olp.guardrail_attachments SET policy_revision_id=$2,etag=$3 WHERE id=$1", id, policyRevisionID, etag)
	return etag, err
}

func PlaceAttachment(ctx context.Context, tx pgx.Tx, actor string, a Attachment) error {
	var id, current string
	err := tx.QueryRow(ctx, "SELECT id::text,policy_revision_id::text FROM olp.guardrail_attachments WHERE scope=$1 AND scope_id IS NOT DISTINCT FROM $2::uuid AND position=$3", a.Scope, projectPointer(a.ScopeID), a.Order).Scan(&id, &current)
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
