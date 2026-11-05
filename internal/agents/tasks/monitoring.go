package tasks

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/agentcontract"
)

type Remote struct {
	ID               string
	KeyID            string
	ProjectID        string
	PublicationID    string
	RevisionID       string
	UpstreamID       string
	Deadline         time.Time
	WorkloadRevision string
	CallerExpiresAt  *time.Time
	Definition       agentcontract.Draft
	Binding          string
	Delegation       string
}

func (s *Store) Horizon(ctx context.Context) (string, error) {
	var horizon string
	err := s.pool.QueryRow(ctx, `SELECT id::text FROM olp.a2a_tasks ORDER BY id DESC LIMIT 1`).Scan(&horizon)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return horizon, err
}
func (s *Store) Page(ctx context.Context, cursor, horizon string) ([]Remote, error) {
	rows, err := s.pool.Query(ctx, `SELECT t.id::text,t.api_key_id::text,t.project_id::text,t.publication_id::text,t.revision_id::text,t.upstream_id,t.deadline_at,COALESCE(t.caller_workload_revision_id::text,''),t.caller_expires_at,v.definition,COALESCE(t.oauth_binding_digest,''),t.delegation_token_digest
 FROM olp.a2a_tasks t JOIN olp.agent_publication_revisions v ON v.id=t.revision_id
 WHERE t.state IN ('active','outcome_unknown') AND (t.cancel_claimed_until IS NULL OR t.cancel_claimed_until<=now()) AND t.id>COALESCE(NULLIF($1,'')::uuid,'00000000-0000-0000-0000-000000000000'::uuid) AND t.id<=$2::uuid
 ORDER BY t.id LIMIT 128`, cursor, horizon)
	if err != nil {
		return nil, err
	}
	tasks, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Remote, error) {
		var task Remote
		err := row.Scan(&task.ID, &task.KeyID, &task.ProjectID, &task.PublicationID, &task.RevisionID, &task.UpstreamID, &task.Deadline, &task.WorkloadRevision, &task.CallerExpiresAt, &task.Definition, &task.Binding, &task.Delegation)
		return task, err
	})
	if err != nil {
		return nil, err
	}
	return tasks, nil
}
func (s *Store) Claim(ctx context.Context, taskID string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE olp.a2a_tasks SET cancel_claimed_until=now()+interval '30 seconds'
 WHERE id=$1 AND state IN ('active','outcome_unknown') AND (cancel_claimed_until IS NULL OR cancel_claimed_until<=now())`, taskID)
	return err == nil && tag.RowsAffected() == 1, err
}

func (s *Store) Remote(ctx context.Context, id string) (Remote, error) {
	var task Remote
	err := s.pool.QueryRow(ctx, `SELECT t.id::text,t.project_id::text,t.publication_id::text,t.revision_id::text,t.upstream_id,COALESCE(t.oauth_binding_digest,''),v.definition
 FROM olp.a2a_tasks t JOIN olp.agent_publication_revisions v ON v.id=t.revision_id AND v.publication_id=t.publication_id
 WHERE t.id=$1`, id).Scan(&task.ID, &task.ProjectID, &task.PublicationID, &task.RevisionID, &task.UpstreamID, &task.Binding, &task.Definition)
	if errors.Is(err, pgx.ErrNoRows) {
		return Remote{}, ErrNotFound
	}
	return task, err
}
