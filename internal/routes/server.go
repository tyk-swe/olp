// Package routes serves route drafts, published routes, revision history, and
// routing simulation on the management surface.
package routes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/usage"
)

// Server serves the route management surface.
type Server struct {
	Access *access.Server
	Inputs func() *usage.RoutingInputs
}

// New prepares the route surface.
func New(a *access.Server) *Server { return &Server{Access: a} }

const (
	maxTargets      = 64
	maxTimeoutMS    = 3600000
	maxAttempts     = 32767
	maxPriority     = 32767
	maxTargetWeight = 1000000
)

var supportedOperations = []string{
	"generation", "token_count", "embeddings", "moderation",
	"image_generation", "image_edit", "image_variation", "speech", "transcription",
	"video_create", "video_list", "video_get", "video_content", "video_delete",
}

type draft struct {
	ID               string
	Slug             string
	State            string
	Operations       []string
	OverallTimeoutMS int
	MaxAttempts      int
	Targets          []runtime.PublishedTarget
	BasedOnRevision  *string
	ETag             string
	CreatedBy        string
	CreatedAt        time.Time
	UpdatedAt        time.Time
	CreatedByEmail   *string
}

const draftColumns = "d.id::text,d.slug,d.state,d.operations,d.overall_timeout_ms,d.max_attempts,d.targets,d.based_on_revision_id::text,d.etag::text,d.created_by::text,d.created_at,d.updated_at,u.email"

func scanDraft(row pgx.Row) (*draft, error) {
	var d draft
	var operations, targets []byte
	if err := row.Scan(&d.ID, &d.Slug, &d.State, &operations, &d.OverallTimeoutMS, &d.MaxAttempts, &targets, &d.BasedOnRevision, &d.ETag, &d.CreatedBy, &d.CreatedAt, &d.UpdatedAt, &d.CreatedByEmail); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(operations, &d.Operations); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(targets, &d.Targets); err != nil {
		return nil, err
	}
	d.CreatedAt, d.UpdatedAt = d.CreatedAt.UTC(), d.UpdatedAt.UTC()
	return &d, nil
}

func loadDraft(ctx context.Context, q access.Queryer, id string, lock bool) (*draft, error) {
	query := "SELECT " + draftColumns + " FROM olp_go.route_drafts d JOIN olp_go.users u ON u.id=d.created_by WHERE d.id=$1"
	if lock {
		query += " FOR UPDATE OF d"
	}
	return scanDraft(q.QueryRow(ctx, query, id))
}

// resolved is the live state of one provider model referenced by a target.
type resolved struct {
	ProviderID    string
	ProviderName  string
	ProviderModel string
	ProviderState string
	Published     bool
	Certified     map[string]bool
	AuthMode      string
	Slots         []runtime.Slot
}

func (r *resolved) available() bool {
	return r != nil && r.ProviderState == "active" && r.Published && len(r.Certified) > 0
}

// resolve looks up every provider model referenced by the targets against the
// providers' activated revisions, which is what the gateway will serve.
func resolve(ctx context.Context, q access.Queryer, targets []runtime.PublishedTarget) (map[string]*resolved, error) {
	ids := make([]string, 0, len(targets))
	for _, t := range targets {
		ids = append(ids, t.ProviderModelID)
	}
	rows, err := q.Query(ctx, `SELECT m.id::text,p.id::text,p.name,m.upstream_model,p.state,
		coalesce(r.models,'[]'::jsonb),coalesce(r.configuration->>'auth_mode','none'),
		coalesce(r.slots,'[]'::jsonb),
		ARRAY(SELECT c.id::text FROM olp_go.provider_credentials c WHERE c.provider_id=p.id AND c.revoked_at IS NULL)
		FROM olp_go.provider_models m JOIN olp_go.providers p ON p.id=m.provider_id
		LEFT JOIN olp_go.provider_revisions r ON r.id=p.active_revision_id
		WHERE m.id=ANY($1::uuid[])`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*resolved{}
	for rows.Next() {
		var id string
		var models, slots []byte
		var credentials []string
		r := &resolved{Certified: map[string]bool{}}
		if err = rows.Scan(&id, &r.ProviderID, &r.ProviderName, &r.ProviderModel, &r.ProviderState, &models, &r.AuthMode, &slots, &credentials); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(slots, &r.Slots); err != nil {
			return nil, err
		}
		for i := range r.Slots {
			slot := &r.Slots[i]
			if slot.CredentialID != nil && !slices.Contains(credentials, *slot.CredentialID) {
				slot.CredentialID = nil
			}
		}
		var published []runtime.RevisionModel
		if err = json.Unmarshal(models, &published); err != nil {
			return nil, err
		}
		for _, m := range published {
			if m.ID != id {
				continue
			}
			r.Published = true
			for _, c := range m.Capabilities {
				if c.Source == "certified" {
					r.Certified[c.Operation+"/"+c.Surface+"/"+c.Mode] = true
				}
			}
		}
		out[id] = r
	}
	return out, rows.Err()
}

func targetJSON(t runtime.PublishedTarget, live *resolved) map[string]any {
	name, model, providerID := t.ProviderName, t.ProviderModel, t.ProviderID
	if live != nil {
		name, model, providerID = live.ProviderName, live.ProviderModel, live.ProviderID
	}
	return map[string]any{"id": t.ID, "provider_model_id": t.ProviderModelID, "provider_id": providerID, "provider_name": name, "provider_model": model, "available": live.available(), "priority": t.Priority, "weight": t.Weight, "timeout_ms": t.TimeoutMS, "position": t.Position}
}

func targetsJSON(targets []runtime.PublishedTarget, live map[string]*resolved) []map[string]any {
	out := make([]map[string]any, 0, len(targets))
	for _, t := range targets {
		out = append(out, targetJSON(t, live[t.ProviderModelID]))
	}
	return out
}

func (d *draft) summary() map[string]any {
	return map[string]any{"id": d.ID, "slug": d.Slug, "state": d.State, "etag": d.ETag}
}

func (d *draft) detail(live map[string]*resolved) map[string]any {
	return map[string]any{"id": d.ID, "slug": d.Slug, "state": d.State, "overall_timeout_ms": d.OverallTimeoutMS, "max_attempts": d.MaxAttempts, "etag": d.ETag, "operations": d.Operations, "targets": targetsJSON(d.Targets, live), "created_at": d.CreatedAt, "updated_at": d.UpdatedAt, "based_on_revision_id": d.BasedOnRevision, "created_by_email": d.CreatedByEmail}
}

func (s *Server) draftDetail(ctx context.Context, q access.Queryer, d *draft) (access.Reply, error) {
	live, err := resolve(ctx, q, d.Targets)
	if err != nil {
		return access.Reply{}, err
	}
	return access.Detail(d.detail(live), d.ETag), nil
}

type targetInput struct {
	ProviderID      *string `json:"provider_id"`
	ProviderModel   *string `json:"provider_model"`
	ProviderModelID *string `json:"provider_model_id"`
	Priority        int     `json:"priority"`
	Weight          int64   `json:"weight"`
	TimeoutMS       int     `json:"timeout_ms"`
}

type draftInput struct {
	Slug             string        `json:"slug"`
	Operations       []string      `json:"operations"`
	OverallTimeoutMS int           `json:"overall_timeout_ms"`
	MaxAttempts      int           `json:"max_attempts"`
	Targets          []targetInput `json:"targets"`
}

// validateInput checks the envelope and resolves every target to a provider
// model, keeping the caller's order as the stored position.
func validateInput(ctx context.Context, q access.Queryer, in *draftInput, existing []runtime.PublishedTarget) ([]runtime.PublishedTarget, error) {
	if !access.RouteSlug.MatchString(in.Slug) {
		return nil, access.Invalid("slug", "Use 1–100 lowercase letters, digits, dots, underscores, or hyphens, starting with a letter or digit.")
	}
	if len(in.Operations) == 0 {
		in.Operations = []string{"generation"}
	}
	for _, op := range in.Operations {
		if !slices.Contains(supportedOperations, op) {
			return nil, access.Fail(422, "operation_unavailable", "The "+op+" operation is not available in this release.")
		}
	}
	slices.Sort(in.Operations)
	in.Operations = slices.Compact(in.Operations)
	if in.OverallTimeoutMS < 1 || in.OverallTimeoutMS > maxTimeoutMS {
		return nil, access.Invalid("overall_timeout_ms", "Use a timeout from 1 to 3600000 milliseconds.")
	}
	if in.MaxAttempts < 1 || in.MaxAttempts > maxAttempts {
		return nil, access.Invalid("max_attempts", "Use an attempt budget from 1 to 32767.")
	}
	if len(in.Targets) == 0 || len(in.Targets) > maxTargets {
		return nil, access.Invalid("targets", "Use 1–64 targets.")
	}
	previous := map[string]runtime.PublishedTarget{}
	for _, t := range existing {
		previous[t.ProviderModelID] = t
	}
	targets := make([]runtime.PublishedTarget, 0, len(in.Targets))
	seen := map[string]bool{}
	for i, t := range in.Targets {
		if t.Priority < 0 || t.Priority > maxPriority {
			return nil, access.Invalid("targets.priority", "Use a priority from 0 to 32767.")
		}
		if t.Weight < 1 || t.Weight > maxTargetWeight {
			return nil, access.Invalid("targets.weight", "Use a weight from 1 to 1000000.")
		}
		if t.TimeoutMS < 1 || t.TimeoutMS > in.OverallTimeoutMS {
			return nil, access.Invalid("targets.timeout_ms", "Use a target timeout from 1 millisecond up to the overall timeout.")
		}
		var modelID, providerID, providerName, providerModel string
		var err error
		switch {
		case t.ProviderModelID != nil:
			if modelID, err = access.ParseUUID(*t.ProviderModelID); err != nil {
				return nil, access.Invalid("targets.provider_model_id", "Use a provider model identifier.")
			}
			err = q.QueryRow(ctx, "SELECT p.id::text,p.name,m.upstream_model FROM olp_go.provider_models m JOIN olp_go.providers p ON p.id=m.provider_id WHERE m.id=$1", modelID).Scan(&providerID, &providerName, &providerModel)
		case t.ProviderID != nil && t.ProviderModel != nil:
			if providerID, err = access.ParseUUID(*t.ProviderID); err != nil {
				return nil, access.Invalid("targets.provider_id", "Use a provider identifier.")
			}
			err = q.QueryRow(ctx, "SELECT m.id::text,p.name,m.upstream_model FROM olp_go.provider_models m JOIN olp_go.providers p ON p.id=m.provider_id WHERE p.id=$1 AND m.upstream_model=$2", providerID, *t.ProviderModel).Scan(&modelID, &providerName, &providerModel)
		default:
			return nil, access.Invalid("targets", "Name each target by provider_model_id or by provider_id and provider_model.")
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, access.Invalid("targets."+strconv.Itoa(i)+".provider_model", "Target "+strconv.Itoa(i)+" names a model this installation does not know.")
		}
		if err != nil {
			return nil, err
		}
		if seen[modelID] {
			return nil, access.Invalid("targets", "Each provider model may appear once per route.")
		}
		seen[modelID] = true
		id := access.NewID()
		if prev, ok := previous[modelID]; ok {
			id = prev.ID
		}
		targets = append(targets, runtime.PublishedTarget{ID: id, ProviderModelID: modelID, ProviderID: providerID, ProviderName: providerName, ProviderModel: providerModel, Priority: t.Priority, Weight: t.Weight, TimeoutMS: int64(t.TimeoutMS), Position: i})
	}
	return targets, nil
}

func (s *Server) drafts(r *http.Request) (access.Reply, error) {
	if _, err := s.Access.Principal(r, s.Access.Pool, "read"); err != nil {
		return access.Reply{}, err
	}
	page, err := access.Page(r)
	if err != nil {
		return access.Reply{}, err
	}
	rows, err := s.Access.Pool.Query(r.Context(), "SELECT "+draftColumns+" FROM olp_go.route_drafts d JOIN olp_go.users u ON u.id=d.created_by WHERE d.id<$1 ORDER BY d.id DESC LIMIT $2", page.Before, page.Limit+1)
	if err != nil {
		return access.Reply{}, err
	}
	defer rows.Close()
	var drafts []*draft
	var all []runtime.PublishedTarget
	for rows.Next() {
		d, err := scanDraft(rows)
		if err != nil {
			return access.Reply{}, err
		}
		drafts = append(drafts, d)
		all = append(all, d.Targets...)
	}
	if err = rows.Err(); err != nil {
		return access.Reply{}, err
	}
	rows.Close()
	live, err := resolve(r.Context(), s.Access.Pool, all)
	if err != nil {
		return access.Reply{}, err
	}
	items := []map[string]any{}
	for _, d := range drafts {
		items = append(items, d.detail(live))
	}
	return access.ListReply(items, page), nil
}

func (s *Server) draft(r *http.Request) (access.Reply, error) {
	if _, err := s.Access.Principal(r, s.Access.Pool, "read"); err != nil {
		return access.Reply{}, err
	}
	id, err := access.IDParam(r, "draft_id")
	if err != nil {
		return access.Reply{}, err
	}
	d, err := loadDraft(r.Context(), s.Access.Pool, id, false)
	if err != nil {
		return access.Reply{}, err
	}
	return s.draftDetail(r.Context(), s.Access.Pool, d)
}

func (s *Server) createDraft(r *http.Request) (access.Reply, error) {
	a := s.Access
	var input draftInput
	if err := access.Decode(r, &input); err != nil {
		return access.Reply{}, err
	}
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
	targets, err := validateInput(r.Context(), tx, &input, nil)
	if err != nil {
		return access.Reply{}, err
	}
	id, etag := access.NewID(), access.NewID()
	operations, _ := json.Marshal(input.Operations)
	encoded, _ := json.Marshal(targets)
	if _, err = tx.Exec(r.Context(), "INSERT INTO olp_go.route_drafts(id,slug,state,operations,overall_timeout_ms,max_attempts,targets,etag,created_by) VALUES($1,$2,'draft',$3,$4,$5,$6,$7,$8)", id, input.Slug, operations, input.OverallTimeoutMS, input.MaxAttempts, encoded, etag, p.ID); err != nil {
		return access.Reply{}, err
	}
	if err = access.Audit(r.Context(), tx, r, p.ID, "route_draft.create", "route_draft", id, "success"); err != nil {
		return access.Reply{}, err
	}
	result := access.Reply{Status: 201, Location: "/api/v3/route-drafts/" + id, ETag: etag, Body: map[string]any{"id": id, "slug": input.Slug, "state": "draft", "etag": etag}}
	if err = a.CompleteReplay(r, tx, claim, result); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, result)
}

func (s *Server) replaceDraft(r *http.Request) (access.Reply, error) {
	a := s.Access
	id, err := access.IDParam(r, "draft_id")
	if err != nil {
		return access.Reply{}, err
	}
	var input draftInput
	if err = access.Decode(r, &input); err != nil {
		return access.Reply{}, err
	}
	tx, err := a.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := a.Principal(r, tx, "configure")
	if err != nil {
		return access.Reply{}, err
	}
	current, err := loadDraft(r.Context(), tx, id, true)
	if err != nil {
		return access.Reply{}, err
	}
	if err = access.Match(r, current.ETag); err != nil {
		return access.Reply{}, err
	}
	targets, err := validateInput(r.Context(), tx, &input, current.Targets)
	if err != nil {
		return access.Reply{}, err
	}
	etag := access.NewID()
	operations, _ := json.Marshal(input.Operations)
	encoded, _ := json.Marshal(targets)
	if _, err = tx.Exec(r.Context(), "UPDATE olp_go.route_drafts SET slug=$2,state='draft',operations=$3,overall_timeout_ms=$4,max_attempts=$5,targets=$6,etag=$7,updated_at=now() WHERE id=$1", id, input.Slug, operations, input.OverallTimeoutMS, input.MaxAttempts, encoded, etag); err != nil {
		return access.Reply{}, err
	}
	if err = access.Audit(r.Context(), tx, r, p.ID, "route_draft.update", "route_draft", id, "success"); err != nil {
		return access.Reply{}, err
	}
	updated, err := loadDraft(r.Context(), tx, id, false)
	if err != nil {
		return access.Reply{}, err
	}
	result, err := s.draftDetail(r.Context(), tx, updated)
	if err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, result)
}

func (s *Server) deleteDraft(r *http.Request) (access.Reply, error) {
	a := s.Access
	id, err := access.IDParam(r, "draft_id")
	if err != nil {
		return access.Reply{}, err
	}
	tx, err := a.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := a.Principal(r, tx, "configure")
	if err != nil {
		return access.Reply{}, err
	}
	current, err := loadDraft(r.Context(), tx, id, true)
	if err != nil {
		return access.Reply{}, err
	}
	if err = access.Match(r, current.ETag); err != nil {
		return access.Reply{}, err
	}
	if _, err = tx.Exec(r.Context(), "DELETE FROM olp_go.route_drafts WHERE id=$1", id); err != nil {
		return access.Reply{}, err
	}
	if err = access.Audit(r.Context(), tx, r, p.ID, "route_draft.delete", "route_draft", id, "success"); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, access.Reply{Status: 204})
}
