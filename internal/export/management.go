package export

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/secrets"
)

type Management struct {
	Access       *access.Server
	Pool         access.Queryer
	Keys         *secrets.KeyRing
	Installation string
	Egress       *egress.Policy
}

type Sink struct {
	ID                   string   `json:"export_sink_id"`
	Name                 string   `json:"name"`
	ProjectID            *string  `json:"project_id"`
	Type                 string   `json:"type"`
	Destination          string   `json:"destination"`
	Streams              []string `json:"streams"`
	Format               string   `json:"format"`
	Filter               *Filter  `json:"filter"`
	Enabled              bool     `json:"enabled"`
	CredentialConfigured bool     `json:"credential_configured"`
	ETag                 string   `json:"etag"`
}

type Filter struct {
	Project *string `json:"project,omitempty"`
	Route   *string `json:"route,omitempty"`
	Outcome *string `json:"outcome,omitempty"`
}

type Gap struct {
	ID          string `json:"id"`
	Stream      string `json:"stream"`
	RecordCount int64  `json:"record_count"`
	FirstAt     string `json:"first_occurred_at"`
	LastAt      string `json:"last_occurred_at"`
	Reason      string `json:"reason"`
}

var sinkFormats = map[string]string{"https": "json", "otlp_logs": "otlp", "s3": "jsonl", "gcs": "jsonl", "azure_blob": "jsonl"}
var sinkStreams = map[string]bool{"requests": true, "attempts": true, "usage_rollups": true, "guardrail_decisions": true, "audit": true}

const maxSinks = 1000

func (m *Management) Register(mux *http.ServeMux) {
	m.Access.Route(mux, "GET /api/v1/observability/sinks", m.listSinks)
	m.Access.Route(mux, "POST /api/v1/observability/sinks", m.createSink)
	m.Access.Route(mux, "GET /api/v1/observability/sinks/{export_sink_id}", m.sinkDetail)
	m.Access.Route(mux, "PATCH /api/v1/observability/sinks/{export_sink_id}", m.updateSink)
	m.Access.Route(mux, "DELETE /api/v1/observability/sinks/{export_sink_id}", m.deleteSink)
	m.Access.Route(mux, "GET /api/v1/observability/sinks/{export_sink_id}/gaps", m.sinkGaps)
	m.Access.Route(mux, "GET /api/v1/observability/capture-sinks", m.listCaptureSinks)
	m.Access.Route(mux, "GET /api/v1/observability/capture", m.captureConfiguration)
	m.Access.Route(mux, "PATCH /api/v1/observability/capture", m.updateCaptureConfiguration)
	m.Access.Route(mux, "GET /api/v1/observability/capture-policies", m.listCapturePolicies)
	m.Access.Route(mux, "POST /api/v1/observability/capture-policies", m.createCapturePolicy)
	m.Access.Route(mux, "GET /api/v1/observability/capture-policies/{capture_policy_id}", m.capturePolicy)
	m.Access.Route(mux, "PATCH /api/v1/observability/capture-policies/{capture_policy_id}", m.updateCapturePolicy)
	m.Access.Route(mux, "DELETE /api/v1/observability/capture-policies/{capture_policy_id}", m.deleteCapturePolicy)
}

func (m *Management) listSinks(r *http.Request, p access.Principal) (access.Reply, error) {
	page, err := access.Page(r)
	if err != nil {
		return access.Reply{}, err
	}
	rows, err := m.Pool.Query(r.Context(), listSinksSQL, page.Before, p.AllProjects, p.ProjectIDs(), page.Limit+1)
	if err != nil {
		return access.Reply{}, err
	}
	items := []map[string]any{}
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			rows.Close()
			return access.Reply{}, err
		}
		var item map[string]any
		if err := json.Unmarshal(data, &item); err != nil {
			rows.Close()
			return access.Reply{}, err
		}
		var sink Sink
		if err := decodeSink(data, &sink); err != nil {
			rows.Close()
			return access.Reply{}, err
		}
		item["export_sink_id"] = sink.ID
		delete(item, "id")
		items = append(items, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return access.Reply{}, err
	}
	for _, item := range items {
		status, err := m.sinkStatus(r, m.Pool, item["export_sink_id"].(string))
		if err != nil {
			return access.Reply{}, err
		}
		item["status"] = status
	}
	return access.ListReply(items, page), nil
}

func (m *Management) sinkDetail(r *http.Request, p access.Principal) (access.Reply, error) {
	sink, etag, projectID, err := scanSinkDetail(m.Pool.QueryRow(r.Context(), getSinkSQL, r.PathValue("export_sink_id")))
	if err != nil {
		return access.Reply{}, err
	}
	if err := p.Project(projectID, access.View); err != nil {
		return access.Reply{}, err
	}
	status, err := m.sinkStatus(r, m.Pool, sink.ID)
	if err != nil {
		return access.Reply{}, err
	}
	reply := map[string]any{}
	if body, err := json.Marshal(sink); err == nil {
		_ = json.Unmarshal(body, &reply)
	}
	reply["status"] = status
	return access.Detail(reply, etag), nil
}

func (m *Management) sinkWithStatus(r *http.Request, q access.Queryer, sink Sink) (map[string]any, error) {
	status, err := m.sinkStatus(r, q, sink.ID)
	if err != nil {
		return nil, err
	}
	reply := map[string]any{}
	if body, err := json.Marshal(sink); err == nil {
		_ = json.Unmarshal(body, &reply)
	}
	reply["status"] = status
	return reply, nil
}

func scanSinkDetail(row pgx.Row) (Sink, string, *string, error) {
	var data []byte
	var etag string
	var projectID *string
	if err := row.Scan(&data, &etag, &projectID); err != nil {
		return Sink{}, "", nil, notFoundErr(err)
	}
	var sink Sink
	if err := decodeSink(data, &sink); err != nil {
		return Sink{}, "", nil, err
	}
	return sink, etag, projectID, nil
}

func decodeSink(data []byte, sink *Sink) error {
	type alias Sink
	var raw struct {
		alias
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*sink = Sink(raw.alias)
	sink.ID = raw.ID
	return nil
}

func (m *Management) sinkStatus(r *http.Request, q access.Queryer, id string) ([]map[string]any, error) {
	rows, err := q.Query(r.Context(), sinkStatusSQL, id)
	if err != nil {
		return nil, err
	}
	items, err := access.JSONRows(rows)
	if items == nil {
		items = []map[string]any{}
	}
	return items, err
}

func (m *Management) createSink(r *http.Request, _ access.Principal) (access.Reply, error) {
	var input struct {
		ProjectID   *string         `json:"project_id"`
		Name        string          `json:"name"`
		Type        string          `json:"type"`
		Destination string          `json:"destination"`
		Streams     []string        `json:"streams"`
		Format      string          `json:"format"`
		Filter      json.RawMessage `json:"filter"`
		Credential  json.RawMessage `json:"credential"`
		Enabled     *bool           `json:"enabled"`
	}
	if err := access.Decode(r, &input); err != nil {
		return access.Reply{}, err
	}
	if input.ProjectID != nil {
		project, err := access.ParseUUID(*input.ProjectID)
		if err != nil {
			return access.Reply{}, access.Invalid("project_id", "project_id must be a valid UUID")
		}
		input.ProjectID = &project
	}
	sink := Sink{ID: access.NewID(), ETag: access.NewID(), Name: strings.TrimSpace(input.Name), ProjectID: input.ProjectID, Type: input.Type, Destination: input.Destination, Streams: input.Streams, Format: input.Format, Enabled: true}
	if input.Enabled != nil {
		sink.Enabled = *input.Enabled
	}
	filter, err := m.filterFor(r, m.Pool, input.Filter, sink.ProjectID)
	if err != nil {
		return access.Reply{}, err
	}
	sink.Filter = filter
	if err := m.validateSink(&sink); err != nil {
		return access.Reply{}, err
	}
	tx, err := m.Access.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := m.Access.Reauthorize(r, tx)
	if err != nil {
		return access.Reply{}, err
	}
	if err := m.requireSinkScope(r, tx, p, sink.ProjectID); err != nil {
		return access.Reply{}, err
	}
	claim, replayed, err := m.Access.Replay(r, tx, p, input)
	if err != nil {
		return access.Reply{}, err
	}
	if replayed != nil {
		return access.Commit(r, tx, *replayed)
	}
	var count int64
	if err := tx.QueryRow(r.Context(), countSinksSQL).Scan(&count); err != nil {
		return access.Reply{}, err
	}
	if count >= maxSinks {
		return access.Reply{}, access.Invalid("sinks", "This installation already has 1000 export sinks.")
	}
	if _, err := tx.Exec(r.Context(), createSinkSQL, sink.ID, sink.Name, sink.ProjectID, sink.Type, sink.Destination, sink.Streams, sink.Format, canonicalFilter(sink.Filter), sink.Enabled, sink.ETag, p.UserID()); err != nil {
		return access.Reply{}, err
	}
	if len(input.Credential) > 0 && string(input.Credential) != "null" {
		if len(input.Credential) > 65536 {
			return access.Reply{}, access.Invalid("credential", "credential must be at most 65536 bytes")
		}
		credentialID := access.NewID()
		if err := m.Keys.Store(r.Context(), tx, m.Installation, credentialID, secrets.SinkCredential, input.Credential, nil); err != nil {
			return access.Reply{}, err
		}
		if _, err := tx.Exec(r.Context(), setCredentialSQL, sink.ID, credentialID); err != nil {
			return access.Reply{}, err
		}
		sink.CredentialConfigured = true
	}
	if err := access.AuditForProject(r.Context(), tx, r, p.Actor(), "export_sink.create", "export_sink", sink.ID, "success", sink.ProjectID); err != nil {
		return access.Reply{}, err
	}
	body, err := m.sinkWithStatus(r, tx, sink)
	if err != nil {
		return access.Reply{}, err
	}
	result := access.Reply{Status: 201, ETag: sink.ETag, Location: "/api/v1/observability/sinks/" + sink.ID, Body: body}
	if err := m.Access.CompleteReplay(r, tx, claim, result); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, result)
}

func (m *Management) updateSink(r *http.Request, _ access.Principal) (access.Reply, error) {
	var input struct {
		Name        *string         `json:"name"`
		Destination *string         `json:"destination"`
		Filter      json.RawMessage `json:"filter"`
		Credential  json.RawMessage `json:"credential"`
		Enabled     *bool           `json:"enabled"`
		Type        *string         `json:"type"`
		Format      *string         `json:"format"`
		Streams     []string        `json:"streams"`
		ProjectID   *string         `json:"project_id"`
	}
	if err := access.Decode(r, &input); err != nil {
		return access.Reply{}, err
	}
	tx, err := m.Access.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := m.Access.Reauthorize(r, tx)
	if err != nil {
		return access.Reply{}, err
	}
	current, etag, projectID, credentialID, err := m.lockSink(r, tx)
	if err != nil {
		return access.Reply{}, err
	}
	if err := m.requireSinkScope(r, tx, p, projectID); err != nil {
		return access.Reply{}, err
	}
	if err := access.Match(r, etag); err != nil {
		return access.Reply{}, err
	}
	if input.ProjectID != nil && (projectID == nil || *input.ProjectID != *projectID) {
		return access.Reply{}, access.Invalid("project_id", "sink project is immutable")
	}
	if input.Type != nil && *input.Type != current.Type {
		return access.Reply{}, access.Invalid("type", "sink type is immutable")
	}
	if input.Format != nil && *input.Format != current.Format {
		return access.Reply{}, access.Invalid("format", "sink format is immutable")
	}
	if input.Streams != nil && !equalStrings(input.Streams, current.Streams) {
		return access.Reply{}, access.Invalid("streams", "sink streams are immutable")
	}
	updated := current
	if input.Name != nil {
		updated.Name = strings.TrimSpace(*input.Name)
	}
	if input.Destination != nil {
		updated.Destination = *input.Destination
	}
	if input.Enabled != nil {
		updated.Enabled = *input.Enabled
	}
	if input.Filter != nil {
		filter, err := m.filterFor(r, tx, input.Filter, projectID)
		if err != nil {
			return access.Reply{}, err
		}
		updated.Filter = filter
	}
	if err := m.validateSink(&updated); err != nil {
		return access.Reply{}, err
	}
	if updated.Destination != current.Destination && credentialID != nil && input.Credential == nil {
		return access.Reply{}, access.Invalid("credential", "changing the destination requires a replacement credential or null")
	}
	etag = access.NewID()
	if _, err := tx.Exec(r.Context(), updateSinkSQL, current.ID, updated.Name, updated.Destination, updated.Streams, canonicalFilter(updated.Filter), updated.Enabled, etag); err != nil {
		return access.Reply{}, err
	}
	if input.Credential != nil {
		if len(input.Credential) == 0 || string(input.Credential) == "null" {
			if credentialID != nil {
				if _, err := tx.Exec(r.Context(), setCredentialSQL, current.ID, nil); err != nil {
					return access.Reply{}, err
				}
				if _, err := tx.Exec(r.Context(), deleteCredentialSQL, *credentialID, secrets.SinkCredential); err != nil {
					return access.Reply{}, err
				}
				updated.CredentialConfigured = false
			}
		} else {
			if len(input.Credential) > 65536 {
				return access.Reply{}, access.Invalid("credential", "credential must be at most 65536 bytes")
			}
			newID := access.NewID()
			if err := m.Keys.Store(r.Context(), tx, m.Installation, newID, secrets.SinkCredential, input.Credential, nil); err != nil {
				return access.Reply{}, err
			}
			if _, err := tx.Exec(r.Context(), setCredentialSQL, current.ID, newID); err != nil {
				return access.Reply{}, err
			}
			if credentialID != nil {
				if _, err := tx.Exec(r.Context(), deleteCredentialSQL, *credentialID, secrets.SinkCredential); err != nil {
					return access.Reply{}, err
				}
			}
			updated.CredentialConfigured = true
		}
	}
	if err := access.AuditForProject(r.Context(), tx, r, p.Actor(), "export_sink.update", "export_sink", current.ID, "success", projectID); err != nil {
		return access.Reply{}, err
	}
	updated.ETag = etag
	body, err := m.sinkWithStatus(r, tx, updated)
	if err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, access.Detail(body, etag))
}

func (m *Management) deleteSink(r *http.Request, _ access.Principal) (access.Reply, error) {
	tx, err := m.Access.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := m.Access.Reauthorize(r, tx)
	if err != nil {
		return access.Reply{}, err
	}
	current, etag, projectID, credentialID, err := m.lockSink(r, tx)
	if err != nil {
		return access.Reply{}, err
	}
	if err := m.requireSinkScope(r, tx, p, projectID); err != nil {
		return access.Reply{}, err
	}
	if err := access.Match(r, etag); err != nil {
		return access.Reply{}, err
	}
	if _, err := tx.Exec(r.Context(), deleteSinkSQL, current.ID); err != nil {
		return access.Reply{}, err
	}
	if credentialID != nil {
		if _, err := tx.Exec(r.Context(), deleteCredentialSQL, *credentialID, secrets.SinkCredential); err != nil {
			return access.Reply{}, err
		}
	}
	if err := access.AuditForProject(r.Context(), tx, r, p.Actor(), "export_sink.delete", "export_sink", current.ID, "success", projectID); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, access.Reply{Status: 204})
}

func (m *Management) lockSink(r *http.Request, tx pgx.Tx) (Sink, string, *string, *string, error) {
	var data []byte
	var etag string
	var projectID, credentialID *string
	if err := tx.QueryRow(r.Context(), lockSinkSQL, r.PathValue("export_sink_id")).Scan(&data, &etag, &projectID, &credentialID); err != nil {
		return Sink{}, "", nil, nil, notFoundErr(err)
	}
	var sink Sink
	if err := decodeSink(data, &sink); err != nil {
		return Sink{}, "", nil, nil, err
	}
	return sink, etag, projectID, credentialID, nil
}

func (m *Management) sinkGaps(r *http.Request, p access.Principal) (access.Reply, error) {
	page, err := access.Page(r)
	if err != nil {
		return access.Reply{}, err
	}
	rows, err := m.Pool.Query(r.Context(), listGapsSQL, r.PathValue("export_sink_id"), page.Before, p.AllProjects, p.ProjectIDs(), page.Limit+1)
	if err != nil {
		return access.Reply{}, err
	}
	var items []Gap
	for rows.Next() {
		var gap Gap
		if err := rows.Scan(&gap); err != nil {
			rows.Close()
			return access.Reply{}, err
		}
		items = append(items, gap)
	}
	rows.Close()
	return access.ListReplyBy(items, page, func(g Gap) string { return g.ID }), rows.Err()
}

func (m *Management) requireSinkScope(r *http.Request, q access.Queryer, p access.Principal, projectID *string) error {
	if projectID == nil {
		return p.Authorize(access.Settings)
	}
	if err := p.Authorize(access.Keys); err != nil {
		return err
	}
	return m.Access.RequireProject(r.Context(), q, p, projectID)
}

func (m *Management) validateSink(sink *Sink) error {
	format, ok := sinkFormats[sink.Type]
	if !ok {
		return access.Invalid("type", "type must be https, otlp_logs, s3, gcs or azure_blob")
	}
	if sink.Format != format {
		return access.Invalid("format", fmt.Sprintf("%s sinks require format %s", sink.Type, format))
	}
	if sink.Name == "" || len(sink.Name) > 100 {
		return access.Invalid("name", "name must be from 1 to 100 characters")
	}
	if len(sink.Streams) < 1 || len(sink.Streams) > 5 {
		return access.Invalid("streams", "streams must contain from 1 to 5 entries")
	}
	seen := map[string]bool{}
	for _, stream := range sink.Streams {
		if !sinkStreams[stream] || seen[stream] {
			return access.Invalid("streams", "streams must contain distinct requests, attempts, usage_rollups, guardrail_decisions or audit entries")
		}
		seen[stream] = true
	}
	if sink.Destination == "" || len(sink.Destination) > 2048 {
		return access.Invalid("destination", "destination must be from 1 to 2048 characters")
	}
	u, err := url.Parse(sink.Destination)
	plainHTTP := err == nil && u != nil && m.Egress != nil && slices.Contains(m.Egress.PlainHTTPHosts, u.Hostname())
	if err != nil || u == nil || u.Scheme != "https" && !(plainHTTP && u.Scheme == "http") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return access.Invalid("destination", "destination must be an HTTPS URL without credentials, query or fragment")
	}
	if _, err := m.Egress.ValidateEndpoint(sink.Destination); err != nil {
		return access.Invalid("destination", "the egress policy refuses this destination")
	}
	if sink.Filter != nil {
		raw, err := json.Marshal(sink.Filter)
		if err != nil || len(raw) > 4096 {
			return access.Invalid("filter", "filter must be an object of at most 4096 bytes")
		}
	}
	return nil
}

func (m *Management) filterFor(r *http.Request, q access.Queryer, raw json.RawMessage, sinkProject *string) (*Filter, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var filter Filter
	if err := dec.Decode(&filter); err != nil || dec.Decode(new(any)) != io.EOF {
		return nil, access.Invalid("filter", "filter must be an object with project, route and outcome fields")
	}
	if filter.Project != nil {
		project, err := access.ParseUUID(*filter.Project)
		if err != nil {
			return nil, access.Invalid("filter.project", "the filtered project must be a valid UUID")
		}
		filter.Project = &project
		var exists bool
		if err := q.QueryRow(r.Context(), filterProjectScopeSQL, project).Scan(&exists); err != nil {
			return nil, err
		}
		if !exists {
			return nil, access.Invalid("filter.project", "the filtered project does not exist")
		}
		if sinkProject != nil && project != *sinkProject {
			return nil, access.Invalid("filter.project", "a project sink may only filter its own project")
		}
	}
	if filter.Route != nil {
		var routeProject *string
		if err := q.QueryRow(r.Context(), filterRouteScopeSQL, *filter.Route).Scan(&routeProject); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, access.Invalid("filter.route", "the filtered route does not exist")
			}
			return nil, err
		}
		if sinkProject != nil && routeProject != nil && *routeProject != *sinkProject {
			return nil, access.Invalid("filter.route", "a project sink may only filter routes of its own project")
		}
	}
	if filter.Outcome != nil {
		switch *filter.Outcome {
		case "success", "failure":
		default:
			return nil, access.Invalid("filter.outcome", "outcome must be success or failure")
		}
	}
	return &filter, nil
}

func canonicalFilter(filter *Filter) []byte {
	if filter == nil {
		return []byte("{}")
	}
	raw, err := json.Marshal(filter)
	if err != nil {
		return []byte("{}")
	}
	return raw
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func notFoundErr(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return access.Fail(404, "not_found", "The resource was not found.")
	}
	return err
}
