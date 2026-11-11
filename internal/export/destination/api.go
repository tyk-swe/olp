package destination

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/usage/retention"
)

const maxSinks = 1000

type API struct {
	server *access.Server
	egress *egress.Policy
}

func NewAPI(server *access.Server, policy *egress.Policy) *API {
	return &API{server: server, egress: policy}
}

func (a *API) Register(mux *http.ServeMux) {
	a.server.Route(mux, "GET /api/v1/observability/sinks", a.list)
	a.server.Route(mux, "POST /api/v1/observability/sinks", a.create)
	a.server.Route(mux, "GET /api/v1/observability/sinks/{export_sink_id}", a.detail)
	a.server.Route(mux, "PATCH /api/v1/observability/sinks/{export_sink_id}", a.update)
	a.server.Route(mux, "DELETE /api/v1/observability/sinks/{export_sink_id}", a.delete)
	a.server.Route(mux, "GET /api/v1/observability/sinks/{export_sink_id}/gaps", a.gaps)
}

func RequireScope(ctx context.Context, server *access.Server, q access.Queryer, p access.Principal, projectID *string) error {
	if projectID == nil {
		return p.Authorize(access.Settings)
	}
	if err := p.Authorize(access.Keys); err != nil {
		return err
	}
	return server.RequireProject(ctx, q, p, projectID)
}

func (a *API) list(r *http.Request, p access.Principal) (access.Reply, error) {
	page, err := access.Page(r)
	if err != nil {
		return access.Reply{}, err
	}
	items, err := listSinks(r.Context(), a.server.Pool, page, p)
	if err != nil {
		return access.Reply{}, err
	}
	return access.ListReplyBy(items, page, func(item listed) string { return item.id }), nil
}

func (l listed) MarshalJSON() ([]byte, error) { return l.body, nil }

func (a *API) detail(r *http.Request, p access.Principal) (access.Reply, error) {
	body, etag, projectID, err := readSink(r.Context(), a.server.Pool, r.PathValue("export_sink_id"))
	if err != nil {
		return access.Reply{}, err
	}
	if err := p.Project(projectID, access.View); err != nil {
		return access.Reply{}, err
	}
	return access.Detail(body, etag), nil
}

type createRequest struct {
	ProjectID   *string         `json:"project_id"`
	Name        string          `json:"name"`
	Type        Type            `json:"type"`
	Destination string          `json:"destination"`
	Streams     []Stream        `json:"streams"`
	Format      Format          `json:"format"`
	Filter      json.RawMessage `json:"filter"`
	Settings    json.RawMessage `json:"settings"`
	Credential  json.RawMessage `json:"credential"`
	Enabled     *bool           `json:"enabled"`
}

func (a *API) create(r *http.Request, _ access.Principal) (access.Reply, error) {
	ctx := r.Context()
	var input createRequest
	if err := access.Decode(r, &input); err != nil {
		return access.Reply{}, err
	}
	projectID, err := parseProject(input.ProjectID)
	if err != nil {
		return access.Reply{}, err
	}
	filter, err := a.filter(ctx, a.server.Pool, input.Filter, projectID)
	if err != nil {
		return access.Reply{}, err
	}
	k, err := lookup(input.Type, input.Format)
	if err != nil {
		return access.Reply{}, err
	}
	name := strings.TrimSpace(input.Name)
	if err := checkName(name); err != nil {
		return access.Reply{}, err
	}
	if err := checkStreams(k, input.Streams); err != nil {
		return access.Reply{}, err
	}
	if err := a.checkTarget(k, input.Destination, filter); err != nil {
		return access.Reply{}, err
	}
	spec, err := k.parse(input.Type, input.Settings)
	if err != nil {
		return access.Reply{}, err
	}
	if err := checkSpec(ctx, a.server.Pool, spec, nil, projectID); err != nil {
		return access.Reply{}, err
	}
	enabled := input.Enabled == nil || *input.Enabled
	if err := checkCredential(spec, input.Credential, enabled, present(input.Credential)); err != nil {
		return access.Reply{}, err
	}
	settings, err := json.Marshal(spec)
	if err != nil {
		return access.Reply{}, err
	}
	tx, p, err := a.server.BeginMutation(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(ctx)
	if err := RequireScope(ctx, a.server, tx, p, projectID); err != nil {
		return access.Reply{}, err
	}
	claim, replayed, err := a.server.Replay(r, tx, p, input)
	if err != nil {
		return access.Reply{}, err
	}
	if replayed != nil {
		return *replayed, tx.Commit(ctx)
	}
	var count int64
	if err := tx.QueryRow(ctx, countSinksSQL).Scan(&count); err != nil {
		return access.Reply{}, err
	}
	if count >= maxSinks {
		return access.Reply{}, access.Invalid("sinks", fmt.Sprintf("This installation already has %d export sinks.", maxSinks))
	}
	id, etag := access.NewID(), access.NewID()
	if _, err := tx.Exec(ctx, createSinkSQL, id, name, projectID, input.Type, input.Destination, input.Streams, input.Format, filter, settings, enabled, etag, p.UserID()); err != nil {
		return access.Reply{}, err
	}
	if err := a.replaceCredential(ctx, tx, id, nil, input.Credential); err != nil {
		return access.Reply{}, err
	}
	if err := access.AuditForProject(ctx, tx, r, p.Actor(), "export_sink.create", "export_sink", id, "success", projectID); err != nil {
		return access.Reply{}, err
	}
	body, _, _, err := readSink(ctx, tx, id)
	if err != nil {
		return access.Reply{}, err
	}
	result := access.Reply{Status: http.StatusCreated, ETag: etag, Location: "/api/v1/observability/sinks/" + id, Body: body}
	if err := a.server.CompleteReplay(r, tx, claim, result); err != nil {
		return access.Reply{}, err
	}
	return result, tx.Commit(ctx)
}

type updateRequest struct {
	Name        *string         `json:"name"`
	Destination *string         `json:"destination"`
	Filter      json.RawMessage `json:"filter"`
	Settings    json.RawMessage `json:"settings"`
	Credential  json.RawMessage `json:"credential"`
	Enabled     *bool           `json:"enabled"`
}

func (a *API) update(r *http.Request, _ access.Principal) (access.Reply, error) {
	ctx := r.Context()
	var input updateRequest
	if err := access.Decode(r, &input); err != nil {
		return access.Reply{}, err
	}
	tx, p, err := a.server.BeginMutation(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(ctx)
	current, err := lockSink(ctx, tx, r.PathValue("export_sink_id"))
	if err != nil {
		return access.Reply{}, err
	}
	if err := RequireScope(ctx, a.server, tx, p, current.ProjectID); err != nil {
		return access.Reply{}, err
	}
	if err := access.Match(r, current.ETag); err != nil {
		return access.Reply{}, err
	}
	k, err := lookup(current.Type, current.Format)
	if err != nil {
		return access.Reply{}, err
	}
	previous, err := k.parse(current.Type, current.Settings)
	if err != nil {
		return access.Reply{}, err
	}
	filter, err := a.updatedFilter(ctx, tx, input.Filter, current)
	if err != nil {
		return access.Reply{}, err
	}
	name := strings.TrimSpace(valueOr(input.Name, current.Name))
	destination := valueOr(input.Destination, current.Destination)
	enabled := valueOr(input.Enabled, current.Enabled)
	spec, err := a.updatedSpec(ctx, tx, k, input.Settings, previous, current)
	if err != nil {
		return access.Reply{}, err
	}
	if err := checkName(name); err != nil {
		return access.Reply{}, err
	}
	if err := a.checkTarget(k, destination, filter); err != nil {
		return access.Reply{}, err
	}
	if destination != current.Destination && current.CredentialID != nil && input.Credential == nil {
		return access.Reply{}, access.Invalid("credential", "changing the destination requires a replacement credential or null")
	}
	configured := present(input.Credential) || input.Credential == nil && current.CredentialID != nil
	if err := checkCredential(spec, input.Credential, enabled, configured); err != nil {
		return access.Reply{}, err
	}
	settings, err := json.Marshal(spec)
	if err != nil {
		return access.Reply{}, err
	}
	etag := access.NewID()
	if _, err := tx.Exec(ctx, updateSinkSQL, current.ID, name, destination, filter, enabled, etag, settings); err != nil {
		return access.Reply{}, err
	}
	if input.Credential != nil {
		if err := a.replaceCredential(ctx, tx, current.ID, current.CredentialID, input.Credential); err != nil {
			return access.Reply{}, err
		}
	}
	if err := access.AuditForProject(ctx, tx, r, p.Actor(), "export_sink.update", "export_sink", current.ID, "success", current.ProjectID); err != nil {
		return access.Reply{}, err
	}
	body, _, _, err := readSink(ctx, tx, current.ID)
	if err != nil {
		return access.Reply{}, err
	}
	return access.Detail(body, etag), tx.Commit(ctx)
}

func (a *API) delete(r *http.Request, _ access.Principal) (access.Reply, error) {
	ctx := r.Context()
	tx, p, err := a.server.BeginMutation(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(ctx)
	current, err := lockSink(ctx, tx, r.PathValue("export_sink_id"))
	if err != nil {
		return access.Reply{}, err
	}
	if err := RequireScope(ctx, a.server, tx, p, current.ProjectID); err != nil {
		return access.Reply{}, err
	}
	if err := access.Match(r, current.ETag); err != nil {
		return access.Reply{}, err
	}
	if _, err := tx.Exec(ctx, deleteSinkSQL, current.ID); err != nil {
		var constraint *pgconn.PgError
		if errors.As(err, &constraint) && constraint.Code == "23503" {
			return access.Reply{}, access.Fail(http.StatusConflict, "sink_in_use", "Remove or repoint capture policies before deleting this sink.")
		}
		return access.Reply{}, err
	}
	if err := deleteCredential(ctx, tx, current.CredentialID); err != nil {
		return access.Reply{}, err
	}
	if err := access.AuditForProject(ctx, tx, r, p.Actor(), "export_sink.delete", "export_sink", current.ID, "success", current.ProjectID); err != nil {
		return access.Reply{}, err
	}
	return access.Reply{Status: http.StatusNoContent}, tx.Commit(ctx)
}

func (a *API) gaps(r *http.Request, p access.Principal) (access.Reply, error) {
	page, err := access.Page(r)
	if err != nil {
		return access.Reply{}, err
	}
	gaps, err := listGaps(r.Context(), a.server.Pool, r.PathValue("export_sink_id"), page, p)
	if err != nil {
		return access.Reply{}, err
	}
	return access.ListReplyBy(gaps, page, func(g Gap) string { return g.ID }), nil
}

func parseProject(raw *string) (*string, error) {
	if raw == nil {
		return nil, nil
	}
	project, err := access.ParseUUID(*raw)
	if err != nil {
		return nil, access.Invalid("project_id", "project_id must be a valid UUID")
	}
	return &project, nil
}

func checkName(name string) error {
	if name == "" || len(name) > 100 {
		return access.Invalid("name", "name must be from 1 to 100 characters")
	}
	return nil
}

func (a *API) checkTarget(k kind, destination string, filter Filter) error {
	if k.only == FOCUSStream && filter.Outcome != nil {
		return access.Invalid("filter.outcome", "focus datasets do not filter by outcome")
	}
	if destination == "" || len(destination) > 2048 {
		return access.Invalid("destination", "destination must be from 1 to 2048 characters")
	}
	u, err := url.Parse(destination)
	if err != nil || !a.allowsScheme(u) || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return access.Invalid("destination", "destination must be an HTTPS URL without credentials, query or fragment")
	}
	if _, err := a.egress.ValidateEndpoint(destination); err != nil {
		return access.Invalid("destination", "the egress policy refuses this destination")
	}
	if raw, err := json.Marshal(filter); err != nil || len(raw) > 4096 {
		return access.Invalid("filter", "filter must be an object of at most 4096 bytes")
	}
	return nil
}

func (a *API) allowsScheme(u *url.URL) bool {
	return u.Scheme == "https" || u.Scheme == "http" && slices.Contains(a.egress.PlainHTTPHosts, u.Hostname())
}

func (a *API) filter(ctx context.Context, q access.Queryer, raw json.RawMessage, sinkProject *string) (Filter, error) {
	if !present(raw) {
		return Filter{}, nil
	}
	var input Filter
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil || decoder.Decode(new(any)) != io.EOF {
		return Filter{}, access.Invalid("filter", "filter must be an object with project, route and outcome fields")
	}
	project, err := filterProject(ctx, q, input.Project, sinkProject)
	if err != nil {
		return Filter{}, err
	}
	if err := filterRoute(ctx, q, input.Route, sinkProject); err != nil {
		return Filter{}, err
	}
	if input.Outcome != nil && !input.Outcome.Valid() {
		return Filter{}, access.Invalid("filter.outcome", "outcome must be success or failure")
	}
	return Filter{Project: project, Route: input.Route, Outcome: input.Outcome}, nil
}

func (a *API) updatedFilter(ctx context.Context, q access.Queryer, raw json.RawMessage, current stored) (Filter, error) {
	if raw == nil {
		return current.Filter, nil
	}
	return a.filter(ctx, q, raw, current.ProjectID)
}

func filterProject(ctx context.Context, q access.Queryer, raw, sinkProject *string) (*string, error) {
	if raw == nil {
		return nil, nil
	}
	project, err := access.ParseUUID(*raw)
	if err != nil {
		return nil, access.Invalid("filter.project", "the filtered project must be a valid UUID")
	}
	exists, err := ProjectExists(ctx, q, project)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, access.Invalid("filter.project", "the filtered project does not exist")
	}
	if sinkProject != nil && project != *sinkProject {
		return nil, access.Invalid("filter.project", "a project sink may only filter its own project")
	}
	return &project, nil
}

func filterRoute(ctx context.Context, q access.Queryer, route, sinkProject *string) error {
	if route == nil {
		return nil
	}
	routeProject, err := RouteProject(ctx, q, *route)
	if errors.Is(err, pgx.ErrNoRows) {
		return access.Invalid("filter.route", "the filtered route does not exist")
	}
	if err != nil {
		return err
	}
	if sinkProject != nil && routeProject != nil && *routeProject != *sinkProject {
		return access.Invalid("filter.route", "a project sink may only filter routes of its own project")
	}
	return nil
}

func (a *API) updatedSpec(ctx context.Context, q access.Queryer, k kind, raw json.RawMessage, previous Spec, current stored) (Spec, error) {
	if raw == nil {
		return previous, nil
	}
	spec, err := k.parse(current.Type, raw)
	if err != nil {
		return nil, err
	}
	focus, isFOCUS := spec.(FOCUS)
	prior, _ := previous.(FOCUS)
	if isFOCUS && focus.CostBasis != prior.CostBasis {
		return nil, access.Invalid("settings.cost_basis", "cost_basis is immutable on a focus sink")
	}
	return spec, checkSpec(ctx, q, spec, previous, current.ProjectID)
}

func checkSpec(ctx context.Context, q access.Queryer, spec, previous Spec, projectID *string) error {
	switch s := spec.(type) {
	case StripeMeter:
		return checkCustomers(ctx, q, s, projectID)
	case FOCUS:
		return checkStart(ctx, q, s, previous)
	}
	return nil
}

func checkCustomers(ctx context.Context, q access.Queryer, spec StripeMeter, projectID *string) error {
	projects := slices.Collect(maps.Keys(spec.Customers))
	if projectID != nil && slices.ContainsFunc(projects, func(project string) bool { return project != *projectID }) {
		return access.Invalid("settings.customers", "a project sink maps only its own project")
	}
	var missing string
	err := q.QueryRow(ctx, missingProjectSQL, projects).Scan(&missing)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return access.Invalid("settings.customers", fmt.Sprintf("customers names project %s which does not exist", missing))
}

func checkStart(ctx context.Context, q access.Queryer, spec FOCUS, previous Spec) error {
	start, err := spec.Start()
	if err != nil || start == nil {
		return err
	}
	if prior, ok := previous.(FOCUS); ok && prior.StartDate != nil && *prior.StartDate == *spec.StartDate {
		return nil
	}
	days, err := retention.RetentionDays(ctx, q)
	if err != nil {
		return err
	}
	today := time.Now().UTC().Truncate(24 * time.Hour)
	earliest := today.AddDate(0, 0, -min(90, days-1))
	if start.Before(earliest) || start.After(today) {
		return access.Invalid("settings.start_date", "start_date must be a YYYY-MM-DD date within the retained window")
	}
	return nil
}

func checkCredential(spec Spec, raw json.RawMessage, enabled, configured bool) error {
	if present(raw) {
		if err := CheckCredential(spec, raw); err != nil {
			return access.Invalid("credential", err.Error())
		}
	}
	if _, ok := spec.(StripeMeter); ok && enabled && !configured {
		return access.Invalid("credential", "an enabled stripe_meter sink requires a credential")
	}
	return nil
}

func (a *API) replaceCredential(ctx context.Context, tx pgx.Tx, sinkID string, previous *string, raw json.RawMessage) error {
	next, err := a.storeCredential(ctx, tx, raw)
	if err != nil {
		return err
	}
	if next != nil || previous != nil {
		if _, err := tx.Exec(ctx, setCredentialSQL, sinkID, next); err != nil {
			return err
		}
	}
	return deleteCredential(ctx, tx, previous)
}

func deleteCredential(ctx context.Context, tx pgx.Tx, id *string) error {
	if id == nil {
		return nil
	}
	_, err := tx.Exec(ctx, deleteCredentialSQL, *id, secrets.SinkCredential)
	return err
}

func (a *API) storeCredential(ctx context.Context, tx pgx.Tx, raw json.RawMessage) (*string, error) {
	if !present(raw) {
		return nil, nil
	}
	id := access.NewID()
	return &id, a.server.Keys.Store(ctx, tx, a.server.Installation, id, secrets.SinkCredential, raw, nil)
}

func present(raw json.RawMessage) bool {
	return len(raw) > 0 && string(raw) != "null"
}

func valueOr[T any](value *T, fallback T) T {
	if value == nil {
		return fallback
	}
	return *value
}
