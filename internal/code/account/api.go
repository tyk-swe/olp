package account

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/code/vendor"
)

type API struct {
	server *access.Server
}

func NewAPI(server *access.Server) *API { return &API{server: server} }

func (a *API) Register(mux *http.ServeMux) {
	a.server.Route(mux, "GET /api/v1/code/accounts", a.list)
	a.server.Route(mux, "POST /api/v1/code/accounts", a.create, access.MaxBody(128<<10))
	a.server.Route(mux, "PUT /api/v1/code/accounts/{id}", a.update, access.MaxBody(128<<10))
	a.server.Route(mux, "POST /api/v1/code/accounts/{id}/allowance-reads", a.requestAllowanceRead)
}

func (a *API) list(r *http.Request, p access.Principal) (access.Reply, error) {
	page, err := access.Page(r)
	if err != nil {
		return access.Reply{}, err
	}
	project := r.URL.Query().Get("project_id")
	if project != "" {
		project, err = access.ParseUUID(project)
		if err != nil {
			return access.Reply{}, err
		}
		if err = p.Project(&project, access.View); err != nil {
			return access.Reply{}, err
		}
	}
	rows, err := a.server.Pool.Query(r.Context(), selectRows+
		` WHERE x.id<$1 AND ($2 OR x.project_id=ANY($3::uuid[])) AND ($4='' OR x.project_id::text=$4) ORDER BY x.id DESC LIMIT $5`,
		page.Before, p.AllProjects, p.ProjectIDs(), project, page.Limit+1)
	if err != nil {
		return access.Reply{}, err
	}
	now := time.Now().UTC()
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (View, error) { return scanView(row, now) })
	if err != nil {
		return access.Reply{}, err
	}
	return access.ListReplyBy(items, page, func(item View) string { return item.ID }), nil
}

func (a *API) create(r *http.Request, _ access.Principal) (access.Reply, error) {
	var in Input
	if err := access.DecodeUnique(r, &in, 128<<10); err != nil {
		return access.Reply{}, err
	}
	draft, err := ParseDraft(in)
	if err != nil {
		return access.Reply{}, err
	}
	ctx := r.Context()
	return access.Mutate(a.server, r, in, func(tx pgx.Tx, p access.Principal) (struct{}, error) {
		project := draft.ProjectID()
		return struct{}{}, a.server.RequireProject(ctx, tx, p, &project)
	}, func(tx pgx.Tx, p access.Principal, _ struct{}) (access.Reply, error) {
		id, etag := access.NewID(), access.NewID()
		saved, err := Save(ctx, tx, p, id, etag, draft, false)
		if err != nil {
			return access.Reply{}, err
		}
		if err := access.Audit(ctx, tx, r, p.Actor(), "code_account.write", "code_account", id, "success"); err != nil {
			return access.Reply{}, err
		}
		return access.Reply{Status: http.StatusCreated, Body: saved, ETag: etag, Location: r.URL.Path + "/" + id}, nil
	})
}

func (a *API) update(r *http.Request, _ access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "id")
	if err != nil {
		return access.Reply{}, err
	}
	var in Input
	if err := access.DecodeUnique(r, &in, 128<<10); err != nil {
		return access.Reply{}, err
	}
	draft, err := ParseDraft(in)
	if err != nil {
		return access.Reply{}, err
	}
	ctx := r.Context()
	tx, p, err := a.server.BeginMutation(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(ctx)
	project := draft.ProjectID()
	if err := a.server.RequireProject(ctx, tx, p, &project); err != nil {
		return access.Reply{}, err
	}
	var oldProject, oldETag string
	if err := tx.QueryRow(ctx, `SELECT project_id::text,etag::text FROM olp.code_accounts WHERE id=$1 FOR UPDATE`, id).Scan(&oldProject, &oldETag); err != nil {
		return access.Reply{}, err
	}
	if err := p.Project(&oldProject, access.Change); err != nil {
		return access.Reply{}, err
	}
	if oldProject != project {
		return access.Reply{}, access.Invalid("project_id", "The project is immutable.")
	}
	if err := access.Match(r, oldETag); err != nil {
		return access.Reply{}, err
	}
	etag := access.NewID()
	saved, err := Save(ctx, tx, p, id, etag, draft, true)
	if err != nil {
		return access.Reply{}, err
	}
	if err := access.Audit(ctx, tx, r, p.Actor(), "code_account.write", "code_account", id, "success"); err != nil {
		return access.Reply{}, err
	}
	return access.Detail(saved, etag), tx.Commit(ctx)
}

func (a *API) requestAllowanceRead(r *http.Request, _ access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "id")
	if err != nil {
		return access.Reply{}, err
	}
	ctx := r.Context()
	type lockedAccount struct {
		config []byte
	}
	return access.Mutate(a.server, r, id, func(tx pgx.Tx, p access.Principal) (lockedAccount, error) {
		var project string
		var config []byte
		if err := tx.QueryRow(ctx, `SELECT a.project_id::text,p.configuration FROM olp.code_accounts a JOIN olp.providers p ON p.id=a.provider_id WHERE a.id=$1 FOR UPDATE OF a`, id).Scan(&project, &config); err != nil {
			return lockedAccount{}, err
		}
		return lockedAccount{config: config}, p.Project(&project, access.Change)
	}, func(tx pgx.Tx, p access.Principal, locked lockedAccount) (access.Reply, error) {
		var cfg vendor.ConnectionIdentity
		if len(locked.config) > 0 {
			if err := json.Unmarshal(locked.config, &cfg); err != nil {
				return access.Reply{}, err
			}
		}
		signal, known := vendor.AllowanceSignalForProfile(cfg.Kind, cfg.AuthMode, cfg.ProfileID)
		if !known || !signal.ActiveQualified {
			return access.Reply{}, access.Invalid("adapter", "Only accounts with a proven allowance signal support an active allowance read.")
		}
		if _, err := tx.Exec(ctx, `UPDATE olp.code_accounts SET allowance_read_requested_at=now() WHERE id=$1`, id); err != nil {
			return access.Reply{}, err
		}
		if err := access.Audit(ctx, tx, r, p.Actor(), "code_account.allowance_read", "code_account", id, "success"); err != nil {
			return access.Reply{}, err
		}
		view, err := Load(ctx, tx, id)
		if err != nil {
			return access.Reply{}, err
		}
		return access.OK(view), nil
	})
}
