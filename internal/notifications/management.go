package notifications

import (
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
			"SELECT document FROM ("+res.documentsSQL+") documents WHERE id<$1::uuid AND ($2 OR project_id=ANY($3::uuid[])) ORDER BY id DESC LIMIT $4",
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
		found, err := res.find(r.Context(), s.access.Pool, id)
		if err != nil {
			return access.Reply{}, err
		}
		if err := p.Project(found.projectID, access.View); err != nil {
			return access.Reply{}, err
		}
		return access.Detail(found.body, found.etag), nil
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
	if err := p.Authorize(notificationOperation(c.projectID)); err != nil {
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
	projectID, err := access.ParseOptionalUUID(c.projectID)
	if err != nil {
		return access.Reply{}, err
	}
	if err := s.access.RequireProject(r.Context(), tx, p, projectID); err != nil {
		return access.Reply{}, err
	}
	target, err := c.parse(projectID)
	if err != nil {
		return access.Reply{}, err
	}
	id, etag := access.NewID(), access.NewID()
	if err := c.insert(tx, p, projectID, target, id, etag); err != nil {
		return access.Reply{}, err
	}
	created, err := res.find(r.Context(), tx, id)
	if err != nil {
		return access.Reply{}, err
	}
	result := access.Reply{Status: http.StatusCreated, ETag: created.etag, Location: res.path + id, Body: created.body}
	if err := access.Audit(r.Context(), tx, r, p.Actor(), res.kind+".create", res.kind, id, "success"); err != nil {
		return access.Reply{}, err
	}
	if err := s.access.CompleteReplay(r, tx, claim, result); err != nil {
		return access.Reply{}, err
	}
	return result, tx.Commit(r.Context())
}

func (s *Server) update(r *http.Request, res resource, apply func(tx pgx.Tx, current document, etag string) error) (access.Reply, error) {
	id, err := access.IDParam(r, res.parameter)
	if err != nil {
		return access.Reply{}, err
	}
	tx, p, err := s.access.BeginMutation(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	locked, err := res.lockForChange(r.Context(), tx, p, id)
	if err != nil {
		return access.Reply{}, err
	}
	if err := access.Match(r, locked.etag); err != nil {
		return access.Reply{}, err
	}
	current, err := res.find(r.Context(), tx, id)
	if err != nil {
		return access.Reply{}, err
	}
	if err := apply(tx, current, access.NewID()); err != nil {
		return access.Reply{}, err
	}
	updated, err := res.find(r.Context(), tx, id)
	if err != nil {
		return access.Reply{}, err
	}
	if err := access.Audit(r.Context(), tx, r, p.Actor(), res.kind+".update", res.kind, id, "success"); err != nil {
		return access.Reply{}, err
	}
	return access.Detail(updated.body, updated.etag), tx.Commit(r.Context())
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
		locked, err := res.lockForChange(r.Context(), tx, p, id)
		if err != nil {
			return access.Reply{}, err
		}
		if err := access.Match(r, locked.etag); err != nil {
			return access.Reply{}, err
		}
		if err := removalError(res.kind, res.remove(r.Context(), tx, id)); err != nil {
			return access.Reply{}, err
		}
		if err := access.AuditForProject(r.Context(), tx, r, p.Actor(), res.kind+".delete", res.kind, id, "success", locked.projectID); err != nil {
			return access.Reply{}, err
		}
		result := access.Reply{Status: http.StatusNoContent}
		if err := s.access.CompleteReplay(r, tx, claim, result); err != nil {
			return access.Reply{}, err
		}
		return result, tx.Commit(r.Context())
	}
}

func removalError(kind string, err error) error {
	var constraint *pgconn.PgError
	if errors.As(err, &constraint) && constraint.Code == "23503" {
		return access.Fail(409, kind+"_in_use", "The resource still has dependent resources or retained history.")
	}
	return err
}
