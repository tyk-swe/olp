package runs

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/limits/debits"
	"github.com/tyk-swe/olp/internal/secrets"
)

type WorkflowStore struct {
	pool         *pgxpool.Pool
	auth         *secrets.AuthKey
	keys         *secrets.KeyRing
	installation string
}

func NewWorkflowStore(pool *pgxpool.Pool, auth *secrets.AuthKey, keys *secrets.KeyRing, installation string) *WorkflowStore {
	return &WorkflowStore{pool: pool, auth: auth, keys: keys, installation: installation}
}

func (s *WorkflowStore) Start(ctx context.Context, authority keyauthority.Authority, routeID, revisionID string) (string, error) {
	if s == nil || s.pool == nil || s.auth == nil || authority.ProjectID == nil || !validApprovalID(routeID) || !validApprovalID(revisionID) {
		return "", debits.ErrAuthority
	}
	id, err := uuid.NewV7()
	if err != nil {
		return "", err
	}
	digest := PrincipalDigest(s.auth, authority)
	tag, err := s.pool.Exec(ctx, `INSERT INTO olp.agent_runs(id,api_key_id,project_id,route_revision_id,principal_digest) SELECT $1,k.id,$3,v.id,$5 FROM olp.api_keys k JOIN olp.route_revisions v ON v.id=$4 JOIN olp.routes r ON r.id=v.route_id WHERE k.id=$2 AND k.project_id=$3 AND k.revoked_at IS NULL AND (k.expires_at IS NULL OR k.expires_at>clock_timestamp()) AND r.id=$6 AND r.retired_at IS NULL AND (r.project_id IS NULL OR r.project_id=$3)`, id.String(), authority.ID, *authority.ProjectID, revisionID, digest, routeID)
	if err != nil {
		return "", err
	}
	if tag.RowsAffected() != 1 {
		return "", debits.ErrAuthority
	}
	return id.String(), nil
}

func (s *WorkflowStore) Prepare(ctx context.Context, authority keyauthority.Authority, intent debits.Intent, ordinal int, owners []limits.Owner) (debits.State, error) {
	if s == nil || s.pool == nil {
		return "", debits.ErrAuthority
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	state, err := PrepareStepTx(ctx, tx, authority, intent, ordinal, owners)
	if err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return state, nil
}
