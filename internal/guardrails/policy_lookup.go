package guardrails

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
)

type LatestPolicy struct {
	Revision         PolicyRevision
	ActiveRevisionID *string
}

func FindLatestPolicy(ctx context.Context, q access.Queryer, projectID *string, name string) (*LatestPolicy, error) {
	var p LatestPolicy
	err := q.QueryRow(ctx, "SELECT p.id::text,v.id::text,p.name,v.revision,v.entries,p.active_revision_id::text FROM olp.guardrail_policies p JOIN olp.guardrail_policy_revisions v ON v.id=p.latest_revision_id WHERE p.project_id IS NOT DISTINCT FROM $1::uuid AND lower(p.name)=lower($2) AND p.retired_at IS NULL", projectID, name).
		Scan(&p.Revision.PolicyID, &p.Revision.ID, &p.Revision.Name, &p.Revision.Revision, &p.Revision.Entries, &p.ActiveRevisionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func ActivePolicyRevisionID(ctx context.Context, q access.Queryer, projectID *string, name string) (string, error) {
	var id string
	err := q.QueryRow(ctx, "SELECT active_revision_id::text FROM olp.guardrail_policies WHERE project_id IS NOT DISTINCT FROM $1::uuid AND lower(name)=lower($2) AND retired_at IS NULL", projectID, name).Scan(&id)
	return id, err
}
