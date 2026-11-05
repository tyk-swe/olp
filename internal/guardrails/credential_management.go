package guardrails

import (
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/secrets"
)

type credentialMetadata struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"project_id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Server) listCredentials(r *http.Request, p access.Principal) (access.Reply, error) {
	page, err := access.Page(r)
	if err != nil {
		return access.Reply{}, err
	}
	rows, err := s.Access.Pool.Query(r.Context(), "SELECT id::text,project_id::text,name,created_at FROM olp.guardrail_credentials WHERE id<$1 AND ($2 OR project_id=ANY($3::uuid[])) ORDER BY id DESC LIMIT $4", page.Before, p.AllProjects, p.ProjectIDs(), page.Limit+1)
	if err != nil {
		return access.Reply{}, err
	}
	defer rows.Close()
	items, err := pgx.CollectRows(rows, pgx.RowToStructByPos[credentialMetadata])
	if err != nil {
		return access.Reply{}, err
	}
	return access.ListReplyBy(items, page, func(item credentialMetadata) string { return item.ID }), nil
}

func (s *Server) getCredential(r *http.Request, p access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "credential_id")
	if err != nil {
		return access.Reply{}, err
	}
	var value credentialMetadata
	if err := s.Access.Pool.QueryRow(r.Context(), "SELECT id::text,project_id::text,name,created_at FROM olp.guardrail_credentials WHERE id=$1", id).Scan(&value.ID, &value.ProjectID, &value.Name, &value.CreatedAt); err != nil {
		return access.Reply{}, err
	}
	if err := p.Project(&value.ProjectID, access.View); err != nil {
		return access.Reply{}, err
	}
	return access.Detail(value, value.ID), nil
}

func (s *Server) deleteCredential(r *http.Request, _ access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "credential_id")
	if err != nil {
		return access.Reply{}, err
	}
	tx, err := s.Access.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.Access.Reauthorize(r, tx)
	if err != nil {
		return access.Reply{}, err
	}
	claim, replay, err := s.Access.Replay(r, tx, p, id)
	if err != nil {
		return access.Reply{}, err
	}
	if replay != nil {
		return access.Commit(r, tx, *replay)
	}
	var project string
	if err := tx.QueryRow(r.Context(), "SELECT project_id::text FROM olp.guardrail_credentials WHERE id=$1 FOR UPDATE", id).Scan(&project); err != nil {
		return access.Reply{}, err
	}
	if err := p.Project(&project, access.Change); err != nil {
		return access.Reply{}, err
	}
	if err := access.Match(r, id); err != nil {
		return access.Reply{}, err
	}
	var referenced bool
	if err := tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM olp.guardrail_revisions WHERE CASE WHEN pg_input_is_valid(configuration->>'credential_ref','uuid') THEN (configuration->>'credential_ref')::uuid END=$1::uuid OR CASE WHEN pg_input_is_valid(configuration->'network'->>'credential_id','uuid') THEN (configuration->'network'->>'credential_id')::uuid END=$1::uuid)", id).Scan(&referenced); err != nil {
		return access.Reply{}, err
	}
	if referenced {
		return access.Reply{}, access.Fail(409, "guardrail_credential_in_use", "A retained guardrail revision still references this credential.")
	}
	if _, err := tx.Exec(r.Context(), "DELETE FROM olp.guardrail_credentials WHERE id=$1", id); err != nil {
		return access.Reply{}, err
	}
	if _, err := tx.Exec(r.Context(), "DELETE FROM olp.secrets WHERE id=$1 AND purpose=$2", id, secrets.GuardrailCredential); err != nil {
		return access.Reply{}, err
	}
	if err := access.AuditForProject(r.Context(), tx, r, p.Actor(), "guardrail.credential.delete", "guardrail_credential", id, "success", &project); err != nil {
		return access.Reply{}, err
	}
	reply := access.Reply{Status: http.StatusNoContent}
	if err := s.Access.CompleteReplay(r, tx, claim, reply); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, reply)
}
