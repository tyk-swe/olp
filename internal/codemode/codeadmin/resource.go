package codeadmin

import (
	"context"
	"fmt"
	"net/http"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/domain/attribution"
	"github.com/tyk-swe/olp/internal/domain/ids"
	"github.com/tyk-swe/olp/internal/domain/problem"
	"github.com/tyk-swe/olp/internal/platform/database"
)

type Resource struct{ table, audit string }

var (
	Accounts = Resource{table: "code_accounts", audit: "code_account"}
	Pools    = Resource{table: "code_pools", audit: "code_pool"}
	Routes   = Resource{table: "code_routes", audit: "code_route"}
	Budgets  = Resource{table: "code_token_budgets", audit: "code_budget"}
)

type Writer func(tx pgx.Tx, p authz.Principal, id, etag string, existing bool) (any, error)

func Write(s *access.Server, r *http.Request, resource Resource, project string, input any, write Writer) (httpkit.Reply, error) {
	projectID, err := ids.Parse(project)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if r.PathValue("id") != "" {
		return access.Transact(s, r, target(s, r, resource, projectID), func(tx pgx.Tx, p authz.Principal, id string) (httpkit.Reply, access.Event, error) {
			etag := ids.New()
			body, err := write(tx, p, id, etag, true)
			return httpkit.Detail(body, etag), resource.written(id), err
		})
	}
	admitted := func(tx pgx.Tx, p authz.Principal) (struct{}, error) {
		return struct{}{}, s.RequireProject(r.Context(), tx, p, &projectID)
	}
	return access.Mutate(s, r, input, admitted, func(tx pgx.Tx, p authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		id, etag := ids.New(), ids.New()
		body, err := write(tx, p, id, etag, false)
		return httpkit.Reply{Status: http.StatusCreated, Body: body, ETag: etag, Location: r.URL.Path + "/" + id}, resource.written(id), err
	})
}

func target(s *access.Server, r *http.Request, resource Resource, projectID string) func(pgx.Tx, authz.Principal) (string, error) {
	return func(tx pgx.Tx, p authz.Principal) (string, error) {
		if err := s.RequireProject(r.Context(), tx, p, &projectID); err != nil {
			return "", err
		}
		id, err := ids.Parse(r.PathValue("id"))
		if err != nil {
			return "", err
		}
		var oldProject, oldETag string
		if err := tx.QueryRow(r.Context(), `SELECT project_id::text,etag::text FROM olp.`+resource.table+` WHERE id=$1`, id).Scan(&oldProject, &oldETag); err != nil {
			return "", err
		}
		if err := p.Project(&oldProject, authz.Change); err != nil {
			return "", err
		}
		if oldProject != projectID {
			return "", problem.Invalid("project_id", "The project is immutable.")
		}
		return id, httpkit.Match(r, oldETag)
	}
}

func (resource Resource) written(id string) access.Event {
	return access.Event{Action: resource.audit + ".write", Resource: resource.audit, ID: id}
}

// Match matches a listing's rows to a query field by SQL other than its
// equality with the field of x: a condition on the field's value, written
// $%[1]d.
type Match struct{ Field, SQL string }

func List(s *access.Server, r *http.Request, p authz.Principal, selectSQL string, matches ...Match) (httpkit.Reply, error) {
	return list(s, r, p, selectSQL, false, matches)
}

// UsageList adds digest filtering for the two content-free usage ledgers.
func UsageList(s *access.Server, r *http.Request, p authz.Principal, selectSQL string, matches ...Match) (httpkit.Reply, error) {
	return list(s, r, p, selectSQL, true, matches)
}

func list(s *access.Server, r *http.Request, p authz.Principal, selectSQL string, endUsers bool, matches []Match) (httpkit.Reply, error) {
	page, err := httpkit.Page(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	project := r.URL.Query().Get("project_id")
	if project != "" {
		project, err = ids.Parse(project)
		if err != nil {
			return httpkit.Reply{}, err
		}
		if err = p.Project(&project, authz.View); err != nil {
			return httpkit.Reply{}, err
		}
	}
	where := ` WHERE x.id<$1 AND ($2 OR x.project_id=ANY($3::uuid[])) AND ($4='' OR x.project_id::text=$4)`
	args := []any{page.Before, p.AllProjects, p.ProjectIDs(), project, page.Limit + 1}
	if digest := r.URL.Query().Get("end_user_digest"); endUsers && digest != "" {
		if digest == "unidentified" {
			digest = ""
		} else if !attribution.ValidEndUserDigest(digest) {
			return httpkit.Reply{}, problem.Invalid("end_user_digest", "Use an end-user digest or unidentified.")
		}
		args = append(args, digest)
		where += fmt.Sprintf(" AND x.end_user_digest=$%d", len(args))
	}
	fields := []string{"route_id", "api_key_id", "account_id", "binding_id"}
	extra := slices.Collect(func(yield func(string) bool) {
		for _, match := range matches {
			if !slices.Contains(fields, match.Field) && !yield(match.Field) {
				return
			}
		}
	})
	for _, field := range slices.Concat(fields, extra) {
		if value := r.URL.Query().Get(field); value != "" {
			id, err := ids.Parse(value)
			if err != nil {
				return httpkit.Reply{}, err
			}
			args = append(args, id)
			condition := fmt.Sprintf("to_jsonb(x)->>'%s'=$%d", field, len(args))
			if i := slices.IndexFunc(matches, func(m Match) bool { return m.Field == field }); i >= 0 {
				condition = fmt.Sprintf(matches[i].SQL, len(args))
			}
			where += " AND " + condition
		}
	}
	items, err := database.QueryJSON(r.Context(), s.Pool, selectSQL+where+` ORDER BY x.id DESC LIMIT $5`, args...)
	return httpkit.ListReply(items, page), err
}

func scoped(ctx context.Context, tx pgx.Tx, table, id, project string) error {
	var matches bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.`+table+` WHERE id=$1 AND project_id=$2)`, id, project).Scan(&matches)
	if err != nil {
		return err
	}
	if !matches {
		return problem.NotFound
	}
	return nil
}
