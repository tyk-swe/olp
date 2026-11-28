package retrieval

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/domain/ids"
	"github.com/tyk-swe/olp/internal/domain/problem"
)

type API struct{ server *access.Server }

func NewAPI(server *access.Server) *API { return &API{server: server} }

func (a *API) Register(mux *http.ServeMux) {
	a.server.Route(mux, "GET /api/v1/retrieval-bindings", a.list)
	a.server.Route(mux, "POST /api/v1/retrieval-bindings", a.create)
	a.server.Route(mux, "GET /api/v1/retrieval-bindings/{binding_id}", a.get)
	a.server.Route(mux, "PUT /api/v1/retrieval-bindings/{binding_id}", a.update)
	a.server.Route(mux, "DELETE /api/v1/retrieval-bindings/{binding_id}", a.retire)
	a.server.Route(mux, "GET /api/v1/retrieval-bindings/{binding_id}/revisions/{revision_id}", a.revision)
}

func (a *API) list(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	page, err := httpkit.Page(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	items, err := listPage(r.Context(), a.server.Pool, page, p.AllProjects, p.ProjectIDs())
	if err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.ListReplyBy(items, page, func(b *Binding) string { return b.ID }), nil
}

func (a *API) get(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "binding_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	binding, err := a.viewable(r, p, id)
	if err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.Detail(binding, binding.ETag), nil
}

func (a *API) revision(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "binding_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	revisionID, err := httpkit.IDParam(r, "revision_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	if _, err := a.viewable(r, p, id); err != nil {
		return httpkit.Reply{}, err
	}
	detail, err := loadRevision(r.Context(), a.server.Pool, id, revisionID)
	if err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.Detail(detail, revisionID), nil
}

func (a *API) viewable(r *http.Request, p authz.Principal, id string) (*Binding, error) {
	binding, err := Load(r.Context(), a.server.Pool, id, false)
	if err != nil {
		return nil, err
	}
	return binding, p.Project(&binding.ProjectID, authz.View)
}

func decode(r *http.Request) (Input, error) {
	var in Input
	if err := httpkit.DecodeUnique(r, &in, 64<<10); err != nil {
		return Input{}, err
	}
	return normalizeInput(in)
}

func (a *API) create(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	in, err := decode(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	return access.Mutate(a.server, r, in, func(tx pgx.Tx, p authz.Principal) (struct{}, error) {
		if err := p.Authorize(authz.Configure); err != nil {
			return struct{}{}, err
		}
		return struct{}{}, a.server.RequireProject(r.Context(), tx, p, &in.ProjectID)
	}, func(tx pgx.Tx, p authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		next, err := publish(r.Context(), tx, p.UserID(), nil, in)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		return httpkit.Reply{Status: http.StatusCreated, Body: next, ETag: next.ETag, Location: "/api/v1/retrieval-bindings/" + next.ID}, bindingEvent("create", next.ID), nil
	})
}

func (a *API) update(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	in, err := decode(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	return access.Mutate(a.server, r, in, lock(r), func(tx pgx.Tx, p authz.Principal, current *Binding) (httpkit.Reply, access.Event, error) {
		if err := changeable(r, *current); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		next, err := publish(r.Context(), tx, p.UserID(), current, in)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		return httpkit.Detail(next, next.ETag), bindingEvent("update", next.ID), nil
	})
}

func (a *API) retire(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	return access.Mutate(a.server, r, Input{}, lock(r), func(tx pgx.Tx, _ authz.Principal, current *Binding) (httpkit.Reply, access.Event, error) {
		if err := changeable(r, *current); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if err := markRetired(r.Context(), tx, current.ID, ids.New()); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		_, err := access.AdvanceAuthority(r.Context(), tx)
		return httpkit.Reply{Status: http.StatusNoContent}, bindingEvent("retire", current.ID), err
	})
}

func lock(r *http.Request) func(pgx.Tx, authz.Principal) (*Binding, error) {
	return func(tx pgx.Tx, p authz.Principal) (*Binding, error) {
		if err := p.Authorize(authz.Configure); err != nil {
			return nil, err
		}
		id, err := httpkit.IDParam(r, "binding_id")
		if err != nil {
			return nil, err
		}
		current, err := Load(r.Context(), tx, id, true)
		if err != nil {
			return nil, err
		}
		return current, p.Project(&current.ProjectID, authz.Change)
	}
}

func changeable(r *http.Request, current Binding) error {
	if err := httpkit.Match(r, current.ETag); err != nil {
		return err
	}
	if current.RetiredAt != nil {
		return problem.Fail(409, "retrieval_binding_retired", "Create a new binding instead of changing retired history.")
	}
	return nil
}

func publish(ctx context.Context, tx pgx.Tx, actor string, current *Binding, in Input) (Binding, error) {
	next, err := Save(ctx, tx, actor, current, in)
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" {
		return Binding{}, problem.Fail(409, "retrieval_binding_exists", "Choose an unused binding name in this project.")
	}
	if err != nil {
		return Binding{}, err
	}
	if _, err := access.AdvanceAuthority(ctx, tx); err != nil {
		return Binding{}, err
	}
	return next, nil
}

func bindingEvent(action, id string) access.Event {
	return access.Event{Action: "retrieval_binding." + action, Resource: "retrieval_binding", ID: id}
}
