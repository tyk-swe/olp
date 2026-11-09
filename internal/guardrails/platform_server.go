package guardrails

import (
	"context"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
)

type Server struct {
	Access  *access.Server
	Publish Publisher
}

func (s *Server) Register(mux *http.ServeMux) {
	s.Access.Route(mux, "GET /api/v1/guardrail-attachments", s.listAttachments)
	s.Access.Route(mux, "POST /api/v1/guardrail-attachments", s.attach)
	s.Access.Route(mux, "DELETE /api/v1/guardrail-attachments/{attachment_id}", s.detach)
	s.Access.Route(mux, "PUT /api/v1/guardrail-attachments/{attachment_id}", s.replaceAttachment)
}
func (s *Server) publish(ctx context.Context, tx pgx.Tx, actor string) error {
	return s.Publish(ctx, tx, actor)
}

// AuthorizeAttachment preserves the authority of the resource being changed.
// A configure token alone cannot change key, organization or installation policy.
func AuthorizeAttachment(ctx context.Context, q access.Queryer, p access.Principal, a Attachment, need access.Need) error {
	if need == access.Change {
		op := access.Configure
		switch a.Scope {
		case InstallationScope:
			op = access.Settings
		case OrganizationScope:
			op = access.ManageOrganization
		case KeyScope:
			op = access.Keys
		}
		if err := p.Authorize(op); err != nil {
			return err
		}
	}
	switch a.Scope {
	case InstallationScope:
		if !p.AllProjects {
			return access.Forbidden()
		}
		return p.Project(nil, need)
	case OrganizationScope:
		return p.Organization(a.ScopeID, need)
	case ProjectScope:
		return p.Project(&a.ScopeID, need)
	case RouteScope, KeyScope, ToolsetScope:
		project, err := attachmentProject(ctx, q, a.Scope, a.ScopeID)
		if err != nil {
			return err
		}
		return p.Project(project, need)
	default:
		return access.Invalid("scope", "Choose installation, organization, project, route, toolset or api-key.")
	}
}

func attachmentProject(ctx context.Context, q access.Queryer, scope Scope, id string) (*string, error) {
	queries := map[Scope]string{
		RouteScope:   "SELECT project_id::text FROM olp.routes WHERE id=$1",
		KeyScope:     "SELECT project_id::text FROM olp.api_keys WHERE id=$1",
		ToolsetScope: "SELECT project_id::text FROM olp.agent_publications WHERE id=$1 AND kind='toolset'",
	}
	query, ok := queries[scope]
	if !ok {
		return nil, access.Invalid("scope", "Choose route, toolset or api-key.")
	}
	var project *string
	if err := q.QueryRow(ctx, query, id).Scan(&project); err != nil {
		return nil, err
	}
	return project, nil
}

func (s *Server) listAttachments(r *http.Request, p access.Principal) (access.Reply, error) {
	rows, err := s.Access.Pool.Query(r.Context(), "SELECT a.id::text,a.scope,COALESCE(a.scope_id::text,'installation'),a.policy_revision_id::text,a.position,a.etag::text,v.policy_id::text,p.active_revision_id::text FROM olp.guardrail_attachments a JOIN olp.guardrail_policy_revisions v ON v.id=a.policy_revision_id JOIN olp.guardrail_policies p ON p.id=v.policy_id ORDER BY a.position,a.id LIMIT 10001")
	if err != nil {
		return access.Reply{}, err
	}
	type item struct {
		Attachment
		ETag             string  `json:"etag"`
		PolicyID         string  `json:"policy_id"`
		ActiveRevisionID *string `json:"active_revision_id"`
	}
	values := []item{}
	for rows.Next() {
		var a item
		if err = rows.Scan(&a.ID, &a.Scope, &a.ScopeID, &a.PolicyRevisionID, &a.Order, &a.ETag, &a.PolicyID, &a.ActiveRevisionID); err != nil {
			rows.Close()
			return access.Reply{}, err
		}
		values = append(values, a)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return access.Reply{}, err
	}
	visible := []item{}
	for _, a := range values {
		if AuthorizeAttachment(r.Context(), s.Access.Pool, p, a.Attachment, access.View) == nil {
			visible = append(visible, a)
		}
	}
	return access.Reply{Status: 200, Body: map[string]any{"items": visible}}, nil
}
func (s *Server) attach(r *http.Request, _ access.Principal) (access.Reply, error) {
	return s.writeAttachment(r, false)
}
func (s *Server) detach(r *http.Request, _ access.Principal) (access.Reply, error) {
	return s.writeAttachment(r, true)
}
func (s *Server) writeAttachment(r *http.Request, remove bool) (access.Reply, error) {
	var in Attachment
	if !remove {
		if err := access.DecodeUnique(r, &in, 65536); err != nil {
			return access.Reply{}, err
		}
		if in.ID != "" || in.Order < 0 {
			return access.Reply{}, access.Invalid("attachment", "The server assigns identities; position must be nonnegative.")
		}
		if !in.Scope.Valid() {
			return access.Reply{}, access.Invalid("scope", "Choose installation, organization, project, route, toolset or api-key.")
		}
	}
	tx, p, err := s.Access.BeginMutation(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	etag := ""
	if remove {
		id, err := access.IDParam(r, "attachment_id")
		if err != nil {
			return access.Reply{}, err
		}
		err = tx.QueryRow(r.Context(), "SELECT id::text,scope,COALESCE(scope_id::text,'installation'),policy_revision_id::text,position,etag::text FROM olp.guardrail_attachments WHERE id=$1 FOR UPDATE", id).Scan(&in.ID, &in.Scope, &in.ScopeID, &in.PolicyRevisionID, &in.Order, &etag)
		if err != nil {
			return access.Reply{}, err
		}
	}
	if err = AuthorizeAttachment(r.Context(), tx, p, in, access.Change); err != nil {
		return access.Reply{}, err
	}
	claim, replayed, err := s.Access.Replay(r, tx, p, in)
	if err != nil {
		return access.Reply{}, err
	}
	if replayed != nil {
		return *replayed, tx.Commit(r.Context())
	}
	if remove {
		if err = access.Match(r, etag); err != nil {
			return access.Reply{}, err
		}
		_, err = tx.Exec(r.Context(), "DELETE FROM olp.guardrail_attachments WHERE id=$1", in.ID)
	} else {
		if err = validateAttachmentPolicy(r.Context(), tx, p, in); err != nil {
			return access.Reply{}, err
		}
		in, err = createAttachment(r.Context(), tx, p.UserID(), in)
	}
	if err != nil {
		return access.Reply{}, err
	}
	if err = s.publish(r.Context(), tx, p.UserID()); err != nil {
		return access.Reply{}, access.Invalid("attachment", "The effective policy is incompatible with an attached route.")
	}
	action := "guardrail.attach"
	if remove {
		action = "guardrail.detach"
	}
	if err = access.Audit(r.Context(), tx, r, p.Actor(), action, "guardrail_attachment", in.ID, "success"); err != nil {
		return access.Reply{}, err
	}
	reply := access.Reply{Status: 204}
	if !remove {
		reply = access.Reply{Status: 201, Body: in}
	}
	if err = s.Access.CompleteReplay(r, tx, claim, reply); err != nil {
		return access.Reply{}, err
	}
	return reply, tx.Commit(r.Context())
}
func validateAttachmentPolicy(ctx context.Context, tx pgx.Tx, p access.Principal, in Attachment) error {
	var project *string
	err := tx.QueryRow(ctx, "SELECT p.project_id::text FROM olp.guardrail_policies p JOIN olp.guardrail_policy_revisions v ON v.policy_id=p.id WHERE v.id=$1 AND p.retired_at IS NULL", in.PolicyRevisionID).Scan(&project)
	if err != nil {
		return access.Invalid("policy_revision_id", "Choose an existing policy revision.")
	}
	if err = p.Project(project, access.Change); err != nil {
		return err
	}
	ownedBy := func(id string) bool { return project != nil && *project == id }
	if in.Scope == ProjectScope && !ownedBy(in.ScopeID) {
		return access.Invalid("scope_id", "The policy must belong to this project.")
	}
	if in.Scope == RouteScope || in.Scope == KeyScope || in.Scope == ToolsetScope {
		owning, ownerErr := attachmentProject(ctx, tx, in.Scope, in.ScopeID)
		if ownerErr != nil || owning == nil || !ownedBy(*owning) {
			return access.Invalid("scope_id", "The policy must belong to this project.")
		}
		if in.Scope == RouteScope && in.Order == 0 {
			var legacy bool
			if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM olp.guardrail_policies p JOIN olp.routes r ON r.latest_revision_id=p.managed_route_revision_id WHERE r.id=$1)", in.ScopeID).Scan(&legacy); err != nil {
				return err
			}
			if legacy {
				return access.Invalid("order", "Position zero preserves this route's migrated content policy; choose a later position.")
			}
		}
	}
	return nil
}
