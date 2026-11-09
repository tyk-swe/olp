package credential

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
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

func (a *API) list(r *http.Request, p access.Principal) (access.Reply, error) {
	page, err := access.Page(r)
	if err != nil {
		return access.Reply{}, err
	}
	rows, err := a.server.Pool.Query(r.Context(), "SELECT id::text,project_id::text,name,created_at FROM olp.guardrail_credentials WHERE id<$1 AND ($2 OR project_id=ANY($3::uuid[])) ORDER BY id DESC LIMIT $4", page.Before, p.AllProjects, p.ProjectIDs(), page.Limit+1)
	if err != nil {
		return access.Reply{}, err
	}
	defer rows.Close()
	items, err := pgx.CollectRows(rows, pgx.RowToStructByPos[metadata])
	if err != nil {
		return access.Reply{}, err
	}
	return access.ListReplyBy(items, page, func(item metadata) string { return item.ID }), nil
}

func (a *API) get(r *http.Request, p access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "credential_id")
	if err != nil {
		return access.Reply{}, err
	}
	var value metadata
	if err := a.server.Pool.QueryRow(r.Context(), "SELECT id::text,project_id::text,name,created_at FROM olp.guardrail_credentials WHERE id=$1", id).Scan(&value.ID, &value.ProjectID, &value.Name, &value.CreatedAt); err != nil {
		return access.Reply{}, err
	}
	if err := p.Project(&value.ProjectID, access.View); err != nil {
		return access.Reply{}, err
	}
	return access.Detail(value, value.ID), nil
}

func (a *API) create(r *http.Request, _ access.Principal) (access.Reply, error) {
	var in struct {
		ProjectID  string          `json:"project_id"`
		Name       string          `json:"name"`
		Credential json.RawMessage `json:"credential"`
	}
	if err := access.DecodeUnique(r, &in, 256<<10); err != nil {
		return access.Reply{}, err
	}
	if guardrails.ValidateCredential(in.Credential) != nil || in.Name == "" || len(in.Name) > 100 {
		return access.Reply{}, access.Invalid("credential", "Supply a named credential object.")
	}
	ctx := r.Context()
	return access.Mutate(a.server, r, in, func(tx pgx.Tx, p access.Principal) (struct{}, error) {
		return struct{}{}, a.server.RequireProject(ctx, tx, p, &in.ProjectID)
	}, func(tx pgx.Tx, p access.Principal, _ struct{}) (access.Reply, error) {
		id, err := Create(ctx, tx, a.server.Keys, a.server.Installation, New{ProjectID: &in.ProjectID, Name: in.Name, Secret: in.Credential, CreatedBy: p.UserID()})
		if err != nil {
			return access.Reply{}, err
		}
		if err = access.Audit(ctx, tx, r, p.Actor(), "guardrail.credential.create", "guardrail_credential", id, "success"); err != nil {
			return access.Reply{}, err
		}
		return access.Reply{Status: http.StatusCreated, Body: created{ID: id, Name: in.Name, ProjectID: in.ProjectID}}, nil
	})
}

func (a *API) delete(r *http.Request, _ access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "credential_id")
	if err != nil {
		return access.Reply{}, err
	}
	ctx := r.Context()
	return access.Remove(a.server, r, id, func(tx pgx.Tx, p access.Principal) (string, error) {
		project, err := lock(ctx, tx, id)
		if err != nil {
			return "", err
		}
		return project, p.Project(&project, access.Change)
	}, func(tx pgx.Tx, p access.Principal, project string) (access.Reply, error) {
		if err := access.Match(r, id); err != nil {
			return access.Reply{}, err
		}
		inUse, err := referenced(ctx, tx, id)
		if err != nil {
			return access.Reply{}, err
		}
		if inUse {
			return access.Reply{}, access.Fail(http.StatusConflict, "guardrail_credential_in_use", "A retained guardrail revision still references this credential.")
		}
		if err := remove(ctx, tx, id); err != nil {
			return access.Reply{}, err
		}
		if err := access.AuditForProject(ctx, tx, r, p.Actor(), "guardrail.credential.delete", "guardrail_credential", id, "success", &project); err != nil {
			return access.Reply{}, err
		}
		return access.Reply{Status: http.StatusNoContent}, nil
	})
}
