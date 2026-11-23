package routes

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/platform/database"
	"github.com/tyk-swe/olp/internal/routing"
)

type revisionRow struct {
	ID               string
	RouteID          string
	Revision         int
	Slug             string
	Operations       []string
	OverallTimeoutMS int
	MaxAttempts      int
	Targets          []routing.PublishedTarget
	Fidelity         []byte
	Behavior         []byte
	SourceDraftID    string
	ActivatedBy      string
	ActivatedAt      time.Time
	Policy           *routing.Policy
}

const revisionColumns = "v.id::text,v.route_id::text,v.revision,v.slug,v.operations,v.overall_timeout_ms,v.max_attempts,v.targets,v.source_draft_id::text,v.activated_by::text,v.activated_at,v.routing_policy,v.fidelity,v.behavior"

func scanRevision(row pgx.Row) (*revisionRow, error) {
	var v revisionRow
	var operations, targets, policy []byte
	if err := row.Scan(&v.ID, &v.RouteID, &v.Revision, &v.Slug, &operations, &v.OverallTimeoutMS, &v.MaxAttempts, &targets, &v.SourceDraftID, &v.ActivatedBy, &v.ActivatedAt, &policy, &v.Fidelity, &v.Behavior); err != nil {
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
	return behaviorJSON(map[string]any{"id": v.ID, "route_id": v.RouteID, "revision": v.Revision, "slug": v.Slug, "overall_timeout_ms": v.OverallTimeoutMS, "max_attempts": v.MaxAttempts, "source_draft_id": v.SourceDraftID, "activated_by": v.ActivatedBy, "activated_at": v.ActivatedAt, "operations": v.Operations, "targets": targetsJSON(v.Targets, live), "routing_policy": policyOrDefault(v.Policy), "fidelity": json.RawMessage(v.Fidelity)}, v.Behavior)
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
	if err := row.Scan(&r.ID, &r.Slug, &r.State, &r.ETag, &r.RetiredAt, &r.RetiredBy, &r.CreatedAt, &r.RevisionCount, &r.CreatedByEmail, &r.ProjectID, &r.ProjectName, &v.ID, &v.RouteID, &v.Revision, &v.Slug, &operations, &v.OverallTimeoutMS, &v.MaxAttempts, &targets, &v.SourceDraftID, &v.ActivatedBy, &v.ActivatedAt, &policy, &v.Fidelity, &v.Behavior); err != nil {
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
		"fidelity_changed": !bytes.Equal(bytes.TrimSpace(from.Fidelity), bytes.TrimSpace(to.Fidelity)), "fidelity_before": json.RawMessage(from.Fidelity), "fidelity_after": json.RawMessage(to.Fidelity),
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
			Fidelity: v.Fidelity, Behavior: behavior,
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
