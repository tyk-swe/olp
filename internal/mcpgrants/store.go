package mcpgrants

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/agentcontract"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/secrets"
)

type Store struct {
	Egress       *egress.Policy
	Pool         *pgxpool.Pool
	Keys         *secrets.KeyRing
	Auth         *secrets.AuthKey
	Installation string
}

type Enrollment struct {
	ID        string     `json:"id"`
	Binding   Binding    `json:"binding"`
	ExpiresAt time.Time  `json:"expires_at"`
	ClaimedAt *time.Time `json:"claimed_at"`
	ClaimedBy *string    `json:"-"`
	StartedBy *string    `json:"-"`
}

type Handoff struct {
	EnrollmentID  string    `json:"enrollment_id"`
	EnrollmentURL string    `json:"enrollment_url"`
	ExpiresAt     time.Time `json:"expires_at"`
}

func liveBinding(ctx context.Context, q access.Queryer, expected Binding) (Binding, error) {
	if expected.AgentID != "" {
		return liveAgentBinding(ctx, q, expected)
	}
	var project, endpoint string
	var config *routing.MCPOAuth
	err := q.QueryRow(ctx, `SELECT s.project_id::text,v.endpoint,v.oauth FROM olp.mcp_servers s JOIN olp.mcp_server_revisions v ON v.id=s.latest_revision_id WHERE s.id=$1 AND s.enabled AND s.retired_at IS NULL`, expected.ServerID).Scan(&project, &endpoint, &config)
	if err != nil || config == nil || !config.Valid() {
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return Binding{}, err
		}
		return Binding{}, ErrRequired
	}
	current := binding(expected.ServerID, project, endpoint, *config, expected.Identity)
	if current.Digest() != expected.Digest() {
		return Binding{}, ErrRequired
	}
	return current, nil
}

func liveAgentBinding(ctx context.Context, q access.Queryer, expected Binding) (Binding, error) {
	var project string
	var draft agentcontract.Draft
	err := q.QueryRow(ctx, `SELECT p.project_id::text,v.definition FROM olp.agent_publications p JOIN olp.agent_publication_revisions v ON v.id=p.latest_revision_id WHERE p.id=$1 AND p.kind='agent' AND NOT p.retired`, expected.AgentID).Scan(&project, &draft)
	if err != nil || draft.Agent == nil || draft.Agent.OAuth == nil || !draft.Agent.OAuth.Valid() {
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return Binding{}, err
		}
		return Binding{}, ErrRequired
	}
	current := agentBinding(expected.AgentID, project, *draft.Agent, expected.Identity)
	if current.Digest() != expected.Digest() {
		return Binding{}, ErrRequired
	}
	return current, nil
}

func revisionAgentBinding(ctx context.Context, q access.Queryer, expected Binding, revisionID string) (Binding, error) {
	var project string
	var draft agentcontract.Draft
	err := q.QueryRow(ctx, `SELECT p.project_id::text,v.definition FROM olp.agent_publications p JOIN olp.agent_publication_revisions v ON v.publication_id=p.id WHERE p.id=$1 AND p.kind='agent' AND v.id=$2`, expected.AgentID, revisionID).Scan(&project, &draft)
	if err != nil || draft.Agent == nil || draft.Agent.OAuth == nil || !draft.Agent.OAuth.Valid() {
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return Binding{}, err
		}
		return Binding{}, ErrRequired
	}
	current := agentBinding(expected.AgentID, project, *draft.Agent, expected.Identity)
	if current.Digest() != expected.Digest() {
		return Binding{}, ErrRequired
	}
	return current, nil
}

func (s *Store) Resolve(ctx context.Context, authority access.Authority, tool routing.MCPToolsetTool) ([]byte, error) {
	expected, bindErr := Bind(authority, tool)
	if bindErr != nil {
		return nil, bindErr
	}
	if _, err := liveBinding(ctx, s.Pool, expected); err != nil {
		return nil, err
	}
	return s.resolveGrant(ctx, expected, "")

}

func (s *Store) AgentBinding(authority access.Authority, publicationID string, agent agentcontract.Definition) (string, error) {
	expected, err := BindAgent(authority, publicationID, agent)
	if err != nil {
		return "", err
	}
	return expected.Digest(), nil
}

func (s *Store) StartAgent(ctx context.Context, authority access.Authority, publicationID string, agent agentcontract.Definition) (Handoff, error) {
	expected, err := BindAgent(authority, publicationID, agent)
	if err != nil {
		return Handoff{}, err
	}
	return s.Start(ctx, expected, nil)
}

func (s *Store) ResolveAgent(ctx context.Context, authority access.Authority, publicationID string, agent agentcontract.Definition) ([]byte, string, error) {
	expected, err := BindAgent(authority, publicationID, agent)
	if err != nil {
		return nil, "", err
	}
	if _, err := liveBinding(ctx, s.Pool, expected); err != nil {
		return nil, "", err
	}
	token, err := s.resolveGrant(ctx, expected, "")
	return token, expected.Digest(), err
}

func (s *Store) ResolveAgentTask(ctx context.Context, digest, publicationID, projectID, revisionID string, agent agentcontract.Definition) ([]byte, error) {
	if digest == "" || revisionID == "" || agent.OAuth == nil || !agent.OAuth.Valid() {
		return nil, ErrRequired
	}
	var stored Binding
	if err := s.Pool.QueryRow(ctx, `SELECT binding FROM olp.mcp_user_grants WHERE binding_digest=$1 AND agent_id=$2 AND project_id=$3`, digest, publicationID, projectID).Scan(&stored); err != nil {
		return nil, ErrRequired
	}
	expected := agentBinding(publicationID, projectID, agent, stored.Identity)
	if expected.Digest() != digest || stored.Digest() != digest {
		return nil, ErrRequired
	}
	if _, err := revisionAgentBinding(ctx, s.Pool, expected, revisionID); err != nil {
		return nil, err
	}
	return s.resolveGrant(ctx, expected, revisionID)
}

func (s *Store) Start(ctx context.Context, expected Binding, startedBy *string) (Handoff, error) {
	if expected.OAuth.EntraOBO != nil {
		return Handoff{}, access.Invalid("oauth", "Entra on-behalf-of grants require a verified user request, not browser enrollment.")
	}
	if _, err := liveBinding(ctx, s.Pool, expected); err != nil {
		return Handoff{}, err
	}
	id, token := access.NewID(), rand.Text()
	expires := time.Now().UTC().Add(10 * time.Minute)
	_, err := s.Pool.Exec(ctx, `INSERT INTO olp.mcp_grant_enrollments(id,server_id,agent_id,project_id,binding,binding_digest,handoff_digest,started_by,expires_at) VALUES($1,NULLIF($2,'')::uuid,NULLIF($3,'')::uuid,$4,$5,$6,$7,$8,$9)`, id, expected.ServerID, expected.AgentID, expected.ProjectID, expected, expected.Digest(), s.Auth.Digest(secrets.MCPEnrollmentDigest, token), startedBy, expires)
	if err != nil {
		return Handoff{}, err
	}
	return Handoff{EnrollmentID: id, EnrollmentURL: "/agents/grants#" + id + "." + token, ExpiresAt: expires}, nil
}

func (s *Store) Claim(ctx context.Context, id, token, memberID string) (Enrollment, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Enrollment{}, err
	}
	defer tx.Rollback(ctx)
	var e Enrollment
	var digest []byte
	if err := tx.QueryRow(ctx, `SELECT id::text,binding,expires_at,claimed_at,claimed_by::text,started_by::text,handoff_digest FROM olp.mcp_grant_enrollments WHERE id=$1 FOR UPDATE`, id).Scan(&e.ID, &e.Binding, &e.ExpiresAt, &e.ClaimedAt, &e.ClaimedBy, &e.StartedBy, &digest); err != nil {
		return Enrollment{}, err
	}
	provided := s.Auth.Digest(secrets.MCPEnrollmentDigest, token)
	if subtle.ConstantTimeCompare(provided, digest) != 1 || (e.StartedBy != nil && *e.StartedBy != memberID) {
		return Enrollment{}, access.Fail(404, "not_found", "The enrollment was not found.")
	}
	if e.ClaimedAt != nil {
		return Enrollment{}, access.Fail(409, "enrollment_used", "Start a new enrollment. This link has already been used.")
	}
	if !time.Now().Before(e.ExpiresAt) {
		return Enrollment{}, access.Fail(410, "enrollment_expired", "Start a new enrollment. This link has expired.")
	}
	if _, err := liveBinding(ctx, tx, e.Binding); err != nil {
		return Enrollment{}, err
	}
	claimed := time.Now().UTC()
	if _, err := tx.Exec(ctx, `UPDATE olp.mcp_grant_enrollments SET claimed_at=$2,claimed_by=$3 WHERE id=$1`, id, claimed, memberID); err != nil {
		return Enrollment{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Enrollment{}, err
	}
	return Enrollment{ID: e.ID, Binding: e.Binding, ExpiresAt: e.ExpiresAt, ClaimedAt: &claimed, ClaimedBy: &memberID, StartedBy: e.StartedBy}, nil
}
