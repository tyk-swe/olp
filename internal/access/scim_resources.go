package access

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	filter "github.com/scim2/filter-parser/v2"
	"github.com/tyk-swe/olp/internal/scim"
)

func (s *Server) scimSQL(group bool) string {
	if group {
		return `SELECT g.id,g.etag,g.document || jsonb_build_object('id',g.id,'members',COALESCE((SELECT jsonb_agg(jsonb_build_object('value',u.id,'display',u.display_name,'$ref',` + sqlText(s.Origin+"/scim/v2/Users/") + `||u.id::text,'type','User') ORDER BY u.id) FROM olp.scim_group_members m JOIN olp.users u ON u.id=m.user_id JOIN olp.scim_users su ON su.user_id=u.id WHERE m.group_id=g.id AND su.deleted_at IS NULL AND u.role_management='provisioned'),'[]'::jsonb),'meta',jsonb_build_object('resourceType','Group','created',g.created_at,'lastModified',g.updated_at,'version','W/"'||g.etag::text||'"','location',` + sqlText(s.Origin+"/scim/v2/Groups/") + `||g.id::text)) AS resource FROM olp.scim_groups g`
	}
	return `SELECT u.id,u.etag,su.document || jsonb_build_object('id',u.id,'userName',u.email,'displayName',u.display_name,'active',u.active,'roles',jsonb_build_array(jsonb_build_object('value',u.role,'primary',true)),'groups',COALESCE((SELECT jsonb_agg(jsonb_build_object('value',g.id,'display',g.document->>'displayName','$ref',` + sqlText(s.Origin+"/scim/v2/Groups/") + `||g.id::text,'type','direct') ORDER BY g.id) FROM olp.scim_group_members m JOIN olp.scim_groups g ON g.id=m.group_id WHERE m.user_id=u.id AND u.role_management='provisioned'),'[]'::jsonb),'meta',jsonb_build_object('resourceType','User','created',u.created_at,'lastModified',u.updated_at,'version','W/"'||u.etag::text||'"','location',` + sqlText(s.Origin+"/scim/v2/Users/") + `||u.id::text)) AS resource FROM olp.scim_users su JOIN olp.users u ON u.id=su.user_id WHERE su.deleted_at IS NULL`
}

// Origin is operator configuration, escaped as a SQL literal, never a request Host.
func sqlText(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func (s *Server) scimRead(r *http.Request, q Queryer, group bool, id string) (scim.Document, string, error) {
	var data []byte
	var etag string
	err := q.QueryRow(r.Context(), "SELECT resource,etag::text FROM ("+s.scimSQL(group)+") resource WHERE id=$1", id).Scan(&data, &etag)
	if err != nil {
		return nil, "", err
	}
	var d scim.Document
	err = json.Unmarshal(data, &d)
	return d, etag, err
}

func (s *Server) scimGet(r *http.Request, _ Principal, group bool) (Reply, error) {
	id, err := IDParam(r, "scim_id")
	if err != nil {
		return Reply{}, scim.Fail(404, "", "The resource was not found.")
	}
	d, etag, err := s.scimRead(r, s.Pool, group, id)
	if err != nil {
		return Reply{}, err
	}
	d, err = scim.Project(d, r.URL.Query().Get("attributes"), r.URL.Query().Get("excludedAttributes"))
	return Detail(d, etag), err
}

func (s *Server) scimList(r *http.Request, _ Principal, group bool) (Reply, error) {
	values := r.URL.Query()
	if r.Method == "POST" {
		body, e := scim.Decode(r.Body)
		if e != nil {
			return Reply{}, e
		}
		if e = scim.Schema(body, scim.SearchSchema); e != nil {
			return Reply{}, e
		}
		values = url.Values{}
		for _, key := range []string{"filter", "sortBy", "sortOrder", "startIndex", "count", "attributes", "excludedAttributes"} {
			if value, ok := body[key]; ok {
				switch v := value.(type) {
				case string:
					values.Set(key, v)
				case json.Number:
					values.Set(key, v.String())
				case []any:
					parts := []string{}
					for _, x := range v {
						str, ok := x.(string)
						if !ok {
							return Reply{}, scim.Invalid("Attribute selection must contain strings.")
						}
						parts = append(parts, str)
					}
					values.Set(key, strings.Join(parts, ","))
				default:
					return Reply{}, scim.Invalid("The search parameter has the wrong type.")
				}
			}
		}
	}
	start, count := 1, 100
	var err error
	if raw := values.Get("startIndex"); raw != "" {
		start, err = strconv.Atoi(raw)
		if err != nil {
			return Reply{}, scim.Invalid("startIndex must be an integer.")
		}
		if start < 1 {
			start = 1
		}
	}
	if raw := values.Get("count"); raw != "" {
		count, err = strconv.Atoi(raw)
		if err != nil {
			return Reply{}, scim.Invalid("count must be an integer.")
		}
		if count < 0 {
			count = 0
		}
		if count > 200 {
			count = 200
		}
	}
	if start > 10000000 {
		return Reply{}, scim.Invalid("startIndex exceeds the supported offset.")
	}
	compiled := scim.SQLFilter{}
	where, err := compiled.Compile(values.Get("filter"), "resource")
	if err != nil {
		return Reply{}, err
	}
	order := "id ASC"
	if by := values.Get("sortBy"); by != "" {
		expression, e := compiled.Sort(by, "resource")
		if e != nil {
			return Reply{}, e
		}
		direction := "ASC"
		switch values.Get("sortOrder") {
		case "descending":
			direction = "DESC"
		case "", "ascending":
		default:
			return Reply{}, scim.Invalid("Use ascending or descending sort order.")
		}
		order = expression + " " + direction + " NULLS LAST,id ASC"
	}
	base := "WITH records AS (" + s.scimSQL(group) + "), selected AS (SELECT * FROM records WHERE " + where + ") "
	// A single statement gives count and page the same MVCC snapshot.
	compiled.Args = append(compiled.Args, count, start-1)
	query := base + fmt.Sprintf("SELECT (SELECT count(*) FROM selected), COALESCE((SELECT jsonb_agg(resource) FROM (SELECT resource FROM selected ORDER BY %s LIMIT $%d OFFSET $%d) page),'[]'::jsonb)", order, len(compiled.Args)-1, len(compiled.Args))
	var total int64
	var data []byte
	if err = s.Pool.QueryRow(r.Context(), query, compiled.Args...).Scan(&total, &data); err != nil {
		return Reply{}, err
	}
	var resources []scim.Document
	if err = json.Unmarshal(data, &resources); err != nil {
		return Reply{}, err
	}
	for i, d := range resources {
		resources[i], err = scim.Project(d, values.Get("attributes"), values.Get("excludedAttributes"))
		if err != nil {
			return Reply{}, err
		}
	}
	return OK(map[string]any{"schemas": []string{scim.ListSchema}, "totalResults": total, "startIndex": start, "itemsPerPage": len(resources), "Resources": resources}), nil
}

func scimSelector(r *http.Request, tx pgx.Tx) scim.Select {
	return func(values []any, e filter.Expression) ([]int, error) {
		body, err := json.Marshal(values)
		if err != nil {
			return nil, err
		}
		compiler := scim.SQLFilter{Args: []any{body}}
		predicate, err := compiler.Expression(e, "item.value")
		if err != nil {
			return nil, err
		}
		var raw []byte
		err = tx.QueryRow(r.Context(), "SELECT COALESCE(jsonb_agg(item.position-1),'[]'::jsonb) FROM jsonb_array_elements($1::jsonb) WITH ORDINALITY item(value,position) WHERE "+predicate, compiler.Args...).Scan(&raw)
		if err != nil {
			return nil, err
		}
		var selected []int
		err = json.Unmarshal(raw, &selected)
		return selected, err
	}
}
