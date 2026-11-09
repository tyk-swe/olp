package definition

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/guardrails"
)

type API struct {
	server  *access.Server
	publish guardrails.Publisher
}

func NewAPI(server *access.Server, publish guardrails.Publisher) *API {
	return &API{server: server, publish: publish}
}

func (a *API) Register(mux *http.ServeMux) {
	a.server.Route(mux, "GET /api/v1/guardrails", a.list)
	a.server.Route(mux, "POST /api/v1/guardrails", a.create, access.MaxBody(128<<10))
	a.server.Route(mux, "GET /api/v1/guardrails/{guardrail_id}", a.get)
	a.server.Route(mux, "GET /api/v1/guardrails/{guardrail_id}/revisions/{revision_id}", a.revision)
	a.server.Route(mux, "PUT /api/v1/guardrails/{guardrail_id}", a.update, access.MaxBody(128<<10))
	a.server.Route(mux, "DELETE /api/v1/guardrails/{guardrail_id}", a.retire)
	a.server.Route(mux, "POST /api/v1/guardrails/{guardrail_id}/activate", guardrails.Activation(a.server, a.publish, lineage{}, "guardrail_id", "guardrails"))
}

type revision struct {
	ID            string          `json:"id"`
	GuardrailID   string          `json:"guardrail_id"`
	Revision      int64           `json:"revision"`
	Policy        json.RawMessage `json:"policy"`
	CreatedAt     time.Time       `json:"created_at"`
	Configuration json.RawMessage `json:"configuration"`
	TimeoutMS     int64           `json:"timeout_ms"`
	OnError       string          `json:"on_error"`
}

func (a *API) list(r *http.Request, p access.Principal) (access.Reply, error) {
	page, err := access.Page(r)
	if err != nil {
		return access.Reply{}, err
	}
	search := strings.TrimSpace(r.URL.Query().Get("search"))
	if len(search) > 200 {
		return access.Reply{}, access.Invalid("search", "Use at most 200 bytes.")
	}
	rows, err := a.server.Pool.Query(r.Context(), selected+" WHERE g.retired_at IS NULL AND g.id<$1 AND ($2 OR g.project_id=ANY($3::uuid[])) AND ($5='' OR g.name ILIKE '%'||$5||'%') ORDER BY g.id DESC LIMIT $4", page.Before, p.AllProjects, p.ProjectIDs(), page.Limit+1, search)
	if err != nil {
		return access.Reply{}, err
	}
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Definition, error) { return scan(row) })
	if err != nil {
		return access.Reply{}, err
	}
	return access.ListReplyBy(items, page, func(item Definition) string { return item.ID }), nil
}

func (a *API) get(r *http.Request, p access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "guardrail_id")
	if err != nil {
		return access.Reply{}, err
	}
	value, err := load(r.Context(), a.server.Pool, id)
	if err != nil {
		return access.Reply{}, err
	}
	if err = p.Project(&value.ProjectID, access.View); err != nil {
		return access.Reply{}, err
	}
	return access.Detail(value, value.ETag), nil
}

func (a *API) revision(r *http.Request, p access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "guardrail_id")
	if err != nil {
		return access.Reply{}, err
	}
	revisionID, err := access.IDParam(r, "revision_id")
	if err != nil {
		return access.Reply{}, err
	}
	current, err := load(r.Context(), a.server.Pool, id)
	if err != nil {
		return access.Reply{}, err
	}
	if err = p.Project(&current.ProjectID, access.View); err != nil {
		return access.Reply{}, err
	}
	value := revision{ID: revisionID, GuardrailID: id}
	if err = a.server.Pool.QueryRow(r.Context(), "SELECT revision,policy,created_at,configuration,timeout_ms,on_error FROM olp.guardrail_revisions WHERE guardrail_id=$1 AND id=$2", id, revisionID).Scan(&value.Revision, &value.Policy, &value.CreatedAt, &value.Configuration, &value.TimeoutMS, &value.OnError); err != nil {
		return access.Reply{}, err
	}
	return access.Detail(value, revisionID), nil
}

func (a *API) create(r *http.Request, _ access.Principal) (access.Reply, error) {
	in, change, err := decode(r)
	if err != nil {
		return access.Reply{}, err
	}
	ctx := r.Context()
	return access.Mutate(a.server, r, in, func(tx pgx.Tx, p access.Principal) (struct{}, error) {
		if in.Type == "" {
			return struct{}{}, access.Invalid("type", "Choose a guardrail type.")
		}
		if in.ProjectID == nil {
			return struct{}{}, access.Invalid("project_id", "Choose the owning project.")
		}
		return struct{}{}, a.server.RequireProject(ctx, tx, p, in.ProjectID)
	}, func(tx pgx.Tx, p access.Principal, _ struct{}) (access.Reply, error) {
		value, err := a.save(r, tx, p, nil, change, *in.ProjectID, in.Type, in.Configuration)
		if err != nil {
			return access.Reply{}, err
		}
		reply := access.Detail(value, value.ETag)
		reply.Status, reply.Location = http.StatusCreated, "/api/v1/guardrails/"+value.ID
		return reply, nil
	})
}

func (a *API) update(r *http.Request, _ access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "guardrail_id")
	if err != nil {
		return access.Reply{}, err
	}
	in, change, err := decode(r)
	if err != nil {
		return access.Reply{}, err
	}
	return access.Mutate(a.server, r, in, a.locked(r, id), func(tx pgx.Tx, p access.Principal, current Definition) (access.Reply, error) {
		if err := changeable(r, current); err != nil {
			return access.Reply{}, err
		}
		if in.ProjectID != nil || in.Type != "" {
			return access.Reply{}, access.Invalid("project_id", "A guardrail's project and type are immutable.")
		}
		value, err := a.save(r, tx, p, &current, change, current.ProjectID, current.Type, in.Configuration)
		if err != nil {
			return access.Reply{}, err
		}
		return access.Detail(value, value.ETag), nil
	})
}

func (a *API) retire(r *http.Request, _ access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "guardrail_id")
	if err != nil {
		return access.Reply{}, err
	}
	ctx := r.Context()
	return access.Mutate(a.server, r, input{}, a.locked(r, id), func(tx pgx.Tx, p access.Principal, current Definition) (access.Reply, error) {
		if err := changeable(r, current); err != nil {
			return access.Reply{}, err
		}
		if err := retire(ctx, tx, id); err != nil {
			return access.Reply{}, err
		}
		if err := access.Audit(ctx, tx, r, p.Actor(), "guardrail.retire", "guardrail", id, "success"); err != nil {
			return access.Reply{}, err
		}
		return access.Reply{Status: http.StatusNoContent}, nil
	})
}

func decode(r *http.Request) (input, edit, error) {
	var in input
	if err := access.DecodeUnique(r, &in, 128<<10); err != nil {
		return input{}, edit{}, err
	}
	change, err := parseEdit(in)
	return in, change, err
}

func (a *API) locked(r *http.Request, id string) func(pgx.Tx, access.Principal) (Definition, error) {
	return func(tx pgx.Tx, p access.Principal) (Definition, error) {
		current, err := lock(r.Context(), tx, id)
		if err != nil {
			return Definition{}, err
		}
		return current, p.Project(&current.ProjectID, access.Change)
	}
}

func changeable(r *http.Request, current Definition) error {
	if err := access.Match(r, current.ETag); err != nil {
		return err
	}
	if current.RetiredAt != nil {
		return access.Fail(http.StatusConflict, "guardrail_retired", "Create a new guardrail rather than changing retained history.")
	}
	return nil
}

func (a *API) save(r *http.Request, tx pgx.Tx, p access.Principal, current *Definition, change edit, project string, kind guardrails.Kind, configuration json.RawMessage) (Definition, error) {
	ctx := r.Context()
	draft, err := change.draft(project, kind)
	if err != nil {
		return Definition{}, err
	}
	saved, err := Save(ctx, tx, p.UserID(), current, draft)
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" {
		return Definition{}, access.Fail(http.StatusConflict, "guardrail_exists", "Choose an unused guardrail name in this project.")
	}
	if err != nil {
		return Definition{}, err
	}
	action := "guardrail.update"
	if current == nil {
		action = "guardrail.create"
	}
	if err = access.Audit(ctx, tx, r, p.Actor(), action, "guardrail", saved.ID, "success"); err != nil {
		return Definition{}, err
	}
	value, err := load(ctx, tx, saved.ID)
	if err != nil {
		return Definition{}, err
	}
	if acknowledgesEgress(configuration) {
		if err = access.Audit(ctx, tx, r, p.Actor(), "guardrail.content_egress_acknowledged", "guardrail", saved.ID, "success"); err != nil {
			return Definition{}, err
		}
	}
	return value, nil
}
