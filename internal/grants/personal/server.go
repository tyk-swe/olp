package personal

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/domain/agentcontract"
	"github.com/tyk-swe/olp/internal/routing"
)

type ServerToolsets interface {
	Toolset(ctx context.Context, serverID string) (string, routing.MCPToolsetTool, error)
}

type Server struct {
	access  *access.Server
	servers ServerToolsets
	grants  *Store
}

func New(a *access.Server, servers ServerToolsets) *Server {
	return &Server{access: a, servers: servers, grants: NewStore(a.Pool, a.Installation, a.Auth, a.Keys, a.Egress)}
}

type StandingState string

const (
	Pending StandingState = "pending"
	Active  StandingState = "active"
	Expired StandingState = "expired"
	Revoked StandingState = "revoked"
	Denied  StandingState = "denied"
	Failed  StandingState = "failed"
)

type Standing struct {
	ID        string        `json:"id"`
	GrantID   *string       `json:"grant_id"`
	Binding   Binding       `json:"binding"`
	State     StandingState `json:"state"`
	ExpiresAt time.Time     `json:"expires_at"`
	CreatedAt time.Time     `json:"created_at"`
}

type startRequest struct {
	KeyID string `json:"api_key_id"`
}
type claimRequest struct {
	EnrollmentID string `json:"enrollment_id"`
	HandoffToken string `json:"handoff_token"`
}

type claimResponse struct {
	Enrollment Enrollment    `json:"enrollment"`
	State      StandingState `json:"state"`
}

func (s *Server) Register(mux *http.ServeMux) {
	s.access.Route(mux, "GET /api/v1/mcp-user-grants", s.list)
	s.access.Route(mux, "POST /api/v1/mcp-grant-enrollments/{enrollment_id}/authorize", s.authorize)
	s.access.Route(mux, "POST /api/v1/mcp-grant-enrollments/{enrollment_id}/complete", s.complete)
	s.access.Route(mux, "POST /api/v1/mcp-grant-enrollments/{enrollment_id}/poll", s.poll)
	s.access.Route(mux, "POST /api/v1/mcp-servers/{server_id}/grant-enrollments", s.start)
	s.access.Route(mux, "POST /api/v1/agents/{agent_id}/grant-enrollments", s.startAgent)
	s.access.Route(mux, "POST /api/v1/mcp-grant-enrollments/claim", s.claim)
	s.access.Route(mux, "DELETE /api/v1/mcp-user-grants/{grant_id}", s.revoke)
}

func (s *Server) startAgent(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	if p.Kind != "user" {
		return httpkit.Reply{}, httpkit.Forbidden()
	}
	id, err := httpkit.IDParam(r, "agent_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	var input startRequest
	if err := httpkit.Decode(r, &input); err != nil {
		return httpkit.Reply{}, err
	}
	keyUUID, err := uuid.Parse(input.KeyID)
	if err != nil {
		return httpkit.Reply{}, httpkit.Invalid("api_key_id", "Use an API key UUID.")
	}
	var project string
	var draft agentcontract.Draft
	if err := s.access.Pool.QueryRow(r.Context(), `SELECT p.project_id::text,v.definition FROM olp.agent_publications p JOIN olp.agent_publication_revisions v ON v.id=p.latest_revision_id WHERE p.id=$1 AND p.kind='agent' AND NOT p.retired`, id).Scan(&project, &draft); err != nil {
		return httpkit.Reply{}, err
	}
	if err := p.Project(&project, authz.Change); err != nil {
		return httpkit.Reply{}, err
	}
	var keyProject string
	if err := s.access.Pool.QueryRow(r.Context(), `SELECT project_id::text FROM olp.api_keys WHERE id=$1 AND revoked_at IS NULL`, keyUUID.String()).Scan(&keyProject); err != nil {
		return httpkit.Reply{}, err
	}
	if keyProject != project || draft.Agent == nil {
		return httpkit.Reply{}, httpkit.Fail(404, "not_found", "The API key was not found.")
	}
	bound, err := BindAgent(keyauthority.Authority{ID: keyUUID.String(), ProjectID: &project}, id, *draft.Agent)
	if err != nil {
		return httpkit.Reply{}, httpkit.Invalid("agent_id", "Configure personal OAuth for this agent before enrollment.")
	}
	member := p.UserID()
	handoff, err := s.grants.Start(r.Context(), bound, &member)
	if err != nil {
		return httpkit.Reply{}, grantProblem(err)
	}
	return httpkit.Detail(handoff, ""), nil
}

func (s *Server) start(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	if p.Kind != "user" {
		return httpkit.Reply{}, httpkit.Forbidden()
	}
	id, err := httpkit.IDParam(r, "server_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	var input startRequest
	if err := httpkit.Decode(r, &input); err != nil {
		return httpkit.Reply{}, err
	}
	keyUUID, err := uuid.Parse(input.KeyID)
	if err != nil {
		return httpkit.Reply{}, httpkit.Invalid("api_key_id", "Use an API key UUID.")
	}
	keyID := keyUUID.String()
	serverProject, tool, err := s.servers.Toolset(r.Context(), id)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if err := p.Project(&serverProject, authz.Change); err != nil {
		return httpkit.Reply{}, err
	}
	var project string
	if err := s.access.Pool.QueryRow(r.Context(), `SELECT project_id::text FROM olp.api_keys WHERE id=$1 AND revoked_at IS NULL`, keyID).Scan(&project); err != nil {
		return httpkit.Reply{}, err
	}
	if project != serverProject {
		return httpkit.Reply{}, httpkit.Fail(404, "not_found", "The API key was not found.")
	}
	bound, err := Bind(keyauthority.Authority{ID: keyID, ProjectID: &project}, tool)
	if err != nil {
		return httpkit.Reply{}, httpkit.Invalid("server_id", "Configure personal OAuth for this server before enrollment.")
	}
	member := p.UserID()
	handoff, err := s.grants.Start(r.Context(), bound, &member)
	if err != nil {
		return httpkit.Reply{}, grantProblem(err)
	}
	return httpkit.Detail(handoff, ""), nil
}

func (s *Server) claim(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	if p.Kind != "user" {
		return httpkit.Reply{}, httpkit.Forbidden()
	}
	var input claimRequest
	if err := httpkit.Decode(r, &input); err != nil {
		return httpkit.Reply{}, err
	}
	enrollmentID, err := uuid.Parse(input.EnrollmentID)
	if err != nil {
		return httpkit.Reply{}, httpkit.Invalid("enrollment_id", "Use an enrollment UUID.")
	}
	if len(input.HandoffToken) < 20 || len(input.HandoffToken) > 128 {
		return httpkit.Reply{}, httpkit.Invalid("handoff_token", "Use the one-use browser enrollment link.")
	}
	enrollment, err := s.grants.Claim(r.Context(), enrollmentID.String(), input.HandoffToken, p.UserID())
	if err != nil {
		return httpkit.Reply{}, grantProblem(err)
	}
	return httpkit.Detail(claimResponse{Enrollment: enrollment, State: Pending}, ""), nil
}

func grantProblem(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return httpkit.Fail(404, "not_found", "The personal grant enrollment was not found.")
	}
	if errors.Is(err, ErrRequired) {
		return httpkit.Fail(409, "grant_binding_changed", "Start a new enrollment for the server's current OAuth configuration.")
	}
	return err
}

func (s *Server) list(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	page, err := httpkit.Page(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	rows, err := s.access.Pool.Query(r.Context(), `SELECT e.id::text,g.id::text,e.binding,
 CASE WHEN g.revoked_at IS NOT NULL THEN 'revoked' WHEN g.id IS NOT NULL AND (g.expires_at<=now() OR g.failure_code IS NOT NULL) THEN 'expired' WHEN g.id IS NOT NULL THEN 'active' WHEN e.outcome IS NOT NULL AND e.outcome<>'completed' THEN e.outcome WHEN e.expires_at<=now() THEN 'expired' ELSE 'pending' END,
 COALESCE(g.expires_at,e.expires_at),e.created_at
 FROM olp.mcp_grant_enrollments e LEFT JOIN olp.mcp_user_grants g ON g.enrollment_id=e.id
 WHERE (e.claimed_by=$1 OR e.started_by=$1 OR $2 OR e.project_id=ANY($3::uuid[])) AND e.id<$4::uuid ORDER BY e.id DESC LIMIT $5`, p.UserID(), p.AllProjects, p.ProjectIDs(), page.Before, page.Limit+1)
	if err != nil {
		return httpkit.Reply{}, err
	}
	defer rows.Close()
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Standing, error) {
		var item Standing
		err := row.Scan(&item.ID, &item.GrantID, &item.Binding, &item.State, &item.ExpiresAt, &item.CreatedAt)
		return item, err
	})
	if err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.ListReplyBy(items, page, func(item Standing) string { return item.ID }), nil
}

func (s *Server) revoke(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	if p.Kind != "user" {
		return httpkit.Reply{}, httpkit.Forbidden()
	}
	id, err := httpkit.IDParam(r, "grant_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	return access.Transact(s.access, r, access.Unlocked, func(tx pgx.Tx, p authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		var project, member string
		if err := tx.QueryRow(r.Context(), `SELECT project_id::text,COALESCE(member_id::text,'') FROM olp.mcp_user_grants WHERE id=$1 FOR UPDATE`, id).Scan(&project, &member); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if member != p.UserID() {
			if err := p.Project(&project, authz.Change); err != nil {
				return httpkit.Reply{}, access.Event{}, err
			}
		}
		if _, err := tx.Exec(r.Context(), `UPDATE olp.mcp_user_grants SET revoked_at=COALESCE(revoked_at,now()) WHERE id=$1`, id); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		return httpkit.Reply{Status: http.StatusNoContent}, access.Event{Action: "mcp_grant_revoke", Resource: "mcp_user_grant", ID: id, Project: &project}, nil
	})
}
