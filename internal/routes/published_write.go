package routes

import (
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/authz"
	"github.com/tyk-swe/olp/internal/routing"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/release"
)

// Published writes use the same draft validation and promotion as the console,
// inside one conditional transaction. Independent console drafts are untouched.
func (s *Server) createRoute(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	return s.writePublished(r, "")
}

func (s *Server) updateRoute(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "route_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	return s.writePublished(r, id)
}

func (s *Server) writePublished(r *http.Request, id string) (httpkit.Reply, error) {
	var input DraftInput
	if err := httpkit.DecodeUnique(r, &input, 1<<20); err != nil {
		return httpkit.Reply{}, err
	}
	a := s.Access
	return access.Mutate(a, r, input, func(tx pgx.Tx, p authz.Principal) (*routeRow, error) {
		if id == "" {
			return nil, a.RequireProject(r.Context(), tx, p, input.ProjectID)
		}
		previous, err := scanRoute(tx.QueryRow(r.Context(), "SELECT "+routeColumns+routeFrom+" WHERE r.id=$1 FOR UPDATE OF r", id))
		if err != nil {
			return nil, err
		}
		return previous, p.Project(previous.ProjectID, authz.Change)
	}, func(tx pgx.Tx, p authz.Principal, previous *routeRow) (httpkit.Reply, access.Event, error) {
		var err error
		var targets []routing.PublishedTarget
		if previous != nil {
			if err = httpkit.Match(r, previous.ETag); err != nil {
				return httpkit.Reply{}, access.Event{}, err
			}
			if previous.State != "active" {
				return httpkit.Reply{}, access.Event{}, httpkit.Fail(409, "route_retired", "Import or restore retired routes explicitly before publishing a replacement.")
			}
			if input.Slug != previous.Slug {
				return httpkit.Reply{}, access.Event{}, httpkit.Invalid("slug", "A published route's slug is immutable.")
			}
			if input.ProjectID != nil && !sameProject(input.ProjectID, previous.ProjectID) {
				return httpkit.Reply{}, access.Event{}, httpkit.Invalid("project_id", "A published route's project is immutable.")
			}
			input.ProjectID = previous.ProjectID
			targets = previous.Latest.Targets
		} else {
			var exists bool
			if err = tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM olp.routes WHERE slug=$1)", input.Slug).Scan(&exists); err != nil {
				return httpkit.Reply{}, access.Event{}, err
			}
			if exists {
				return httpkit.Reply{}, access.Event{}, httpkit.Fail(409, "route_exists", "Import the existing route before changing its published configuration.")
			}
		}
		targets, err = ValidateAndNormalizeDraftInput(r.Context(), tx, &input, input.ProjectID, targets)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		draftID, draftETag := httpkit.NewID(), httpkit.NewID()
		if err = StageDraft(r.Context(), tx, NewDraft{ID: draftID, CreatedBy: p.UserID(), ProjectID: input.ProjectID}, draftETag, input.Content(targets, nil)); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if previous != nil {
			if err = StageDraftPolicy(r.Context(), tx, draftID, draftETag, p.UserID(), policyOrDefault(previous.Latest.Policy)); err != nil {
				return httpkit.Reply{}, access.Event{}, err
			}
		}
		current, err := loadDraft(r.Context(), tx, draftID, false)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		promoted, err := s.promote(r.Context(), tx, current, p.UserID())
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if _, err = release.Publish(r.Context(), tx, p.UserID()); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		action := "route.update"
		if previous == nil {
			action = "route.create"
		}
		row, err := scanRoute(tx.QueryRow(r.Context(), "SELECT "+routeColumns+routeFrom+" WHERE r.id=$1", promoted.routeID))
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		body, err := s.routeJSON(r.Context(), tx, row)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		reply := httpkit.Detail(body, row.ETag)
		if previous == nil {
			reply.Status, reply.Location = http.StatusCreated, "/api/v1/routes/"+row.ID
		}
		return reply, access.Event{Action: action, Resource: "route", ID: promoted.routeID}, nil
	})
}
