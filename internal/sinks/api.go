package sinks

import (
	"cmp"
	"encoding/json"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/domain/ids"
	"github.com/tyk-swe/olp/internal/domain/problem"
	"github.com/tyk-swe/olp/internal/platform/egress"
)

type API struct {
	server *access.Server
	egress *egress.Policy
}

func NewAPI(server *access.Server, policy *egress.Policy) *API {
	return &API{server: server, egress: policy}
}

func (a *API) Register(mux *http.ServeMux) {
	a.server.Route(mux, "GET /api/v1/sinks", a.list)
	a.server.Route(mux, "POST /api/v1/sinks", a.create)
	a.server.Route(mux, "GET /api/v1/sinks/{sink_id}", a.get)
	a.server.Route(mux, "PUT /api/v1/sinks/{sink_id}", a.update)
	a.server.Route(mux, "DELETE /api/v1/sinks/{sink_id}", a.retire)
}

type input struct {
	ProjectID   *string         `json:"project_id,omitempty"`
	Name        string          `json:"name"`
	Type        string          `json:"type,omitempty"`
	Destination string          `json:"destination"`
	Streams     []string        `json:"streams"`
	Enabled     *bool           `json:"enabled"`
	Credential  json.RawMessage `json:"credential,omitempty"`
}

func (in input) spec(project *string) Spec {
	return Spec{ProjectID: project, Name: in.Name, Type: in.Type, Destination: in.Destination, Streams: in.Streams, Enabled: *in.Enabled}
}

func decode(r *http.Request) (input, error) {
	var in input
	err := httpkit.DecodeUnique(r, &in, 64<<10)
	return in, err
}

func (a *API) list(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	page, err := httpkit.Page(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	items, err := listActive(r.Context(), a.server.Pool, page, p.AllProjects, p.ProjectIDs())
	if err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.ListReplyBy(items, page, func(d *Definition) string { return d.ID }), nil
}

func (a *API) get(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "sink_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	d, err := load(r.Context(), a.server.Pool, id, false)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if err := p.Project(d.ProjectID, authz.View); err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.Detail(d, d.ETag), nil
}

func (a *API) create(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	in, err := decode(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	normalized, invalid := normalize(in, a.egress)
	return access.Mutate(a.server, r, normalized, func(tx pgx.Tx, p authz.Principal) (struct{}, error) {
		if err := a.writable(r, tx, p, in.ProjectID); err != nil {
			return struct{}{}, err
		}
		return struct{}{}, cmp.Or(invalid, checkScope(normalized.Streams, in.ProjectID))
	}, func(tx pgx.Tx, p authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		return a.insert(r, tx, p, normalized)
	})
}

func (a *API) update(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "sink_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	in, err := decode(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	normalized, invalid := normalize(in, a.egress)
	return access.Mutate(a.server, r, normalized, func(tx pgx.Tx, p authz.Principal) (*Definition, error) {
		current, err := a.lock(r, tx, p, id)
		if err != nil {
			return nil, err
		}
		return current, cmp.Or(invalid, checkScope(normalized.Streams, current.ProjectID))
	}, func(tx pgx.Tx, _ authz.Principal, current *Definition) (httpkit.Reply, access.Event, error) {
		return a.replace(r, tx, *current, normalized)
	})
}

func (a *API) retire(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "sink_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	return access.Mutate(a.server, r, input{}, func(tx pgx.Tx, p authz.Principal) (*Definition, error) {
		return a.lock(r, tx, p, id)
	}, func(tx pgx.Tx, _ authz.Principal, current *Definition) (httpkit.Reply, access.Event, error) {
		if err := changeable(r, *current); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if err := markRetired(r.Context(), tx, current.ID, ids.New()); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if err := a.credentials().Replace(r.Context(), tx, current.ID, nil); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		return httpkit.Reply{Status: http.StatusNoContent}, access.Event{Action: "sink.retire", Resource: "sink", ID: current.ID}, nil
	})
}

func operation(project *string) authz.Operation {
	if project == nil {
		return authz.Settings
	}
	return authz.Keys
}

func (a *API) writable(r *http.Request, tx pgx.Tx, p authz.Principal, project *string) error {
	if err := p.Authorize(operation(project)); err != nil {
		return err
	}
	if project == nil {
		return nil
	}
	return a.server.RequireProject(r.Context(), tx, p, project)
}

func (a *API) lock(r *http.Request, tx pgx.Tx, p authz.Principal, id string) (*Definition, error) {
	current, err := load(r.Context(), tx, id, true)
	if err != nil {
		return nil, err
	}
	return current, a.writable(r, tx, p, current.ProjectID)
}

func changeable(r *http.Request, current Definition) error {
	if err := httpkit.Match(r, current.ETag); err != nil {
		return err
	}
	if current.RetiredAt != nil {
		return problem.Fail(409, "sink_retired", "Create a new sink instead of altering retained history.")
	}
	return nil
}

func (a *API) insert(r *http.Request, tx pgx.Tx, p authz.Principal, in input) (httpkit.Reply, access.Event, error) {
	ctx := r.Context()
	if in.Type != "https" {
		return httpkit.Reply{}, access.Event{}, problem.Invalid("type", "Use the HTTPS JSON sink type.")
	}
	count, err := countActive(ctx, tx)
	if err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	if count >= 64 {
		return httpkit.Reply{}, access.Event{}, problem.Invalid("sinks", "At most 64 active sinks are supported.")
	}
	id, etag := ids.New(), ids.New()
	if err := Insert(ctx, tx, id, in.spec(in.ProjectID), etag, p.UserID()); err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	saved, err := a.saved(r, tx, id, in.Credential)
	if err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	return httpkit.Reply{Status: http.StatusCreated, Body: saved, ETag: etag, Location: "/api/v1/sinks/" + id}, access.Event{Action: "sink.create", Resource: "sink", ID: id}, nil
}

func (a *API) replace(r *http.Request, tx pgx.Tx, current Definition, in input) (httpkit.Reply, access.Event, error) {
	if err := changeable(r, current); err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	if in.ProjectID != nil || in.Type != "" {
		return httpkit.Reply{}, access.Event{}, problem.Invalid("project_id", "Sink project and type are immutable.")
	}
	etag := ids.New()
	if err := Update(r.Context(), tx, current.ID, in.spec(current.ProjectID), etag); err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	saved, err := a.saved(r, tx, current.ID, in.Credential)
	if err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	return httpkit.Detail(saved, etag), access.Event{Action: "sink.update", Resource: "sink", ID: current.ID}, nil
}

func (a *API) saved(r *http.Request, tx pgx.Tx, id string, credential json.RawMessage) (*Definition, error) {
	if len(credential) > 0 {
		secret, err := signingCredential(credential)
		if err != nil {
			return nil, err
		}
		if err := a.credentials().Replace(r.Context(), tx, id, secret); err != nil {
			return nil, err
		}
	}
	return load(r.Context(), tx, id, false)
}

func (a *API) credentials() Credentials {
	return Credentials{Keys: a.server.Keys, Installation: a.server.Installation}
}
