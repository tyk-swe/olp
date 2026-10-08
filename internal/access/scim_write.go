package access

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/scim"
)

func (s *Server) scimWrite(r *http.Request, _ Principal, group bool) (Reply, error) {
	var body scim.Document
	var err error
	if r.Method != "DELETE" {
		body, err = scim.Decode(r.Body)
		if err != nil {
			return Reply{}, err
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
	create := r.Method == "POST"
	id := NewID()
	var current scim.Document
	var etag string
	if !create {
		id, err = IDParam(r, "scim_id")
		if err != nil {
			return Reply{}, scim.Fail(404, "", "The resource was not found.")
		}
		current, etag, err = s.scimRead(r, tx, group, id)
		if err != nil {
			return Reply{}, err
		}
		if match := r.Header.Get("If-Match"); match != "" && match != "*" && match != `"`+etag+`"` && match != `W/"`+etag+`"` {
			return Reply{}, scim.Fail(412, "invalidVers", "The resource changed; read its current version.")
		}
	}
	if r.Method == "PUT" {
		extension := scim.UserExtension
		if group {
			extension = scim.GroupExtension
		}
		if _, present := body[extension]; !present {
			body[extension] = current[extension]
		}
	}
	if r.Method == "PATCH" {
		body, err = scim.Patch(current, body, group, scimSelector(r, tx))
		if err != nil {
			return Reply{}, err
		}
	}
	var affected []string
	kind := "user"
	if group {
		kind = "group"
		affected, err = s.writeSCIMGroup(r, tx, id, body, r.Method)
	} else {
		id, err = s.writeSCIMUser(r, tx, id, body, r.Method)
		affected = []string{id}
	}
	if err != nil {
		return Reply{}, err
	}
	if err = s.reconcileSCIMUsers(r, tx, p, affected); err != nil {
		return Reply{}, err
	}
	if err = s.usableOwner(r, tx); err != nil {
		return Reply{}, err
	}
	if _, err = AdvanceAuthority(r.Context(), tx); err != nil {
		return Reply{}, err
	}
	if err = Audit(r.Context(), tx, r, p.Actor(), "scim."+kind+"."+strings.ToLower(r.Method), kind, id, "success"); err != nil {
		return Reply{}, err
	}
	if r.Method == "DELETE" {
		return Commit(r, tx, Reply{Status: 204})
	}
	resource, etag, err := s.scimRead(r, tx, group, id)
	if err != nil {
		return Reply{}, err
	}
	reply := Detail(resource, etag)
	if create {
		reply.Status = 201
		reply.Location = s.Origin + "/scim/v2/Users/" + id
		if group {
			reply.Location = s.Origin + "/scim/v2/Groups/" + id
		}
	}
	return Commit(r, tx, reply)
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

func scimExtension(d scim.Document, key string, user bool) (map[string]any, error) {
	e := map[string]any{}
	if raw := d[key]; raw != nil {
		var ok bool
		e, ok = raw.(map[string]any)
		if !ok {
			return nil, scim.Invalid("The OLP extension must be an object.")
		}
	}
	role, err := boundedSCIMText(e["role"], 16, false)
	if err != nil {
		return nil, err
	}
	if user && role == "" {
		role = "viewer"
	}
	if role != "" && !validRole(role) {
		return nil, scim.Invalid("The role must be owner, operator, developer or viewer.")
	}
	scope, err := boundedSCIMText(e["accessScope"], 16, false)
	if err != nil {
		return nil, err
	}
	if scope == "" {
		scope = "assigned"
	}
	if scope != "assigned" && scope != "global" {
		return nil, scim.Invalid("Use assigned or global accessScope.")
	}
	out := map[string]any{"accessScope": scope}
	if role != "" {
		out["role"] = role
	}
	return out, nil
}

func (s *Server) writeSCIMUser(r *http.Request, tx pgx.Tx, id string, d scim.Document, method string) (string, error) {
	if method != "POST" {
		var management string
		if err := tx.QueryRow(r.Context(), "SELECT role_management FROM olp.users WHERE id=$1 FOR UPDATE", id).Scan(&management); err != nil {
			return "", err
		}
		if management != "provisioned" {
			return "", scim.Fail(409, "mutability", "This identity is now managed locally.")
		}
	}
	if method == "DELETE" {
		if _, err := tx.Exec(r.Context(), "UPDATE olp.scim_users SET deleted_at=now() WHERE user_id=$1", id); err != nil {
			return "", err
		}
		if _, err := tx.Exec(r.Context(), "UPDATE olp.users SET active=false,etag=$2,updated_at=now() WHERE id=$1", id, NewID()); err != nil {
			return "", err
		}
		_, err := tx.Exec(r.Context(), "DELETE FROM olp.scim_group_members WHERE user_id=$1", id)
		return id, err
	}
	if err := scim.Schema(d, scim.UserSchema); err != nil {
		return "", err
	}
	if _, ok := d["password"]; ok {
		return "", scim.Fail(400, "mutability", "SCIM cannot provision local passwords.")
	}
	raw, err := boundedSCIMText(d["userName"], 320, true)
	if err != nil {
		return "", err
	}
	address, err := email(raw)
	if err != nil {
		return "", scim.Invalid("userName must be an email address.")
	}
	external, err := boundedSCIMText(d["externalId"], 255, false)
	if err != nil {
		return "", err
	}
	display, err := boundedSCIMText(d["displayName"], 100, false)
	if err != nil {
		return "", err
	}
	name := map[string]any{}
	if raw := d["name"]; raw != nil {
		fields, ok := raw.(map[string]any)
		if !ok {
			return "", scim.Invalid("name must be an object.")
		}
		for _, key := range []string{"formatted", "familyName", "givenName", "middleName", "honorificPrefix", "honorificSuffix"} {
			if value, exists := fields[key]; exists {
				v, e := boundedSCIMText(value, 100, false)
				if e != nil {
					return "", e
				}
				if v != "" {
					name[key] = v
				}
			}
		}
	}
	if display == "" {
		display = scim.String(name, "formatted")
	}
	if display == "" {
		display = string([]rune(address)[:min(100, len([]rune(address)))])
	}
	active := true
	if raw, exists := d["active"]; exists && raw != nil {
		var ok bool
		active, ok = raw.(bool)
		if !ok {
			return "", scim.Invalid("active must be boolean.")
		}
	}
	extension, err := scimExtension(d, scim.UserExtension, true)
	if err != nil {
		return "", err
	}
	stored := scim.Document{"schemas": []string{scim.UserSchema, scim.UserExtension}, "userName": address, "displayName": display, "active": active, scim.UserExtension: extension}
	if external != "" {
		stored["externalId"] = external
	}
	if len(name) > 0 {
		stored["name"] = name
	}
	if raw := d["emails"]; raw != nil {
		items, ok := raw.([]any)
		if !ok || len(items) > 8 {
			return "", scim.Invalid("emails must contain at most eight values.")
		}
		emails := []any{}
		primary := false
		for _, raw := range items {
			item, ok := raw.(map[string]any)
			if !ok {
				return "", scim.Invalid("Email values must be objects.")
			}
			value, e := boundedSCIMText(item["value"], 320, true)
			if e != nil {
				return "", e
			}
			value, e = email(value)
			if e != nil {
				return "", scim.Invalid("An email value is invalid.")
			}
			entry := map[string]any{"value": value}
			typ, e := boundedSCIMText(item["type"], 32, false)
			if e != nil {
				return "", e
			}
			if typ != "" {
				entry["type"] = typ
			}
			if flag, exists := item["primary"]; exists && flag != nil {
				b, ok := flag.(bool)
				if !ok || b && primary {
					return "", scim.Invalid("At most one email may be primary.")
				}
				entry["primary"] = b
				primary = primary || b
			}
			emails = append(emails, entry)
		}
		stored["emails"] = emails
	}
	data, _ := json.Marshal(stored)
	if method == "POST" {
		// Re-enrolling the same deleted external identity preserves its stable ID,
		// but never adopts an existing local or live account by email.
		var existing, management string
		var deleted bool
		var oldExternal *string
		e := tx.QueryRow(r.Context(), "SELECT u.id::text,u.role_management,COALESCE(su.deleted_at IS NOT NULL,false),su.document->>'externalId' FROM olp.users u LEFT JOIN olp.scim_users su ON su.user_id=u.id WHERE u.email=$1 FOR UPDATE OF u", address).Scan(&existing, &management, &deleted, &oldExternal)
		if e == nil {
			old := ""
			if oldExternal != nil {
				old = *oldExternal
			}
			if management != "provisioned" || !deleted || old != external {
				return "", scim.Fail(409, "uniqueness", "Another account already uses this userName.")
			}
			id = existing
		} else if !errors.Is(e, pgx.ErrNoRows) {
			return "", e
		} else {
			if _, e = tx.Exec(r.Context(), "INSERT INTO olp.users(id,email,display_name,role,active,access_scope,role_management,etag) VALUES($1,$2,$3,$4,$5,$6,'provisioned',$7)", id, address, display, extension["role"], active, extension["accessScope"], NewID()); e != nil {
				return "", e
			}
			if _, e = tx.Exec(r.Context(), "INSERT INTO olp.provisioned_users(source,external_id,user_id) VALUES('scim',$1::uuid::text,$1::uuid)", id); e != nil {
				return "", e
			}
		}
	}
	if _, err = tx.Exec(r.Context(), "INSERT INTO olp.scim_users(user_id,document) VALUES($1,$2) ON CONFLICT(user_id) DO UPDATE SET document=excluded.document,deleted_at=NULL", id, data); err != nil {
		return "", err
	}
	_, err = tx.Exec(r.Context(), "UPDATE olp.users SET email=$2,display_name=$3,active=$4,updated_at=now(),etag=$5 WHERE id=$1", id, address, display, active, NewID())
	return id, err
}

func (s *Server) writeSCIMGroup(r *http.Request, tx pgx.Tx, id string, d scim.Document, method string) ([]string, error) {
	affected := []string{}
	rows, err := tx.Query(r.Context(), "SELECT user_id::text FROM olp.scim_group_members WHERE group_id=$1", id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var u string
		if err = rows.Scan(&u); err != nil {
			rows.Close()
			return nil, err
		}
		affected = append(affected, u)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if method == "DELETE" {
		_, err = tx.Exec(r.Context(), "DELETE FROM olp.scim_groups WHERE id=$1", id)
		return affected, err
	}
	if err = scim.Schema(d, scim.GroupSchema); err != nil {
		return nil, err
	}
	display, err := boundedSCIMText(d["displayName"], 100, true)
	if err != nil {
		return nil, err
	}
	external, err := boundedSCIMText(d["externalId"], 255, false)
	if err != nil {
		return nil, err
	}
	extension, err := scimExtension(d, scim.GroupExtension, false)
	if err != nil {
		return nil, err
	}
	projects := []map[string]string{}
	if ext, ok := d[scim.GroupExtension].(map[string]any); ok && ext["projects"] != nil {
		values, ok := ext["projects"].([]any)
		if !ok || len(values) > 100 {
			return nil, scim.Invalid("A group may map to at most 100 projects.")
		}
		seen := map[string]bool{}
		for _, raw := range values {
			item, ok := raw.(map[string]any)
			if !ok {
				return nil, scim.Invalid("Project mappings must be objects.")
			}
			parsed, e := uuid.Parse(scim.String(item, "value"))
			role := scim.String(item, "role")
			if e != nil || seen[parsed.String()] || role != "manager" && role != "viewer" {
				return nil, scim.Invalid("Project mappings require unique UUIDs and manager or viewer roles.")
			}
			project := parsed.String()
			seen[project] = true

			projects = append(projects, map[string]string{"value": project, "role": role})
		}
	}
	projectIDs := make([]string, 0, len(projects))
	for _, project := range projects {
		projectIDs = append(projectIDs, project["value"])
	}
	var validProjects int
	if err = tx.QueryRow(r.Context(), "SELECT count(*) FROM olp.projects WHERE id=ANY($1::uuid[])", projectIDs).Scan(&validProjects); err != nil {
		return nil, err
	}
	if validProjects != len(projectIDs) {
		return nil, scim.Invalid("A mapped project does not exist.")
	}
	extension["projects"] = projects
	stored := scim.Document{"schemas": []string{scim.GroupSchema, scim.GroupExtension}, "displayName": display, scim.GroupExtension: extension}
	if external != "" {
		stored["externalId"] = external
	}
	members := []string{}
	if raw := d["members"]; raw != nil {
		items, ok := raw.([]any)
		if !ok || len(items) > 1000 {
			return nil, scim.Invalid("A group may contain at most 1000 users.")
		}
		for _, raw := range items {
			item, ok := raw.(map[string]any)
			if !ok {
				return nil, scim.Invalid("Members must be objects.")
			}
			parsed, e := uuid.Parse(scim.String(item, "value"))
			if e != nil {
				return nil, scim.Invalid("A member must identify a SCIM user by UUID.")
			}
			user := parsed.String()
			if slices.Contains(members, user) {
				continue
			}

			members = append(members, user)
		}
	}
	var validMembers int
	if err = tx.QueryRow(r.Context(), "SELECT count(*) FROM olp.scim_users s JOIN olp.users u ON u.id=s.user_id WHERE s.user_id=ANY($1::uuid[]) AND s.deleted_at IS NULL AND u.role_management='provisioned'", members).Scan(&validMembers); err != nil {
		return nil, err
	}
	if validMembers != len(members) {
		return nil, scim.Invalid("Only current SCIM-managed users may join SCIM groups.")
	}
	data, _ := json.Marshal(stored)
	if method == "POST" {
		_, err = tx.Exec(r.Context(), "INSERT INTO olp.scim_groups(id,document,etag) VALUES($1,$2,$3)", id, data, NewID())
	} else {
		_, err = tx.Exec(r.Context(), "UPDATE olp.scim_groups SET document=$2,etag=$3,updated_at=now() WHERE id=$1", id, data, NewID())
	}
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(r.Context(), "DELETE FROM olp.scim_group_members WHERE group_id=$1", id); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(r.Context(), "DELETE FROM olp.scim_group_projects WHERE group_id=$1", id); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(r.Context(), "INSERT INTO olp.scim_group_members(group_id,user_id) SELECT $1,unnest($2::uuid[])", id, members); err != nil {
		return nil, err
	}
	for _, user := range members {
		if !slices.Contains(affected, user) {
			affected = append(affected, user)
		}
	}
	projectJSON, _ := json.Marshal(projects)
	if _, err = tx.Exec(r.Context(), "INSERT INTO olp.scim_group_projects(group_id,project_id,role) SELECT $1,x.value,x.role FROM jsonb_to_recordset($2::jsonb) x(value uuid,role text)", id, projectJSON); err != nil {
		return nil, err
	}

	return affected, nil
}

func (s *Server) reconcileSCIMUsers(r *http.Request, tx pgx.Tx, p Principal, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	if err := tx.QueryRow(r.Context(), "SELECT COALESCE(array_agg(id::text),'{}'::text[]) FROM olp.users WHERE id=ANY($1::uuid[]) AND role_management='provisioned'", ids).Scan(&ids); err != nil {
		return err
	}

	_, err := tx.Exec(r.Context(), `WITH grants AS (
 SELECT u.user_id, u.document->'`+scim.UserExtension+`'->>'role' AS role, u.document->'`+scim.UserExtension+`'->>'accessScope' AS scope FROM olp.scim_users u WHERE u.user_id=ANY($1::uuid[])
 UNION ALL SELECT m.user_id,g.document->'`+scim.GroupExtension+`'->>'role',g.document->'`+scim.GroupExtension+`'->>'accessScope' FROM olp.scim_group_members m JOIN olp.scim_groups g ON g.id=m.group_id WHERE m.user_id=ANY($1::uuid[])
), desired AS (SELECT user_id,max(CASE role WHEN 'owner' THEN 4 WHEN 'operator' THEN 3 WHEN 'developer' THEN 2 ELSE 1 END) AS level,bool_or(scope='global') AS global FROM grants GROUP BY user_id)
UPDATE olp.users u SET role=CASE d.level WHEN 4 THEN 'owner' WHEN 3 THEN 'operator' WHEN 2 THEN 'developer' ELSE 'viewer' END,access_scope=CASE WHEN d.global THEN 'global' ELSE 'assigned' END,etag=uuidv7(),updated_at=now() FROM desired d WHERE u.id=d.user_id AND u.role_management='provisioned'`, ids)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(r.Context(), "DELETE FROM olp.sessions WHERE user_id=ANY($1::uuid[])", ids); err != nil {
		return err
	}
	for _, id := range ids {
		var owner bool
		if err = tx.QueryRow(r.Context(), "SELECT active AND role='owner' AND access_scope='global' FROM olp.users WHERE id=$1", id).Scan(&owner); err != nil {
			return err
		}
		if !owner {
			if err = retireIssuedInvitations(r, tx, id, p.Actor(), p.UserID()); err != nil {
				return err
			}
		}
	}
	return nil
}
