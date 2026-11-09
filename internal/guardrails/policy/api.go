package policy

import (
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/guardrails"
)

type Evaluator func(*http.Request, access.Principal, guardrails.Pinned, guardrails.Application) (guardrails.Result, error)

type API struct {
	server   *access.Server
	publish  guardrails.Publisher
	evaluate Evaluator
}

func NewAPI(server *access.Server, publish guardrails.Publisher, evaluate Evaluator) *API {
	return &API{server: server, publish: publish, evaluate: evaluate}
}

func (a *API) Register(mux *http.ServeMux) {
	a.server.Route(mux, "GET /api/v1/guardrail-policies", a.list)
	a.server.Route(mux, "POST /api/v1/guardrail-policies", a.create, access.MaxBody(128<<10))
	a.server.Route(mux, "GET /api/v1/guardrail-policies/{policy_id}", a.get)
	a.server.Route(mux, "PUT /api/v1/guardrail-policies/{policy_id}", a.update, access.MaxBody(128<<10))
	a.server.Route(mux, "DELETE /api/v1/guardrail-policies/{policy_id}", a.retire)
	a.server.Route(mux, "POST /api/v1/guardrail-policies/{policy_id}/activate", guardrails.Activation(a.server, a.publish, lineage{}, "policy_id", "guardrail_policies"))
	a.server.Route(mux, "GET /api/v1/guardrail-policies/{policy_id}/revisions/{revision_id}", a.revision)
	a.server.Route(mux, "POST /api/v1/guardrails/evaluate", a.apply, access.MaxBody(guardrails.MaxContentBytes))
}

func (a *API) list(r *http.Request, p access.Principal) (access.Reply, error) {
	page, err := access.Page(r)
	if err != nil {
		return access.Reply{}, err
	}
	rows, err := a.server.Pool.Query(r.Context(), selected+" WHERE p.retired_at IS NULL AND p.id<$1 AND ($2 OR p.project_id=ANY($3::uuid[])) ORDER BY p.id DESC LIMIT $4", page.Before, p.AllProjects, p.ProjectIDs(), page.Limit+1)
	if err != nil {
		return access.Reply{}, err
	}
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Policy, error) { return scan(row) })
	if err != nil {
		return access.Reply{}, err
	}
	return access.ListReplyBy(items, page, func(item Policy) string { return item.ID }), nil
}

func (a *API) get(r *http.Request, p access.Principal) (access.Reply, error) {
	value, err := a.visible(r, p)
	if err != nil {
		return access.Reply{}, err
	}
	return access.Detail(value, value.ETag), nil
}

func (a *API) revision(r *http.Request, p access.Principal) (access.Reply, error) {
	value, err := a.visible(r, p)
	if err != nil {
		return access.Reply{}, err
	}
	revisionID, err := access.IDParam(r, "revision_id")
	if err != nil {
		return access.Reply{}, err
	}
	selected, err := revisionOf(r.Context(), a.server.Pool, revisionID)
	if err != nil {
		return access.Reply{}, err
	}
	if selected.PolicyID != value.ID {
		return access.Reply{}, pgx.ErrNoRows
	}
	value.RevisionID, value.Revision, value.Entries = selected.ID, selected.Revision, selected.Entries
	return access.Detail(value, revisionID), nil
}

func (a *API) visible(r *http.Request, p access.Principal) (Policy, error) {
	id, err := access.IDParam(r, "policy_id")
	if err != nil {
		return Policy{}, err
	}
	value, err := load(r.Context(), a.server.Pool, id)
	if err != nil {
		return Policy{}, err
	}
	return value, p.Project(guardrails.ProjectPointer(value.ProjectID), access.View)
}

func (a *API) create(r *http.Request, _ access.Principal) (access.Reply, error) {
	in, err := decode(r)
	if err != nil {
		return access.Reply{}, err
	}
	ctx := r.Context()
	return access.Mutate(a.server, r, in, func(tx pgx.Tx, p access.Principal) (struct{}, error) {
		if in.ProjectID == "" {
			return struct{}{}, access.Invalid("project_id", "Choose a project.")
		}
		return struct{}{}, a.server.RequireProject(ctx, tx, p, &in.ProjectID)
	}, func(tx pgx.Tx, p access.Principal, _ struct{}) (access.Reply, error) {
		value, err := a.save(r, tx, p, nil, in.ProjectID, in)
		if err != nil {
			return access.Reply{}, err
		}
		reply := access.Detail(value, value.ETag)
		reply.Status, reply.Location = http.StatusCreated, "/api/v1/guardrail-policies/"+value.ID
		return reply, nil
	})
}

func (a *API) update(r *http.Request, _ access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "policy_id")
	if err != nil {
		return access.Reply{}, err
	}
	in, err := decode(r)
	if err != nil {
		return access.Reply{}, err
	}
	return access.Mutate(a.server, r, in, a.locked(r, id), func(tx pgx.Tx, p access.Principal, current Policy) (access.Reply, error) {
		if err := changeable(r, current); err != nil {
			return access.Reply{}, err
		}
		if in.ProjectID != "" && in.ProjectID != current.ProjectID {
			return access.Reply{}, access.Invalid("project_id", "The owning project is immutable.")
		}
		value, err := a.save(r, tx, p, &current, current.ProjectID, in)
		if err != nil {
			return access.Reply{}, err
		}
		return access.Detail(value, value.ETag), nil
	})
}

func (a *API) retire(r *http.Request, _ access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "policy_id")
	if err != nil {
		return access.Reply{}, err
	}
	ctx := r.Context()
	return access.Mutate(a.server, r, input{}, a.locked(r, id), func(tx pgx.Tx, p access.Principal, current Policy) (access.Reply, error) {
		if err := changeable(r, current); err != nil {
			return access.Reply{}, err
		}
		if err := retire(ctx, tx, id); err != nil {
			return access.Reply{}, err
		}
		if err := access.Audit(ctx, tx, r, p.Actor(), "guardrail_policy.retire", "guardrail_policy", id, "success"); err != nil {
			return access.Reply{}, err
		}
		return access.Reply{Status: http.StatusNoContent}, nil
	})
}

func (a *API) apply(r *http.Request, p access.Principal) (access.Reply, error) {
	var in guardrails.Application
	if err := access.DecodeUnique(r, &in, guardrails.MaxContentBytes); err != nil {
		return access.Reply{}, err
	}
	if !in.Phase.Valid() {
		return access.Reply{}, access.Invalid("phase", "Choose a supported evaluation phase.")
	}
	if err := guardrails.ValidateSegments(in.Segments); err != nil {
		return access.Reply{}, access.Invalid("segments", "Supply bounded UTF-8 segments with unique identifiers.")
	}
	if uuid.Validate(in.PolicyRevisionID) != nil {
		return access.Reply{}, access.Invalid("policy_revision_id", "Choose an existing policy revision.")
	}
	ctx := r.Context()
	selected, err := revisionOf(ctx, a.server.Pool, in.PolicyRevisionID)
	if err != nil {
		return access.Reply{}, err
	}
	if err := p.Project(guardrails.ProjectPointer(selected.ProjectID), access.View); err != nil {
		return access.Reply{}, err
	}
	ids := revisionIDs(selected.Entries)
	definitions, err := guardrails.ReadRevisions(ctx, a.server.Pool, ids)
	if err != nil {
		return access.Reply{}, err
	}
	if !owned(definitions, ids, selected.ProjectID) {
		return access.Reply{}, access.Invalid("policy", "Policy references must stay in their project.")
	}
	result, err := a.evaluate(r, p, guardrails.Pinned{Policy: selected, Definitions: definitions}, in)
	if err != nil && !errors.Is(err, guardrails.ErrBlocked) {
		return access.Reply{}, err
	}
	return access.Reply{Status: http.StatusOK, Body: guardrails.Applied(result, err)}, nil
}

func decode(r *http.Request) (input, error) {
	var in input
	if err := access.DecodeUnique(r, &in, 128<<10); err != nil {
		return input{}, err
	}
	name, err := shape(in.Name, in.Entries)
	return input{Name: name, ProjectID: in.ProjectID, Entries: in.Entries}, err
}

func (a *API) locked(r *http.Request, id string) func(pgx.Tx, access.Principal) (Policy, error) {
	return func(tx pgx.Tx, p access.Principal) (Policy, error) {
		current, err := lock(r.Context(), tx, id)
		if err != nil {
			return Policy{}, err
		}
		return current, p.Project(guardrails.ProjectPointer(current.ProjectID), access.Change)
	}
}

func changeable(r *http.Request, current Policy) error {
	if err := access.Match(r, current.ETag); err != nil {
		return err
	}
	if current.RetiredAt != nil {
		return access.Fail(http.StatusConflict, "guardrail_policy_retired", "Create a new policy.")
	}
	return nil
}

func (a *API) save(r *http.Request, tx pgx.Tx, p access.Principal, current *Policy, project string, in input) (Policy, error) {
	ctx := r.Context()
	draft, err := NewDraft(ctx, tx, project, in.Name, in.Entries)
	if err != nil {
		return Policy{}, err
	}
	saved, err := Save(ctx, tx, p.UserID(), current, draft)
	if err != nil {
		return Policy{}, err
	}
	action := "guardrail_policy.update"
	if current == nil {
		action = "guardrail_policy.create"
	}
	if err = access.Audit(ctx, tx, r, p.Actor(), action, "guardrail_policy", saved.ID, "success"); err != nil {
		return Policy{}, err
	}
	return load(ctx, tx, saved.ID)
}
