package routes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/runtime"
)

// check verifies that every target can serve the draft's operations from its
// provider's activated revision.
func check(d *draft, live map[string]*resolved) error {
	if err := ValidateFidelityPolicy(d.Fidelity, d.ContentPolicy); err != nil {
		return err
	}
	for _, t := range d.Targets {
		r, ok := live[t.ProviderModelID]
		label := t.ProviderName + "/" + t.ProviderModel
		switch {
		case !ok:
			return access.Fail(422, "target_unknown", "Target "+label+" no longer exists.")
		case r.ProviderState != "active":
			return access.Fail(422, "provider_not_active", "Target "+label+" belongs to a connection that is not active.")
		case !r.Published:
			return access.Fail(422, "model_not_published", "Target "+label+" is not part of the connection's activated revision.")
		}
		for _, op := range d.Operations {
			certified := false
			for key := range r.Certified {
				certified = certified || len(key) > len(op) && key[:len(op)+1] == op+"/"
			}
			if !certified {
				return access.Fail(422, "capability_not_certified", "Target "+label+" has no certified "+op+" capability.")
			}
		}
	}
	return nil
}

func (s *Server) validateDraft(r *http.Request) (access.Reply, error) {
	a := s.Access
	id, err := access.IDParam(r, "draft_id")
	if err != nil {
		return access.Reply{}, err
	}
	tx, err := a.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := a.Principal(r, tx, "configure")
	if err != nil {
		return access.Reply{}, err
	}
	current, err := loadDraft(r.Context(), tx, id, true)
	if err != nil {
		return access.Reply{}, err
	}
	if err := access.ProjectAccess(p, current.ProjectID, true); err != nil {
		return access.Reply{}, err
	}
	if err = access.Match(r, current.ETag); err != nil {
		return access.Reply{}, err
	}
	live, err := resolve(r.Context(), tx, current.Targets)
	if err != nil {
		return access.Reply{}, err
	}
	if err = check(current, live); err != nil {
		return access.Reply{}, err
	}
	if err = compileDraftExecution(r.Context(), tx, current); err != nil {
		return access.Reply{}, err
	}
	if err = ValidateFidelityMigration(r.Context(), tx, current.Slug, current.Fidelity); err != nil {
		return access.Reply{}, err
	}
	if len(current.ContentPolicy) > 0 {
		var policy contentpolicy.Policy
		if err = json.Unmarshal(current.ContentPolicy, &policy); err != nil {
			return access.Reply{}, err
		}
		if _, err = contentpolicy.Compile(&policy); err != nil {
			return access.Reply{}, access.Invalid("content_policy", err.Error())
		}
	}
	etag := access.NewID()
	if _, err = tx.Exec(r.Context(), "UPDATE olp_go.route_drafts SET state='validated',etag=$2,updated_at=now() WHERE id=$1", id, etag); err != nil {
		return access.Reply{}, err
	}
	if err = access.Audit(r.Context(), tx, r, p.ID, "route_draft.validate", "route_draft", id, "success"); err != nil {
		return access.Reply{}, err
	}
	current.State, current.ETag = "validated", etag
	return access.Commit(r, tx, access.Detail(current.summary(), etag))
}

func (s *Server) activateDraft(r *http.Request) (access.Reply, error) {
	a := s.Access
	id, err := access.IDParam(r, "draft_id")
	if err != nil {
		return access.Reply{}, err
	}
	tx, err := a.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := a.Principal(r, tx, "configure")
	if err != nil {
		return access.Reply{}, err
	}
	claim, replayed, err := a.Replay(r, tx, p, nil)
	if err != nil {
		return access.Reply{}, err
	}
	if replayed != nil {
		return access.Commit(r, tx, *replayed)
	}
	current, err := loadDraft(r.Context(), tx, id, true)
	if err != nil {
		return access.Reply{}, err
	}
	if err := access.ProjectAccess(p, current.ProjectID, true); err != nil {
		return access.Reply{}, err
	}
	if err = access.Match(r, current.ETag); err != nil {
		return access.Reply{}, err
	}
	live, err := resolve(r.Context(), tx, current.Targets)
	if err != nil {
		return access.Reply{}, err
	}
	if err = check(current, live); err != nil {
		return access.Reply{}, err
	}
	if err = requireFidelityExecution(current.Fidelity); err != nil {
		return access.Reply{}, err
	}
	if err = compileDraftExecution(r.Context(), tx, current); err != nil {
		return access.Reply{}, err
	}
	if err = ValidateFidelityMigration(r.Context(), tx, current.Slug, current.Fidelity); err != nil {
		return access.Reply{}, err
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
	err = tx.QueryRow(r.Context(), "SELECT id::text,latest_revision,project_id::text FROM olp_go.routes WHERE slug=$1 FOR UPDATE", current.Slug).Scan(&routeID, &revision, &routeProject)
	revisionID := access.NewID()
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		routeID, revision = access.NewID(), 1
		fidelity, err := runtime.DecodeFidelity(current.Fidelity)
		if err != nil {
			return access.Reply{}, err
		}
		if _, err = tx.Exec(r.Context(), "INSERT INTO olp_go.routes(id,slug,created_by,latest_revision,latest_revision_id,etag,project_id,strict_contract) VALUES($1,$2,$3,1,$4,$5,$6,$7)", routeID, current.Slug, p.UserID(), revisionID, access.NewID(), current.ProjectID, runtime.FidelityMode(fidelity) == runtime.FidelityStrict); err != nil {
			return access.Reply{}, err
		}
	case err != nil:
		return access.Reply{}, err
	case !sameProject(routeProject, current.ProjectID):
		return access.Reply{}, access.Fail(409, "route_project_mismatch", "An existing route with this slug belongs to a different project.")
	default:
		revision++
	}
	operations, _ := json.Marshal(current.Operations)
	targets, _ := json.Marshal(current.Targets)
	if _, err = tx.Exec(r.Context(), "INSERT INTO olp_go.route_revisions(id,route_id,revision,slug,operations,overall_timeout_ms,max_attempts,targets,source_draft_id,activated_by,content_policy,fidelity) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)", revisionID, routeID, revision, current.Slug, operations, current.OverallTimeoutMS, current.MaxAttempts, targets, current.ID, p.UserID(), current.ContentPolicy, current.Fidelity); err != nil {
		return access.Reply{}, err
	}
	policy, _, err := loadPolicy(r.Context(), tx, "route-draft", current.ID, false)
	if err != nil {
		return access.Reply{}, err
	}
	encodedPolicy, _ := json.Marshal(policy)
	if _, err = tx.Exec(r.Context(), "UPDATE olp_go.route_revisions SET routing_policy=$2 WHERE id=$1", revisionID, encodedPolicy); err != nil {
		return access.Reply{}, err
	}
	if _, err = tx.Exec(r.Context(), "UPDATE olp_go.routes SET latest_revision=$2,latest_revision_id=$3,state='active',retired_at=NULL,retired_by=NULL,etag=$4 WHERE id=$1", routeID, revision, revisionID, access.NewID()); err != nil {
		return access.Reply{}, err
	}
	etag := access.NewID()
	if _, err = tx.Exec(r.Context(), "UPDATE olp_go.route_drafts SET state='validated',targets=$2,etag=$3,based_on_revision_id=$4,updated_at=now() WHERE id=$1", id, targets, etag, revisionID); err != nil {
		return access.Reply{}, err
	}
	generation, err := runtime.Publish(r.Context(), tx, p.UserID())
	if err != nil {
		return access.Reply{}, err
	}
	if err = access.Audit(r.Context(), tx, r, p.ID, "route.activate", "route", routeID, "success"); err != nil {
		return access.Reply{}, err
	}
	result := access.Detail(map[string]any{"route_id": routeID, "revision_id": revisionID, "revision": revision, "draft_etag": etag, "runtime_generation": generation}, etag)
	if err = a.CompleteReplay(r, tx, claim, result); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, result)
}

type revisionRow struct {
	ID               string
	RouteID          string
	Revision         int
	Slug             string
	Operations       []string
	OverallTimeoutMS int
	MaxAttempts      int
	Targets          []runtime.PublishedTarget
	ContentPolicy    []byte
	Fidelity         []byte
	SourceDraftID    string
	ActivatedBy      string
	ActivatedAt      time.Time
	Policy           *runtime.Policy
}

const revisionColumns = "v.id::text,v.route_id::text,v.revision,v.slug,v.operations,v.overall_timeout_ms,v.max_attempts,v.targets,v.content_policy,v.source_draft_id::text,v.activated_by::text,v.activated_at,v.routing_policy,v.fidelity"

func scanRevision(row pgx.Row) (*revisionRow, error) {
	var v revisionRow
	var operations, targets, policy []byte
	if err := row.Scan(&v.ID, &v.RouteID, &v.Revision, &v.Slug, &operations, &v.OverallTimeoutMS, &v.MaxAttempts, &targets, &v.ContentPolicy, &v.SourceDraftID, &v.ActivatedBy, &v.ActivatedAt, &policy, &v.Fidelity); err != nil {
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

func loadRevision(ctx context.Context, q access.Queryer, routeID, ref string) (*revisionRow, error) {
	if n, err := strconv.Atoi(ref); err == nil {
		return scanRevision(q.QueryRow(ctx, "SELECT "+revisionColumns+" FROM olp_go.route_revisions v WHERE v.route_id=$1 AND v.revision=$2", routeID, n))
	}
	id, err := access.ParseUUID(ref)
	if err != nil {
		return nil, access.Fail(404, "not_found", "Unknown revision.")
	}
	return scanRevision(q.QueryRow(ctx, "SELECT "+revisionColumns+" FROM olp_go.route_revisions v WHERE v.route_id=$1 AND v.id=$2", routeID, id))
}

func (v *revisionRow) json(live map[string]*resolved) map[string]any {
	return map[string]any{"id": v.ID, "route_id": v.RouteID, "revision": v.Revision, "slug": v.Slug, "overall_timeout_ms": v.OverallTimeoutMS, "max_attempts": v.MaxAttempts, "source_draft_id": v.SourceDraftID, "activated_by": v.ActivatedBy, "activated_at": v.ActivatedAt, "operations": v.Operations, "targets": targetsJSON(v.Targets, live), "routing_policy": policyOrDefault(v.Policy), "content_policy": json.RawMessage(v.ContentPolicy), "fidelity": json.RawMessage(v.Fidelity)}
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

func (s *Server) routeJSON(ctx context.Context, q access.Queryer, row *routeRow) (map[string]any, error) {
	live, err := resolve(ctx, q, row.Latest.Targets)
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
const routeFrom = " FROM olp_go.routes r JOIN olp_go.users u ON u.id=r.created_by JOIN olp_go.route_revisions v ON v.id=r.latest_revision_id LEFT JOIN olp_go.projects pr ON pr.id=r.project_id"

func routeProject(ctx context.Context, q access.Queryer, id string) (*string, error) {
	var project *string
	err := q.QueryRow(ctx, "SELECT project_id::text FROM olp_go.routes WHERE id=$1", id).Scan(&project)
	return project, err
}

func scanRoute(row pgx.Row) (*routeRow, error) {
	var r routeRow
	var v revisionRow
	var operations, targets, policy []byte
	if err := row.Scan(&r.ID, &r.Slug, &r.State, &r.ETag, &r.RetiredAt, &r.RetiredBy, &r.CreatedAt, &r.RevisionCount, &r.CreatedByEmail, &r.ProjectID, &r.ProjectName, &v.ID, &v.RouteID, &v.Revision, &v.Slug, &operations, &v.OverallTimeoutMS, &v.MaxAttempts, &targets, &v.ContentPolicy, &v.SourceDraftID, &v.ActivatedBy, &v.ActivatedAt, &policy, &v.Fidelity); err != nil {
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

func (s *Server) routes(r *http.Request) (access.Reply, error) {
	p, err := s.Access.Principal(r, s.Access.Pool, "read")
	if err != nil {
		return access.Reply{}, err
	}
	page, err := access.Page(r)
	if err != nil {
		return access.Reply{}, err
	}
	rows, err := s.Access.Pool.Query(r.Context(), "SELECT "+routeColumns+routeFrom+" WHERE r.id<$1 AND ($2 OR r.project_id=ANY($3::uuid[])) ORDER BY r.id DESC LIMIT $4", page.Before, p.AllProjects, p.ProjectIDs(), page.Limit+1)
	if err != nil {
		return access.Reply{}, err
	}
	defer rows.Close()
	var routes []*routeRow
	for rows.Next() {
		row, err := scanRoute(rows)
		if err != nil {
			return access.Reply{}, err
		}
		routes = append(routes, row)
	}
	if err = rows.Err(); err != nil {
		return access.Reply{}, err
	}
	rows.Close()
	items := []map[string]any{}
	for _, row := range routes {
		item, err := s.routeJSON(r.Context(), s.Access.Pool, row)
		if err != nil {
			return access.Reply{}, err
		}
		items = append(items, item)
	}
	return access.ListReply(items, page), nil
}

func (s *Server) route(r *http.Request) (access.Reply, error) {
	p, err := s.Access.Principal(r, s.Access.Pool, "read")
	if err != nil {
		return access.Reply{}, err
	}
	id, err := access.IDParam(r, "route_id")
	if err != nil {
		return access.Reply{}, err
	}
	row, err := scanRoute(s.Access.Pool.QueryRow(r.Context(), "SELECT "+routeColumns+routeFrom+" WHERE r.id=$1", id))
	if err != nil {
		return access.Reply{}, err
	}
	if !p.CanProject(row.ProjectID, false) {
		return access.Reply{}, pgx.ErrNoRows
	}
	item, err := s.routeJSON(r.Context(), s.Access.Pool, row)
	if err != nil {
		return access.Reply{}, err
	}
	return access.OK(item), nil
}

func (s *Server) revisions(r *http.Request) (access.Reply, error) {
	p, err := s.Access.Principal(r, s.Access.Pool, "read")
	if err != nil {
		return access.Reply{}, err
	}
	id, err := access.IDParam(r, "route_id")
	if err != nil {
		return access.Reply{}, err
	}
	page, err := access.Page(r)
	if err != nil {
		return access.Reply{}, err
	}
	project, err := routeProject(r.Context(), s.Access.Pool, id)
	if err != nil {
		return access.Reply{}, err
	}
	if !p.CanProject(project, false) {
		return access.Reply{}, pgx.ErrNoRows
	}
	rows, err := s.Access.Pool.Query(r.Context(), "SELECT "+revisionColumns+" FROM olp_go.route_revisions v WHERE v.route_id=$1 AND v.id<$2 ORDER BY v.id DESC LIMIT $3", id, page.Before, page.Limit+1)
	if err != nil {
		return access.Reply{}, err
	}
	defer rows.Close()
	var revisions []*revisionRow
	var all []runtime.PublishedTarget
	for rows.Next() {
		v, err := scanRevision(rows)
		if err != nil {
			return access.Reply{}, err
		}
		revisions = append(revisions, v)
		all = append(all, v.Targets...)
	}
	if err = rows.Err(); err != nil {
		return access.Reply{}, err
	}
	rows.Close()
	live, err := resolve(r.Context(), s.Access.Pool, all)
	if err != nil {
		return access.Reply{}, err
	}
	items := []map[string]any{}
	for _, v := range revisions {
		items = append(items, v.json(live))
	}
	return access.ListReply(items, page), nil
}

func (s *Server) revision(r *http.Request) (access.Reply, error) {
	p, err := s.Access.Principal(r, s.Access.Pool, "read")
	if err != nil {
		return access.Reply{}, err
	}
	id, err := access.IDParam(r, "route_id")
	if err != nil {
		return access.Reply{}, err
	}
	project, err := routeProject(r.Context(), s.Access.Pool, id)
	if err != nil {
		return access.Reply{}, err
	}
	if !p.CanProject(project, false) {
		return access.Reply{}, pgx.ErrNoRows
	}
	v, err := loadRevision(r.Context(), s.Access.Pool, id, r.PathValue("revision_id"))
	if err != nil {
		return access.Reply{}, err
	}
	live, err := resolve(r.Context(), s.Access.Pool, v.Targets)
	if err != nil {
		return access.Reply{}, err
	}
	return access.OK(v.json(live)), nil
}

func (s *Server) retireRoute(r *http.Request) (access.Reply, error) {
	a := s.Access
	id, err := access.IDParam(r, "route_id")
	if err != nil {
		return access.Reply{}, err
	}
	tx, err := a.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := a.Principal(r, tx, "configure")
	if err != nil {
		return access.Reply{}, err
	}
	claim, replayed, err := a.Replay(r, tx, p, nil)
	if err != nil {
		return access.Reply{}, err
	}
	if replayed != nil {
		return access.Commit(r, tx, *replayed)
	}
	var state, current string
	var project *string
	if err = tx.QueryRow(r.Context(), "SELECT state,etag::text,project_id::text FROM olp_go.routes WHERE id=$1 FOR UPDATE", id).Scan(&state, &current, &project); err != nil {
		return access.Reply{}, err
	}
	if err := access.ProjectAccess(p, project, true); err != nil {
		return access.Reply{}, err
	}
	if err = access.Match(r, current); err != nil {
		return access.Reply{}, err
	}
	if state == "retired" {
		return access.Reply{}, access.Fail(409, "route_retired", "This route is already retired.")
	}
	etag := access.NewID()
	if _, err = tx.Exec(r.Context(), "UPDATE olp_go.routes SET state='retired',retired_at=now(),retired_by=$2,etag=$3 WHERE id=$1", id, p.UserID(), etag); err != nil {
		return access.Reply{}, err
	}
	generation, err := runtime.Publish(r.Context(), tx, p.UserID())
	if err != nil {
		return access.Reply{}, err
	}
	if err = access.Audit(r.Context(), tx, r, p.ID, "route.retire", "route", id, "success"); err != nil {
		return access.Reply{}, err
	}
	result := access.Detail(map[string]any{"etag": etag, "runtime_generation": generation}, etag)
	if err = a.CompleteReplay(r, tx, claim, result); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, result)
}

func (s *Server) revisionDiff(r *http.Request) (access.Reply, error) {
	p, err := s.Access.Principal(r, s.Access.Pool, "read")
	if err != nil {
		return access.Reply{}, err
	}
	id, err := access.IDParam(r, "route_id")
	if err != nil {
		return access.Reply{}, err
	}
	project, err := routeProject(r.Context(), s.Access.Pool, id)
	if err != nil {
		return access.Reply{}, err
	}
	if !p.CanProject(project, false) {
		return access.Reply{}, pgx.ErrNoRows
	}
	from, err := loadRevision(r.Context(), s.Access.Pool, id, r.URL.Query().Get("from"))
	if err != nil {
		return access.Reply{}, err
	}
	to, err := loadRevision(r.Context(), s.Access.Pool, id, r.URL.Query().Get("to"))
	if err != nil {
		return access.Reply{}, err
	}
	label := func(t runtime.PublishedTarget) string { return t.ProviderName + "/" + t.ProviderModel }
	before := map[string]runtime.PublishedTarget{}
	for _, t := range from.Targets {
		before[t.ProviderModelID] = t
	}
	after := map[string]runtime.PublishedTarget{}
	for _, t := range to.Targets {
		after[t.ProviderModelID] = t
	}
	added, removed, changed := []string{}, []string{}, []string{}
	for key, t := range after {
		prev, ok := before[key]
		switch {
		case !ok:
			added = append(added, label(t))
		case prev.Priority != t.Priority || prev.Weight != t.Weight || prev.TimeoutMS != t.TimeoutMS:
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
	return access.OK(map[string]any{
		"from_revision": from.Revision, "to_revision": to.Revision,
		"slug_changed": from.Slug != to.Slug, "timeout_changed": from.OverallTimeoutMS != to.OverallTimeoutMS, "max_attempts_changed": from.MaxAttempts != to.MaxAttempts,
		"operations_added": opsAdded, "operations_removed": opsRemoved,
		"targets_added": added, "targets_removed": removed, "targets_changed": changed,
		"routing_policy_changed": !samePolicy(from.Policy, to.Policy), "routing_policy_before": policyOrDefault(from.Policy), "routing_policy_after": policyOrDefault(to.Policy),
		"content_policy_changed": !bytes.Equal(bytes.TrimSpace(from.ContentPolicy), bytes.TrimSpace(to.ContentPolicy)),
		"fidelity_changed":       !bytes.Equal(bytes.TrimSpace(from.Fidelity), bytes.TrimSpace(to.Fidelity)), "fidelity_before": json.RawMessage(from.Fidelity), "fidelity_after": json.RawMessage(to.Fidelity),
	}), nil
}

func (s *Server) restoreRevision(r *http.Request) (access.Reply, error) {
	a := s.Access
	id, err := access.IDParam(r, "route_id")
	if err != nil {
		return access.Reply{}, err
	}
	ref := r.PathValue("revision_id")
	tx, err := a.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := a.Principal(r, tx, "configure")
	if err != nil {
		return access.Reply{}, err
	}
	claim, replayed, err := a.Replay(r, tx, p, nil)
	if err != nil {
		return access.Reply{}, err
	}
	if replayed != nil {
		return access.Commit(r, tx, *replayed)
	}
	project, err := routeProject(r.Context(), tx, id)
	if err != nil {
		return access.Reply{}, err
	}
	if err := access.ProjectAccess(p, project, true); err != nil {
		return access.Reply{}, err
	}
	v, err := loadRevision(r.Context(), tx, id, ref)
	if err != nil {
		return access.Reply{}, err
	}
	if err = ValidateFidelityMigration(r.Context(), tx, v.Slug, v.Fidelity); err != nil {
		return access.Reply{}, err
	}
	draftID, etag := access.NewID(), access.NewID()
	operations, _ := json.Marshal(v.Operations)
	targets := slices.Clone(v.Targets)
	for i := range targets {
		targets[i].ID = access.NewID()
	}
	encoded, _ := json.Marshal(targets)
	if _, err = tx.Exec(r.Context(), "INSERT INTO olp_go.route_drafts(id,slug,state,operations,overall_timeout_ms,max_attempts,targets,content_policy,based_on_revision_id,etag,created_by,project_id,fidelity) VALUES($1,$2,'draft',$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)", draftID, v.Slug, operations, v.OverallTimeoutMS, v.MaxAttempts, encoded, v.ContentPolicy, v.ID, etag, p.UserID(), project, v.Fidelity); err != nil {
		return access.Reply{}, err
	}
	if v.Policy != nil {
		policy, _ := json.Marshal(v.Policy)
		if _, err = tx.Exec(r.Context(), "INSERT INTO olp_go.routing_policies(scope,scope_id,policy,etag,updated_by) VALUES('route-draft',$1,$2,$3,$4)", draftID, policy, etag, p.UserID()); err != nil {
			return access.Reply{}, err
		}
	}
	if err = access.Audit(r.Context(), tx, r, p.ID, "route_draft.restore", "route_draft", draftID, "success"); err != nil {
		return access.Reply{}, err
	}
	d, err := loadDraft(r.Context(), tx, draftID, false)
	if err != nil {
		return access.Reply{}, err
	}
	result, err := s.draftDetail(r.Context(), tx, d)
	if err != nil {
		return access.Reply{}, err
	}
	result.Status, result.Location = 201, "/api/v1/route-drafts/"+draftID
	if err = a.CompleteReplay(r, tx, claim, result); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, result)
}
