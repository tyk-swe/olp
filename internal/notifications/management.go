package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/authz"
	"github.com/tyk-swe/olp/internal/platform/database"
	"github.com/tyk-swe/olp/internal/platform/egress"
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

func notificationOperation(projectID *string) authz.Operation {
	if projectID == nil {
		return authz.Settings
	}
	return authz.Keys
}

func (s *Server) list(res resource) access.Handler {
	return func(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
		page, err := httpkit.Page(r)
		if err != nil {
			return httpkit.Reply{}, err
		}
		rows, err := s.access.Pool.Query(r.Context(),
			"SELECT document FROM ("+res.documentsSQL+") documents WHERE id<$1::uuid AND ($2 OR project_id=ANY($3::uuid[])) ORDER BY id DESC LIMIT $4",
			page.Before, p.AllProjects, p.ProjectIDs(), page.Limit+1)
		if err != nil {
			return httpkit.Reply{}, err
		}
		items, err := httpkit.JSONRows(rows)
		return httpkit.ListReply(items, page), err
	}
}

func (s *Server) get(res resource) access.Handler {
	return func(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
		id, err := httpkit.IDParam(r, res.parameter)
		if err != nil {
			return httpkit.Reply{}, err
		}
		found, err := res.find(r.Context(), s.access.Pool, id)
		if err != nil {
			return httpkit.Reply{}, err
		}
		if err := p.Project(found.projectID, authz.View); err != nil {
			return httpkit.Reply{}, err
		}
		return httpkit.Detail(found.body, found.etag), nil
	}
}

type creation[T any] struct {
	projectID   *string
	fingerprint any
	parse       func(projectID *string) (T, error)
	insert      func(tx pgx.Tx, p authz.Principal, projectID *string, target T, id, etag string) error
}

func create[T any](s *Server, r *http.Request, res resource, c creation[T]) (httpkit.Reply, error) {
	return access.Mutate(s.access, r, c.fingerprint, func(tx pgx.Tx, p authz.Principal) (*string, error) {
		if err := p.Authorize(notificationOperation(c.projectID)); err != nil {
			return nil, err
		}
		projectID, err := httpkit.ParseOptionalUUID(c.projectID)
		if err != nil {
			return nil, err
		}
		return projectID, s.access.RequireProject(r.Context(), tx, p, projectID)
	}, func(tx pgx.Tx, p authz.Principal, projectID *string) (httpkit.Reply, access.Event, error) {
		target, err := c.parse(projectID)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		id, etag := httpkit.NewID(), httpkit.NewID()
		if err := c.insert(tx, p, projectID, target, id, etag); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		created, err := res.find(r.Context(), tx, id)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		return httpkit.Reply{Status: http.StatusCreated, ETag: created.etag, Location: res.path + id, Body: created.body},
			access.Event{Action: res.kind + ".create", Resource: res.kind, ID: id}, nil
	})
}

func (s *Server) update(r *http.Request, res resource, apply func(tx pgx.Tx, current document, etag string) error) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, res.parameter)
	if err != nil {
		return httpkit.Reply{}, err
	}
	return access.Transact(s.access, r, access.Unlocked, func(tx pgx.Tx, p authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		locked, err := res.lockForChange(r.Context(), tx, p, id)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if err := httpkit.Match(r, locked.etag); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		current, err := res.find(r.Context(), tx, id)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if err := apply(tx, current, httpkit.NewID()); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		updated, err := res.find(r.Context(), tx, id)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		return httpkit.Detail(updated.body, updated.etag), access.Event{Action: res.kind + ".update", Resource: res.kind, ID: id}, nil
	})
}

func (s *Server) delete(res resource) access.Handler {
	return func(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
		id, err := httpkit.IDParam(r, res.parameter)
		if err != nil {
			return httpkit.Reply{}, err
		}
		return access.Remove(s.access, r, map[string]string{res.parameter: id}, func(tx pgx.Tx, p authz.Principal) (revision, error) {
			return res.lockForChange(r.Context(), tx, p, id)
		}, func(tx pgx.Tx, _ authz.Principal, locked revision) (httpkit.Reply, access.Event, error) {
			if err := httpkit.Match(r, locked.etag); err != nil {
				return httpkit.Reply{}, access.Event{}, err
			}
			if err := removalError(res.kind, res.remove(r.Context(), tx, id)); err != nil {
				return httpkit.Reply{}, access.Event{}, err
			}
			return httpkit.Reply{Status: http.StatusNoContent},
				access.Event{Action: res.kind + ".delete", Resource: res.kind, ID: id, Project: locked.projectID}, nil
		})
	}
}

func decodePatch[V any](r *http.Request, subject, unknownField string, known map[string]V) (map[string]json.RawMessage, error) {
	var patch map[string]json.RawMessage
	if err := httpkit.Decode(r, &patch); err != nil {
		return nil, err
	}
	if len(patch) == 0 {
		return nil, httpkit.Invalid(subject, "Send at least one field to update.")
	}
	for field := range patch {
		if _, ok := known[field]; !ok {
			return nil, httpkit.Invalid(field, unknownField)
		}
	}
	return patch, nil
}

func removalError(kind string, err error) error {
	var constraint *pgconn.PgError
	if errors.As(err, &constraint) && constraint.Code == "23503" {
		return httpkit.Fail(http.StatusConflict, kind+"_in_use", "The resource still has dependent resources or retained history.")
	}
	return err
}

const nameLimit = 100

type resource struct {
	parameter, kind, path string
	documentsSQL, lockSQL string
	remove                func(context.Context, pgx.Tx, string) error
}

type revision struct {
	etag      string
	projectID *string
}

type document struct {
	revision
	id   string
	body json.RawMessage
}

func (res resource) find(ctx context.Context, q database.Queryer, id string) (document, error) {
	var found document
	err := q.QueryRow(ctx, "SELECT id::text,document,etag::text,project_id::text FROM ("+res.documentsSQL+") documents WHERE id=$1", id).
		Scan(&found.id, &found.body, &found.etag, &found.projectID)
	return found, err
}

func (res resource) lockForChange(ctx context.Context, tx pgx.Tx, p authz.Principal, id string) (revision, error) {
	var locked revision
	if err := tx.QueryRow(ctx, res.lockSQL, id).Scan(&locked.etag, &locked.projectID); err != nil {
		return revision{}, err
	}
	if err := p.Project(locked.projectID, authz.View); err != nil {
		return revision{}, err
	}
	if err := p.Authorize(notificationOperation(locked.projectID)); err != nil {
		return revision{}, err
	}
	if err := p.Project(locked.projectID, authz.Change); err != nil {
		return revision{}, err
	}
	return locked, nil
}
