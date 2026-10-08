package access

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const projectFields = `'organization_id',p.organization_id,'id',p.id,'name',p.name,'etag',p.etag,'created_by',p.created_by,'created_by_email',u.email,'created_at',p.created_at,'updated_at',p.updated_at,'member_count',(SELECT count(*) FROM olp.project_members m WHERE m.project_id=p.id)`
const projectFrom = " FROM olp.projects p JOIN olp.users u ON u.id=p.created_by"

func (s *Server) projects(r *http.Request, principal Principal) (Reply, error) {
	if err := organizationProject(r, s.Pool, principal, View); err != nil {
		return Reply{}, err
	}
	p, err := Page(r)
	if err != nil {
		return Reply{}, err
	}
	rows, err := s.Pool.Query(r.Context(), "SELECT jsonb_build_object("+projectFields+")"+projectFrom+" WHERE p.id<$1 AND ($3::uuid IS NULL OR p.organization_id=$3) ORDER BY p.id DESC LIMIT $2", p.Before, p.Limit+1, nullableOrganizationPath(r))
	if err != nil {
		return Reply{}, err
	}
	items, err := JSONRows(rows)
	return ListReply(items, p), err
}

func (s *Server) project(r *http.Request, principal Principal) (Reply, error) {
	id, err := IDParam(r, "project_id")
	if err != nil {
		return Reply{}, err
	}
	if err = organizationProject(r, s.Pool, principal, View); err != nil {
		return Reply{}, err
	}
	var data []byte
	var etag string
	err = s.Pool.QueryRow(r.Context(), "SELECT jsonb_build_object("+projectFields+"),p.etag::text"+projectFrom+" WHERE p.id=$1", id).Scan(&data, &etag)
	return Detail(json.RawMessage(data), etag), err
}

func duplicateName(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505" && pg.ConstraintName == "projects_name"
}

func (s *Server) createProject(r *http.Request, _ Principal) (Reply, error) {
	var input struct {
		Name string `json:"name"`
	}
	if err := Decode(r, &input); err != nil {
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
	if err = organizationProject(r, tx, p, Change); err != nil {
		return Reply{}, err
	}
	claim, replayed, err := s.Replay(r, tx, p, input)
	if err != nil {
		return Reply{}, err
	}
	if replayed != nil {
		return Commit(r, tx, *replayed)
	}
	input.Name = strings.TrimSpace(input.Name)
	if err = ValidText("name", input.Name, 100); err != nil {
		return Reply{}, err
	}
	id, etag, err := createProjectInOrganization(r.Context(), tx, input.Name, p.UserID(), nullableOrganizationPath(r))
	if err != nil {
		return Reply{}, err
	}
	if err = Audit(r.Context(), tx, r, p.Actor(), "project.create", "project", id, "success"); err != nil {
		return Reply{}, err
	}
	var data []byte
	if err = tx.QueryRow(r.Context(), "SELECT jsonb_build_object("+projectFields+")"+projectFrom+" WHERE p.id=$1", id).Scan(&data); err != nil {
		return Reply{}, err
	}
	result := Reply{Status: 201, Location: "/api/v1/projects/" + id, ETag: etag, Body: json.RawMessage(data)}
	if err = s.CompleteReplay(r, tx, claim, result); err != nil {
		return Reply{}, err
	}
	return Commit(r, tx, result)
}

func loadProject(r *http.Request, tx pgx.Tx, id string) (string, error) {
	var etag string
	err := tx.QueryRow(r.Context(), "SELECT etag::text FROM olp.projects WHERE id=$1 FOR UPDATE", id).Scan(&etag)
	return etag, err
}

func (s *Server) updateProject(r *http.Request, _ Principal) (Reply, error) {
	var input struct {
		Name string `json:"name"`
	}
	if err := Decode(r, &input); err != nil {
		return Reply{}, err
	}
	id, err := IDParam(r, "project_id")
	if err != nil {
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
	if err = organizationProject(r, tx, p, Change); err != nil {
		return Reply{}, err
	}
	etag, err := loadProject(r, tx, id)
	if err != nil {
		return Reply{}, err
	}
	if err = Match(r, etag); err != nil {
		return Reply{}, err
	}
	input.Name = strings.TrimSpace(input.Name)
	if err = ValidText("name", input.Name, 100); err != nil {
		return Reply{}, err
	}
	etag = NewID()
	if _, err = tx.Exec(r.Context(), "UPDATE olp.projects SET name=$2,etag=$3,updated_at=now() WHERE id=$1", id, input.Name, etag); duplicateName(err) {
		return Reply{}, Fail(409, "project_name_taken", "A project with this name already exists.")
	} else if err != nil {
		return Reply{}, err
	}
	if err = Audit(r.Context(), tx, r, p.Actor(), "project.update", "project", id, "success"); err != nil {
		return Reply{}, err
	}
	var data []byte
	if err = tx.QueryRow(r.Context(), "SELECT jsonb_build_object("+projectFields+")"+projectFrom+" WHERE p.id=$1", id).Scan(&data); err != nil {
		return Reply{}, err
	}
	return Commit(r, tx, Detail(json.RawMessage(data), etag))
}

const projectMemberFields = `'user_id',m.user_id,'project_role',m.role,'email',u.email,'display_name',u.display_name,'role',u.role,'active',u.active,'added_by',m.added_by,'added_by_email',a.email,'created_at',m.created_at`
const projectMemberFrom = " FROM olp.project_members m JOIN olp.users u ON u.id=m.user_id JOIN olp.users a ON a.id=m.added_by"

func (s *Server) projectMembers(r *http.Request, principal Principal) (Reply, error) {
	id, err := IDParam(r, "project_id")
	if err != nil {
		return Reply{}, err
	}
	if err = organizationProject(r, s.Pool, principal, View); err != nil {
		return Reply{}, err
	}
	var exists bool
	if err = s.Pool.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM olp.projects WHERE id=$1)", id).Scan(&exists); err != nil {
		return Reply{}, err
	}
	if !exists {
		return Reply{}, pgx.ErrNoRows
	}
	p, err := Page(r)
	if err != nil {
		return Reply{}, err
	}
	rows, err := s.Pool.Query(r.Context(), "SELECT jsonb_build_object("+projectMemberFields+")"+projectMemberFrom+" WHERE m.project_id=$1 AND m.user_id<$2 ORDER BY m.user_id DESC LIMIT $3", id, p.Before, p.Limit+1)
	if err != nil {
		return Reply{}, err
	}
	items, err := JSONRows(rows)
	if err != nil {
		return Reply{}, err
	}
	return ListReplyBy(items, p, func(item map[string]any) string { return item["user_id"].(string) }), nil
}

func (s *Server) projectMemberships(r *http.Request, p Principal) (Reply, error) {
	// The principal already carries its project roles, whether a member's
	// own or those a management token inherits from its creator.
	rows, err := s.Pool.Query(r.Context(), "SELECT id::text,name FROM olp.projects WHERE $1 OR id=ANY($2::uuid[]) ORDER BY lower(name),id", p.AllProjects, p.ProjectIDs())
	if err != nil {
		return Reply{}, err
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, name string
		if err = rows.Scan(&id, &name); err != nil {
			return Reply{}, err
		}
		role := "manager"
		if !p.AllProjects {
			role = p.Projects[id]
		}
		items = append(items, map[string]any{"id": id, "name": name, "role": role})
	}
	return OK(map[string]any{"items": items}), rows.Err()
}

func (s *Server) putProjectMember(r *http.Request, _ Principal) (Reply, error) {
	return s.writeProjectMember(r, false)
}

func (s *Server) deleteProjectMember(r *http.Request, _ Principal) (Reply, error) {
	return s.writeProjectMember(r, true)
}

func (s *Server) writeProjectMember(r *http.Request, remove bool) (Reply, error) {
	var input struct {
		Role string `json:"role"`
	}
	if !remove {
		if err := Decode(r, &input); err != nil {
			return Reply{}, err
		}
		if input.Role != "manager" && input.Role != "viewer" {
			return Reply{}, Invalid("role", "Use manager or viewer.")
		}
	}
	projectID, err := IDParam(r, "project_id")
	if err != nil {
		return Reply{}, err
	}
	userID, err := IDParam(r, "user_id")
	if err != nil {
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
	if err = organizationProject(r, tx, p, Change); err != nil {
		return Reply{}, err
	}
	etag, err := loadProject(r, tx, projectID)
	if err != nil {
		return Reply{}, err
	}
	if err = Match(r, etag); err != nil {
		return Reply{}, err
	}
	var exists bool
	if err = tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM olp.users WHERE id=$1)", userID).Scan(&exists); err != nil {
		return Reply{}, err
	}
	if !exists {
		return Reply{}, pgx.ErrNoRows
	}
	if remove {
		var memberRole *string
		if err = tx.QueryRow(r.Context(), "DELETE FROM olp.project_members WHERE project_id=$1 AND user_id=$2 RETURNING role", projectID, userID).Scan(&memberRole); errors.Is(err, pgx.ErrNoRows) {
			return Reply{}, pgx.ErrNoRows
		}
		if err != nil {
			return Reply{}, err
		}

	} else {

		if _, err = tx.Exec(r.Context(), "INSERT INTO olp.project_members(project_id,user_id,role,added_by) VALUES($1,$2,$3,$4) ON CONFLICT(project_id,user_id) DO UPDATE SET role=excluded.role", projectID, userID, input.Role, p.UserID()); err != nil {
			return Reply{}, err
		}
	}
	var hasManager bool
	if err = tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM olp.effective_project_members WHERE project_id=$1 AND role='manager')", projectID).Scan(&hasManager); err != nil {
		return Reply{}, err
	}
	if !hasManager {
		return Reply{}, Fail(409, "last_project_manager", "Keep at least one project manager.")
	}
	if _, err = AdvanceAuthority(r.Context(), tx); err != nil {
		return Reply{}, err
	}
	if err = s.usableOwner(r, tx); err != nil {
		return Reply{}, err
	}
	etag = NewID()
	if _, err = tx.Exec(r.Context(), "UPDATE olp.projects SET etag=$2,updated_at=now() WHERE id=$1", projectID, etag); err != nil {
		return Reply{}, err
	}
	action := "project.member.assign"
	if remove {
		action = "project.member.remove"
	}
	if err = Audit(r.Context(), tx, r, p.Actor(), action, "project", projectID, "success"); err != nil {
		return Reply{}, err
	}
	if remove {
		return Commit(r, tx, Reply{Status: 204})
	}
	var data []byte
	if err = tx.QueryRow(r.Context(), "SELECT jsonb_build_object("+projectMemberFields+")"+projectMemberFrom+" WHERE m.project_id=$1 AND m.user_id=$2", projectID, userID).Scan(&data); err != nil {
		return Reply{}, err
	}
	return Commit(r, tx, Detail(json.RawMessage(data), etag))
}
