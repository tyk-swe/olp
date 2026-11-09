package guardrails

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
)

type NamedPolicyRevision struct {
	ID     string
	Name   string
	Active bool
}

func ReadUnmanagedActiveRevisions(ctx context.Context, q access.Queryer) ([]Revision, error) {
	rows, err := q.Query(ctx, "SELECT v.id::text,g.id::text,COALESCE(g.project_id::text,'installation'),g.name,g.type,v.configuration,v.timeout_ms,v.on_error FROM olp.guardrails g JOIN olp.guardrail_revisions v ON v.id=g.active_revision_id WHERE g.retired_at IS NULL AND g.managed_route_revision_id IS NULL")
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Revision, error) {
		var r Revision
		err := row.Scan(&r.ID, &r.GuardrailID, &r.ProjectID, &r.Name, &r.Type, &r.Configuration, &r.TimeoutMS, &r.OnError)
		return r, err
	})
}

func ReadUnmanagedPolicyRevisions(ctx context.Context, q access.Queryer) ([]NamedPolicyRevision, error) {
	rows, err := q.Query(ctx, `SELECT v.id::text,p.name,(v.id=p.active_revision_id) FROM olp.guardrail_policies p JOIN olp.guardrail_policy_revisions v ON v.policy_id=p.id
 WHERE p.managed_route_revision_id IS NULL AND (v.id=p.active_revision_id OR v.id IN(SELECT policy_revision_id FROM olp.guardrail_attachments UNION SELECT policy_revision_id FROM olp.guardrail_draft_attachments))`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[NamedPolicyRevision])
}
