package access

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Organization membership widens project scope, never installation roles or
// token scopes. Explicitly project-scoped tokens cannot administer an organization.
func (p Principal) Organization(id string, need Need) error {
	if p.AllProjects && (need == View || p.Authorize(Access) == nil) {
		return nil
	}
	role, ok := p.Organizations[id]
	if !ok {
		return notFound
	}
	if need == Change && role != "manager" {
		return Forbidden()
	}
	return nil
}

func (p Principal) OrganizationIDs() []string {
	ids := make([]string, 0, len(p.Organizations))
	for id := range p.Organizations {
		ids = append(ids, id)
	}
	return ids
}

func (p *Principal) loadOrganizations(ctx context.Context, q Queryer, user string) error {
	rows, err := q.Query(ctx, "SELECT organization_id::text,role FROM olp.organization_members WHERE user_id=$1", user)
	if err != nil {
		return err
	}
	defer rows.Close()
	p.Organizations = map[string]string{}
	for rows.Next() {
		var id, role string
		if err = rows.Scan(&id, &role); err != nil {
			return err
		}
		p.Organizations[id] = role
	}
	return rows.Err()
}

const organizationFields = `'id',o.id,'name',o.name,'etag',o.etag,'created_by',o.created_by,'created_at',o.created_at,'updated_at',o.updated_at,'project_count',(SELECT count(*) FROM olp.projects p WHERE p.organization_id=o.id)`

func (s *Server) organizations(r *http.Request, p Principal) (Reply, error) {
	page, err := Page(r)
	if err != nil {
		return Reply{}, err
	}
	rows, err := s.Pool.Query(r.Context(), "SELECT jsonb_build_object("+organizationFields+") FROM olp.organizations o WHERE o.id<$1 AND ($2 OR o.id=ANY($3::uuid[])) ORDER BY o.id DESC LIMIT $4", page.Before, p.AllProjects, p.OrganizationIDs(), page.Limit+1)
	if err != nil {
		return Reply{}, err
	}
	items, err := JSONRows(rows)
	return ListReply(items, page), err
}

func (s *Server) organization(r *http.Request, p Principal) (Reply, error) {
	id, err := IDParam(r, "organization_id")
	if err != nil {
		return Reply{}, err
	}
	if err = p.Organization(id, View); err != nil {
		return Reply{}, err
	}
	var data json.RawMessage
	var etag string
	err = s.Pool.QueryRow(r.Context(), "SELECT jsonb_build_object("+organizationFields+"),o.etag::text FROM olp.organizations o WHERE id=$1", id).Scan(&data, &etag)
	return Detail(data, etag), err
}

func CreateOrganization(ctx context.Context, tx pgx.Tx, name, creator string) (string, string, error) {
	id, etag := NewID(), NewID()
	_, err := tx.Exec(ctx, "INSERT INTO olp.organizations(id,name,etag,created_by) VALUES($1,$2,$3,$4)", id, name, etag, creator)
	if err != nil {
		return "", "", err
	}
	_, err = tx.Exec(ctx, "INSERT INTO olp.organization_members(organization_id,user_id,role,added_by) VALUES($1,$2,'manager',$3)", id, creator, creator)
	return id, etag, err
}

func (s *Server) createOrganization(r *http.Request, _ Principal) (Reply, error) {
	var in struct {
		Name string `json:"name"`
	}
	if err := Decode(r, &in); err != nil {
		return Reply{}, err
	}
	in.Name = strings.TrimSpace(in.Name)
	if err := ValidText("name", in.Name, 100); err != nil {
		return Reply{}, err
	}
	tx, err := s.Begin(r)
	if err != nil {
		return Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.Reauthorize(r, tx)
	if err != nil {
		return Reply{}, err
	}
	claim, replayed, err := s.Replay(r, tx, p, in)
	if err != nil {
		return Reply{}, err
	}
	if replayed != nil {
		return Commit(r, tx, *replayed)
	}
	id, etag, err := CreateOrganization(r.Context(), tx, in.Name, p.UserID())
	if err != nil {
		return Reply{}, err
	}
	if err = Audit(r.Context(), tx, r, p.Actor(), "organization.create", "organization", id, "success"); err != nil {
		return Reply{}, err
	}
	var data json.RawMessage
	if err = tx.QueryRow(r.Context(), "SELECT jsonb_build_object("+organizationFields+") FROM olp.organizations o WHERE id=$1", id).Scan(&data); err != nil {
		return Reply{}, err
	}
	reply := Reply{Status: 201, Body: data, ETag: etag, Location: "/api/v1/organizations/" + id}
	if err = s.CompleteReplay(r, tx, claim, reply); err != nil {
		return Reply{}, err
	}
	return Commit(r, tx, reply)
}

func (s *Server) updateOrganization(r *http.Request, _ Principal) (Reply, error) {
	id, err := IDParam(r, "organization_id")
	if err != nil {
		return Reply{}, err
	}
	var in struct {
		Name string `json:"name"`
	}
	if err = Decode(r, &in); err != nil {
		return Reply{}, err
	}
	in.Name = strings.TrimSpace(in.Name)
	if err = ValidText("name", in.Name, 100); err != nil {
		return Reply{}, err
	}
	tx, err := s.Begin(r)
	if err != nil {
		return Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.Reauthorize(r, tx)
	if err != nil {
		return Reply{}, err
	}
	if err = p.Organization(id, Change); err != nil {
		return Reply{}, err
	}
	var etag string
	if err = tx.QueryRow(r.Context(), "SELECT etag::text FROM olp.organizations WHERE id=$1 FOR UPDATE", id).Scan(&etag); err != nil {
		return Reply{}, err
	}
	if err = Match(r, etag); err != nil {
		return Reply{}, err
	}
	etag = NewID()
	if _, err = tx.Exec(r.Context(), "UPDATE olp.organizations SET name=$2,etag=$3,updated_at=now() WHERE id=$1", id, in.Name, etag); err != nil {
		return Reply{}, err
	}
	if err = Audit(r.Context(), tx, r, p.Actor(), "organization.update", "organization", id, "success"); err != nil {
		return Reply{}, err
	}
	var data json.RawMessage
	if err = tx.QueryRow(r.Context(), "SELECT jsonb_build_object("+organizationFields+") FROM olp.organizations o WHERE id=$1", id).Scan(&data); err != nil {
		return Reply{}, err
	}
	return Commit(r, tx, Detail(data, etag))
}

func (s *Server) organizationMembers(r *http.Request, p Principal) (Reply, error) {
	id, err := IDParam(r, "organization_id")
	if err != nil {
		return Reply{}, err
	}
	if err = p.Organization(id, View); err != nil {
		return Reply{}, err
	}
	rows, err := s.Pool.Query(r.Context(), `SELECT jsonb_build_object('user_id',u.id,'email',u.email,'display_name',u.display_name,'role',m.role,'installation_role',u.role,'active',u.active) FROM olp.organization_members m JOIN olp.users u ON u.id=m.user_id WHERE organization_id=$1 ORDER BY u.id`, id)
	if err != nil {
		return Reply{}, err
	}
	items, err := JSONRows(rows)
	return OK(map[string]any{"items": items}), err
}

func (s *Server) putOrganizationMember(r *http.Request, p Principal) (Reply, error) {
	return s.writeOrganizationMember(r, false)
}

func (s *Server) deleteOrganizationMember(r *http.Request, p Principal) (Reply, error) {
	return s.writeOrganizationMember(r, true)
}

func (s *Server) writeOrganizationMember(r *http.Request, remove bool) (Reply, error) {
	id, err := IDParam(r, "organization_id")
	if err != nil {
		return Reply{}, err
	}
	user, err := IDParam(r, "user_id")
	if err != nil {
		return Reply{}, err
	}
	var in struct {
		Role string `json:"role"`
	}
	if !remove {
		if err = Decode(r, &in); err != nil {
			return Reply{}, err
		}
		if in.Role != "manager" && in.Role != "viewer" {
			return Reply{}, Invalid("role", "Use manager or viewer.")
		}
	}
	tx, err := s.Begin(r)
	if err != nil {
		return Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.Reauthorize(r, tx)
	if err != nil {
		return Reply{}, err
	}
	if err = p.Organization(id, Change); err != nil {
		return Reply{}, err
	}
	var etag string
	if err = tx.QueryRow(r.Context(), "SELECT etag::text FROM olp.organizations WHERE id=$1 FOR UPDATE", id).Scan(&etag); err != nil {
		return Reply{}, err
	}
	if err = Match(r, etag); err != nil {
		return Reply{}, err
	}
	if remove {
		tag, e := tx.Exec(r.Context(), "DELETE FROM olp.organization_members WHERE organization_id=$1 AND user_id=$2", id, user)
		if e != nil {
			return Reply{}, e
		}
		if tag.RowsAffected() == 0 {
			return Reply{}, notFound
		}
	} else {
		if _, err = tx.Exec(r.Context(), `INSERT INTO olp.organization_members(organization_id,user_id,role,added_by) VALUES($1,$2,$3,$4) ON CONFLICT(organization_id,user_id) DO UPDATE SET role=excluded.role`, id, user, in.Role, p.UserID()); err != nil {
			return Reply{}, err
		}
	}
	var managers bool
	if err = tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM olp.organization_members WHERE organization_id=$1 AND role='manager')", id).Scan(&managers); err != nil {
		return Reply{}, err
	}
	if !managers {
		return Reply{}, Fail(409, "last_organization_manager", "Keep at least one organization manager.")
	}
	etag = NewID()
	if _, err = tx.Exec(r.Context(), "UPDATE olp.organizations SET etag=$2,updated_at=now() WHERE id=$1", id, etag); err != nil {
		return Reply{}, err
	}
	if _, err = AdvanceAuthority(r.Context(), tx); err != nil {
		return Reply{}, err
	}
	action := "organization.member.assign"
	if remove {
		action = "organization.member.remove"
	}
	if err = Audit(r.Context(), tx, r, p.Actor(), action, "organization", id, "success"); err != nil {
		return Reply{}, err
	}
	return Commit(r, tx, Reply{Status: 204, ETag: etag})
}

// organizationProject authorizes nested project administration. The write
// handlers call it again after transactional reauthentication.
func organizationProject(r *http.Request, q Queryer, p Principal, need Need) error {
	raw := r.PathValue("organization_id")
	if raw == "" {
		return nil
	}
	id, err := ParseUUID(raw)
	if err != nil {
		return err
	}
	if err = p.Organization(id, need); err != nil {
		return err
	}
	if project := r.PathValue("project_id"); project != "" {
		var matches bool
		if err = q.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM olp.projects WHERE id=$1 AND organization_id=$2)", project, id).Scan(&matches); err != nil {
			return err
		}
		if !matches {
			return notFound
		}
	}
	return nil
}
