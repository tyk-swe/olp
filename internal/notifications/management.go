package notifications

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/egress"
)

type Server struct {
	access *access.Server
	egress egress.Policy
}

func NewManagement(auth *access.Server, policy egress.Policy) *Server {
	return &Server{access: auth, egress: policy}
}

func (s *Server) Register(mux *http.ServeMux) {
	for pattern, handler := range map[string]access.Handler{
		"GET /api/v1/notifications/destinations":                                  s.list(destinations),
		"POST /api/v1/notifications/destinations":                                 s.createNotificationDestination,
		"GET /api/v1/notifications/destinations/{notification_destination_id}":    s.get(destinations),
		"PATCH /api/v1/notifications/destinations/{notification_destination_id}":  s.updateNotificationDestination,
		"DELETE /api/v1/notifications/destinations/{notification_destination_id}": s.delete(destinations),
		"GET /api/v1/notifications/rules":                                         s.list(rules),
		"POST /api/v1/notifications/rules":                                        s.createNotificationRule,
		"GET /api/v1/notifications/rules/{notification_rule_id}":                  s.get(rules),
		"PATCH /api/v1/notifications/rules/{notification_rule_id}":                s.updateNotificationRule,
		"DELETE /api/v1/notifications/rules/{notification_rule_id}":               s.delete(rules),
		"GET /api/v1/notifications/deliveries":                                    s.notificationDeliveries,
	} {
		s.access.Route(mux, pattern, handler)
	}
}

func notificationOperation(projectID *string) access.Operation {
	if projectID == nil {
		return access.Settings
	}
	return access.Keys
}

func (s *Server) list(res resource) access.Handler {
	return func(r *http.Request, p access.Principal) (access.Reply, error) {
		page, err := access.Page(r)
		if err != nil {
			return access.Reply{}, err
		}
		rows, err := s.access.Pool.Query(r.Context(),
			"SELECT jsonb_build_object("+res.fields+")"+res.from+
				" WHERE "+res.alias+".id<$1::uuid AND ($2 OR "+res.alias+".project_id=ANY($3::uuid[])) ORDER BY "+res.alias+".id DESC LIMIT $4",
			page.Before, p.AllProjects, p.ProjectIDs(), page.Limit+1)
		if err != nil {
			return access.Reply{}, err
		}
		items, err := access.JSONRows(rows)
		return access.ListReply(items, page), err
	}
}

func (s *Server) get(res resource) access.Handler {
	return func(r *http.Request, p access.Principal) (access.Reply, error) {
		id, err := access.IDParam(r, res.parameter)
		if err != nil {
			return access.Reply{}, err
		}
		var data []byte
		var etag string
		var projectID *string
		if err = s.access.Pool.QueryRow(r.Context(),
			"SELECT jsonb_build_object("+res.fields+"),"+res.alias+".etag::text,"+res.alias+".project_id::text"+res.from+" WHERE "+res.alias+".id=$1",
			id).Scan(&data, &etag, &projectID); err != nil {
			return access.Reply{}, err
		}
		if err := p.Project(projectID, access.View); err != nil {
			return access.Reply{}, err
		}
		return access.Detail(json.RawMessage(data), etag), nil
	}
}

type creation[T any] struct {
	projectID   *string
	fingerprint any
	parse       func(projectID *string) (T, error)
	insert      func(tx pgx.Tx, p access.Principal, projectID *string, target T, id, etag string) error
}

func create[T any](s *Server, r *http.Request, res resource, c creation[T]) (access.Reply, error) {
	tx, p, err := s.access.BeginMutation(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	if err = p.Authorize(notificationOperation(c.projectID)); err != nil {
		return access.Reply{}, err
	}
	claim, replayed, err := s.access.Replay(r, tx, p, c.fingerprint)
	if err != nil {
		return access.Reply{}, err
	}
	if replayed != nil {
		if err := s.access.RequireProject(r.Context(), tx, p, c.projectID); err != nil {
			return access.Reply{}, err
		}
		return *replayed, tx.Commit(r.Context())
	}
	projectID, err := parseProject(c.projectID)
	if err != nil {
		return access.Reply{}, err
	}
	if err = s.access.RequireProject(r.Context(), tx, p, projectID); err != nil {
		return access.Reply{}, err
	}
	target, err := c.parse(projectID)
	if err != nil {
		return access.Reply{}, err
	}
	id, etag := access.NewID(), access.NewID()
	if err = c.insert(tx, p, projectID, target, id, etag); err != nil {
		return access.Reply{}, err
	}
	body, err := res.read(r.Context(), tx, id)
	if err != nil {
		return access.Reply{}, err
	}
	result := access.Reply{Status: http.StatusCreated, ETag: etag, Location: res.path + id, Body: body}
	if err = access.Audit(r.Context(), tx, r, p.Actor(), res.kind+".create", res.kind, id, "success"); err != nil {
		return access.Reply{}, err
	}
	if err = s.access.CompleteReplay(r, tx, claim, result); err != nil {
		return access.Reply{}, err
	}
	return result, tx.Commit(r.Context())
}

func parseProject(raw *string) (*string, error) {
	if raw == nil {
		return nil, nil
	}
	parsed, err := access.ParseUUID(*raw)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

type locked struct {
	id, etag  string
	current   json.RawMessage
	projectID *string
}

func (s *Server) lockForChange(r *http.Request, tx pgx.Tx, p access.Principal, res resource, id string) (locked, error) {
	row := locked{id: id}
	if err := tx.QueryRow(r.Context(),
		"SELECT jsonb_build_object("+res.fields+"),"+res.alias+".etag::text,"+res.alias+".project_id::text"+res.from+" WHERE "+res.alias+".id=$1 FOR UPDATE OF "+res.alias,
		id).Scan(&row.current, &row.etag, &row.projectID); err != nil {
		return locked{}, err
	}
	if err := p.Project(row.projectID, access.View); err != nil {
		return locked{}, err
	}
	if err := p.Authorize(notificationOperation(row.projectID)); err != nil {
		return locked{}, err
	}
	if err := p.Project(row.projectID, access.Change); err != nil {
		return locked{}, err
	}
	return row, access.Match(r, row.etag)
}

func (s *Server) update(r *http.Request, res resource, apply func(tx pgx.Tx, row locked, etag string) error) (access.Reply, error) {
	id, err := access.IDParam(r, res.parameter)
	if err != nil {
		return access.Reply{}, err
	}
	tx, p, err := s.access.BeginMutation(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	row, err := s.lockForChange(r, tx, p, res, id)
	if err != nil {
		return access.Reply{}, err
	}
	etag := access.NewID()
	if err = apply(tx, row, etag); err != nil {
		return access.Reply{}, err
	}
	body, err := res.read(r.Context(), tx, id)
	if err != nil {
		return access.Reply{}, err
	}
	if err = access.Audit(r.Context(), tx, r, p.Actor(), res.kind+".update", res.kind, id, "success"); err != nil {
		return access.Reply{}, err
	}
	return access.Detail(body, etag), tx.Commit(r.Context())
}

func (s *Server) delete(res resource) access.Handler {
	return func(r *http.Request, _ access.Principal) (access.Reply, error) {
		id, err := access.IDParam(r, res.parameter)
		if err != nil {
			return access.Reply{}, err
		}
		tx, p, err := s.access.BeginMutation(r)
		if err != nil {
			return access.Reply{}, err
		}
		defer tx.Rollback(r.Context())
		claim, replayed, err := s.access.Replay(r, tx, p, map[string]string{res.parameter: id})
		if err != nil {
			return access.Reply{}, err
		}
		if replayed != nil {
			return *replayed, tx.Commit(r.Context())
		}
		row, err := s.lockForChange(r, tx, p, res, id)
		if err != nil {
			return access.Reply{}, err
		}
		var constraint *pgconn.PgError
		if err = res.remove(r.Context(), tx, id); errors.As(err, &constraint) && constraint.Code == "23503" {
			return access.Reply{}, access.Fail(409, res.kind+"_in_use", "The resource still has dependent resources or retained history.")
		}
		if err != nil {
			return access.Reply{}, err
		}
		if err = access.AuditForProject(r.Context(), tx, r, p.Actor(), res.kind+".delete", res.kind, id, "success", row.projectID); err != nil {
			return access.Reply{}, err
		}
		result := access.Reply{Status: http.StatusNoContent}
		if err = s.access.CompleteReplay(r, tx, claim, result); err != nil {
			return access.Reply{}, err
		}
		return result, tx.Commit(r.Context())
	}
}
