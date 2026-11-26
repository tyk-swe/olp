package identity

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	filter "github.com/scim2/filter-parser/v2"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/identity/scim"
	"github.com/tyk-swe/olp/internal/platform/database"
)

type scimListResponse[T any] struct {
	Schemas      []string `json:"schemas"`
	TotalResults int64    `json:"totalResults"`
	StartIndex   int      `json:"startIndex"`
	ItemsPerPage int      `json:"itemsPerPage"`
	Resources    []T      `json:"Resources"`
}

func (s *scimService) scimSQL(group bool) string {
	if group {
		return `SELECT g.id,g.etag,g.document || jsonb_build_object('id',g.id,'members',COALESCE((SELECT jsonb_agg(jsonb_build_object('value',u.id,'display',u.display_name,'$ref',` + sqlText(s.access.Origin+"/scim/v2/Users/") + `||u.id::text,'type','User') ORDER BY u.id) FROM olp.scim_group_members m JOIN olp.users u ON u.id=m.user_id JOIN olp.scim_users su ON su.user_id=u.id WHERE m.group_id=g.id AND su.deleted_at IS NULL AND u.role_management='provisioned'),'[]'::jsonb),'meta',jsonb_build_object('resourceType','Group','created',g.created_at,'lastModified',g.updated_at,'version','W/"'||g.etag::text||'"','location',` + sqlText(s.access.Origin+"/scim/v2/Groups/") + `||g.id::text)) AS resource FROM olp.scim_groups g`
	}
	return `SELECT u.id,u.etag,su.document || jsonb_build_object('id',u.id,'userName',u.email,'displayName',u.display_name,'active',u.active,'roles',jsonb_build_array(jsonb_build_object('value',u.role,'primary',true)),'groups',COALESCE((SELECT jsonb_agg(jsonb_build_object('value',g.id,'display',g.document->>'displayName','$ref',` + sqlText(s.access.Origin+"/scim/v2/Groups/") + `||g.id::text,'type','direct') ORDER BY g.id) FROM olp.scim_group_members m JOIN olp.scim_groups g ON g.id=m.group_id WHERE m.user_id=u.id AND u.role_management='provisioned'),'[]'::jsonb),'meta',jsonb_build_object('resourceType','User','created',u.created_at,'lastModified',u.updated_at,'version','W/"'||u.etag::text||'"','location',` + sqlText(s.access.Origin+"/scim/v2/Users/") + `||u.id::text)) AS resource FROM olp.scim_users su JOIN olp.users u ON u.id=su.user_id WHERE su.deleted_at IS NULL`
}

// Origin is operator configuration, escaped as a SQL literal, never a request Host.
func sqlText(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func (s *scimService) scimRead(ctx context.Context, q database.Queryer, group bool, id string) (scim.Document, string, error) {
	var data []byte
	var etag string
	err := q.QueryRow(ctx, "SELECT resource,etag::text FROM ("+s.scimSQL(group)+") resource WHERE id=$1", id).Scan(&data, &etag)
	if err != nil {
		return nil, "", err
	}
	var d scim.Document
	err = json.Unmarshal(data, &d)
	return d, etag, err
}

func (s *scimService) scimGet(r *http.Request, _ authz.Principal, group bool) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "scim_id")
	if err != nil {
		return httpkit.Reply{}, scim.Fail(404, "", "The resource was not found.")
	}
	d, etag, err := s.scimRead(r.Context(), s.access.Pool, group, id)
	if err != nil {
		return httpkit.Reply{}, err
	}
	projection, err := scim.ParseProjection(r.URL.Query().Get("attributes"), r.URL.Query().Get("excludedAttributes"), scimResourceSchema(group))
	if err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.Detail(projection.Apply(d), etag), nil
}

func scimResourceSchema(group bool) string {
	if group {
		return scim.GroupSchema
	}
	return scim.UserSchema
}

func scimListValues(r *http.Request) (url.Values, error) {
	if r.Method != "POST" {
		return r.URL.Query(), nil
	}
	body, err := scim.Decode(r.Body)
	if err != nil {
		return nil, err
	}
	if err = scim.Schema(body, scim.SearchSchema); err != nil {
		return nil, err
	}
	values := url.Values{}
	for _, key := range []string{"filter", "sortBy", "sortOrder", "startIndex", "count", "attributes", "excludedAttributes"} {
		value, ok := body[key]
		if !ok {
			continue
		}
		switch v := value.(type) {
		case string:
			values.Set(key, v)
		case json.Number:
			values.Set(key, v.String())
		case []any:
			parts := make([]string, 0, len(v))
			for _, x := range v {
				str, ok := x.(string)
				if !ok {
					return nil, scim.Invalid("Attribute selection must contain strings.")
				}
				parts = append(parts, str)
			}
			values.Set(key, strings.Join(parts, ","))
		default:
			return nil, scim.Invalid("The search parameter has the wrong type.")
		}
	}
	return values, nil
}

func scimListWindow(values url.Values) (int, int, error) {
	start, count := 1, 100
	if raw := values.Get("startIndex"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			return 0, 0, scim.Invalid("startIndex must be an integer.")
		}
		start = max(1, parsed)
	}
	if raw := values.Get("count"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			return 0, 0, scim.Invalid("count must be an integer.")
		}
		count = max(0, min(parsed, 200))
	}
	if start > 10000000 {
		return 0, 0, scim.Invalid("startIndex exceeds the supported offset.")
	}
	return start, count, nil
}

func scimOrderClause(compiled *scim.SQLFilter, values url.Values) (string, error) {
	by := values.Get("sortBy")
	if by == "" {
		return "id ASC", nil
	}
	expression, err := compiled.Sort(by, "resource")
	if err != nil {
		return "", err
	}
	direction := "ASC"
	switch values.Get("sortOrder") {
	case "descending":
		direction = "DESC"
	case "", "ascending":
	default:
		return "", scim.Invalid("Use ascending or descending sort order.")
	}
	return expression + " " + direction + " NULLS LAST,id ASC", nil
}

func (s *scimService) scimList(r *http.Request, _ authz.Principal, group bool) (httpkit.Reply, error) {
	values, err := scimListValues(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	start, count, err := scimListWindow(values)
	if err != nil {
		return httpkit.Reply{}, err
	}
	schema := scimResourceSchema(group)
	projection, err := scim.ParseProjection(values.Get("attributes"), values.Get("excludedAttributes"), schema)
	if err != nil {
		return httpkit.Reply{}, err
	}
	compiled := scim.SQLFilter{Schema: schema}
	where, err := compiled.Compile(values.Get("filter"), "resource")
	if err != nil {
		return httpkit.Reply{}, err
	}
	order, err := scimOrderClause(&compiled, values)
	if err != nil {
		return httpkit.Reply{}, err
	}
	base := "WITH records AS (" + s.scimSQL(group) + "), selected AS (SELECT * FROM records WHERE " + where + ") "
	// A single statement gives count and page the same MVCC snapshot.
	compiled.Args = append(compiled.Args, count, start-1)
	query := base + fmt.Sprintf("SELECT (SELECT count(*) FROM selected), COALESCE((SELECT jsonb_agg(resource) FROM (SELECT resource FROM selected ORDER BY %s LIMIT $%d OFFSET $%d) page),'[]'::jsonb)", order, len(compiled.Args)-1, len(compiled.Args))
	var total int64
	var data []byte
	if err = s.access.Pool.QueryRow(r.Context(), query, compiled.Args...).Scan(&total, &data); err != nil {
		return httpkit.Reply{}, err
	}
	var resources []scim.Document
	if err = json.Unmarshal(data, &resources); err != nil {
		return httpkit.Reply{}, err
	}
	projected := make([]scim.Document, len(resources))
	for i, d := range resources {
		projected[i] = projection.Apply(d)
	}
	return httpkit.OK(scimListResponse[scim.Document]{
		Schemas:      []string{scim.ListSchema},
		TotalResults: total,
		StartIndex:   start,
		ItemsPerPage: len(projected),
		Resources:    projected,
	}), nil
}

func scimSelector(ctx context.Context, tx pgx.Tx) scim.Select {
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
		err = tx.QueryRow(ctx, "SELECT COALESCE(jsonb_agg(item.position-1),'[]'::jsonb) FROM jsonb_array_elements($1::jsonb) WITH ORDINALITY item(value,position) WHERE "+predicate, compiler.Args...).Scan(&raw)
		if err != nil {
			return nil, err
		}
		var selected []int
		err = json.Unmarshal(raw, &selected)
		return selected, err
	}
}
