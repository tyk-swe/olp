package agents

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/secrets"
)

type WorkflowStore struct {
	Pool   *pgxpool.Pool
	Auth   *secrets.AuthKey
	Debits *limits.DebitStore
}

func (s *WorkflowStore) Start(ctx context.Context, authority access.Authority, routeID, revisionID string) (string, error) {
	if s == nil || s.Pool == nil || s.Auth == nil || authority.ProjectID == nil || !validApprovalID(routeID) || !validApprovalID(revisionID) {
		return "", limits.ErrDebitAuthority
	}
	id, err := uuid.NewV7()
	if err != nil {
		return "", err
	}
	digest := PrincipalDigest(s.Auth, authority)
	tag, err := s.Pool.Exec(ctx, `INSERT INTO olp.agent_runs(id,api_key_id,project_id,route_revision_id,principal_digest) SELECT $1,k.id,$3,v.id,$5 FROM olp.api_keys k JOIN olp.route_revisions v ON v.id=$4 JOIN olp.routes r ON r.id=v.route_id WHERE k.id=$2 AND k.project_id=$3 AND k.revoked_at IS NULL AND (k.expires_at IS NULL OR k.expires_at>clock_timestamp()) AND r.id=$6 AND r.retired_at IS NULL AND (r.project_id IS NULL OR r.project_id=$3)`, id.String(), authority.ID, *authority.ProjectID, revisionID, digest, routeID)
	if err != nil {
		return "", err
	}
	if tag.RowsAffected() != 1 {
		return "", limits.ErrDebitAuthority
	}
	return id.String(), nil
}

func (s *WorkflowStore) Prepare(ctx context.Context, authority access.Authority, intent limits.DebitIntent, ordinal int, owners []limits.DebitOwner) (limits.DebitState, error) {
	if s == nil || s.Pool == nil || s.Debits == nil {
		return "", limits.ErrDebitAuthority
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	state, err := PrepareStepTx(ctx, tx, s.Debits, authority, intent, ordinal, owners)
	if err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return state, nil
}
