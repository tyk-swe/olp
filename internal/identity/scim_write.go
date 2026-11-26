package identity

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strings"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/domain/ids"
	"github.com/tyk-swe/olp/internal/identity/scim"
)

type scimUserExtension struct {
	Role        string `json:"role,omitempty"`
	AccessScope string `json:"accessScope"`
}

type scimStoredGroup struct {
	Schemas     []string           `json:"schemas"`
	DisplayName string             `json:"displayName"`
	ExternalID  string             `json:"externalId,omitempty"`
	Extension   SCIMGroupExtension `json:"urn:openllmproxy:params:scim:schemas:extension:2.0:Group"`
}

type parsedSCIMGroup struct {
	display   string
	external  string
	extension SCIMGroupExtension
	members   []string
}

func (s *scimService) scimWrite(r *http.Request, _ authz.Principal, group bool) (httpkit.Reply, error) {
	var body scim.Document
	var err error
	if r.Method != "DELETE" {
		body, err = scim.Decode(r.Body)
		if err != nil {
			return httpkit.Reply{}, err
		}
	}
	return access.Transact(s.access, r, access.Unlocked, func(tx pgx.Tx, p authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		ctx := r.Context()
		id, current, err := s.loadSCIMTarget(ctx, r, tx, group)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		prepared, err := prepareSCIMWriteBody(ctx, tx, r.Method, group, current, body)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		managed, err := scimManagedProjects(ctx, tx)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		writtenID, affected, err := s.applySCIMWrite(ctx, tx, id, prepared, r.Method, group)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if err = s.reconcileSCIMUsers(ctx, tx, r, p, affected); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if err = s.finishSCIMWrite(ctx, tx, managed); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		return s.scimWriteReply(ctx, tx, writtenID, r.Method, group)
	})
}

func (s *scimService) loadSCIMTarget(ctx context.Context, r *http.Request, tx pgx.Tx, group bool) (string, scim.Document, error) {
	if r.Method == "POST" {
		return ids.New(), nil, nil
	}
	id, err := httpkit.IDParam(r, "scim_id")
	if err != nil {
		return "", nil, scim.Fail(404, "", "The resource was not found.")
	}
	current, etag, err := s.scimRead(ctx, tx, group, id)
	if err != nil {
		return "", nil, err
	}
	if match := r.Header.Get("If-Match"); match != "" && match != "*" && match != `"`+etag+`"` && match != `W/"`+etag+`"` {
		return "", nil, scim.Fail(412, "invalidVers", "The resource changed; read its current version.")
	}
	return id, current, nil
}

func prepareSCIMWriteBody(ctx context.Context, tx pgx.Tx, method string, group bool, current, body scim.Document) (scim.Document, error) {
	switch method {
	case "PUT":
		extension := scim.UserExtension
		if group {
			extension = scim.GroupExtension
		}
		if _, present := body[extension]; present {
			return body, nil
		}
		out := maps.Clone(body)
		out[extension] = current[extension]
		return out, nil
	case "PATCH":
		return scim.Patch(current, body, group, scimSelector(ctx, tx))
	default:
		return body, nil
	}
}

func (s *scimService) applySCIMWrite(ctx context.Context, tx pgx.Tx, id string, body scim.Document, method string, group bool) (string, []string, error) {
	if group {
		affected, err := s.writeSCIMGroup(ctx, tx, id, body, method)
		return id, affected, err
	}
	userID, err := s.writeSCIMUser(ctx, tx, id, body, method)
	if err != nil {
		return "", nil, err
	}
	return userID, []string{userID}, nil
}

func (s *scimService) finishSCIMWrite(ctx context.Context, tx pgx.Tx, managed []string) error {
	orphaned, err := scimOrphansProject(ctx, tx, managed)
	if err != nil {
		return err
	}
	if orphaned {
		return scim.Fail(409, "", "Keep at least one project manager.")
	}
	if err = EnsureUsableOwner(ctx, tx, s.access.LocalLoginDisabled()); err != nil {
		return err
	}
	_, err = access.AdvanceAuthority(ctx, tx)
	return err
}

func (s *scimService) scimWriteReply(ctx context.Context, tx pgx.Tx, id, method string, group bool) (httpkit.Reply, access.Event, error) {
	kind := "user"
	if group {
		kind = "group"
	}
	written := access.Event{Action: "scim." + kind + "." + strings.ToLower(method), Resource: kind, ID: id}
	if method == "DELETE" {
		return httpkit.Reply{Status: 204}, written, nil
	}
	resource, etag, err := s.scimRead(ctx, tx, group, id)
	if err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	reply := httpkit.Detail(resource, etag)
	if method == "POST" {
		reply.Status = 201
		reply.Location = s.access.Origin + "/scim/v2/Users/" + id
		if group {
			reply.Location = s.access.Origin + "/scim/v2/Groups/" + id
		}
	}
	return reply, written, nil
}

func boundedSCIMText(v any, max int, required bool) (string, error) {
	if v == nil && !required {
		return "", nil
	}
	s, ok := v.(string)
	if !ok || len(s) > max || required && strings.TrimSpace(s) == "" || strings.ContainsFunc(s, unicode.IsControl) {
		return "", scim.Invalid("An identity attribute has an invalid type, length or control character.")
	}
	return s, nil
}

func validateSCIMRoleScope(role, scope string, defaultViewer bool) (string, string, error) {
	resolvedRole := role
	if defaultViewer && resolvedRole == "" {
		resolvedRole = "viewer"
	}
	if resolvedRole != "" && !authz.ValidRole(resolvedRole) {
		return "", "", scim.Invalid("The role must be owner, operator, developer or viewer.")
	}
	resolvedScope := scope
	if resolvedScope == "" {
		resolvedScope = "assigned"
	}
	if resolvedScope != "assigned" && resolvedScope != "global" {
		return "", "", scim.Invalid("Use assigned or global accessScope.")
	}
	return resolvedRole, resolvedScope, nil
}

func scimExtension(d scim.Document, key string, user bool) (scimUserExtension, error) {
	e := map[string]any{}
	if raw := d[key]; raw != nil {
		var ok bool
		e, ok = raw.(map[string]any)
		if !ok {
			return scimUserExtension{}, scim.Invalid("The OLP extension must be an object.")
		}
	}
	role, err := boundedSCIMText(e["role"], 16, false)
	if err != nil {
		return scimUserExtension{}, err
	}
	scope, err := boundedSCIMText(e["accessScope"], 16, false)
	if err != nil {
		return scimUserExtension{}, err
	}
	resolvedRole, resolvedScope, err := validateSCIMRoleScope(role, scope, user)
	if err != nil {
		return scimUserExtension{}, err
	}
	return scimUserExtension{Role: resolvedRole, AccessScope: resolvedScope}, nil
}

func optionalRole(role string) *string {
	if role == "" {
		return nil
	}
	return &role
}

func parseSCIMGroupProjectsRaw(d scim.Document) ([]SCIMGroupProjectRef, error) {
	ext, ok := d[scim.GroupExtension].(map[string]any)
	if !ok || ext["projects"] == nil {
		return []SCIMGroupProjectRef{}, nil
	}
	values, ok := ext["projects"].([]any)
	if !ok || len(values) > 100 {
		return nil, scim.Invalid("A group may map to at most 100 projects.")
	}
	projects := make([]SCIMGroupProjectRef, 0, len(values))
	for _, raw := range values {
		item, ok := raw.(map[string]any)
		if !ok {
			return nil, scim.Invalid("Project mappings must be objects.")
		}
		projects = append(projects, SCIMGroupProjectRef{
			Value: scim.String(item, "value"),
			Role:  scim.String(item, "role"),
		})
	}
	return projects, nil
}

func normalizeSCIMGroupExtension(ctx context.Context, tx pgx.Tx, ext SCIMGroupExtension) (SCIMGroupExtension, error) {
	rawRole := ""
	if ext.Role != nil {
		rawRole = *ext.Role
	}
	roleText, err := boundedSCIMText(rawRole, 16, false)
	if err != nil {
		return SCIMGroupExtension{}, err
	}
	scopeText, err := boundedSCIMText(ext.AccessScope, 16, false)
	if err != nil {
		return SCIMGroupExtension{}, err
	}
	role, scope, err := validateSCIMRoleScope(roleText, scopeText, false)
	if err != nil {
		return SCIMGroupExtension{}, err
	}
	projects, err := validateSCIMGroupProjects(ctx, tx, ext.Projects)
	if err != nil {
		return SCIMGroupExtension{}, err
	}
	return SCIMGroupExtension{Role: optionalRole(role), AccessScope: scope, Projects: projects}, nil
}

func validateSCIMGroupProjects(ctx context.Context, tx pgx.Tx, raw []SCIMGroupProjectRef) ([]SCIMGroupProjectRef, error) {
	if len(raw) > 100 {
		return nil, scim.Invalid("A group may map to at most 100 projects.")
	}
	projects := make([]SCIMGroupProjectRef, 0, len(raw))
	projectIDs := make([]string, 0, len(raw))
	seen := make(map[string]bool, len(raw))
	for _, item := range raw {
		parsed, err := uuid.Parse(item.Value)
		if err != nil || seen[parsed.String()] || item.Role != "manager" && item.Role != "viewer" {
			return nil, scim.Invalid("Project mappings require unique UUIDs and manager or viewer roles.")
		}
		project := parsed.String()
		seen[project] = true
		projects = append(projects, SCIMGroupProjectRef{Value: project, Role: item.Role})
		projectIDs = append(projectIDs, project)
	}
	var validProjects int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM olp.projects WHERE id=ANY($1::uuid[])", projectIDs).Scan(&validProjects); err != nil {
		return nil, err
	}
	if validProjects != len(projectIDs) {
		return nil, scim.Invalid("A mapped project does not exist.")
	}
	return projects, nil
}

func parseSCIMGroupMembers(ctx context.Context, tx pgx.Tx, d scim.Document) ([]string, error) {
	members := []string{}
	if raw := d["members"]; raw != nil {
		items, ok := raw.([]any)
		if !ok || len(items) > 1000 {
			return nil, scim.Invalid("A group may contain at most 1000 users.")
		}
		for _, entry := range items {
			item, ok := entry.(map[string]any)
			if !ok {
				return nil, scim.Invalid("Members must be objects.")
			}
			parsed, err := uuid.Parse(scim.String(item, "value"))
			if err != nil {
				return nil, scim.Invalid("A member must identify a SCIM user by UUID.")
			}
			user := parsed.String()
			if !slices.Contains(members, user) {
				members = append(members, user)
			}
		}
	}
	var validMembers int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM olp.scim_users s JOIN olp.users u ON u.id=s.user_id WHERE s.user_id=ANY($1::uuid[]) AND s.deleted_at IS NULL AND u.role_management='provisioned'", members).Scan(&validMembers); err != nil {
		return nil, err
	}
	if validMembers != len(members) {
		return nil, scim.Invalid("Only current SCIM-managed users may join SCIM groups.")
	}
	return members, nil
}

func parseSCIMGroupDocument(ctx context.Context, tx pgx.Tx, d scim.Document) (parsedSCIMGroup, error) {
	if err := scim.Schema(d, scim.GroupSchema); err != nil {
		return parsedSCIMGroup{}, err
	}
	display, err := boundedSCIMText(d["displayName"], 100, true)
	if err != nil {
		return parsedSCIMGroup{}, err
	}
	external, err := boundedSCIMText(d["externalId"], 255, false)
	if err != nil {
		return parsedSCIMGroup{}, err
	}
	baseExt, err := scimExtension(d, scim.GroupExtension, false)
	if err != nil {
		return parsedSCIMGroup{}, err
	}
	rawProjects, err := parseSCIMGroupProjectsRaw(d)
	if err != nil {
		return parsedSCIMGroup{}, err
	}
	projects, err := validateSCIMGroupProjects(ctx, tx, rawProjects)
	if err != nil {
		return parsedSCIMGroup{}, err
	}
	members, err := parseSCIMGroupMembers(ctx, tx, d)
	if err != nil {
		return parsedSCIMGroup{}, err
	}
	return parsedSCIMGroup{
		display:   display,
		external:  external,
		extension: SCIMGroupExtension{Role: optionalRole(baseExt.Role), AccessScope: baseExt.AccessScope, Projects: projects},
		members:   members,
	}, nil
}

func scimGroupMemberIDs(ctx context.Context, tx pgx.Tx, id string) ([]string, error) {
	rows, err := tx.Query(ctx, "SELECT user_id::text FROM olp.scim_group_members WHERE group_id=$1", id)
	if err != nil {
		return nil, err
	}
	members, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	if members == nil {
		return []string{}, nil
	}
	return members, nil
}

func (s *scimService) writeSCIMGroup(ctx context.Context, tx pgx.Tx, id string, d scim.Document, method string) ([]string, error) {
	if method == "DELETE" {
		affected, err := scimGroupMemberIDs(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		_, err = tx.Exec(ctx, "DELETE FROM olp.scim_groups WHERE id=$1", id)
		return affected, err
	}
	group, err := parseSCIMGroupDocument(ctx, tx, d)
	if err != nil {
		return nil, err
	}
	return s.saveSCIMGroup(ctx, tx, id, group, method == "POST")
}

func (s *scimService) saveSCIMGroup(ctx context.Context, tx pgx.Tx, id string, group parsedSCIMGroup, create bool) ([]string, error) {
	affected, err := scimGroupMemberIDs(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	stored := scimStoredGroup{
		Schemas:     []string{scim.GroupSchema, scim.GroupExtension},
		DisplayName: group.display,
		ExternalID:  group.external,
		Extension:   group.extension,
	}
	data, err := json.Marshal(stored)
	if err != nil {
		return nil, err
	}
	if create {
		_, err = tx.Exec(ctx, "INSERT INTO olp.scim_groups(id,document,etag) VALUES($1,$2,$3)", id, data, ids.New())
	} else {
		_, err = tx.Exec(ctx, "UPDATE olp.scim_groups SET document=$2,etag=$3,updated_at=now() WHERE id=$1", id, data, ids.New())
	}
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, "DELETE FROM olp.scim_group_members WHERE group_id=$1", id); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, "DELETE FROM olp.scim_group_projects WHERE group_id=$1", id); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, "INSERT INTO olp.scim_group_members(group_id,user_id) SELECT $1,unnest($2::uuid[])", id, group.members); err != nil {
		return nil, err
	}
	projectJSON, err := json.Marshal(group.extension.Projects)
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, "INSERT INTO olp.scim_group_projects(group_id,project_id,role) SELECT $1,x.value,x.role FROM jsonb_to_recordset($2::jsonb) x(value uuid,role text)", id, projectJSON); err != nil {
		return nil, err
	}
	for _, user := range group.members {
		if !slices.Contains(affected, user) {
			affected = append(affected, user)
		}
	}
	return affected, nil
}

func (s *scimService) reconcileSCIMUsers(ctx context.Context, tx pgx.Tx, r *http.Request, p authz.Principal, userIDs []string) error {
	if len(userIDs) == 0 {
		return nil
	}
	var provisioned []string
	if err := tx.QueryRow(ctx, "SELECT COALESCE(array_agg(id::text),'{}'::text[]) FROM olp.users WHERE id=ANY($1::uuid[]) AND role_management='provisioned'", userIDs).Scan(&provisioned); err != nil {
		return err
	}
	if len(provisioned) == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `WITH grants AS (
 SELECT u.user_id, u.document->'`+scim.UserExtension+`'->>'role' AS role, u.document->'`+scim.UserExtension+`'->>'accessScope' AS scope FROM olp.scim_users u WHERE u.user_id=ANY($1::uuid[])
 UNION ALL SELECT m.user_id,g.document->'`+scim.GroupExtension+`'->>'role',g.document->'`+scim.GroupExtension+`'->>'accessScope' FROM olp.scim_group_members m JOIN olp.scim_groups g ON g.id=m.group_id WHERE m.user_id=ANY($1::uuid[])
), desired AS (SELECT user_id,max(CASE role WHEN 'owner' THEN 4 WHEN 'operator' THEN 3 WHEN 'developer' THEN 2 ELSE 1 END) AS level,bool_or(scope='global') AS global FROM grants GROUP BY user_id)
UPDATE olp.users u SET role=CASE d.level WHEN 4 THEN 'owner' WHEN 3 THEN 'operator' WHEN 2 THEN 'developer' ELSE 'viewer' END,access_scope=CASE WHEN d.global THEN 'global' ELSE 'assigned' END,etag=uuidv7(),updated_at=now() FROM desired d WHERE u.id=d.user_id AND u.role_management='provisioned'`, provisioned)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "DELETE FROM olp.sessions WHERE user_id=ANY($1::uuid[])", provisioned); err != nil {
		return err
	}
	for _, id := range provisioned {
		var owner bool
		if err = tx.QueryRow(ctx, "SELECT active AND role='owner' AND access_scope='global' FROM olp.users WHERE id=$1", id).Scan(&owner); err != nil {
			return err
		}
		if !owner {
			if err = retireIssuedInvitations(ctx, tx, r, id, p.Actor(), p.UserID()); err != nil {
				return err
			}
		}
	}
	return nil
}
