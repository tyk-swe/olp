package route

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/code"
	"github.com/tyk-swe/olp/internal/code/client"
	"github.com/tyk-swe/olp/internal/code/session"
	"github.com/tyk-swe/olp/internal/plugins"
)

type API struct {
	server  *access.Server
	plugins *plugins.Host
	publish func(context.Context, pgx.Tx, string) error
}

func NewAPI(
	server *access.Server,
	host *plugins.Host,
	publish func(context.Context, pgx.Tx, string) error,
) *API {
	return &API{server: server, plugins: host, publish: publish}
}

func (a *API) Register(mux *http.ServeMux) {
	a.server.Route(mux, "GET /api/v1/code/routes", a.list)
	a.server.Route(mux, "POST /api/v1/code/routes", a.create, access.MaxBody(128<<10))
	a.server.Route(mux, "PUT /api/v1/code/routes/{id}", a.update, access.MaxBody(128<<10))
	a.server.Route(mux, "POST /api/v1/code/routes/{id}/publish", a.publishRoute)
	a.server.Route(mux, "GET /api/v1/code/routes/{id}/revisions", a.revisions)
	a.server.Route(mux, "GET /api/v1/code/routes/{id}/client-config", a.clientConfig)
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
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (View, error) { return scanView(row) })
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
		if err := access.Audit(ctx, tx, r, p.Actor(), "code_route.write", "code_route", id, "success"); err != nil {
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
	if err := tx.QueryRow(ctx, `SELECT project_id::text,etag::text FROM olp.code_routes WHERE id=$1 FOR UPDATE`, id).Scan(&oldProject, &oldETag); err != nil {
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
	if err := access.Audit(ctx, tx, r, p.Actor(), "code_route.write", "code_route", id, "success"); err != nil {
		return access.Reply{}, err
	}
	return access.Detail(saved, etag), tx.Commit(ctx)
}

func (a *API) publishRoute(r *http.Request, _ access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "id")
	if err != nil {
		return access.Reply{}, err
	}
	ctx := r.Context()
	return access.Mutate(a.server, r, id, func(tx pgx.Tx, p access.Principal) (string, error) {
		var project, etag string
		if err := tx.QueryRow(ctx, `SELECT project_id::text,etag::text FROM olp.code_routes WHERE id=$1 FOR UPDATE`, id).Scan(&project, &etag); err != nil {
			return "", err
		}
		return etag, p.Project(&project, access.Change)
	}, func(tx pgx.Tx, p access.Principal, currentETag string) (access.Reply, error) {
		if err := access.Match(r, currentETag); err != nil {
			return access.Reply{}, err
		}
		saved, err := Publish(ctx, tx, p, a.plugins, id)
		if err != nil {
			return access.Reply{}, err
		}
		if a.publish != nil {
			if err = a.publish(ctx, tx, p.UserID()); err != nil {
				return access.Reply{}, err
			}
		}
		if err = access.Audit(ctx, tx, r, p.Actor(), "code_route.publish", "code_route", id, "success"); err != nil {
			return access.Reply{}, err
		}
		return access.Detail(saved, saved.ETag), nil
	})
}

func (a *API) revisions(r *http.Request, p access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "id")
	if err != nil {
		return access.Reply{}, err
	}
	page, err := access.Page(r)
	if err != nil {
		return access.Reply{}, err
	}
	var project string
	if err = a.server.Pool.QueryRow(r.Context(), `SELECT project_id::text FROM olp.code_routes WHERE id=$1`, id).Scan(&project); err != nil {
		return access.Reply{}, err
	}
	if err = p.Project(&project, access.View); err != nil {
		return access.Reply{}, err
	}
	rows, err := a.server.Pool.Query(r.Context(), `SELECT jsonb_build_object('id',id,'route',document)
		FROM olp.code_route_revisions WHERE route_id=$1 AND id<$2 ORDER BY id DESC LIMIT $3`, id, page.Before, page.Limit+1)
	if err != nil {
		return access.Reply{}, err
	}
	items, err := access.JSONRows(rows)
	return access.ListReply(items, page), err
}

func (a *API) clientConfig(r *http.Request, p access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "id")
	if err != nil {
		return access.Reply{}, err
	}
	var project string
	var latest *string
	if err = a.server.Pool.QueryRow(r.Context(), `SELECT project_id::text,latest_revision_id::text FROM olp.code_routes WHERE id=$1`, id).Scan(&project, &latest); err != nil {
		return access.Reply{}, err
	}
	if err = p.Project(&project, access.View); err != nil {
		return access.Reply{}, err
	}
	if latest == nil {
		return access.Reply{}, access.Fail(409, "code_route_unpublished", "Publish an enabled code-mode route first.")
	}
	var document []byte
	if err = a.server.Pool.QueryRow(r.Context(), `SELECT document FROM olp.code_route_revisions WHERE id=$1`, *latest).Scan(&document); err != nil {
		return access.Reply{}, err
	}
	var published code.Route
	if err = json.Unmarshal(document, &published); err != nil {
		return access.Reply{}, err
	}
	offers, err := session.Offers(r.Context(), a.server.Pool, published.RevisionID, "")
	if err != nil {
		return access.Reply{}, err
	}
	query := r.URL.Query()
	configuration, err := client.Generate(published, offers, client.Request{
		GatewayURL: query.Get("gateway_url"),
		Client:     query.Get("client"),
		Platform:   query.Get("platform"),
		Model:      query.Get("model"),
		PlanModel:  query.Get("plan_model"),
		SmallModel: query.Get("small_model"),
	})
	if err != nil {
		var cfgErr *client.Error
		if errors.As(err, &cfgErr) {
			if cfgErr.Field != "" {
				return access.Reply{}, access.Invalid(cfgErr.Field, cfgErr.Message)
			}
			return access.Reply{}, access.Fail(cfgErr.Status, cfgErr.Code, cfgErr.Message)
		}
		return access.Reply{}, err
	}
	return access.OK(configuration), nil
}
