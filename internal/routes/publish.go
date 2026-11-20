package routes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/authz"
	"github.com/tyk-swe/olp/internal/domain/contentpolicy"
	guardrailattachment "github.com/tyk-swe/olp/internal/guardrails/attachment"
	"github.com/tyk-swe/olp/internal/platform/database"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/routing"
)

func PassthroughAccessError(err error) error {
	if refusal, ok := errors.AsType[*routing.Refusal](err); ok {
		return httpkit.Fail(422, refusal.Code, refusal.Message)
	}
	if validation, ok := errors.AsType[*routing.ValidationError](err); ok {
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
	if len(d.ContentPolicy) > 0 && !bytes.Equal(bytes.TrimSpace(d.ContentPolicy), []byte("null")) {
		route.ContentPolicy, err = contentpolicy.Decode(d.ContentPolicy)
		if err != nil {
			return routing.Route{}, httpkit.Invalid("content_policy", err.Error())
		}
	}
	return route, nil
}

// check verifies that every target can serve the draft's operations from its
// provider's activated revision.
func check(d *draft, live map[string]*resolved) error {
	if err := ValidateFidelityPolicy(d.Fidelity, d.ContentPolicy); err != nil {
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
		if len(current.ContentPolicy) > 0 {
			var policy contentpolicy.Policy
			if err = json.Unmarshal(current.ContentPolicy, &policy); err != nil {
				return httpkit.Reply{}, access.Event{}, err
			}
			if _, err = contentpolicy.Compile(&policy); err != nil {
				return httpkit.Reply{}, access.Event{}, httpkit.Invalid("content_policy", err.Error())
			}
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
	if _, err = tx.Exec(ctx, "INSERT INTO olp.route_revisions(id,route_id,revision,slug,operations,overall_timeout_ms,max_attempts,targets,source_draft_id,activated_by,routing_policy,content_policy,fidelity,behavior) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)", revisionID, routeID, revision, current.Slug, operations, current.OverallTimeoutMS, current.MaxAttempts, targets, current.ID, actor, encodedPolicy, current.ContentPolicy, current.Fidelity, current.Behavior); err != nil {
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

type revisionRow struct {
	ID               string
	RouteID          string
	Revision         int
	Slug             string
	Operations       []string
	OverallTimeoutMS int
	MaxAttempts      int
	Targets          []routing.PublishedTarget
	ContentPolicy    []byte
	Fidelity         []byte
	Behavior         []byte
	SourceDraftID    string
	ActivatedBy      string
	ActivatedAt      time.Time
	Policy           *routing.Policy
}

const revisionColumns = "v.id::text,v.route_id::text,v.revision,v.slug,v.operations,v.overall_timeout_ms,v.max_attempts,v.targets,v.content_policy,v.source_draft_id::text,v.activated_by::text,v.activated_at,v.routing_policy,v.fidelity,v.behavior"

func scanRevision(row pgx.Row) (*revisionRow, error) {
	var v revisionRow
	var operations, targets, policy []byte
	if err := row.Scan(&v.ID, &v.RouteID, &v.Revision, &v.Slug, &operations, &v.OverallTimeoutMS, &v.MaxAttempts, &targets, &v.ContentPolicy, &v.SourceDraftID, &v.ActivatedBy, &v.ActivatedAt, &policy, &v.Fidelity, &v.Behavior); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(operations, &v.Operations); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(targets, &v.Targets); err != nil {
		return nil, err
	}
	if len(policy) > 0 {
		if err := json.Unmarshal(policy, &v.Policy); err != nil {
			return nil, err
		}
	}
	v.ActivatedAt = v.ActivatedAt.UTC()
	return &v, nil
}

func loadRevision(ctx context.Context, q database.Queryer, routeID, ref string) (*revisionRow, error) {
	if n, err := strconv.Atoi(ref); err == nil {
		return scanRevision(q.QueryRow(ctx, "SELECT "+revisionColumns+" FROM olp.route_revisions v WHERE v.route_id=$1 AND v.revision=$2", routeID, n))
	}
	id, err := httpkit.ParseUUID(ref)
	if err != nil {
		return nil, httpkit.Fail(404, "not_found", "Unknown revision.")
	}
	return scanRevision(q.QueryRow(ctx, "SELECT "+revisionColumns+" FROM olp.route_revisions v WHERE v.route_id=$1 AND v.id=$2", routeID, id))
}

func (v *revisionRow) json(live map[string]*resolved) map[string]any {
	return behaviorJSON(map[string]any{"id": v.ID, "route_id": v.RouteID, "revision": v.Revision, "slug": v.Slug, "overall_timeout_ms": v.OverallTimeoutMS, "max_attempts": v.MaxAttempts, "source_draft_id": v.SourceDraftID, "activated_by": v.ActivatedBy, "activated_at": v.ActivatedAt, "operations": v.Operations, "targets": targetsJSON(v.Targets, live), "routing_policy": policyOrDefault(v.Policy), "content_policy": json.RawMessage(v.ContentPolicy), "fidelity": json.RawMessage(v.Fidelity)}, v.Behavior)
}

type routeRow struct {
	ID             string
	Slug           string
	State          string
	ETag           string
	RetiredAt      *time.Time
	RetiredBy      *string
	CreatedAt      time.Time
	RevisionCount  int
	CreatedByEmail *string
	ProjectID      *string
	ProjectName    *string
	Latest         *revisionRow
}

func (s *Server) routeJSON(ctx context.Context, q database.Queryer, row *routeRow) (map[string]any, error) {
	live, err := s.resolve(ctx, q, row.Latest.Targets)
	if err != nil {
		return nil, err
	}
	var retiredAt any
	if row.RetiredAt != nil {
		retiredAt = row.RetiredAt.UTC()
	}
	return map[string]any{"id": row.ID, "slug": row.Slug, "state": row.State, "etag": row.ETag, "retired_at": retiredAt, "retired_by": row.RetiredBy, "created_at": row.CreatedAt.UTC(), "revision_count": row.RevisionCount, "latest_revision": row.Latest.json(live), "created_by_email": row.CreatedByEmail, "project_id": row.ProjectID, "project_name": row.ProjectName}, nil
}

const routeColumns = "r.id::text,r.slug,r.state,r.etag::text,r.retired_at,r.retired_by::text,r.created_at,r.latest_revision,u.email,r.project_id::text,pr.name," + revisionColumns
const routeFrom = " FROM olp.routes r JOIN olp.users u ON u.id=r.created_by JOIN olp.route_revisions v ON v.id=r.latest_revision_id LEFT JOIN olp.projects pr ON pr.id=r.project_id"

func routeProject(ctx context.Context, q database.Queryer, id string) (*string, error) {
	var project *string
	err := q.QueryRow(ctx, "SELECT project_id::text FROM olp.routes WHERE id=$1", id).Scan(&project)
	return project, err
}

func scanRoute(row pgx.Row) (*routeRow, error) {
	var r routeRow
	var v revisionRow
	var operations, targets, policy []byte
	if err := row.Scan(&r.ID, &r.Slug, &r.State, &r.ETag, &r.RetiredAt, &r.RetiredBy, &r.CreatedAt, &r.RevisionCount, &r.CreatedByEmail, &r.ProjectID, &r.ProjectName, &v.ID, &v.RouteID, &v.Revision, &v.Slug, &operations, &v.OverallTimeoutMS, &v.MaxAttempts, &targets, &v.ContentPolicy, &v.SourceDraftID, &v.ActivatedBy, &v.ActivatedAt, &policy, &v.Fidelity, &v.Behavior); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(operations, &v.Operations); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(targets, &v.Targets); err != nil {
		return nil, err
	}
	if len(policy) > 0 {
		if err := json.Unmarshal(policy, &v.Policy); err != nil {
			return nil, err
		}
	}
	v.ActivatedAt = v.ActivatedAt.UTC()
	r.Latest = &v
	return &r, nil
}

func (s *Server) routes(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	page, err := httpkit.Page(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	rows, err := s.Access.Pool.Query(r.Context(), "SELECT "+routeColumns+routeFrom+" WHERE r.id<$1 AND ($2 OR r.project_id=ANY($3::uuid[])) ORDER BY r.id DESC LIMIT $4", page.Before, p.AllProjects, p.ProjectIDs(), page.Limit+1)
	if err != nil {
		return httpkit.Reply{}, err
	}
	defer rows.Close()
	var routes []*routeRow
	for rows.Next() {
		row, err := scanRoute(rows)
		if err != nil {
			return httpkit.Reply{}, err
		}
		routes = append(routes, row)
	}
	if err = rows.Err(); err != nil {
		return httpkit.Reply{}, err
	}
	rows.Close()
	items := []map[string]any{}
	for _, row := range routes {
		item, err := s.routeJSON(r.Context(), s.Access.Pool, row)
		if err != nil {
			return httpkit.Reply{}, err
		}
		items = append(items, item)
	}
	return httpkit.ListReply(items, page), nil
}

func (s *Server) route(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "route_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	row, err := scanRoute(s.Access.Pool.QueryRow(r.Context(), "SELECT "+routeColumns+routeFrom+" WHERE r.id=$1", id))
	if err != nil {
		return httpkit.Reply{}, err
	}
	if err := p.Project(row.ProjectID, authz.View); err != nil {
		return httpkit.Reply{}, err
	}
	item, err := s.routeJSON(r.Context(), s.Access.Pool, row)
	if err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.Detail(item, row.ETag), nil
}

func (s *Server) revisions(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "route_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	page, err := httpkit.Page(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	project, err := routeProject(r.Context(), s.Access.Pool, id)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if err := p.Project(project, authz.View); err != nil {
		return httpkit.Reply{}, err
	}
	rows, err := s.Access.Pool.Query(r.Context(), "SELECT "+revisionColumns+" FROM olp.route_revisions v WHERE v.route_id=$1 AND v.id<$2 ORDER BY v.id DESC LIMIT $3", id, page.Before, page.Limit+1)
	if err != nil {
		return httpkit.Reply{}, err
	}
	defer rows.Close()
	var revisions []*revisionRow
	var all []routing.PublishedTarget
	for rows.Next() {
		v, err := scanRevision(rows)
		if err != nil {
			return httpkit.Reply{}, err
		}
		revisions = append(revisions, v)
		all = append(all, v.Targets...)
	}
	if err = rows.Err(); err != nil {
		return httpkit.Reply{}, err
	}
	rows.Close()
	live, err := s.resolve(r.Context(), s.Access.Pool, all)
	if err != nil {
		return httpkit.Reply{}, err
	}
	items := []map[string]any{}
	for _, v := range revisions {
		items = append(items, v.json(live))
	}
	return httpkit.ListReply(items, page), nil
}

func (s *Server) revision(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "route_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	project, err := routeProject(r.Context(), s.Access.Pool, id)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if err := p.Project(project, authz.View); err != nil {
		return httpkit.Reply{}, err
	}
	v, err := loadRevision(r.Context(), s.Access.Pool, id, r.PathValue("revision_id"))
	if err != nil {
		return httpkit.Reply{}, err
	}
	live, err := s.resolve(r.Context(), s.Access.Pool, v.Targets)
	if err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.OK(v.json(live)), nil
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

func (s *Server) revisionDiff(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "route_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	project, err := routeProject(r.Context(), s.Access.Pool, id)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if err := p.Project(project, authz.View); err != nil {
		return httpkit.Reply{}, err
	}
	from, err := loadRevision(r.Context(), s.Access.Pool, id, r.URL.Query().Get("from"))
	if err != nil {
		return httpkit.Reply{}, err
	}
	to, err := loadRevision(r.Context(), s.Access.Pool, id, r.URL.Query().Get("to"))
	if err != nil {
		return httpkit.Reply{}, err
	}
	label := func(t routing.PublishedTarget) string { return t.ProviderName + "/" + t.ProviderModel }
	before := map[string]routing.PublishedTarget{}
	for _, t := range from.Targets {
		before[t.ProviderModelID] = t
	}
	after := map[string]routing.PublishedTarget{}
	for _, t := range to.Targets {
		after[t.ProviderModelID] = t
	}
	added, removed, changed := []string{}, []string{}, []string{}
	for key, t := range after {
		prev, ok := before[key]
		switch {
		case !ok:
			added = append(added, label(t))
		case prev.Priority != t.Priority || prev.Weight != t.Weight || prev.TimeoutMS != t.TimeoutMS || !slices.Equal(prev.Tags, t.Tags) || !reflect.DeepEqual(prev.Shadow, t.Shadow):
			changed = append(changed, label(t))
		}
	}
	for key, t := range before {
		if _, ok := after[key]; !ok {
			removed = append(removed, label(t))
		}
	}
	opsAdded, opsRemoved := []string{}, []string{}
	for _, op := range to.Operations {
		if !slices.Contains(from.Operations, op) {
			opsAdded = append(opsAdded, op)
		}
	}
	for _, op := range from.Operations {
		if !slices.Contains(to.Operations, op) {
			opsRemoved = append(opsRemoved, op)
		}
	}
	for _, list := range []*[]string{&added, &removed, &changed} {
		slices.Sort(*list)
	}
	return httpkit.OK(map[string]any{
		"from_revision": from.Revision, "to_revision": to.Revision,
		"slug_changed": from.Slug != to.Slug, "timeout_changed": from.OverallTimeoutMS != to.OverallTimeoutMS, "max_attempts_changed": from.MaxAttempts != to.MaxAttempts,
		"operations_added": opsAdded, "operations_removed": opsRemoved,
		"targets_added": added, "targets_removed": removed, "targets_changed": changed,
		"routing_policy_changed": !SamePolicy(from.Policy, to.Policy), "routing_policy_before": policyOrDefault(from.Policy), "routing_policy_after": policyOrDefault(to.Policy),
		"content_policy_changed": !bytes.Equal(bytes.TrimSpace(from.ContentPolicy), bytes.TrimSpace(to.ContentPolicy)),
		"fidelity_changed":       !bytes.Equal(bytes.TrimSpace(from.Fidelity), bytes.TrimSpace(to.Fidelity)), "fidelity_before": json.RawMessage(from.Fidelity), "fidelity_after": json.RawMessage(to.Fidelity),
		"behavior_changed": !sameBehavior(from.Behavior, to.Behavior), "behavior_before": behaviorJSON(map[string]any{}, from.Behavior), "behavior_after": behaviorJSON(map[string]any{}, to.Behavior),
	}), nil
}

func (s *Server) restoreRevision(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	a := s.Access
	id, err := httpkit.IDParam(r, "route_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	ref := r.PathValue("revision_id")
	return access.Mutate(a, r, nil, func(tx pgx.Tx, p authz.Principal) (*string, error) {
		// The stored reply discloses the restored draft, so the caller must still
		// reach the route's project before a replay is served.
		project, err := routeProject(r.Context(), tx, id)
		if err != nil {
			return nil, err
		}
		if err := p.Project(project, authz.Change); err != nil {
			return nil, err
		}
		return project, nil
	}, func(tx pgx.Tx, p authz.Principal, project *string) (httpkit.Reply, access.Event, error) {
		v, err := loadRevision(r.Context(), tx, id, ref)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		behavior, err := routing.DecodeBehavior(v.Behavior)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		draftID, etag := httpkit.NewID(), httpkit.NewID()
		targets := slices.Clone(v.Targets)
		for i := range targets {
			targets[i].ID = httpkit.NewID()
		}
		content := DraftContent{
			Slug: v.Slug, Operations: v.Operations, OverallTimeoutMS: v.OverallTimeoutMS, MaxAttempts: v.MaxAttempts, Targets: targets,
			ContentPolicy: v.ContentPolicy, Fidelity: v.Fidelity, Behavior: behavior,
		}
		if err = StageDraft(r.Context(), tx, NewDraft{ID: draftID, CreatedBy: p.UserID(), ProjectID: project, BasedOn: &v.ID}, etag, content); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if err = StageDraftPolicy(r.Context(), tx, draftID, etag, p.UserID(), v.Policy); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		d, err := loadDraft(r.Context(), tx, draftID, false)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		result, err := s.draftDetail(r.Context(), tx, d)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		result.Status, result.Location = 201, "/api/v1/route-drafts/"+draftID
		return result, access.Event{Action: "route_draft.restore", Resource: "route_draft", ID: draftID}, nil
	})
}

type retirable struct{ state, etag string }
