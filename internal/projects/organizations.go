package projects

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/domain/ids"
	"github.com/tyk-swe/olp/internal/platform/database"
)

const organizationFields = `'id',o.id,'name',o.name,'etag',o.etag,'created_by',o.created_by,'created_at',o.created_at,'updated_at',o.updated_at,'project_count',(SELECT count(*) FROM olp.projects p WHERE p.organization_id=o.id)`

func (s *Server) organizations(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	page, err := httpkit.Page(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	items, err := httpkit.QueryJSON(r.Context(), s.access.Pool, "SELECT jsonb_build_object("+organizationFields+") FROM olp.organizations o WHERE o.id<$1 AND ($2 OR o.id=ANY($3::uuid[])) ORDER BY o.id DESC LIMIT $4", page.Before, p.AllProjects, p.OrganizationIDs(), page.Limit+1)
	return httpkit.ListReply(items, page), err
}

func (s *Server) organization(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "organization_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	if err = p.Organization(id, authz.View); err != nil {
		return httpkit.Reply{}, err
	}
	var data json.RawMessage
	var etag string
	err = s.access.Pool.QueryRow(r.Context(), "SELECT jsonb_build_object("+organizationFields+"),o.etag::text FROM olp.organizations o WHERE id=$1", id).Scan(&data, &etag)
	return httpkit.Detail(data, etag), err
}

func CreateOrganization(ctx context.Context, tx pgx.Tx, name, creator string) (string, string, error) {
	id, etag := ids.New(), ids.New()
	_, err := tx.Exec(ctx, "INSERT INTO olp.organizations(id,name,etag,created_by) VALUES($1,$2,$3,$4)", id, name, etag, creator)
	if err != nil {
		return "", "", err
	}
	_, err = tx.Exec(ctx, "INSERT INTO olp.organization_members(organization_id,user_id,role,added_by) VALUES($1,$2,'manager',$3)", id, creator, creator)
	return id, etag, err
}

func (s *Server) createOrganization(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	var in struct {
		Name string `json:"name"`
	}
	if err := httpkit.Decode(r, &in); err != nil {
		return httpkit.Reply{}, err
	}
	in.Name = strings.TrimSpace(in.Name)
	if err := httpkit.ValidText("name", in.Name, 100); err != nil {
		return httpkit.Reply{}, err
	}
	return access.Mutate(s.access, r, in, access.Unlocked, func(tx pgx.Tx, p authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		id, etag, err := CreateOrganization(r.Context(), tx, in.Name, p.UserID())
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		var data json.RawMessage
		if err = tx.QueryRow(r.Context(), "SELECT jsonb_build_object("+organizationFields+") FROM olp.organizations o WHERE id=$1", id).Scan(&data); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		reply := httpkit.Reply{Status: 201, Body: data, ETag: etag, Location: "/api/v1/organizations/" + id}
		return reply, access.Event{Action: "organization.create", Resource: "organization", ID: id}, nil
	})
}

func (s *Server) updateOrganization(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "organization_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	var in struct {
		Name string `json:"name"`
	}
	if err = httpkit.Decode(r, &in); err != nil {
		return httpkit.Reply{}, err
	}
	in.Name = strings.TrimSpace(in.Name)
	if err = httpkit.ValidText("name", in.Name, 100); err != nil {
		return httpkit.Reply{}, err
	}
	return access.Transact(s.access, r, access.Unlocked, func(tx pgx.Tx, p authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		if err = p.Organization(id, authz.Change); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		var etag string
		if err = tx.QueryRow(r.Context(), "SELECT etag::text FROM olp.organizations WHERE id=$1 FOR UPDATE", id).Scan(&etag); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if err = httpkit.Match(r, etag); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		etag = ids.New()
		if _, err = tx.Exec(r.Context(), "UPDATE olp.organizations SET name=$2,etag=$3,updated_at=now() WHERE id=$1", id, in.Name, etag); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		var data json.RawMessage
		if err = tx.QueryRow(r.Context(), "SELECT jsonb_build_object("+organizationFields+") FROM olp.organizations o WHERE id=$1", id).Scan(&data); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		return httpkit.Detail(data, etag), access.Event{Action: "organization.update", Resource: "organization", ID: id}, nil
	})
}

func (s *Server) organizationMembers(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "organization_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	if err = p.Organization(id, authz.View); err != nil {
		return httpkit.Reply{}, err
	}
	page, err := httpkit.Page(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	items, err := httpkit.QueryJSON(r.Context(), s.access.Pool, `SELECT jsonb_build_object('user_id',u.id,'email',u.email,'display_name',u.display_name,'role',m.role,'installation_role',u.role,'active',u.active) FROM olp.organization_members m JOIN olp.users u ON u.id=m.user_id WHERE m.organization_id=$1 AND m.user_id<$2 ORDER BY m.user_id DESC LIMIT $3`, id, page.Before, page.Limit+1)
	if err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.ListReplyBy(items, page, func(item map[string]any) string { return item["user_id"].(string) }), nil
}

func (s *Server) putOrganizationMember(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	return s.writeOrganizationMember(r, false)
}

func (s *Server) deleteOrganizationMember(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	return s.writeOrganizationMember(r, true)
}

func (s *Server) writeOrganizationMember(r *http.Request, remove bool) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "organization_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	user, err := httpkit.IDParam(r, "user_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	var in struct {
		Role string `json:"role"`
	}
	if !remove {
		if err = httpkit.Decode(r, &in); err != nil {
			return httpkit.Reply{}, err
		}
		if in.Role != "manager" && in.Role != "viewer" {
			return httpkit.Reply{}, httpkit.Invalid("role", "Use manager or viewer.")
		}
	}
	return access.Transact(s.access, r, access.Unlocked, func(tx pgx.Tx, p authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		if err = p.Organization(id, authz.Change); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		var etag string
		if err = tx.QueryRow(r.Context(), "SELECT etag::text FROM olp.organizations WHERE id=$1 FOR UPDATE", id).Scan(&etag); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if err = httpkit.Match(r, etag); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if remove {
			tag, e := tx.Exec(r.Context(), "DELETE FROM olp.organization_members WHERE organization_id=$1 AND user_id=$2", id, user)
			if e != nil {
				return httpkit.Reply{}, access.Event{}, e
			}
			if tag.RowsAffected() == 0 {
				return httpkit.Reply{}, access.Event{}, httpkit.Fail(404, "not_found", "The resource was not found.")
			}
		} else {
			var exists bool
			if err = tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM olp.users WHERE id=$1)", user).Scan(&exists); err != nil {
				return httpkit.Reply{}, access.Event{}, err
			}
			if !exists {
				return httpkit.Reply{}, access.Event{}, httpkit.Fail(404, "not_found", "The resource was not found.")
			}
			if _, err = tx.Exec(r.Context(), `INSERT INTO olp.organization_members(organization_id,user_id,role,added_by) VALUES($1,$2,$3,$4) ON CONFLICT(organization_id,user_id) DO UPDATE SET role=excluded.role`, id, user, in.Role, p.UserID()); err != nil {
				return httpkit.Reply{}, access.Event{}, err
			}
		}
		var managers bool
		if err = tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM olp.organization_members WHERE organization_id=$1 AND role='manager')", id).Scan(&managers); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if !managers {
			return httpkit.Reply{}, access.Event{}, httpkit.Fail(409, "last_organization_manager", "Keep at least one organization manager.")
		}
		etag = ids.New()
		if _, err = tx.Exec(r.Context(), "UPDATE olp.organizations SET etag=$2,updated_at=now() WHERE id=$1", id, etag); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if _, err = access.AdvanceAuthority(r.Context(), tx); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		action := "organization.member.assign"
		if remove {
			action = "organization.member.remove"
		}
		return httpkit.Reply{Status: 204, ETag: etag}, access.Event{Action: action, Resource: "organization", ID: id}, nil
	})
}

func organizationProject(r *http.Request, q database.Queryer, p authz.Principal, need authz.Need) error {
	raw := r.PathValue("organization_id")
	if raw == "" {
		return nil
	}
	id, err := httpkit.ParseUUID(raw)
	if err != nil {
		return err
	}
	if err = p.Organization(id, need); err != nil {
		return err
	}
	project := r.PathValue("project_id")
	var matches bool
	if err := q.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM olp.organizations o WHERE o.id=$1 AND
		(NULLIF($2,'')::uuid IS NULL OR EXISTS(SELECT 1 FROM olp.projects p WHERE p.id=NULLIF($2,'')::uuid AND p.organization_id=o.id)))`, id, project).Scan(&matches); err != nil {
		return err
	}
	if !matches {
		return httpkit.NotFound
	}
	return nil
}
