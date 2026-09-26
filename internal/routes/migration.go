package routes

import (
	"encoding/json"
	"net/http"
	"slices"

	"github.com/tyk-swe/olp/internal/access"
)

// migrationDraft copies the published configuration for review under an unseen
// route identity. Creation never activates a route or invokes a provider.
func (s *Server) migrationDraft(r *http.Request) (access.Reply, error) {
	var input struct {
		Slug     string          `json:"slug"`
		Fidelity json.RawMessage `json:"fidelity,omitempty"`
	}
	if err := access.DecodeUnique(r, &input, 4096); err != nil {
		return access.Reply{}, err
	}
	if !access.RouteSlug.MatchString(input.Slug) {
		return access.Reply{}, access.Invalid("slug", "Use a previously unpublished route slug with 1–100 lowercase letters, digits, dots, underscores, or hyphens.")
	}
	if len(input.Fidelity) == 0 {
		input.Fidelity = json.RawMessage(`{"mode":"strict"}`)
	}
	fidelity, err := normalizedFidelity(input.Fidelity)
	if err != nil {
		return access.Reply{}, err
	}
	id, err := access.IDParam(r, "route_id")
	if err != nil {
		return access.Reply{}, err
	}
	a := s.Access
	tx, err := a.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := a.Principal(r, tx, "configure")
	if err != nil {
		return access.Reply{}, err
	}
	claim, replayed, err := a.Replay(r, tx, p, input)
	if err != nil {
		return access.Reply{}, err
	}
	if replayed != nil {
		return access.Commit(r, tx, *replayed)
	}
	current, err := scanRoute(tx.QueryRow(r.Context(), "SELECT "+routeColumns+routeFrom+" WHERE r.id=$1 FOR UPDATE OF r", id))
	if err != nil {
		return access.Reply{}, err
	}
	if err = access.ProjectAccess(p, current.ProjectID, true); err != nil {
		return access.Reply{}, err
	}
	if err = access.Match(r, current.ETag); err != nil {
		return access.Reply{}, err
	}
	var published bool
	if err = tx.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM olp.routes WHERE slug=$1)", input.Slug).Scan(&published); err != nil {
		return access.Reply{}, err
	}
	if published {
		return access.Reply{}, access.Fail(422, "route_fidelity_migration_required", "Choose a previously unpublished slug; retired route identities cannot be reused.")
	}
	v := current.Latest
	draftID, etag := access.NewID(), access.NewID()
	operations, _ := json.Marshal(v.Operations)
	targets := slices.Clone(v.Targets)
	for i := range targets {
		targets[i].ID = access.NewID()
	}
	encoded, _ := json.Marshal(targets)
	if _, err = tx.Exec(r.Context(), "INSERT INTO olp.route_drafts(id,slug,state,operations,overall_timeout_ms,max_attempts,targets,content_policy,based_on_revision_id,etag,created_by,project_id,fidelity) VALUES($1,$2,'draft',$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)", draftID, input.Slug, operations, v.OverallTimeoutMS, v.MaxAttempts, encoded, v.ContentPolicy, v.ID, etag, p.UserID(), current.ProjectID, fidelity); err != nil {
		return access.Reply{}, err
	}
	if v.Policy != nil {
		policy, _ := json.Marshal(v.Policy)
		if _, err = tx.Exec(r.Context(), "INSERT INTO olp.routing_policies(scope,scope_id,policy,etag,updated_by) VALUES('route-draft',$1,$2,$3,$4)", draftID, policy, etag, p.UserID()); err != nil {
			return access.Reply{}, err
		}
	}
	if err = access.Audit(r.Context(), tx, r, p.ID, "route_draft.migrate", "route_draft", draftID, "success"); err != nil {
		return access.Reply{}, err
	}
	d, err := loadDraft(r.Context(), tx, draftID, false)
	if err != nil {
		return access.Reply{}, err
	}
	result, err := s.draftDetail(r.Context(), tx, d)
	if err != nil {
		return access.Reply{}, err
	}
	result.Status, result.Location = 201, "/api/v1/route-drafts/"+draftID
	if err = a.CompleteReplay(r, tx, claim, result); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, result)
}
