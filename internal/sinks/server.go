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
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/secrets"
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
func load(ctx context.Context, q access.Queryer, id string, lock bool) (*Definition, error) {
	query := "SELECT " + columns + " FROM olp.managed_export_sinks WHERE id=$1"
	if lock {
		query += " FOR NO KEY UPDATE"
	}
	return scan(q.QueryRow(ctx, query, id))
}
func operation(project *string) access.Operation {
	if project == nil {
		return access.Settings
	}
	return access.Keys
}
func (s *Server) Register(mux *http.ServeMux) {
	a := s.Access
	a.Route(mux, "GET /api/v1/sinks", s.list)
	a.Route(mux, "POST /api/v1/sinks", s.create)
	a.Route(mux, "GET /api/v1/sinks/{sink_id}", s.get)
	a.Route(mux, "PUT /api/v1/sinks/{sink_id}", s.update)
	a.Route(mux, "DELETE /api/v1/sinks/{sink_id}", s.retire)
}
func (s *Server) list(r *http.Request, p access.Principal) (access.Reply, error) {
	page, err := access.Page(r)
	if err != nil {
		return access.Reply{}, err
	}
	rows, err := s.Access.Pool.Query(r.Context(), "SELECT "+columns+" FROM olp.managed_export_sinks WHERE retired_at IS NULL AND id<$1 AND ($2 OR project_id=ANY($3::uuid[])) ORDER BY id DESC LIMIT $4", page.Before, p.AllProjects, p.ProjectIDs(), page.Limit+1)
	if err != nil {
		return access.Reply{}, err
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		d, err := scan(rows)
		if err != nil {
			return access.Reply{}, err
		}
		encoded, _ := json.Marshal(d)
		var item map[string]any
		_ = json.Unmarshal(encoded, &item)
		items = append(items, item)
	}
	return access.ListReply(items, page), rows.Err()
}
func (s *Server) get(r *http.Request, p access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "sink_id")
	if err != nil {
		return access.Reply{}, err
	}
	d, err := load(r.Context(), s.Access.Pool, id, false)
	if err != nil {
		return access.Reply{}, err
	}
	if err = p.Project(d.ProjectID, access.View); err != nil {
		return access.Reply{}, err
	}
	return access.Detail(d, d.ETag), nil
}
func validate(in *input, policy *egress.Policy, project *string) error {
	in.Name = strings.TrimSpace(in.Name)
	if err := access.ValidText("name", in.Name, 100); err != nil {
		return err
	}
	if in.Enabled == nil {
		return access.Invalid("enabled", "Supply an explicit enabled flag.")
	}
	if policy == nil {
		return access.Fail(503, "egress_policy_unavailable", "Export delivery requires provider egress policy.")
	}
	target, err := policy.ValidateEndpoint(in.Destination)
	if err != nil {
		return access.Invalid("destination", "The endpoint is not permitted by egress policy.")
	}
	in.Destination = target.String()
	if len(in.Streams) < 1 || len(in.Streams) > 4 {
		return access.Invalid("streams", "Choose supported content-free fact streams.")
	}
	seen := map[string]bool{}
	for _, stream := range in.Streams {
		if !slices.Contains([]string{"requests", "attempts", "guardrail_decisions", "audit"}, stream) || seen[stream] || (stream == "audit" && project != nil) {
			return access.Invalid("streams", "Use unique streams; audit export requires an installation sink.")
		}
		seen[stream] = true
	}
	return nil
}

// ValidateDefinition shares endpoint, stream, and name validation with
// configuration promotion without accepting secret material.
func ValidateDefinition(d *Definition, policy *egress.Policy) error {
	if d.Type != "https" {
		return access.Invalid("type", "Use the HTTPS JSON sink type.")
	}
	in := input{Name: d.Name, Destination: d.Destination, Streams: d.Streams, Enabled: &d.Enabled}
	if err := validate(&in, policy, d.ProjectID); err != nil {
		return err
	}
	d.Name, d.Destination, d.Streams = in.Name, in.Destination, in.Streams
	return nil
}
func (s *Server) create(r *http.Request, _ access.Principal) (access.Reply, error) {
	return s.write(r, "", false)
}
func (s *Server) update(r *http.Request, _ access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "sink_id")
	if err != nil {
		return access.Reply{}, err
	}
	return s.write(r, id, false)
}
func (s *Server) retire(r *http.Request, _ access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "sink_id")
	if err != nil {
		return access.Reply{}, err
	}
	return s.write(r, id, true)
}

func (s *Server) write(r *http.Request, id string, retire bool) (access.Reply, error) {
	var in input
	if !retire {
		if err := access.DecodeUnique(r, &in, 64<<10); err != nil {
			return access.Reply{}, err
		}
	}
	a := s.Access
	tx, err := a.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := a.Reauthorize(r, tx)
	if err != nil {
		return access.Reply{}, err
	}
	var current *Definition
	project := in.ProjectID
	if id != "" {
		current, err = load(r.Context(), tx, id, true)
		if err != nil {
			return access.Reply{}, err
		}
		project = current.ProjectID
	}
	if err = p.Authorize(operation(project)); err != nil {
		return access.Reply{}, err
	}
	if project != nil {
		if err = a.RequireProject(r.Context(), tx, p, project); err != nil {
			return access.Reply{}, err
		}
	}
	if !retire {
		if err = validate(&in, s.Egress, project); err != nil {
			return access.Reply{}, err
		}
	}
	claim, replay, err := a.Replay(r, tx, p, in)
	if err != nil {
		return access.Reply{}, err
	}
	if replay != nil {
		return access.Commit(r, tx, *replay)
	}
	if current != nil {
		if err = access.Match(r, current.ETag); err != nil {
			return access.Reply{}, err
		}
		if current.RetiredAt != nil {
			return access.Reply{}, access.Fail(409, "sink_retired", "Create a new sink instead of altering retained history.")
		}
		if in.ProjectID != nil || in.Type != "" {
			return access.Reply{}, access.Invalid("project_id", "Sink project and type are immutable.")
		}
	}
	etag := access.NewID()
	if retire {
		_, err = tx.Exec(r.Context(), "UPDATE olp.managed_export_sinks SET enabled=false,retired_at=now(),etag=$2 WHERE id=$1", id, etag)
		if err == nil {
			_, err = tx.Exec(r.Context(), "UPDATE olp.managed_export_deliveries SET status='cancelled',lease=NULL WHERE sink_id=$1 AND status='pending'", id)
		}
	} else if current == nil {
		if in.Type != "https" {
			return access.Reply{}, access.Invalid("type", "Use the HTTPS JSON sink type.")
		}
		var count int
		if err = tx.QueryRow(r.Context(), "SELECT count(*) FROM olp.managed_export_sinks WHERE retired_at IS NULL").Scan(&count); err != nil {
			return access.Reply{}, err
		}
		if count >= 64 {
			return access.Reply{}, access.Invalid("sinks", "At most 64 active sinks are supported.")
		}
		id = access.NewID()
		_, err = tx.Exec(r.Context(), "INSERT INTO olp.managed_export_sinks(id,project_id,name,type,destination,streams,enabled,etag,created_by) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)", id, project, in.Name, in.Type, in.Destination, in.Streams, *in.Enabled, etag, p.UserID())
	} else {
		_, err = tx.Exec(r.Context(), "UPDATE olp.managed_export_sinks SET name=$2,destination=$3,streams=$4,enabled=$5,etag=$6 WHERE id=$1", id, in.Name, in.Destination, in.Streams, *in.Enabled, etag)
	}
	if err != nil {
		if problem, ok := err.(*pgconn.PgError); ok && problem.Code == "23505" {
			return access.Reply{}, access.Fail(409, "sink_name_exists", "Use a unique active sink name inside its scope.")
		}
		return access.Reply{}, err
	}
	if retire {
		in.Credential = json.RawMessage("null")
	}
	if len(in.Credential) > 0 {
		var previous *string
		if err = tx.QueryRow(r.Context(), "SELECT credential_id::text FROM olp.managed_export_sinks WHERE id=$1", id).Scan(&previous); err != nil {
			return access.Reply{}, err
		}
		var next *string
		if string(in.Credential) != "null" {
			var value string
			if err = json.Unmarshal(in.Credential, &value); err != nil || len(value) < 1 || len(value) > 4096 {
				return access.Reply{}, access.Invalid("credential", "Use a signing credential of 1–4096 bytes or null.")
			}
			secretID := access.NewID()
			material := []byte(value)
			err = a.Keys.Store(r.Context(), tx, a.Installation, secretID, secrets.SinkCredential, material, nil)
			clear(material)
			if err != nil {
				return access.Reply{}, err
			}
			next = &secretID
		}
		if _, err = tx.Exec(r.Context(), "UPDATE olp.managed_export_sinks SET credential_id=$2 WHERE id=$1", id, next); err != nil {
			return access.Reply{}, err
		}
		if previous != nil {
			if _, err = tx.Exec(r.Context(), "DELETE FROM olp.secrets WHERE id=$1 AND purpose=$2", *previous, secrets.SinkCredential); err != nil {
				return access.Reply{}, err
			}
		}
	}
	action := "sink.update"
	if current == nil {
		action = "sink.create"
	}
	if retire {
		action = "sink.retire"
	}
	if err = access.Audit(r.Context(), tx, r, p.Actor(), action, "sink", id, "success"); err != nil {
		return access.Reply{}, err
	}
	reply := access.Reply{Status: 204}
	if !retire {
		d, err := load(r.Context(), tx, id, false)
		if err != nil {
			return access.Reply{}, err
		}
		reply = access.Detail(d, etag)
		if current == nil {
			reply.Status = 201
			reply.Location = "/api/v1/sinks/" + id
		}
	}
	if err = a.CompleteReplay(r, tx, claim, reply); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, reply)
}
