package credential

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/authz"
	"github.com/tyk-swe/olp/internal/guardrails"
)

type API struct{ server *access.Server }

func NewAPI(server *access.Server) *API { return &API{server: server} }

func (a *API) Register(mux *http.ServeMux) {
	a.server.Route(mux, "GET /api/v1/guardrail-credentials", a.list)
	a.server.Route(mux, "POST /api/v1/guardrail-credentials", a.create, access.MaxBody(256<<10))
	a.server.Route(mux, "GET /api/v1/guardrail-credentials/{credential_id}", a.get)
	a.server.Route(mux, "DELETE /api/v1/guardrail-credentials/{credential_id}", a.delete)
}

type metadata struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"project_id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

type created struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	ProjectID string `json:"project_id"`
}

func (a *API) list(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	page, err := httpkit.Page(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	rows, err := a.server.Pool.Query(r.Context(), "SELECT id::text,project_id::text,name,created_at FROM olp.guardrail_credentials WHERE id<$1 AND ($2 OR project_id=ANY($3::uuid[])) ORDER BY id DESC LIMIT $4", page.Before, p.AllProjects, p.ProjectIDs(), page.Limit+1)
	if err != nil {
		return httpkit.Reply{}, err
	}
	defer rows.Close()
	items, err := pgx.CollectRows(rows, pgx.RowToStructByPos[metadata])
	if err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.ListReplyBy(items, page, func(item metadata) string { return item.ID }), nil
}

func (a *API) get(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "credential_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	var value metadata
	if err := a.server.Pool.QueryRow(r.Context(), "SELECT id::text,project_id::text,name,created_at FROM olp.guardrail_credentials WHERE id=$1", id).Scan(&value.ID, &value.ProjectID, &value.Name, &value.CreatedAt); err != nil {
		return httpkit.Reply{}, err
	}
	if err := p.Project(&value.ProjectID, authz.View); err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.Detail(value, value.ID), nil
}

func (a *API) create(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	var in struct {
		ProjectID  string          `json:"project_id"`
		Name       string          `json:"name"`
		Credential json.RawMessage `json:"credential"`
	}
	if err := httpkit.DecodeUnique(r, &in, 256<<10); err != nil {
		return httpkit.Reply{}, err
	}
	if guardrails.ValidateCredential(in.Credential) != nil || in.Name == "" || len(in.Name) > 100 {
		return httpkit.Reply{}, httpkit.Invalid("credential", "Supply a named credential object.")
	}
	ctx := r.Context()
	return access.Mutate(a.server, r, in, func(tx pgx.Tx, p authz.Principal) (struct{}, error) {
		return struct{}{}, a.server.RequireProject(ctx, tx, p, &in.ProjectID)
	}, func(tx pgx.Tx, p authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		id, err := Create(ctx, tx, a.server.Keys, a.server.Installation, New{ProjectID: &in.ProjectID, Name: in.Name, Secret: in.Credential, CreatedBy: p.UserID()})
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		return httpkit.Reply{Status: http.StatusCreated, Body: created{ID: id, Name: in.Name, ProjectID: in.ProjectID}}, access.Event{Action: "guardrail.credential.create", Resource: "guardrail_credential", ID: id}, nil
	})
}

func (a *API) delete(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "credential_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	ctx := r.Context()
	return access.Remove(a.server, r, id, func(tx pgx.Tx, p authz.Principal) (string, error) {
		project, err := lock(ctx, tx, id)
		if err != nil {
			return "", err
		}
		return project, p.Project(&project, authz.Change)
	}, func(tx pgx.Tx, p authz.Principal, project string) (httpkit.Reply, access.Event, error) {
		if err := httpkit.Match(r, id); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		inUse, err := referenced(ctx, tx, id)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if inUse {
			return httpkit.Reply{}, access.Event{}, httpkit.Fail(http.StatusConflict, "guardrail_credential_in_use", "A retained guardrail revision still references this credential.")
		}
		if err := remove(ctx, tx, id); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		return httpkit.Reply{Status: http.StatusNoContent}, access.Event{Action: "guardrail.credential.delete", Resource: "guardrail_credential", ID: id, Project: &project}, nil
	})
}
