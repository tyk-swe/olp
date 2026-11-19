// Package sinks exports durable, content-free facts through bounded workers.
package sinks

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/authz"
	"github.com/tyk-swe/olp/internal/database"
	"github.com/tyk-swe/olp/internal/egress"
)

type Server struct {
	Access *access.Server
	Egress *egress.Policy
}

type Definition struct {
	ID            string        `json:"id"`
	ProjectID     *string       `json:"project_id"`
	Name          string        `json:"name"`
	Type          string        `json:"type"`
	Destination   string        `json:"destination"`
	Streams       []string      `json:"streams"`
	Enabled       bool          `json:"enabled"`
	HasCredential bool          `json:"has_credential"`
	ETag          string        `json:"etag"`
	RetiredAt     *time.Time    `json:"retired_at"`
	Delivery      DeliveryState `json:"delivery"`
}

type DeliveryState struct {
	Delivered       int64      `json:"delivered"`
	Failed          int64      `json:"failed"`
	Expired         int64      `json:"expired"`
	LastDeliveredAt *time.Time `json:"last_delivered_at"`
}
type input struct {
	ProjectID   *string         `json:"project_id,omitempty"`
	Name        string          `json:"name"`
	Type        string          `json:"type,omitempty"`
	Destination string          `json:"destination"`
	Streams     []string        `json:"streams"`
	Enabled     *bool           `json:"enabled"`
	Credential  json.RawMessage `json:"credential,omitempty"`
}

const columns = "id::text,project_id::text,name,type,destination,streams,enabled,credential_id IS NOT NULL,etag::text,retired_at,delivered_total,failed_total,expired_total,last_delivered_at"

func scan(row pgx.Row) (*Definition, error) {
	var d Definition
	err := row.Scan(&d.ID, &d.ProjectID, &d.Name, &d.Type, &d.Destination, &d.Streams, &d.Enabled, &d.HasCredential, &d.ETag, &d.RetiredAt, &d.Delivery.Delivered, &d.Delivery.Failed, &d.Delivery.Expired, &d.Delivery.LastDeliveredAt)
	return &d, err
}
func load(ctx context.Context, q database.Queryer, id string, lock bool) (*Definition, error) {
	query := "SELECT " + columns + " FROM olp.managed_export_sinks WHERE id=$1"
	if lock {
		query += " FOR NO KEY UPDATE"
	}
	return scan(q.QueryRow(ctx, query, id))
}
func operation(project *string) authz.Operation {
	if project == nil {
		return authz.Settings
	}
	return authz.Keys
}
func (s *Server) Register(mux *http.ServeMux) {
	a := s.Access
	a.Route(mux, "GET /api/v1/sinks", s.list)
	a.Route(mux, "POST /api/v1/sinks", s.create)
	a.Route(mux, "GET /api/v1/sinks/{sink_id}", s.get)
	a.Route(mux, "PUT /api/v1/sinks/{sink_id}", s.update)
	a.Route(mux, "DELETE /api/v1/sinks/{sink_id}", s.retire)
}
func (s *Server) list(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	page, err := httpkit.Page(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	rows, err := s.Access.Pool.Query(r.Context(), "SELECT "+columns+" FROM olp.managed_export_sinks WHERE retired_at IS NULL AND id<$1 AND ($2 OR project_id=ANY($3::uuid[])) ORDER BY id DESC LIMIT $4", page.Before, p.AllProjects, p.ProjectIDs(), page.Limit+1)
	if err != nil {
		return httpkit.Reply{}, err
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		d, err := scan(rows)
		if err != nil {
			return httpkit.Reply{}, err
		}
		encoded, _ := json.Marshal(d)
		var item map[string]any
		_ = json.Unmarshal(encoded, &item)
		items = append(items, item)
	}
	return httpkit.ListReply(items, page), rows.Err()
}
func (s *Server) get(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "sink_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	d, err := load(r.Context(), s.Access.Pool, id, false)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if err = p.Project(d.ProjectID, authz.View); err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.Detail(d, d.ETag), nil
}
func validateAndNormalize(in *input, policy *egress.Policy, project *string) error {
	in.Name = strings.TrimSpace(in.Name)
	if err := httpkit.ValidText("name", in.Name, 100); err != nil {
		return err
	}
	if in.Enabled == nil {
		return httpkit.Invalid("enabled", "Supply an explicit enabled flag.")
	}
	if policy == nil {
		return httpkit.Fail(503, "egress_policy_unavailable", "Export delivery requires provider egress policy.")
	}
	target, err := policy.ValidateEndpoint(in.Destination)
	if err != nil {
		return httpkit.Invalid("destination", "The endpoint is not permitted by egress policy.")
	}
	in.Destination = target.String()
	if len(in.Streams) < 1 || len(in.Streams) > 4 {
		return httpkit.Invalid("streams", "Choose supported content-free fact streams.")
	}
	for index, stream := range in.Streams {
		if !slices.Contains([]string{"requests", "attempts", "guardrail_decisions", "audit", "runs", "trace_spans"}, stream) || slices.Contains(in.Streams[:index], stream) || (stream == "audit" && project != nil) {
			return httpkit.Invalid("streams", "Use unique streams; audit export requires an installation sink.")
		}
	}
	return nil
}
func (s *Server) create(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	return s.write(r, "", false)
}
func (s *Server) update(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "sink_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	return s.write(r, id, false)
}
func (s *Server) retire(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "sink_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	return s.write(r, id, true)
}

func (s *Server) write(r *http.Request, id string, retire bool) (httpkit.Reply, error) {
	var in input
	if !retire {
		if err := httpkit.DecodeUnique(r, &in, 64<<10); err != nil {
			return httpkit.Reply{}, err
		}
	}
	return access.Mutate(s.Access, r, &in, func(tx pgx.Tx, p authz.Principal) (*Definition, error) {
		current, err := target(r, tx, id)
		if err != nil {
			return nil, err
		}
		project := owner(in, current)
		if err = p.Authorize(operation(project)); err != nil {
			return nil, err
		}
		if project != nil {
			if err = s.Access.RequireProject(r.Context(), tx, p, project); err != nil {
				return nil, err
			}
		}
		if retire {
			return current, nil
		}
		return current, validateAndNormalize(&in, s.Egress, project)
	}, func(tx pgx.Tx, p authz.Principal, current *Definition) (httpkit.Reply, access.Event, error) {
		switch {
		case retire:
			return s.retireSink(r, tx, *current)
		case current == nil:
			return s.createSink(r, tx, p, in)
		default:
			return s.updateSink(r, tx, *current, in)
		}
	})
}

func target(r *http.Request, tx pgx.Tx, id string) (*Definition, error) {
	if id == "" {
		return nil, nil
	}
	return load(r.Context(), tx, id, true)
}

func owner(in input, current *Definition) *string {
	if current != nil {
		return current.ProjectID
	}
	return in.ProjectID
}

func changeable(r *http.Request, current Definition, in input) error {
	if err := httpkit.Match(r, current.ETag); err != nil {
		return err
	}
	if current.RetiredAt != nil {
		return httpkit.Fail(409, "sink_retired", "Create a new sink instead of altering retained history.")
	}
	if in.ProjectID != nil || in.Type != "" {
		return httpkit.Invalid("project_id", "Sink project and type are immutable.")
	}
	return nil
}

func (s *Server) retireSink(r *http.Request, tx pgx.Tx, current Definition) (httpkit.Reply, access.Event, error) {
	ctx := r.Context()
	if err := changeable(r, current, input{}); err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	if _, err := tx.Exec(ctx, "UPDATE olp.managed_export_sinks SET enabled=false,retired_at=now(),etag=$2 WHERE id=$1", current.ID, httpkit.NewID()); err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	if _, err := tx.Exec(ctx, "UPDATE olp.managed_export_deliveries SET status='cancelled',lease=NULL WHERE sink_id=$1 AND status='pending'", current.ID); err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	if err := s.credentials().Replace(ctx, tx, current.ID, nil); err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	return httpkit.Reply{Status: 204}, access.Event{Action: "sink.retire", Resource: "sink", ID: current.ID}, nil
}

func (s *Server) createSink(r *http.Request, tx pgx.Tx, p authz.Principal, in input) (httpkit.Reply, access.Event, error) {
	ctx := r.Context()
	if in.Type != "https" {
		return httpkit.Reply{}, access.Event{}, httpkit.Invalid("type", "Use the HTTPS JSON sink type.")
	}
	var count int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM olp.managed_export_sinks WHERE retired_at IS NULL").Scan(&count); err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	if count >= 64 {
		return httpkit.Reply{}, access.Event{}, httpkit.Invalid("sinks", "At most 64 active sinks are supported.")
	}
	id, etag := httpkit.NewID(), httpkit.NewID()
	if err := Insert(ctx, tx, id, in.spec(in.ProjectID), etag, p.UserID()); err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	reply, err := s.saved(r, tx, id, etag, in)
	reply.Status, reply.Location = 201, "/api/v1/sinks/"+id
	return reply, access.Event{Action: "sink.create", Resource: "sink", ID: id}, err
}

func (s *Server) updateSink(r *http.Request, tx pgx.Tx, current Definition, in input) (httpkit.Reply, access.Event, error) {
	if err := changeable(r, current, in); err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	etag := httpkit.NewID()
	if err := Update(r.Context(), tx, current.ID, in.spec(current.ProjectID), etag); err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	reply, err := s.saved(r, tx, current.ID, etag, in)
	return reply, access.Event{Action: "sink.update", Resource: "sink", ID: current.ID}, err
}

func (s *Server) saved(r *http.Request, tx pgx.Tx, id, etag string, in input) (httpkit.Reply, error) {
	if len(in.Credential) > 0 {
		secret, err := signingCredential(in.Credential)
		if err != nil {
			return httpkit.Reply{}, err
		}
		if err = s.credentials().Replace(r.Context(), tx, id, secret); err != nil {
			return httpkit.Reply{}, err
		}
	}
	d, err := load(r.Context(), tx, id, false)
	if err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.Detail(d, etag), nil
}

func (s *Server) credentials() Credentials {
	return Credentials{Keys: s.Access.Keys, Installation: s.Access.Installation}
}

func (in input) spec(project *string) Spec {
	return Spec{ProjectID: project, Name: in.Name, Type: in.Type, Destination: in.Destination, Streams: in.Streams, Enabled: *in.Enabled}
}

func signingCredential(raw json.RawMessage) (*string, error) {
	if string(raw) == "null" {
		return nil, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || len(value) < 1 || len(value) > 4096 {
		return nil, httpkit.Invalid("credential", "Use a signing credential of 1–4096 bytes or null.")
	}
	return &value, nil
}
