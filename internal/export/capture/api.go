package capture

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/export/destination"
)

type API struct {
	server *access.Server
}

func NewAPI(server *access.Server) *API {
	return &API{server: server}
}

func (a *API) Register(mux *http.ServeMux) {
	a.server.Route(mux, "GET /api/v1/observability/capture-sinks", a.sinks)
	a.server.Route(mux, "GET /api/v1/observability/capture", a.configuration)
	a.server.Route(mux, "PATCH /api/v1/observability/capture", a.updateConfiguration)
	a.server.Route(mux, "GET /api/v1/observability/capture-policies", a.list)
	a.server.Route(mux, "POST /api/v1/observability/capture-policies", a.create)
	a.server.Route(mux, "GET /api/v1/observability/capture-policies/{capture_policy_id}", a.detail)
	a.server.Route(mux, "PATCH /api/v1/observability/capture-policies/{capture_policy_id}", a.update)
	a.server.Route(mux, "DELETE /api/v1/observability/capture-policies/{capture_policy_id}", a.delete)
}

type sinkOption struct {
	ID   string           `json:"export_sink_id"`
	Name string           `json:"name"`
	Type destination.Type `json:"type"`
}

type configuration struct {
	Enabled bool   `json:"enabled"`
	ETag    string `json:"etag"`
}

type redactNames []string

func (n *redactNames) UnmarshalJSON(raw []byte) error {
	var names []string
	if err := json.Unmarshal(raw, &names); err != nil {
		return err
	}
	if names == nil {
		return errors.New("redact must be an array")
	}
	*n = names
	return nil
}

type createRequest struct {
	ProjectID   *string     `json:"project_id"`
	Route       *string     `json:"route_slug"`
	SinkID      string      `json:"sink"`
	SampleRatio string      `json:"sample_ratio"`
	Include     []Include   `json:"include"`
	Redact      redactNames `json:"redact"`
	KeyIDs      []string    `json:"key_ids"`
	EndUsers    []string    `json:"end_user_digests"`
	MaxBytes    int         `json:"max_bytes"`
	Enabled     *bool       `json:"enabled"`
}

func (c createRequest) settings() Settings {
	return Settings{
		ProjectID: c.ProjectID, Route: c.Route, SinkID: c.SinkID, SampleRatio: c.SampleRatio, Include: c.Include,
		Redact: orEmpty(c.Redact), KeyIDs: orEmpty(c.KeyIDs), EndUsers: orEmpty(c.EndUsers), MaxBytes: c.MaxBytes,
		Enabled: c.Enabled == nil || *c.Enabled,
	}
}

type updateRequest struct {
	SinkID      *string     `json:"sink"`
	SampleRatio *string     `json:"sample_ratio"`
	Include     []Include   `json:"include"`
	Redact      redactNames `json:"redact"`
	KeyIDs      []string    `json:"key_ids"`
	EndUsers    []string    `json:"end_user_digests"`
	MaxBytes    *int        `json:"max_bytes"`
	Enabled     *bool       `json:"enabled"`
}

func (u updateRequest) apply(s Settings) Settings {
	return Settings{
		ProjectID: s.ProjectID, Route: s.Route, SinkID: valueOr(u.SinkID, s.SinkID), SampleRatio: valueOr(u.SampleRatio, s.SampleRatio),
		Include: patched(u.Include, s.Include), Redact: patched([]string(u.Redact), s.Redact), KeyIDs: patched(u.KeyIDs, s.KeyIDs),
		EndUsers: patched(u.EndUsers, s.EndUsers), MaxBytes: valueOr(u.MaxBytes, s.MaxBytes), Enabled: valueOr(u.Enabled, s.Enabled),
	}
}

func (u updateRequest) disablesOnly() bool {
	return u.Enabled != nil && !*u.Enabled && u.SinkID == nil && u.SampleRatio == nil && u.Include == nil &&
		u.Redact == nil && u.KeyIDs == nil && u.EndUsers == nil && u.MaxBytes == nil
}

type resolved struct {
	project   *string
	sink      string
	detectors *string
}

func (a *API) sinks(r *http.Request, _ access.Principal) (access.Reply, error) {
	rows, err := a.server.Pool.Query(r.Context(), sinkOptionsSQL)
	if err != nil {
		return access.Reply{}, err
	}
	options, err := pgx.CollectRows(rows, pgx.RowToStructByPos[sinkOption])
	if err != nil {
		return access.Reply{}, err
	}
	return access.OK(map[string][]sinkOption{"items": options}), nil
}

func readConfiguration(ctx context.Context, q access.Queryer, query string) (configuration, error) {
	var c configuration
	err := q.QueryRow(ctx, query).Scan(&c.Enabled, &c.ETag)
	return c, err
}

func (a *API) configuration(r *http.Request, _ access.Principal) (access.Reply, error) {
	c, err := readConfiguration(r.Context(), a.server.Pool, configurationSQL)
	if err != nil {
		return access.Reply{}, err
	}
	return access.Detail(c, c.ETag), nil
}

func (a *API) updateConfiguration(r *http.Request, _ access.Principal) (access.Reply, error) {
	var input struct {
		Enabled *bool `json:"enabled"`
	}
	if err := access.Decode(r, &input); err != nil {
		return access.Reply{}, err
	}
	if input.Enabled == nil {
		return access.Reply{}, access.Invalid("enabled", "Send whether capture is enabled.")
	}
	tx, p, err := a.server.BeginMutation(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	if err := p.Authorize(access.Access); err != nil {
		return access.Reply{}, err
	}
	current, err := readConfiguration(r.Context(), tx, lockConfigurationSQL)
	if err != nil {
		return access.Reply{}, err
	}
	if err := access.Match(r, current.ETag); err != nil {
		return access.Reply{}, err
	}
	next := configuration{Enabled: *input.Enabled, ETag: access.NewID()}
	if _, err := tx.Exec(r.Context(), updateConfigurationSQL, next.Enabled, next.ETag, p.UserID()); err != nil {
		return access.Reply{}, err
	}
	if err := access.AuditForProject(r.Context(), tx, r, p.Actor(), "capture_configuration.update", "capture_configuration", "singleton", "success", nil); err != nil {
		return access.Reply{}, err
	}
	return access.Detail(next, next.ETag), tx.Commit(r.Context())
}

func (a *API) list(r *http.Request, p access.Principal) (access.Reply, error) {
	page, err := access.Page(r)
	if err != nil {
		return access.Reply{}, err
	}
	rows, err := a.server.Pool.Query(r.Context(), listPoliciesSQL, page.Before, p.AllProjects, p.ProjectIDs(), page.Limit+1)
	if err != nil {
		return access.Reply{}, err
	}
	policies, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Policy, error) {
		var raw []byte
		if err := row.Scan(&raw); err != nil {
			return Policy{}, err
		}
		return decodePolicy(raw)
	})
	if err != nil {
		return access.Reply{}, err
	}
	return access.ListReplyBy(policies, page, func(p Policy) string { return p.ID }), nil
}

func (a *API) detail(r *http.Request, p access.Principal) (access.Reply, error) {
	policy, err := readPolicy(r.Context(), a.server.Pool, getPolicySQL, r.PathValue("capture_policy_id"))
	if err != nil {
		return access.Reply{}, err
	}
	if err := p.Project(policy.ProjectID, access.View); err != nil {
		return access.Reply{}, err
	}
	return access.Detail(policy, policy.ETag), nil
}

func (a *API) create(r *http.Request, _ access.Principal) (access.Reply, error) {
	var input createRequest
	if err := access.Decode(r, &input); err != nil {
		return access.Reply{}, err
	}
	settings := input.settings()
	tx, p, err := a.server.BeginMutation(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	scope, err := admit(r.Context(), tx, settings)
	if err != nil {
		return access.Reply{}, err
	}
	if err := destination.RequireScope(r.Context(), a.server, tx, p, scope.project); err != nil {
		return access.Reply{}, err
	}
	claim, replayed, err := a.server.Replay(r, tx, p, input)
	if err != nil {
		return access.Reply{}, err
	}
	if replayed != nil {
		return *replayed, tx.Commit(r.Context())
	}
	pins, err := pinFor(r.Context(), tx, scope, settings.Redact)
	if err != nil {
		return access.Reply{}, err
	}
	var count int64
	if err := tx.QueryRow(r.Context(), countPoliciesSQL).Scan(&count); err != nil {
		return access.Reply{}, err
	}
	if count >= maxPolicies {
		return access.Reply{}, access.Invalid("capture_policies", "This installation already has 1000 capture policies.")
	}
	id := access.NewID()
	if _, err := tx.Exec(r.Context(), createPolicySQL, id, scope.project, settings.Route, scope.sink, settings.SampleRatio, settings.Include,
		settings.Redact, settings.MaxBytes, settings.Enabled, access.NewID(), p.UserID(), settings.KeyIDs, settings.EndUsers); err != nil {
		return access.Reply{}, err
	}
	if err := storePins(r.Context(), tx, id, pins); err != nil {
		return access.Reply{}, err
	}
	if err := access.AuditForProject(r.Context(), tx, r, p.Actor(), "capture_policy.create", "capture_policy", id, "success", scope.project); err != nil {
		return access.Reply{}, err
	}
	policy, err := readPolicy(r.Context(), tx, getPolicySQL, id)
	if err != nil {
		return access.Reply{}, err
	}
	result := access.Reply{Status: http.StatusCreated, ETag: policy.ETag, Location: "/api/v1/observability/capture-policies/" + id, Body: policy}
	if err := a.server.CompleteReplay(r, tx, claim, result); err != nil {
		return access.Reply{}, err
	}
	return result, tx.Commit(r.Context())
}

func (a *API) update(r *http.Request, _ access.Principal) (access.Reply, error) {
	var input updateRequest
	if err := access.Decode(r, &input); err != nil {
		return access.Reply{}, err
	}
	tx, p, err := a.server.BeginMutation(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	current, err := a.lock(r, tx, p)
	if err != nil {
		return access.Reply{}, err
	}
	settings := input.apply(current.Settings)
	sink, err := revise(r.Context(), tx, input, current, settings)
	if err != nil {
		return access.Reply{}, err
	}
	if _, err := tx.Exec(r.Context(), updatePolicySQL, current.ID, sink, settings.SampleRatio, settings.Include, settings.Redact,
		settings.MaxBytes, settings.Enabled, access.NewID(), settings.KeyIDs, settings.EndUsers); err != nil {
		return access.Reply{}, err
	}
	if err := access.AuditForProject(r.Context(), tx, r, p.Actor(), "capture_policy.update", "capture_policy", current.ID, "success", current.ProjectID); err != nil {
		return access.Reply{}, err
	}
	policy, err := readPolicy(r.Context(), tx, getPolicySQL, current.ID)
	if err != nil {
		return access.Reply{}, err
	}
	return access.Detail(policy, policy.ETag), tx.Commit(r.Context())
}

func revise(ctx context.Context, tx pgx.Tx, input updateRequest, current Policy, settings Settings) (string, error) {
	if input.disablesOnly() {
		return current.SinkID, nil
	}
	scope, err := admit(ctx, tx, settings)
	if err != nil {
		return "", err
	}
	if input.Redact == nil {
		return scope.sink, nil
	}
	pins, err := pinFor(ctx, tx, scope, settings.Redact)
	if err != nil {
		return "", err
	}
	return scope.sink, storePins(ctx, tx, current.ID, pins)
}

func (a *API) delete(r *http.Request, _ access.Principal) (access.Reply, error) {
	tx, p, err := a.server.BeginMutation(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	current, err := a.lock(r, tx, p)
	if err != nil {
		return access.Reply{}, err
	}
	if _, err := tx.Exec(r.Context(), deletePolicySQL, current.ID); err != nil {
		return access.Reply{}, err
	}
	if err := access.AuditForProject(r.Context(), tx, r, p.Actor(), "capture_policy.delete", "capture_policy", current.ID, "success", current.ProjectID); err != nil {
		return access.Reply{}, err
	}
	return access.Reply{Status: http.StatusNoContent}, tx.Commit(r.Context())
}

func (a *API) lock(r *http.Request, tx pgx.Tx, p access.Principal) (Policy, error) {
	current, err := readPolicy(r.Context(), tx, lockPolicySQL, r.PathValue("capture_policy_id"))
	if err != nil {
		return Policy{}, err
	}
	if err := p.Project(current.ProjectID, access.View); err != nil {
		return Policy{}, err
	}
	if err := destination.RequireScope(r.Context(), a.server, tx, p, current.ProjectID); err != nil {
		return Policy{}, err
	}
	return current, access.Match(r, current.ETag)
}

func readPolicy(ctx context.Context, q access.Queryer, query, id string) (Policy, error) {
	var raw []byte
	if err := q.QueryRow(ctx, query, id).Scan(&raw); err != nil {
		return Policy{}, access.NotFoundOr(err)
	}
	return decodePolicy(raw)
}

func decodePolicy(raw []byte) (Policy, error) {
	var p Policy
	err := json.Unmarshal(raw, &p)
	return p, err
}

func admit(ctx context.Context, q access.Queryer, s Settings) (resolved, error) {
	var enabled configuration
	if err := q.QueryRow(ctx, configurationSQL).Scan(&enabled.Enabled, &enabled.ETag); err != nil {
		return resolved{}, err
	}
	if !enabled.Enabled {
		return resolved{}, access.Invalid("capture", "Enable payload capture before managing capture policies.")
	}
	if err := s.validate(); err != nil {
		return resolved{}, err
	}
	return check(ctx, q, s)
}

func check(ctx context.Context, q access.Queryer, s Settings) (resolved, error) {
	if s.SinkID == "" {
		return resolved{}, access.Invalid("sink", "capture requires an installation-owned sink")
	}
	sink, err := access.ParseUUID(s.SinkID)
	if err != nil {
		return resolved{}, access.Invalid("sink", "sink must be a valid UUID")
	}
	project, err := checkProject(ctx, q, s.ProjectID)
	if err != nil {
		return resolved{}, err
	}
	if err := checkSink(ctx, q, sink); err != nil {
		return resolved{}, err
	}
	routeProject, err := checkRoute(ctx, q, s.Route, project)
	if err != nil {
		return resolved{}, err
	}
	if err := checkKeys(ctx, q, s.KeyIDs, project); err != nil {
		return resolved{}, err
	}
	if project != nil {
		return resolved{project: project, sink: sink, detectors: project}, nil
	}
	return resolved{sink: sink, detectors: routeProject}, nil
}

func checkProject(ctx context.Context, q access.Queryer, raw *string) (*string, error) {
	if raw == nil {
		return nil, nil
	}
	project, err := access.ParseUUID(*raw)
	if err != nil {
		return nil, access.Invalid("project_id", "project_id must be a valid UUID")
	}
	exists, err := destination.ProjectExists(ctx, q, project)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, access.Invalid("project_id", "the policy project does not exist")
	}
	return &project, nil
}

func checkSink(ctx context.Context, q access.Queryer, sink string) error {
	var owned, capturable bool
	err := q.QueryRow(ctx, sinkScopeSQL, sink).Scan(&owned, &capturable)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !owned {
		return access.Invalid("sink", "capture requires an installation-owned enabled sink")
	}
	if err != nil {
		return err
	}
	if !capturable {
		return access.Invalid("sink", "capture requires a json, jsonl or otlp sink")
	}
	return nil
}

func checkRoute(ctx context.Context, q access.Queryer, route, project *string) (*string, error) {
	if route == nil {
		return nil, nil
	}
	routeProject, err := destination.RouteProject(ctx, q, *route)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, access.Invalid("route_slug", "the policy route does not exist")
	}
	if err != nil {
		return nil, err
	}
	if project != nil && routeProject != nil && *routeProject != *project {
		return nil, access.Invalid("route_slug", "the policy route must belong to the policy project")
	}
	return routeProject, nil
}

func checkKeys(ctx context.Context, q access.Queryer, keys []string, project *string) error {
	if len(keys) == 0 {
		return nil
	}
	rows, err := q.Query(ctx, keyProjectsSQL, keys)
	if err != nil {
		return err
	}
	owners, err := pgx.CollectRows(rows, pgx.RowToStructByPos[keyOwner])
	if err != nil {
		return err
	}
	for _, key := range keys {
		i := slices.IndexFunc(owners, func(o keyOwner) bool { return o.ID == key })
		if i < 0 {
			return access.Invalid("key_ids", fmt.Sprintf("key %s does not exist", key))
		}
		if project != nil && (owners[i].Project == nil || *owners[i].Project != *project) {
			return access.Invalid("key_ids", fmt.Sprintf("key %s does not belong to the policy project", key))
		}
	}
	return nil
}

type keyOwner struct {
	ID      string
	Project *string
}

func pinFor(ctx context.Context, q access.Queryer, scope resolved, names []string) ([]Redactor, error) {
	if len(names) == 0 {
		return []Redactor{}, nil
	}
	if scope.detectors == nil {
		return nil, access.Invalid("redact", "redactors require a project-scoped policy or route")
	}
	return pin(ctx, q, *scope.detectors, names)
}

func orEmpty[T any](values []T) []T {
	if values == nil {
		return []T{}
	}
	return values
}

func patched[T any](patch, current []T) []T {
	if patch == nil {
		return current
	}
	return patch
}

func valueOr[T any](value *T, fallback T) T {
	if value == nil {
		return fallback
	}
	return *value
}
