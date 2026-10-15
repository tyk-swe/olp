package guardrails

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/secrets"
)

type ManagedPolicy struct {
	PolicyRevision
	ETag             string     `json:"etag"`
	ActiveRevisionID *string    `json:"active_revision_id"`
	RetiredAt        *time.Time `json:"retired_at"`
}
type policyInput struct {
	Name      string  `json:"name"`
	ProjectID string  `json:"project_id,omitempty"`
	Entries   []Entry `json:"entries"`
}
type ApplyInput struct {
	Policy           *PolicyRevision     `json:"-"`
	Definitions      map[string]Revision `json:"-"`
	PolicyRevisionID string              `json:"policy_revision_id"`
	Phase            string              `json:"phase"`
	Segments         []Segment           `json:"segments"`
}

func (s *Server) registerPlatform(mux *http.ServeMux) {
	a := s.Access
	a.Route(mux, "POST /api/v1/guardrails/{guardrail_id}/activate", s.activateDefinition)
	a.Route(mux, "GET /api/v1/guardrail-policies", s.listPolicies)
	a.Route(mux, "POST /api/v1/guardrail-policies", s.createPolicy, access.MaxBody(128<<10))
	a.Route(mux, "GET /api/v1/guardrail-policies/{policy_id}", s.getPolicy)
	a.Route(mux, "PUT /api/v1/guardrail-policies/{policy_id}", s.updatePolicy, access.MaxBody(128<<10))
	a.Route(mux, "DELETE /api/v1/guardrail-policies/{policy_id}", s.retirePolicy)
	a.Route(mux, "POST /api/v1/guardrail-policies/{policy_id}/activate", s.activatePolicy)
	a.Route(mux, "GET /api/v1/guardrail-policies/{policy_id}/revisions/{revision_id}", s.policyRevision)
	a.Route(mux, "GET /api/v1/guardrail-attachments", s.listAttachments)
	a.Route(mux, "POST /api/v1/guardrail-attachments", s.attach)
	a.Route(mux, "DELETE /api/v1/guardrail-attachments/{attachment_id}", s.detach)
	a.Route(mux, "POST /api/v1/guardrails/evaluate", s.evaluate, access.MaxBody(MaxContentBytes))
	a.Route(mux, "POST /api/v1/guardrail-credentials", s.createCredential, access.MaxBody(256<<10))
}
func (s *Server) publish(ctx context.Context, tx pgx.Tx, actor string) error {
	if s.Publish != nil {
		return s.Publish(ctx, tx, actor)
	}
	_, err := ReadRegistry(ctx, tx)
	return err
}
func projectPointer(project string) *string {
	if project == "installation" || project == "" {
		return nil
	}
	return &project
}

func loadPolicy(ctx context.Context, q access.Queryer, id string, lock bool) (*ManagedPolicy, error) {
	query := `SELECT p.id::text,COALESCE(p.project_id::text,'installation'),p.name,p.etag::text,p.active_revision_id::text,p.retired_at,v.id::text,v.revision,v.entries
 FROM olp.guardrail_policies p JOIN olp.guardrail_policy_revisions v ON v.id=p.latest_revision_id WHERE p.id=$1`
	if lock {
		query += " FOR UPDATE OF p"
	}
	var p ManagedPolicy
	err := q.QueryRow(ctx, query, id).Scan(&p.PolicyID, &p.ProjectID, &p.Name, &p.ETag, &p.ActiveRevisionID, &p.RetiredAt, &p.ID, &p.Revision, &p.Entries)
	return &p, err
}
func (s *Server) listPolicies(r *http.Request, p access.Principal) (access.Reply, error) {
	page, err := access.Page(r)
	if err != nil {
		return access.Reply{}, err
	}
	rows, err := s.Access.Pool.Query(r.Context(), `SELECT id::text FROM olp.guardrail_policies WHERE retired_at IS NULL AND id<$1 AND ($2 OR project_id=ANY($3::uuid[])) ORDER BY id DESC LIMIT $4`, page.Before, p.AllProjects, p.ProjectIDs(), page.Limit+1)
	if err != nil {
		return access.Reply{}, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return access.Reply{}, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return access.Reply{}, err
	}
	items := []map[string]any{}
	for _, id := range ids {
		v, err := loadPolicy(r.Context(), s.Access.Pool, id, false)
		if err != nil {
			return access.Reply{}, err
		}
		items = append(items, map[string]any{"id": v.PolicyID, "project_id": v.ProjectID, "name": v.Name, "revision_id": v.ID, "policy_id": v.PolicyID, "revision": v.Revision, "entries": v.Entries, "etag": v.ETag, "active_revision_id": v.ActiveRevisionID})
	}
	return access.ListReply(items, page), nil
}
func (s *Server) getPolicy(r *http.Request, p access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "policy_id")
	if err != nil {
		return access.Reply{}, err
	}
	v, err := loadPolicy(r.Context(), s.Access.Pool, id, false)
	if err != nil {
		return access.Reply{}, err
	}
	if err = p.Project(projectPointer(v.ProjectID), access.View); err != nil {
		return access.Reply{}, err
	}
	return access.Detail(v, v.ETag), nil
}
func (s *Server) policyRevision(r *http.Request, p access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "policy_id")
	if err != nil {
		return access.Reply{}, err
	}
	v, err := loadPolicy(r.Context(), s.Access.Pool, id, false)
	if err != nil {
		return access.Reply{}, err
	}
	if err = p.Project(projectPointer(v.ProjectID), access.View); err != nil {
		return access.Reply{}, err
	}
	rid, err := access.IDParam(r, "revision_id")
	if err != nil {
		return access.Reply{}, err
	}
	err = s.Access.Pool.QueryRow(r.Context(), "SELECT id::text,revision,entries FROM olp.guardrail_policy_revisions WHERE id=$1 AND policy_id=$2", rid, id).Scan(&v.ID, &v.Revision, &v.Entries)
	return access.Detail(v, rid), err
}
func (s *Server) createPolicy(r *http.Request, _ access.Principal) (access.Reply, error) {
	return s.writePolicy(r, "", false)
}
func (s *Server) updatePolicy(r *http.Request, _ access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "policy_id")
	if err != nil {
		return access.Reply{}, err
	}
	return s.writePolicy(r, id, false)
}
func (s *Server) retirePolicy(r *http.Request, _ access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "policy_id")
	if err != nil {
		return access.Reply{}, err
	}
	return s.writePolicy(r, id, true)
}
func (s *Server) writePolicy(r *http.Request, id string, retire bool) (access.Reply, error) {
	var in policyInput
	if !retire {
		if err := access.DecodeUnique(r, &in, 128<<10); err != nil {
			return access.Reply{}, err
		}
		in.Name = strings.TrimSpace(in.Name)
		if in.Name == "" || len(in.Name) > 100 || len(in.Entries) > MaxEntries {
			return access.Reply{}, access.Invalid("policy", "Supply a name and at most 128 entries.")
		}
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
	var current *ManagedPolicy
	if id != "" {
		current, err = loadPolicy(r.Context(), tx, id, true)
		if err != nil {
			return access.Reply{}, err
		}
		if err = p.Project(projectPointer(current.ProjectID), access.Change); err != nil {
			return access.Reply{}, err
		}
	} else {
		if in.ProjectID == "" {
			return access.Reply{}, access.Invalid("project_id", "Choose a project.")
		}
		if err = s.Access.RequireProject(r.Context(), tx, p, &in.ProjectID); err != nil {
			return access.Reply{}, err
		}
	}
	claim, replayed, err := s.Access.Replay(r, tx, p, in)
	if err != nil {
		return access.Reply{}, err
	}
	if replayed != nil {
		return access.Commit(r, tx, *replayed)
	}
	if current != nil {
		if err = access.Match(r, current.ETag); err != nil {
			return access.Reply{}, err
		}
		if current.RetiredAt != nil {
			return access.Reply{}, access.Fail(409, "guardrail_policy_retired", "Create a new policy.")
		}
		if in.ProjectID != "" && in.ProjectID != current.ProjectID {
			return access.Reply{}, access.Invalid("project_id", "The owning project is immutable.")
		}
		in.ProjectID = current.ProjectID
	}
	if retire {
		_, err = tx.Exec(r.Context(), "UPDATE olp.guardrail_policies SET retired_at=now(),etag=$2 WHERE id=$1", id, access.NewID())
	} else {
		definitions := map[string]Revision{}
		for _, e := range in.Entries {
			var d Revision
			err = tx.QueryRow(r.Context(), `SELECT v.id::text,g.id::text,g.project_id::text,g.name,g.type,v.configuration,v.timeout_ms,v.on_error FROM olp.guardrail_revisions v JOIN olp.guardrails g ON g.id=v.guardrail_id WHERE v.id=$1 AND g.project_id=$2 AND g.retired_at IS NULL`, e.RevisionID, in.ProjectID).Scan(&d.ID, &d.GuardrailID, &d.ProjectID, &d.Name, &d.Type, &d.Configuration, &d.TimeoutMS, &d.OnError)
			if err != nil {
				return access.Reply{}, access.Invalid("entries", "Choose existing guardrail revisions in this project.")
			}
			definitions[d.ID] = d
		}
		if _, err = Compile(in.Entries, definitions, nil); err != nil {
			return access.Reply{}, access.Invalid("entries", "Supply valid phases, actions and pinned revisions.")
		}
		rid, etag, version := access.NewID(), access.NewID(), int64(1)
		if current == nil {
			id = access.NewID()
			_, err = tx.Exec(r.Context(), "INSERT INTO olp.guardrail_policies(id,project_id,name,latest_revision_id,etag,created_by) VALUES($1,$2,$3,$4,$5,$6)", id, in.ProjectID, in.Name, rid, etag, p.UserID())
		} else {
			version = current.Revision + 1
			_, err = tx.Exec(r.Context(), "UPDATE olp.guardrail_policies SET name=$2,latest_revision_id=$3,etag=$4 WHERE id=$1", id, in.Name, rid, etag)
		}
		if err == nil {
			raw, _ := json.Marshal(in.Entries)
			_, err = tx.Exec(r.Context(), "INSERT INTO olp.guardrail_policy_revisions(id,policy_id,revision,entries,created_by) VALUES($1,$2,$3,$4,$5)", rid, id, version, raw, p.UserID())
		}
	}
	if err != nil {
		return access.Reply{}, err
	}
	action := "guardrail_policy.update"
	if current == nil {
		action = "guardrail_policy.create"
	}
	if retire {
		action = "guardrail_policy.retire"
	}
	if err = access.Audit(r.Context(), tx, r, p.Actor(), action, "guardrail_policy", id, "success"); err != nil {
		return access.Reply{}, err
	}
	reply := access.Reply{Status: 204}
	if !retire {
		v, err := loadPolicy(r.Context(), tx, id, false)
		if err != nil {
			return access.Reply{}, err
		}
		reply = access.Detail(v, v.ETag)
		if current == nil {
			reply.Status = 201
			reply.Location = "/api/v1/guardrail-policies/" + id
		}
	}
	if err = s.Access.CompleteReplay(r, tx, claim, reply); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, reply)
}
func (s *Server) activateDefinition(r *http.Request, _ access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "guardrail_id")
	if err != nil {
		return access.Reply{}, err
	}
	return s.activate(r, id, false)
}
func (s *Server) activatePolicy(r *http.Request, _ access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "policy_id")
	if err != nil {
		return access.Reply{}, err
	}
	return s.activate(r, id, true)
}
func (s *Server) activate(r *http.Request, id string, policy bool) (access.Reply, error) {
	var in struct {
		RevisionID string `json:"revision_id"`
	}
	if err := access.DecodeUnique(r, &in, 65536); err != nil {
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
	table, revisionTable, owner := "guardrails", "guardrail_revisions", "guardrail_id"
	if policy {
		table, revisionTable, owner = "guardrail_policies", "guardrail_policy_revisions", "policy_id"
	}
	var project, etag string
	var retired *time.Time
	err = tx.QueryRow(r.Context(), "SELECT COALESCE(project_id::text,'installation'),etag::text,retired_at FROM olp."+table+" WHERE id=$1 FOR UPDATE", id).Scan(&project, &etag, &retired)
	if err != nil {
		return access.Reply{}, err
	}
	if err = p.Project(projectPointer(project), access.Change); err != nil {
		return access.Reply{}, err
	}
	claim, replayed, err := s.Access.Replay(r, tx, p, in)
	if err != nil {
		return access.Reply{}, err
	}
	if replayed != nil {
		return access.Commit(r, tx, *replayed)
	}
	if err = access.Match(r, etag); err != nil {
		return access.Reply{}, err
	}
	if retired != nil {
		return access.Reply{}, access.Fail(409, "guardrail_retired", "A retired resource cannot be activated.")
	}
	var exists bool
	err = tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM olp."+revisionTable+" WHERE id=$1 AND "+owner+"=$2)", in.RevisionID, id).Scan(&exists)
	if err != nil {
		return access.Reply{}, err
	}
	if !exists {
		return access.Reply{}, access.Invalid("revision_id", "Choose a revision owned by this resource.")
	}
	activationTable := "guardrail_revision_activations"
	if policy {
		activationTable = "guardrail_policy_revision_activations"
	}
	if _, err = tx.Exec(r.Context(), "INSERT INTO olp."+activationTable+"(revision_id,activated_by) VALUES($1,$2) ON CONFLICT DO NOTHING", in.RevisionID, p.UserID()); err != nil {
		return access.Reply{}, err
	}
	_, err = tx.Exec(r.Context(), "UPDATE olp."+table+" SET active_revision_id=$2,etag=$3 WHERE id=$1", id, in.RevisionID, access.NewID())
	if err != nil {
		return access.Reply{}, err
	}
	if err = s.publish(r.Context(), tx, p.UserID()); err != nil {
		return access.Reply{}, access.Invalid("revision_id", "This activation is incompatible with a published route or attachment.")
	}
	if err = access.Audit(r.Context(), tx, r, p.Actor(), "guardrail.activate", table, id, "success"); err != nil {
		return access.Reply{}, err
	}
	reply := access.Reply{Status: 204}
	if err = s.Access.CompleteReplay(r, tx, claim, reply); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, reply)
}

// AuthorizeAttachment preserves the authority of the resource being changed.
// A configure token alone cannot change key, organization or installation policy.
func AuthorizeAttachment(ctx context.Context, q access.Queryer, p access.Principal, a Attachment, need access.Need) error {
	if need == access.Change {
		op := access.Configure
		switch a.Scope {
		case "installation":
			op = access.Settings
		case "organization":
			op = access.ManageOrganization
		case "api-key":
			op = access.Keys
		}
		if err := p.Authorize(op); err != nil {
			return err
		}
	}
	switch a.Scope {
	case "installation":
		if !p.AllProjects {
			return access.Forbidden()
		}
		return p.Project(nil, need)
	case "organization":
		return p.Organization(a.ScopeID, need)
	case "project":
		return p.Project(&a.ScopeID, need)
	case "route", "api-key":
		table := "routes"
		if a.Scope == "api-key" {
			table = "api_keys"
		}
		var project *string
		if err := q.QueryRow(ctx, "SELECT project_id::text FROM olp."+table+" WHERE id=$1", a.ScopeID).Scan(&project); err != nil {
			return err
		}
		return p.Project(project, need)
	default:
		return access.Invalid("scope", "Choose installation, organization, project, route or api-key.")
	}
}
func (s *Server) listAttachments(r *http.Request, p access.Principal) (access.Reply, error) {
	rows, err := s.Access.Pool.Query(r.Context(), "SELECT id::text,scope,COALESCE(scope_id::text,'installation'),policy_revision_id::text,position,etag::text FROM olp.guardrail_attachments ORDER BY position,id LIMIT 10001")
	if err != nil {
		return access.Reply{}, err
	}
	type item struct {
		Attachment
		ETag string `json:"etag"`
	}
	values := []item{}
	for rows.Next() {
		var a item
		if err = rows.Scan(&a.ID, &a.Scope, &a.ScopeID, &a.PolicyRevisionID, &a.Order, &a.ETag); err != nil {
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
		if AuthorizeAttachment(r.Context(), s.Access.Pool, p, a.Attachment, access.View) == nil || a.Scope == "installation" && len(p.ProjectIDs()) > 0 {
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
		switch in.Scope {
		case "installation", "organization", "project", "route", "api-key":
		default:
			return access.Reply{}, access.Invalid("scope", "Choose installation, organization, project, route or api-key.")
		}
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
		return access.Commit(r, tx, *replayed)
	}
	if remove {
		if err = access.Match(r, etag); err != nil {
			return access.Reply{}, err
		}
		_, err = tx.Exec(r.Context(), "DELETE FROM olp.guardrail_attachments WHERE id=$1", in.ID)
	} else {
		var project string
		err = tx.QueryRow(r.Context(), "SELECT p.project_id::text FROM olp.guardrail_policies p JOIN olp.guardrail_policy_revisions v ON v.policy_id=p.id WHERE v.id=$1 AND p.retired_at IS NULL", in.PolicyRevisionID).Scan(&project)
		if err != nil {
			return access.Reply{}, access.Invalid("policy_revision_id", "Choose an existing policy revision.")
		}
		if err = p.Project(&project, access.Change); err != nil {
			return access.Reply{}, err
		}
		if in.Scope == "project" && project != in.ScopeID {
			return access.Reply{}, access.Invalid("scope_id", "The policy must belong to this project.")
		}
		if in.Scope == "route" || in.Scope == "api-key" {
			table := "routes"
			if in.Scope == "api-key" {
				table = "api_keys"
			}
			var owning string
			if err = tx.QueryRow(r.Context(), "SELECT project_id::text FROM olp."+table+" WHERE id=$1", in.ScopeID).Scan(&owning); err != nil || owning != project {
				return access.Reply{}, access.Invalid("scope_id", "The policy must belong to this project.")
			}
			if in.Scope == "route" && in.Order == 0 {
				var legacy bool
				if err = tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM olp.guardrail_policies p JOIN olp.routes r ON r.latest_revision_id=p.managed_route_revision_id WHERE r.id=$1)", in.ScopeID).Scan(&legacy); err != nil {
					return access.Reply{}, err
				}
				if legacy {
					return access.Reply{}, access.Invalid("order", "Position zero preserves this route's migrated content policy; choose a later position.")
				}
			}
		}
		in.ID = access.NewID()
		_, err = tx.Exec(r.Context(), "INSERT INTO olp.guardrail_attachments(id,scope,scope_id,policy_revision_id,position,etag,created_by) VALUES($1,$2,$3,$4,$5,$6,$7)", in.ID, in.Scope, projectPointer(in.ScopeID), in.PolicyRevisionID, in.Order, access.NewID(), p.UserID())
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
	return access.Commit(r, tx, reply)
}
func (s *Server) evaluate(r *http.Request, p access.Principal) (access.Reply, error) {
	var in ApplyInput
	if err := access.DecodeUnique(r, &in, MaxContentBytes); err != nil {
		return access.Reply{}, err
	}
	if !validPhase(in.Phase) {
		return access.Reply{}, access.Invalid("phase", "Choose a supported evaluation phase.")
	}
	if err := validateSegments(in.Segments); err != nil {
		return access.Reply{}, access.Invalid("segments", "Supply bounded UTF-8 segments with unique identifiers.")
	}
	var project string
	if err := s.Access.Pool.QueryRow(r.Context(), "SELECT COALESCE(p.project_id::text,'installation') FROM olp.guardrail_policy_revisions v JOIN olp.guardrail_policies p ON p.id=v.policy_id WHERE v.id=$1", in.PolicyRevisionID).Scan(&project); err != nil {
		return access.Reply{}, err
	}
	if err := p.Project(projectPointer(project), access.View); err != nil {
		return access.Reply{}, err
	}
	var selected PolicyRevision
	selected.ID = in.PolicyRevisionID
	selected.ProjectID = project
	if err := s.Access.Pool.QueryRow(r.Context(), "SELECT entries FROM olp.guardrail_policy_revisions WHERE id=$1", in.PolicyRevisionID).Scan(&selected.Entries); err != nil {
		return access.Reply{}, err
	}
	in.Policy = &selected
	in.Definitions = map[string]Revision{}
	for _, entry := range selected.Entries {
		var d Revision
		err := s.Access.Pool.QueryRow(r.Context(), "SELECT v.id::text,g.id::text,COALESCE(g.project_id::text,'installation'),g.name,g.type,v.configuration,v.timeout_ms,v.on_error FROM olp.guardrail_revisions v JOIN olp.guardrails g ON g.id=v.guardrail_id WHERE v.id=$1", entry.RevisionID).Scan(&d.ID, &d.GuardrailID, &d.ProjectID, &d.Name, &d.Type, &d.Configuration, &d.TimeoutMS, &d.OnError)
		if err != nil || d.ProjectID != project {
			return access.Reply{}, access.Invalid("policy", "Policy references must stay in their project.")
		}
		in.Definitions[d.ID] = d
	}
	if s.Evaluate == nil {
		return access.Reply{}, access.Fail(503, "guardrails_unavailable", "The evaluator is unavailable.")
	}
	result, err := s.Evaluate(r, p, in)
	if err != nil {
		return access.Reply{}, err
	}
	return access.Reply{Status: 200, Body: result}, nil
}
func (s *Server) createCredential(r *http.Request, _ access.Principal) (access.Reply, error) {
	var in struct {
		ProjectID  string          `json:"project_id"`
		Name       string          `json:"name"`
		Credential json.RawMessage `json:"credential"`
	}
	if err := access.DecodeUnique(r, &in, 256<<10); err != nil {
		return access.Reply{}, err
	}
	if ValidateCredential(in.Credential) != nil || in.Name == "" || len(in.Name) > 100 {
		return access.Reply{}, access.Invalid("credential", "Supply a named credential object.")
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
	if err = s.Access.RequireProject(r.Context(), tx, p, &in.ProjectID); err != nil {
		return access.Reply{}, err
	}
	claim, replayed, err := s.Access.Replay(r, tx, p, in)
	if err != nil {
		return access.Reply{}, err
	}
	if replayed != nil {
		return access.Commit(r, tx, *replayed)
	}
	id := access.NewID()
	if err = s.Access.Keys.Store(r.Context(), tx, s.Access.Installation, id, secrets.GuardrailCredential, in.Credential, nil); err != nil {
		return access.Reply{}, err
	}
	_, err = tx.Exec(r.Context(), "INSERT INTO olp.guardrail_credentials(id,project_id,name,created_by) VALUES($1,$2,$3,$4)", id, in.ProjectID, in.Name, p.UserID())
	if err != nil {
		return access.Reply{}, err
	}
	if err = access.Audit(r.Context(), tx, r, p.Actor(), "guardrail.credential.create", "guardrail_credential", id, "success"); err != nil {
		return access.Reply{}, err
	}
	reply := access.Reply{Status: 201, Body: map[string]string{"id": id, "name": in.Name, "project_id": in.ProjectID}}
	if err = s.Access.CompleteReplay(r, tx, claim, reply); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, reply)
}

func (p ManagedPolicy) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]any{"id": p.PolicyID, "policy_id": p.PolicyID, "revision_id": p.ID, "project_id": p.ProjectID, "name": p.Name, "revision": p.Revision, "entries": p.Entries, "etag": p.ETag, "active_revision_id": p.ActiveRevisionID, "retired_at": p.RetiredAt})
}
