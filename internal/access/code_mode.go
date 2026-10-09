package access

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/codemode"
)

// CodeWrite shares the management transaction, current authority, optimistic
// concurrency, creation replay and audit rules with the code-mode features.
func (s *Server) CodeWrite(r *http.Request, table, resource, project string, input any, write func(pgx.Tx, Principal, string, string) (any, error)) (Reply, error) {
	if !slices.Contains([]string{"code_accounts", "code_pools", "code_routes", "code_token_budgets"}, table) {
		panic("invalid code management table")
	}
	project, err := ParseUUID(project)
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
	if err = s.RequireProject(r.Context(), tx, p, &project); err != nil {
		return Reply{}, err
	}
	id := r.PathValue("id")
	etag := NewID()
	var claim ReplayClaim
	if id == "" {
		var replayed *Reply
		claim, replayed, err = s.Replay(r, tx, p, input)
		if err != nil {
			return Reply{}, err
		}
		if replayed != nil {
			return Commit(r, tx, *replayed)
		}
		id = NewID()
	} else {
		id, err = ParseUUID(id)
		if err != nil {
			return Reply{}, err
		}
		var oldProject, oldETag string
		if err = tx.QueryRow(r.Context(), `SELECT project_id::text,etag::text FROM olp.`+table+` WHERE id=$1`, id).Scan(&oldProject, &oldETag); err != nil {
			return Reply{}, err
		}
		if err = p.Project(&oldProject, Change); err != nil {
			return Reply{}, err
		}
		if oldProject != project {
			return Reply{}, Invalid("project_id", "The project is immutable.")
		}
		if err = Match(r, oldETag); err != nil {
			return Reply{}, err
		}
	}
	body, err := write(tx, p, id, etag)
	if err != nil {
		return Reply{}, err
	}
	if err = Audit(r.Context(), tx, r, p.Actor(), resource+".write", resource, id, "success"); err != nil {
		return Reply{}, err
	}
	result := Detail(body, etag)
	if r.PathValue("id") == "" {
		result.Status = 201
		result.Location = r.URL.Path + "/" + id
		if err = s.CompleteReplay(r, tx, claim, result); err != nil {
			return Reply{}, err
		}
	}
	return Commit(r, tx, result)
}

// CodeMatch matches a listing's rows to a query field by SQL other than its
// equality with the field of x: a condition on the field's value, written
// $%[1]d.
type CodeMatch struct{ Field, SQL string }

func (s *Server) CodeList(r *http.Request, p Principal, selectSQL string, matches ...CodeMatch) (Reply, error) {
	return s.codeList(r, p, selectSQL, false, matches)
}

// CodeUsageList adds digest filtering for the two content-free usage ledgers.
func (s *Server) CodeUsageList(r *http.Request, p Principal, selectSQL string) (Reply, error) {
	return s.codeList(r, p, selectSQL, true, nil)
}

func (s *Server) codeList(r *http.Request, p Principal, selectSQL string, endUsers bool, matches []CodeMatch) (Reply, error) {
	page, err := Page(r)
	if err != nil {
		return Reply{}, err
	}
	project := r.URL.Query().Get("project_id")
	if project != "" {
		project, err = ParseUUID(project)
		if err != nil {
			return Reply{}, err
		}
		if err = p.Project(&project, View); err != nil {
			return Reply{}, err
		}
	}
	where := ` WHERE x.id<$1 AND ($2 OR x.project_id=ANY($3::uuid[])) AND ($4='' OR x.project_id::text=$4)`
	args := []any{page.Before, p.AllProjects, p.ProjectIDs(), project, page.Limit + 1}
	if digest := r.URL.Query().Get("end_user_digest"); endUsers && digest != "" {
		if digest == "unidentified" {
			digest = ""
		} else if !ValidEndUserDigest(digest) {
			return Reply{}, Invalid("end_user_digest", "Use an end-user digest or unidentified.")
		}
		args = append(args, digest)
		where += fmt.Sprintf(" AND x.end_user_digest=$%d", len(args))
	}
	for _, field := range []string{"route_id", "api_key_id", "account_id", "binding_id"} {
		if value := r.URL.Query().Get(field); value != "" {
			id, err := ParseUUID(value)
			if err != nil {
				return Reply{}, err
			}
			args = append(args, id)
			condition := fmt.Sprintf("to_jsonb(x)->>'%s'=$%d", field, len(args))
			if i := slices.IndexFunc(matches, func(m CodeMatch) bool { return m.Field == field }); i >= 0 {
				condition = fmt.Sprintf(matches[i].SQL, len(args))
			}
			where += " AND " + condition
		}
	}
	rows, err := s.Pool.Query(r.Context(), selectSQL+where+` ORDER BY x.id DESC LIMIT $5`, args...)
	if err != nil {
		return Reply{}, err
	}
	items, err := JSONRows(rows)
	return ListReply(items, page), err
}

func (s *Server) registerCodeMode(mux *http.ServeMux) {
	s.Route(mux, "GET /api/v1/code/pools", s.codePools)
	s.Route(mux, "POST /api/v1/code/pools", s.writeCodePool)
	s.Route(mux, "PUT /api/v1/code/pools/{id}", s.writeCodePool)
	s.Route(mux, "GET /api/v1/code/budgets", s.codeBudgets)
	s.Route(mux, "POST /api/v1/code/budgets", s.writeCodeBudget)
	s.Route(mux, "PUT /api/v1/code/budgets/{id}", s.writeCodeBudget)
}

const codePoolJSON = `SELECT jsonb_build_object('id',x.id,'project_id',x.project_id,'name',x.name,'kind',x.kind,'owner_user_id',x.owner_user_id,'etag',x.etag,
	'account_ids',ARRAY(SELECT account_id FROM olp.code_pool_accounts WHERE pool_id=x.id ORDER BY account_id),
	'api_key_ids',ARRAY(SELECT api_key_id FROM olp.code_pool_keys WHERE pool_id=x.id ORDER BY api_key_id)) FROM olp.code_pools x`

func (s *Server) codePools(r *http.Request, p Principal) (Reply, error) {
	return s.CodeList(r, p, codePoolJSON)
}
func (s *Server) codeBudgets(r *http.Request, p Principal) (Reply, error) {
	return s.CodeList(r, p, `SELECT to_jsonb(x)-'created_by' FROM olp.code_token_budgets x`)
}

type codePoolInput struct {
	ProjectID   string   `json:"project_id"`
	Name        string   `json:"name"`
	Kind        string   `json:"kind"`
	OwnerUserID *string  `json:"owner_user_id"`
	AccountIDs  []string `json:"account_ids"`
	APIKeyIDs   []string `json:"api_key_ids"`
}

func (s *Server) writeCodePool(r *http.Request, _ Principal) (Reply, error) {
	var in codePoolInput
	if err := Decode(r, &in); err != nil {
		return Reply{}, err
	}
	if err := ValidText("name", in.Name, 100); err != nil {
		return Reply{}, err
	}
	if in.Kind != "personal" && in.Kind != "shared" || (in.Kind == "personal") != (in.OwnerUserID != nil) {
		return Reply{}, Invalid("kind", "Personal pools require an owner; shared pools have none.")
	}
	if in.AccountIDs == nil || in.APIKeyIDs == nil || len(in.AccountIDs) > 100 || len(in.APIKeyIDs) > 100 {
		return Reply{}, Invalid("assignments", "Use at most 100 accounts and keys.")
	}
	for _, id := range append(slices.Clone(in.AccountIDs), in.APIKeyIDs...) {
		if _, err := ParseUUID(id); err != nil {
			return Reply{}, err
		}
	}
	if in.OwnerUserID != nil {
		id, err := ParseUUID(*in.OwnerUserID)
		if err != nil {
			return Reply{}, err
		}
		in.OwnerUserID = &id
	}
	return s.CodeWrite(r, "code_pools", "code_pool", in.ProjectID, in, func(tx pgx.Tx, p Principal, id, etag string) (any, error) {
		if in.OwnerUserID != nil {
			var member bool
			if err := tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM olp.effective_project_members WHERE project_id=$1 AND user_id=$2)`, in.ProjectID, *in.OwnerUserID).Scan(&member); err != nil {
				return nil, err
			}
			if !member {
				return nil, Invalid("owner_user_id", "The owner must be a project member.")
			}
		}
		for _, account := range in.AccountIDs {
			if err := codeScoped(r.Context(), tx, "code_accounts", account, in.ProjectID); err != nil {
				return nil, err
			}
		}
		for _, key := range in.APIKeyIDs {
			if err := codeScoped(r.Context(), tx, "api_keys", key, in.ProjectID); err != nil {
				return nil, err
			}
			if in.OwnerUserID != nil {
				var owner string
				if err := tx.QueryRow(r.Context(), `SELECT created_by::text FROM olp.api_keys WHERE id=$1`, key).Scan(&owner); err != nil {
					return nil, err
				}
				if owner != *in.OwnerUserID {
					return nil, Invalid("api_key_ids", "Personal pools accept only their owner's keys.")
				}
			}
		}
		if r.PathValue("id") != "" {
			var kind string
			var owner *string
			if err := tx.QueryRow(r.Context(), `SELECT kind,owner_user_id::text FROM olp.code_pools WHERE id=$1`, id).Scan(&kind, &owner); err != nil {
				return nil, err
			}
			if kind != in.Kind || (owner == nil) != (in.OwnerUserID == nil) || owner != nil && *owner != *in.OwnerUserID {
				return nil, Invalid("kind", "Pool ownership is immutable.")
			}
		}
		_, err := tx.Exec(r.Context(), `INSERT INTO olp.code_pools(id,project_id,name,kind,owner_user_id,etag,created_by) VALUES($1,$2,$3,$4,$5,$6,$7)
			ON CONFLICT(id) DO UPDATE SET name=excluded.name,etag=excluded.etag`, id, in.ProjectID, in.Name, in.Kind, in.OwnerUserID, etag, p.UserID())
		if err != nil {
			return nil, err
		}
		if _, err = tx.Exec(r.Context(), `DELETE FROM olp.code_pool_accounts WHERE pool_id=$1`, id); err != nil {
			return nil, err
		}
		if _, err = tx.Exec(r.Context(), `DELETE FROM olp.code_pool_keys WHERE pool_id=$1`, id); err != nil {
			return nil, err
		}
		for _, account := range in.AccountIDs {
			if _, err = tx.Exec(r.Context(), `INSERT INTO olp.code_pool_accounts(pool_id,account_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, id, account); err != nil {
				return nil, err
			}
		}
		for _, key := range in.APIKeyIDs {
			if _, err = tx.Exec(r.Context(), `INSERT INTO olp.code_pool_keys(pool_id,api_key_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, id, key); err != nil {
				return nil, err
			}
		}
		return codemode.Pool{ID: id, ProjectID: in.ProjectID, Name: in.Name, Kind: in.Kind, OwnerUserID: in.OwnerUserID, AccountIDs: in.AccountIDs, APIKeyIDs: in.APIKeyIDs, ETag: etag}, nil
	})
}

func codeScoped(ctx context.Context, tx pgx.Tx, table, id, project string) error {
	var matches bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.`+table+` WHERE id=$1 AND project_id=$2)`, id, project).Scan(&matches)
	if err != nil {
		return err
	}
	if !matches {
		return notFound
	}
	return nil
}

type codeBudgetInput struct {
	ProjectID     string  `json:"project_id"`
	RouteID       *string `json:"route_id"`
	APIKeyID      *string `json:"api_key_id"`
	DailyTokens   *int64  `json:"daily_tokens"`
	MonthlyTokens *int64  `json:"monthly_tokens"`
	Enabled       bool    `json:"enabled"`
}

func (s *Server) writeCodeBudget(r *http.Request, _ Principal) (Reply, error) {
	var in codeBudgetInput
	if err := Decode(r, &in); err != nil {
		return Reply{}, err
	}
	if in.DailyTokens == nil && in.MonthlyTokens == nil {
		return Reply{}, Invalid("tokens", "Set a daily or monthly token cap.")
	}
	for _, n := range []*int64{in.DailyTokens, in.MonthlyTokens} {
		if n != nil && (*n < 1 || *n > 1<<53-1) {
			return Reply{}, Invalid("tokens", "Use a positive safe integer.")
		}
	}
	for _, id := range []*string{in.RouteID, in.APIKeyID} {
		if id != nil {
			if _, err := ParseUUID(*id); err != nil {
				return Reply{}, err
			}
		}
	}
	return s.CodeWrite(r, "code_token_budgets", "code_budget", in.ProjectID, in, func(tx pgx.Tx, p Principal, id, etag string) (any, error) {
		if in.RouteID != nil {
			if err := codeScoped(r.Context(), tx, "code_routes", *in.RouteID, in.ProjectID); err != nil {
				return nil, err
			}
		}
		if in.APIKeyID != nil {
			if err := codeScoped(r.Context(), tx, "api_keys", *in.APIKeyID, in.ProjectID); err != nil {
				return nil, err
			}
		}
		if r.PathValue("id") != "" {
			var matches bool
			if err := tx.QueryRow(r.Context(), `SELECT route_id IS NOT DISTINCT FROM $2::uuid AND api_key_id IS NOT DISTINCT FROM $3::uuid FROM olp.code_token_budgets WHERE id=$1`, id, in.RouteID, in.APIKeyID).Scan(&matches); err != nil {
				return nil, err
			}
			if !matches {
				return nil, Invalid("scope", "Budget scope is immutable.")
			}
		}
		_, err := tx.Exec(r.Context(), `INSERT INTO olp.code_token_budgets(id,project_id,route_id,api_key_id,daily_tokens,monthly_tokens,enabled,etag,created_by) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)
			ON CONFLICT(id) DO UPDATE SET daily_tokens=excluded.daily_tokens,monthly_tokens=excluded.monthly_tokens,enabled=excluded.enabled,etag=excluded.etag`, id, in.ProjectID, in.RouteID, in.APIKeyID, in.DailyTokens, in.MonthlyTokens, in.Enabled, etag, p.UserID())
		if err != nil {
			return nil, err
		}
		var body json.RawMessage
		err = tx.QueryRow(r.Context(), `SELECT to_jsonb(x)-'created_by' FROM olp.code_token_budgets x WHERE id=$1`, id).Scan(&body)
		return body, err
	})
}
