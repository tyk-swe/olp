package routes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	guardrailattachment "github.com/tyk-swe/olp/internal/guardrails/attachment"
	"github.com/tyk-swe/olp/internal/platform/database"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/routing"
)

func PassthroughAccessError(err error) error {
	var refusal *routing.Refusal
	if errors.As(err, &refusal) {
		return httpkit.Fail(422, refusal.Code, refusal.Message)
	}
	var validation *routing.ValidationError
	if errors.As(err, &validation) {
		return httpkit.Invalid(validation.Field, validation.Message)
	}
	return err
}

func draftRoute(d *draft) (routing.Route, error) {
	fidelity, err := routing.DecodeFidelity(d.Fidelity)
	if err != nil {
		return routing.Route{}, httpkit.Invalid("fidelity", err.Error())
	}
	route := simulationRoute(d.ID, d.Slug, fidelity, d.Operations, d.OverallTimeoutMS, d.MaxAttempts, d.Targets)
	route.ProjectID = d.ProjectID
	if route.Behavior, err = routing.DecodeBehavior(d.Behavior); err != nil {
		return routing.Route{}, err
	}
	return route, nil
}

// check verifies that every target can serve the draft's operations from its
// provider's activated revision.
func check(d *draft, live map[string]*resolved) error {
	if err := ValidateFidelity(d.Fidelity); err != nil {
		return err
	}
	route, err := draftRoute(d)
	if err != nil {
		return err
	}
	if err := route.ValidatePassthrough(); err != nil {
		return PassthroughAccessError(err)
	}
	for _, t := range d.Targets {
		r, ok := live[t.ProviderModelID]
		label := t.ProviderName + "/" + t.ProviderModel
		switch {
		case !ok:
			return httpkit.Fail(422, "target_unknown", "Target "+label+" no longer exists.")
		case r.ProviderState != "active":
			return httpkit.Fail(422, "provider_not_active", "Target "+label+" belongs to a connection that is not active.")
		case !r.Published:
			return httpkit.Fail(422, "model_not_published", "Target "+label+" is not part of the connection's activated revision.")
		}
		for _, op := range d.Operations {
			if route.IsPassthrough() && op == routing.PassthroughOperation {
				continue
			}
			certified := false
			for key := range r.Certified {
				certified = certified || len(key) > len(op) && key[:len(op)+1] == op+"/"
			}
			if !certified {
				return httpkit.Fail(422, "capability_not_certified", "Target "+label+" has no certified "+op+" capability.")
			}
		}
	}
	return nil
}

// checkReferences verifies that every route a draft falls back to, delegates
// to or classifies with is active, and every plugin its selectors run is an
// approved confined plugin, so activation never dangles. A route retired
// afterwards is skipped at request time with a recorded reason.
func checkReferences(ctx context.Context, q database.Queryer, d *draft) error {
	behavior, err := routing.DecodeBehavior(d.Behavior)
	if err != nil {
		return err
	}
	references := behavior.References()

	for _, slug := range references {
		var active bool
		if err := q.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM olp.routes WHERE slug=$1 AND state='active')", slug).Scan(&active); err != nil {
			return err
		}
		if !active {
			return httpkit.Fail(422, "route_reference_unknown", "Route "+slug+" is not an active route.")
		}
	}
	for _, selector := range behavior.Selectors {
		if p := selector.When.Plugin; p != nil {
			var usable bool
			if err := q.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM olp.plugins WHERE digest=$1 AND approved_at IS NOT NULL AND module IS NOT NULL)", p.Digest).Scan(&usable); err != nil {
				return err
			}
			if !usable {
				return httpkit.Fail(422, "route_plugin_unavailable", "Selector "+selector.ID+" names a plugin that is not an approved confined plugin.")
			}
		}
	}
	return nil
}

func sameBehavior(a, b []byte) bool {
	left, _ := routing.DecodeBehavior(a)
	right, _ := routing.DecodeBehavior(b)
	return reflect.DeepEqual(left, right)
}

func (s *Server) validateDraft(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	a := s.Access
	id, err := httpkit.IDParam(r, "draft_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	return access.Transact(a, r, access.Unlocked, func(tx pgx.Tx, p authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		current, err := loadDraft(r.Context(), tx, id, true)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if err := p.Project(current.ProjectID, authz.Change); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if err = httpkit.Match(r, current.ETag); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if err = reserveOrdinarySlug(r.Context(), tx, current.Slug); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		live, err := s.resolve(r.Context(), tx, current.Targets)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if err = check(current, live); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if err = checkReferences(r.Context(), tx, current); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if err = compileDraftExecution(r.Context(), tx, current); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		etag := httpkit.NewID()
		if _, err = tx.Exec(r.Context(), "UPDATE olp.route_drafts SET state='validated',etag=$2,updated_at=now() WHERE id=$1", id, etag); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		current.State, current.ETag = "validated", etag
		return httpkit.Detail(current.summary(), etag), access.Event{Action: "route_draft.validate", Resource: "route_draft", ID: id}, nil
	})
}

func (s *Server) activateDraft(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	a := s.Access
	id, err := httpkit.IDParam(r, "draft_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	return access.Mutate(a, r, nil, func(tx pgx.Tx, p authz.Principal) (*draft, error) {
		// The stored reply discloses the draft, so the caller must still reach
		// its project before a replay is served.
		current, err := loadDraft(r.Context(), tx, id, true)
		if err != nil {
			return nil, err
		}
		if err := p.Project(current.ProjectID, authz.Change); err != nil {
			return nil, err
		}
		return current, nil
	}, func(tx pgx.Tx, p authz.Principal, current *draft) (httpkit.Reply, access.Event, error) {
		if err = httpkit.Match(r, current.ETag); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		promoted, err := s.promote(r.Context(), tx, current, p.UserID())
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		generation, err := release.Publish(r.Context(), tx, p.UserID())
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if behavior, _ := routing.DecodeBehavior(current.Behavior); behavior.Cache != nil && behavior.Cache.AllowUnsealedVectors {
			if err = access.Audit(r.Context(), tx, r, p.Actor(), "cache.semantic_acknowledge", "route", promoted.routeID, "success"); err != nil {
				return httpkit.Reply{}, access.Event{}, err
			}
		}
		result := httpkit.Detail(map[string]any{"route_id": promoted.routeID, "revision_id": promoted.revisionID, "revision": promoted.revision, "draft_etag": promoted.etag, "runtime_generation": generation}, promoted.etag)
		return result, access.Event{Action: "route.activate", Resource: "route", ID: promoted.routeID}, nil
	})
}

// promotion is the route revision a draft became.
type promotion struct {
	routeID, revisionID string
	revision            int
	etag                string
}

// promote checks a locked draft against live provider state and records it
// as its route's next revision without publishing a release, so a caller can
// promote several drafts and publish once.
func (s *Server) promote(ctx context.Context, tx pgx.Tx, current *draft, actor string) (promotion, error) {
	if err := reserveOrdinarySlug(ctx, tx, current.Slug); err != nil {
		return promotion{}, err
	}
	live, err := s.resolve(ctx, tx, current.Targets)
	if err != nil {
		return promotion{}, err
	}
	if err = check(current, live); err != nil {
		return promotion{}, err
	}
	if err = checkReferences(ctx, tx, current); err != nil {
		return promotion{}, err
	}
	if err = compileDraftExecution(ctx, tx, current); err != nil {
		return promotion{}, err
	}
	for i := range current.Targets {
		t := &current.Targets[i]
		if l := live[t.ProviderModelID]; l != nil {
			t.ProviderID, t.ProviderName, t.ProviderModel = l.ProviderID, l.ProviderName, l.ProviderModel
		}
	}
	var routeID string
	var revision int
	var routeProject *string
	err = tx.QueryRow(ctx, "SELECT id::text,latest_revision,project_id::text FROM olp.routes WHERE slug=$1 FOR UPDATE", current.Slug).Scan(&routeID, &revision, &routeProject)
	revisionID := httpkit.NewID()
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		routeID, revision = httpkit.NewID(), 1
		if _, err = tx.Exec(ctx, "INSERT INTO olp.routes(id,slug,created_by,latest_revision,latest_revision_id,etag,project_id) VALUES($1,$2,$3,1,$4,$5,$6)", routeID, current.Slug, actor, revisionID, httpkit.NewID(), current.ProjectID); err != nil {
			return promotion{}, err
		}
	case err != nil:
		return promotion{}, err
	case !sameProject(routeProject, current.ProjectID):
		return promotion{}, httpkit.Fail(409, "route_project_mismatch", "An existing route with this slug belongs to a different project.")
	default:
		revision++
	}
	operations, _ := json.Marshal(current.Operations)
	targets, _ := json.Marshal(current.Targets)
	policy, _, err := loadPolicy(ctx, tx, "route-draft", current.ID, false)
	if err != nil {
		return promotion{}, err
	}
	encodedPolicy, _ := json.Marshal(policy)
	if _, err = tx.Exec(ctx, "INSERT INTO olp.route_revisions(id,route_id,revision,slug,operations,overall_timeout_ms,max_attempts,targets,source_draft_id,activated_by,routing_policy,fidelity,behavior) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)", revisionID, routeID, revision, current.Slug, operations, current.OverallTimeoutMS, current.MaxAttempts, targets, current.ID, actor, encodedPolicy, current.Fidelity, current.Behavior); err != nil {
		return promotion{}, err
	}
	if _, err = tx.Exec(ctx, "UPDATE olp.routes SET latest_revision=$2,latest_revision_id=$3,state='active',retired_at=NULL,retired_by=NULL,etag=$4 WHERE id=$1", routeID, revision, revisionID, httpkit.NewID()); err != nil {
		return promotion{}, err
	}
	if err = guardrailattachment.Promote(ctx, tx, actor, current.ID, routeID); err != nil {
		return promotion{}, err
	}
	var exposure *bool
	if err = tx.QueryRow(ctx, "SELECT catalog_expose_upstream_models FROM olp.route_drafts WHERE id=$1", current.ID).Scan(&exposure); err != nil {
		return promotion{}, err
	}
	if exposure != nil {
		if err = SetCatalogExposure(ctx, tx, routeID, *exposure, httpkit.NewID()); err != nil {
			return promotion{}, err
		}
		if _, err = tx.Exec(ctx, "UPDATE olp.route_drafts SET catalog_expose_upstream_models=NULL WHERE id=$1", current.ID); err != nil {
			return promotion{}, err
		}
	}
	etag := httpkit.NewID()
	if _, err = tx.Exec(ctx, "UPDATE olp.route_drafts SET state='validated',targets=$2,etag=$3,based_on_revision_id=$4,updated_at=now() WHERE id=$1", current.ID, targets, etag, revisionID); err != nil {
		return promotion{}, err
	}
	return promotion{routeID: routeID, revisionID: revisionID, revision: revision, etag: etag}, nil
}

func (s *Server) retireRoute(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	a := s.Access
	id, err := httpkit.IDParam(r, "route_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	return access.Mutate(a, r, nil, func(tx pgx.Tx, p authz.Principal) (retirable, error) {
		// The stored reply discloses the route, so the caller must still reach
		// its project before a replay is served.
		var state, current string
		var project *string
		if err = tx.QueryRow(r.Context(), "SELECT state,etag::text,project_id::text FROM olp.routes WHERE id=$1 FOR UPDATE", id).Scan(&state, &current, &project); err != nil {
			return retirable{}, err
		}
		if err := p.Project(project, authz.Change); err != nil {
			return retirable{}, err
		}
		return retirable{state: state, etag: current}, nil
	}, func(tx pgx.Tx, p authz.Principal, target retirable) (httpkit.Reply, access.Event, error) {
		state, current := target.state, target.etag
		if err = httpkit.Match(r, current); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if state == "retired" {
			return httpkit.Reply{}, access.Event{}, httpkit.Fail(409, "route_retired", "This route is already retired.")
		}
		etag := httpkit.NewID()
		if _, err = tx.Exec(r.Context(), "UPDATE olp.routes SET state='retired',retired_at=now(),retired_by=$2,etag=$3 WHERE id=$1", id, p.UserID(), etag); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		generation, err := release.Publish(r.Context(), tx, p.UserID())
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		result := httpkit.Detail(map[string]any{"etag": etag, "runtime_generation": generation}, etag)
		return result, access.Event{Action: "route.retire", Resource: "route", ID: id}, nil
	})
}

type retirable struct{ state, etag string }
