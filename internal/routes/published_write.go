package routes

import (
	"encoding/json"
	"net/http"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/runtime"
)

// Published writes use the same draft validation and promotion as the console,
// inside one conditional transaction. Independent console drafts are untouched.
func (s *Server) createRoute(r *http.Request, _ access.Principal) (access.Reply, error) {
	return s.writePublished(r, "")
}

func (s *Server) updateRoute(r *http.Request, _ access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "route_id")
	if err != nil {
		return access.Reply{}, err
	}
	return s.writePublished(r, id)
}

func (s *Server) writePublished(r *http.Request, id string) (access.Reply, error) {
	var input DraftInput
	if err := access.DecodeUnique(r, &input, 1<<20); err != nil {
		return access.Reply{}, err
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
	var previous *routeRow
	if id != "" {
		previous, err = scanRoute(tx.QueryRow(r.Context(), "SELECT "+routeColumns+routeFrom+" WHERE r.id=$1 FOR UPDATE OF r", id))
		if err != nil {
			return access.Reply{}, err
		}
		if err = p.Project(previous.ProjectID, access.Change); err != nil {
			return access.Reply{}, err
		}
	} else if err = a.RequireProject(r.Context(), tx, p, input.ProjectID); err != nil {
		return access.Reply{}, err
	}
	claim, replayed, err := a.Replay(r, tx, p, input)
	if err != nil {
		return access.Reply{}, err
	}
	if replayed != nil {
		return access.Commit(r, tx, *replayed)
	}
	var targets []runtime.PublishedTarget
	if previous != nil {
		if err = access.Match(r, previous.ETag); err != nil {
			return access.Reply{}, err
		}
		if previous.State != "active" {
			return access.Reply{}, access.Fail(409, "route_retired", "Import or restore retired routes explicitly before publishing a replacement.")
		}
		if input.Slug != previous.Slug {
			return access.Reply{}, access.Invalid("slug", "A published route's slug is immutable.")
		}
		if input.ProjectID != nil && !sameProject(input.ProjectID, previous.ProjectID) {
			return access.Reply{}, access.Invalid("project_id", "A published route's project is immutable.")
		}
		input.ProjectID = previous.ProjectID
		targets = previous.Latest.Targets
	} else {
		var exists bool
		if err = tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM olp.routes WHERE slug=$1)", input.Slug).Scan(&exists); err != nil {
			return access.Reply{}, err
		}
		if exists {
			return access.Reply{}, access.Fail(409, "route_exists", "Import the existing route before changing its published configuration.")
		}
	}
	targets, err = ValidateDraftInput(r.Context(), tx, &input, input.ProjectID, targets)
	if err != nil {
		return access.Reply{}, err
	}
	draftID, draftETag := access.NewID(), access.NewID()
	operations, _ := json.Marshal(input.Operations)
	encoded, _ := json.Marshal(targets)
	if _, err = tx.Exec(r.Context(), `INSERT INTO olp.route_drafts(id,slug,state,operations,overall_timeout_ms,max_attempts,targets,content_policy,etag,created_by,project_id,fidelity,behavior)
		VALUES($1,$2,'draft',$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, draftID, input.Slug, operations, input.OverallTimeoutMS, input.MaxAttempts, encoded, input.ContentPolicy, draftETag, p.UserID(), input.ProjectID, input.Fidelity, input.Behavior); err != nil {
		return access.Reply{}, err
	}
	if previous != nil {
		policy, _ := json.Marshal(policyOrDefault(previous.Latest.Policy))
		if _, err = tx.Exec(r.Context(), "INSERT INTO olp.routing_policies(scope,scope_id,policy,etag,updated_by) VALUES('route-draft',$1,$2,$3,$4)", draftID, policy, draftETag, p.UserID()); err != nil {
			return access.Reply{}, err
		}
	}
	current, err := loadDraft(r.Context(), tx, draftID, false)
	if err != nil {
		return access.Reply{}, err
	}
	promoted, err := s.promote(r.Context(), tx, current, p.UserID())
	if err != nil {
		return access.Reply{}, err
	}
	if _, err = runtime.Publish(r.Context(), tx, p.UserID()); err != nil {
		return access.Reply{}, err
	}
	action := "route.update"
	if previous == nil {
		action = "route.create"
	}
	if err = access.Audit(r.Context(), tx, r, p.Actor(), action, "route", promoted.routeID, "success"); err != nil {
		return access.Reply{}, err
	}
	row, err := scanRoute(tx.QueryRow(r.Context(), "SELECT "+routeColumns+routeFrom+" WHERE r.id=$1", promoted.routeID))
	if err != nil {
		return access.Reply{}, err
	}
	body, err := s.routeJSON(r.Context(), tx, row)
	if err != nil {
		return access.Reply{}, err
	}
	reply := access.Detail(body, row.ETag)
	if previous == nil {
		reply.Status, reply.Location = http.StatusCreated, "/api/v1/routes/"+row.ID
	}
	if err = a.CompleteReplay(r, tx, claim, reply); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, reply)
}
