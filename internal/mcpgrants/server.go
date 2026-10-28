package mcpgrants

import (
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/mcpservers"
	"github.com/tyk-swe/olp/internal/routing"
)

type Server struct{ Access *access.Server }

type StandingState string

const (
	Pending StandingState = "pending"
	Active  StandingState = "active"
	Expired StandingState = "expired"
	Revoked StandingState = "revoked"
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

func (s *Server) store() *Store {
	return &Store{Pool: s.Access.Pool, Keys: s.Access.Keys, Auth: s.Access.Auth, Installation: s.Access.Installation}
}

func (s *Server) Register(mux *http.ServeMux) {
	s.Access.Route(mux, "GET /api/v1/mcp-user-grants", s.list)
	s.Access.Route(mux, "POST /api/v1/mcp-servers/{server_id}/grant-enrollments", s.start)
	s.Access.Route(mux, "POST /api/v1/mcp-grant-enrollments/claim", s.claim)
	s.Access.Route(mux, "DELETE /api/v1/mcp-user-grants/{grant_id}", s.revoke)
}

func (s *Server) start(r *http.Request, p access.Principal) (access.Reply, error) {
	if p.Kind != "user" {
		return access.Reply{}, access.Forbidden()
	}
	id, err := access.IDParam(r, "server_id")
	if err != nil {
		return access.Reply{}, err
	}
	var input startRequest
	if err := access.Decode(r, &input); err != nil {
		return access.Reply{}, err
	}
	keyUUID, err := uuid.Parse(input.KeyID)
	if err != nil {
		return access.Reply{}, access.Invalid("api_key_id", "Use an API key UUID.")
	}
	keyID := keyUUID.String()
	server, err := mcpservers.Load(r.Context(), s.Access.Pool, id, false)
	if err != nil {
		return access.Reply{}, err
	}
	if err := p.Project(&server.ProjectID, access.Change); err != nil {
		return access.Reply{}, err
	}
	var project string
	if err := s.Access.Pool.QueryRow(r.Context(), `SELECT project_id::text FROM olp.api_keys WHERE id=$1 AND revoked_at IS NULL`, keyID).Scan(&project); err != nil {
		return access.Reply{}, err
	}
	if project != server.ProjectID {
		return access.Reply{}, access.Fail(404, "not_found", "The API key was not found.")
	}
	bound, err := Bind(access.Authority{ID: keyID, ProjectID: &project}, routing.MCPToolsetTool{ServerID: id, Endpoint: server.Endpoint, OAuth: server.OAuth})
	if err != nil {
		return access.Reply{}, access.Invalid("server_id", "Configure personal OAuth for this server before enrollment.")
	}
	member := p.UserID()
	handoff, err := s.store().Start(r.Context(), bound, &member)
	if err != nil {
		return access.Reply{}, grantProblem(err)
	}
	return access.Detail(handoff, ""), nil
}

func (s *Server) claim(r *http.Request, p access.Principal) (access.Reply, error) {
	if p.Kind != "user" {
		return access.Reply{}, access.Forbidden()
	}
	var input claimRequest
	if err := access.Decode(r, &input); err != nil {
		return access.Reply{}, err
	}
	enrollmentID, err := uuid.Parse(input.EnrollmentID)
	if err != nil {
		return access.Reply{}, access.Invalid("enrollment_id", "Use an enrollment UUID.")
	}
	if len(input.HandoffToken) < 20 || len(input.HandoffToken) > 128 {
		return access.Reply{}, access.Invalid("handoff_token", "Use the one-use browser enrollment link.")
	}
	enrollment, err := s.store().Claim(r.Context(), enrollmentID.String(), input.HandoffToken, p.UserID())
	if err != nil {
		return access.Reply{}, grantProblem(err)
	}
	return access.Detail(claimResponse{Enrollment: enrollment, State: Pending}, ""), nil
}

func grantProblem(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return access.Fail(404, "not_found", "The personal grant enrollment was not found.")
	}
	if errors.Is(err, ErrRequired) {
		return access.Fail(409, "grant_binding_changed", "Start a new enrollment for the server's current OAuth configuration.")
	}
	return err
}

func (s *Server) list(r *http.Request, p access.Principal) (access.Reply, error) {
	page, err := access.Page(r)
	if err != nil {
		return access.Reply{}, err
	}
	rows, err := s.Access.Pool.Query(r.Context(), `SELECT e.id::text,g.id::text,e.binding,
 CASE WHEN g.revoked_at IS NOT NULL THEN 'revoked' WHEN g.id IS NOT NULL AND g.expires_at<=now() THEN 'expired' WHEN g.id IS NOT NULL THEN 'active' WHEN e.expires_at<=now() THEN 'expired' ELSE 'pending' END,
 COALESCE(g.expires_at,e.expires_at),e.created_at
 FROM olp.mcp_grant_enrollments e LEFT JOIN olp.mcp_user_grants g ON g.enrollment_id=e.id
 WHERE (e.claimed_by=$1 OR e.started_by=$1 OR $2 OR e.project_id=ANY($3::uuid[])) AND e.id<$4::uuid ORDER BY e.id DESC LIMIT $5`, p.UserID(), p.AllProjects, p.ProjectIDs(), page.Before, page.Limit+1)
	if err != nil {
		return access.Reply{}, err
	}
	defer rows.Close()
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Standing, error) {
		var item Standing
		err := row.Scan(&item.ID, &item.GrantID, &item.Binding, &item.State, &item.ExpiresAt, &item.CreatedAt)
		return item, err
	})
	if err != nil {
		return access.Reply{}, err
	}
	return access.ListReplyBy(items, page, func(item Standing) string { return item.ID }), nil
}

func (s *Server) revoke(r *http.Request, p access.Principal) (access.Reply, error) {
	if p.Kind != "user" {
		return access.Reply{}, access.Forbidden()
	}
	id, err := access.IDParam(r, "grant_id")
	if err != nil {
		return access.Reply{}, err
	}
	tx, err := s.Access.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	var project, member string
	if err := tx.QueryRow(r.Context(), `SELECT project_id::text,member_id::text FROM olp.mcp_user_grants WHERE id=$1 FOR UPDATE`, id).Scan(&project, &member); err != nil {
		return access.Reply{}, err
	}
	if member != p.UserID() {
		if err := p.Project(&project, access.Change); err != nil {
			return access.Reply{}, err
		}
	}
	if _, err := tx.Exec(r.Context(), `UPDATE olp.mcp_user_grants SET revoked_at=COALESCE(revoked_at,now()) WHERE id=$1`, id); err != nil {
		return access.Reply{}, err
	}
	if err := access.AuditForProject(r.Context(), tx, r, p.Actor(), "mcp_grant_revoke", "mcp_user_grant", id, "success", &project); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, access.Reply{Status: http.StatusNoContent})
}
