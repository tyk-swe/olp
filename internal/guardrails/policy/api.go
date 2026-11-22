package policy

import (
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/authz"
	"github.com/tyk-swe/olp/internal/guardrails"
)

type Evaluator func(*http.Request, authz.Principal, guardrails.Pinned, guardrails.Application) (guardrails.Result, error)

type BatchEvaluator func(*http.Request, authz.Principal, []guardrails.PinnedApplication) ([]guardrails.Result, error)

type API struct {
	server        *access.Server
	publish       guardrails.Publisher
	evaluate      Evaluator
	evaluateBatch BatchEvaluator
}

func NewAPI(server *access.Server, publish guardrails.Publisher, evaluate Evaluator, evaluateBatch BatchEvaluator) *API {
	return &API{server: server, publish: publish, evaluate: evaluate, evaluateBatch: evaluateBatch}
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
	a.server.Route(mux, "POST /api/v1/guardrails/run-fixtures", a.runFixtures, access.MaxBody(maxFixtureRunBytes))
}

func (a *API) list(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	page, err := httpkit.Page(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	rows, err := a.server.Pool.Query(r.Context(), selected+" WHERE p.retired_at IS NULL AND p.id<$1 AND ($2 OR p.project_id=ANY($3::uuid[])) ORDER BY p.id DESC LIMIT $4", page.Before, p.AllProjects, p.ProjectIDs(), page.Limit+1)
	if err != nil {
		return httpkit.Reply{}, err
	}
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Policy, error) { return scan(row) })
	if err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.ListReplyBy(items, page, func(item Policy) string { return item.ID }), nil
}

func (a *API) get(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	value, err := a.visible(r, p)
	if err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.Detail(value, value.ETag), nil
}

func (a *API) revision(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	value, err := a.visible(r, p)
	if err != nil {
		return httpkit.Reply{}, err
	}
	revisionID, err := httpkit.IDParam(r, "revision_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	selected, err := revisionOf(r.Context(), a.server.Pool, revisionID)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if selected.PolicyID != value.ID {
		return httpkit.Reply{}, pgx.ErrNoRows
	}
	value.RevisionID, value.Revision, value.Entries = selected.ID, selected.Revision, selected.Entries
	return httpkit.Detail(value, revisionID), nil
}

func (a *API) visible(r *http.Request, p authz.Principal) (Policy, error) {
	id, err := httpkit.IDParam(r, "policy_id")
	if err != nil {
		return Policy{}, err
	}
	value, err := load(r.Context(), a.server.Pool, id)
	if err != nil {
		return Policy{}, err
	}
	return value, p.Project(guardrails.ProjectPointer(value.ProjectID), authz.View)
}

func (a *API) create(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	in, err := decode(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	ctx := r.Context()
	return access.Mutate(a.server, r, in, func(tx pgx.Tx, p authz.Principal) (struct{}, error) {
		if in.ProjectID == "" {
			return struct{}{}, httpkit.Invalid("project_id", "Choose a project.")
		}
		return struct{}{}, a.server.RequireProject(ctx, tx, p, &in.ProjectID)
	}, func(tx pgx.Tx, p authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		value, err := a.save(r, tx, p, nil, in.ProjectID, in)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		reply := httpkit.Detail(value, value.ETag)
		reply.Status, reply.Location = http.StatusCreated, "/api/v1/guardrail-policies/"+value.ID
		return reply, access.Event{Action: "guardrail_policy.create", Resource: "guardrail_policy", ID: value.ID}, nil
	})
}

func (a *API) update(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "policy_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	in, err := decode(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	return access.Mutate(a.server, r, in, a.locked(r, id), func(tx pgx.Tx, p authz.Principal, current Policy) (httpkit.Reply, access.Event, error) {
		if err := changeable(r, current); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if in.ProjectID != "" && in.ProjectID != current.ProjectID {
			return httpkit.Reply{}, access.Event{}, httpkit.Invalid("project_id", "The owning project is immutable.")
		}
		value, err := a.save(r, tx, p, &current, current.ProjectID, in)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		return httpkit.Detail(value, value.ETag), access.Event{Action: "guardrail_policy.update", Resource: "guardrail_policy", ID: value.ID}, nil
	})
}

func (a *API) retire(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "policy_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	ctx := r.Context()
	return access.Mutate(a.server, r, input{}, a.locked(r, id), func(tx pgx.Tx, p authz.Principal, current Policy) (httpkit.Reply, access.Event, error) {
		if err := changeable(r, current); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if err := retire(ctx, tx, id); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		return httpkit.Reply{Status: http.StatusNoContent}, access.Event{Action: "guardrail_policy.retire", Resource: "guardrail_policy", ID: id}, nil
	})
}

func (a *API) apply(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	var in guardrails.Application
	if err := httpkit.DecodeUnique(r, &in, guardrails.MaxContentBytes); err != nil {
		return httpkit.Reply{}, err
	}
	if !in.Phase.Valid() {
		return httpkit.Reply{}, httpkit.Invalid("phase", "Choose a supported evaluation phase.")
	}
	if err := guardrails.ValidateSegments(in.Segments); err != nil {
		return httpkit.Reply{}, httpkit.Invalid("segments", "Supply bounded UTF-8 segments with unique identifiers.")
	}
	if uuid.Validate(in.PolicyRevisionID) != nil {
		return httpkit.Reply{}, httpkit.Invalid("policy_revision_id", "Choose an existing policy revision.")
	}
	pinned, err := a.pinnedRevision(r, p, in.PolicyRevisionID)
	if err != nil {
		return httpkit.Reply{}, err
	}
	result, err := a.evaluate(r, p, pinned, in)
	if err != nil && !errors.Is(err, guardrails.ErrBlocked) {
		return httpkit.Reply{}, err
	}
	return httpkit.Reply{Status: http.StatusOK, Body: guardrails.Applied(result, err)}, nil
}

func decode(r *http.Request) (input, error) {
	var in input
	if err := httpkit.DecodeUnique(r, &in, 128<<10); err != nil {
		return input{}, err
	}
	name, err := shape(in.Name, in.Entries)
	return input{Name: name, ProjectID: in.ProjectID, Entries: in.Entries}, err
}

func (a *API) locked(r *http.Request, id string) func(pgx.Tx, authz.Principal) (Policy, error) {
	return func(tx pgx.Tx, p authz.Principal) (Policy, error) {
		current, err := lock(r.Context(), tx, id)
		if err != nil {
			return Policy{}, err
		}
		return current, p.Project(guardrails.ProjectPointer(current.ProjectID), authz.Change)
	}
}

func changeable(r *http.Request, current Policy) error {
	if err := httpkit.Match(r, current.ETag); err != nil {
		return err
	}
	if current.RetiredAt != nil {
		return httpkit.Fail(http.StatusConflict, "guardrail_policy_retired", "Create a new policy.")
	}
	return nil
}

func (a *API) save(r *http.Request, tx pgx.Tx, p authz.Principal, current *Policy, project string, in input) (Policy, error) {
	ctx := r.Context()
	draft, err := NewDraft(ctx, tx, project, in.Name, in.Entries)
	if err != nil {
		return Policy{}, err
	}
	saved, err := Save(ctx, tx, p.UserID(), current, draft)
	if err != nil {
		return Policy{}, err
	}
	return load(ctx, tx, saved.ID)
}
