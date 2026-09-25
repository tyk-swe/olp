// Package routes serves route drafts, published routes, revision history, and
// routing simulation on the management surface.
package routes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/operationregistry"
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
	"generation", "token_count", "embeddings", "moderation", "rerank",
	"image_generation", "image_edit", "image_variation", "speech", "transcription", "translation",
	"video_create", "video_list", "video_get", "video_content", "video_delete",
	"batch", "realtime", "bedrock_invoke",
}

type draft struct {
	ID               string
	Slug             string
	State            string
	Operations       []string
	OverallTimeoutMS int
	MaxAttempts      int
	Targets          []runtime.PublishedTarget
	ContentPolicy    []byte
	Fidelity         []byte
	BasedOnRevision  *string
	ETag             string
	CreatedBy        string
	CreatedAt        time.Time
	UpdatedAt        time.Time
	CreatedByEmail   *string
	ProjectID        *string
	ProjectName      *string
}

const draftColumns = "d.id::text,d.slug,d.state,d.operations,d.overall_timeout_ms,d.max_attempts,d.targets,d.content_policy,d.based_on_revision_id::text,d.etag::text,d.created_by::text,d.created_at,d.updated_at,u.email,d.project_id::text,pr.name,d.fidelity"
const draftFrom = " FROM olp_go.route_drafts d JOIN olp_go.users u ON u.id=d.created_by LEFT JOIN olp_go.projects pr ON pr.id=d.project_id"

func scanDraft(row pgx.Row) (*draft, error) {
	var d draft
	var operations, targets []byte
	if err := row.Scan(&d.ID, &d.Slug, &d.State, &operations, &d.OverallTimeoutMS, &d.MaxAttempts, &targets, &d.ContentPolicy, &d.BasedOnRevision, &d.ETag, &d.CreatedBy, &d.CreatedAt, &d.UpdatedAt, &d.CreatedByEmail, &d.ProjectID, &d.ProjectName, &d.Fidelity); err != nil {
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
	query := "SELECT " + draftColumns + draftFrom + " WHERE d.id=$1"
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
		coalesce(r.models,'[]'::jsonb)
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
		var models []byte
		r := &resolved{Certified: map[string]bool{}}
		if err = rows.Scan(&id, &r.ProviderID, &r.ProviderName, &r.ProviderModel, &r.ProviderState, &models); err != nil {
			return nil, err
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
	return map[string]any{"id": d.ID, "slug": d.Slug, "state": d.State, "etag": d.ETag, "project_id": d.ProjectID, "project_name": d.ProjectName, "fidelity": json.RawMessage(d.Fidelity)}
}

func (d *draft) detail(live map[string]*resolved) map[string]any {
	return map[string]any{"id": d.ID, "slug": d.Slug, "state": d.State, "overall_timeout_ms": d.OverallTimeoutMS, "max_attempts": d.MaxAttempts, "etag": d.ETag, "operations": d.Operations, "targets": targetsJSON(d.Targets, live), "content_policy": json.RawMessage(d.ContentPolicy), "created_at": d.CreatedAt, "updated_at": d.UpdatedAt, "based_on_revision_id": d.BasedOnRevision, "created_by_email": d.CreatedByEmail, "project_id": d.ProjectID, "project_name": d.ProjectName, "fidelity": json.RawMessage(d.Fidelity)}
}

func (s *Server) draftDetail(ctx context.Context, q access.Queryer, d *draft) (access.Reply, error) {
	live, err := resolve(ctx, q, d.Targets)
	if err != nil {
		return access.Reply{}, err
	}
	return access.Detail(d.detail(live), d.ETag), nil
}

type TargetInput struct {
	ProviderID      *string `json:"provider_id"`
	ProviderModel   *string `json:"provider_model"`
	ProviderModelID *string `json:"provider_model_id"`
	Priority        int     `json:"priority"`
	Weight          int64   `json:"weight"`
	TimeoutMS       int     `json:"timeout_ms"`
}

type DraftInput struct {
	Slug             string          `json:"slug"`
	Operations       []string        `json:"operations"`
	OverallTimeoutMS int             `json:"overall_timeout_ms"`
	MaxAttempts      int             `json:"max_attempts"`
	Targets          []TargetInput   `json:"targets"`
	ContentPolicy    json.RawMessage `json:"content_policy"`
	Fidelity         json.RawMessage `json:"fidelity,omitempty"`
	ProjectID        *string         `json:"project_id"`
}

func sameProject(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func ValidateDraftInput(ctx context.Context, q access.Queryer, in *DraftInput, projectID *string, existing []runtime.PublishedTarget) ([]runtime.PublishedTarget, error) {
	if !access.RouteSlug.MatchString(in.Slug) {
		return nil, access.Invalid("slug", "Use 1–100 lowercase letters, digits, dots, underscores, or hyphens, starting with a letter or digit.")
	}
	if len(in.Operations) == 0 {
		in.Operations = []string{"generation"}
	}
	for _, op := range in.Operations {
		if !slices.Contains(supportedOperations, op) && !registeredOperation(op) {
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
	if len(in.ContentPolicy) > 0 && !bytes.Equal(bytes.TrimSpace(in.ContentPolicy), []byte("null")) {
		policy, err := contentpolicy.Decode(in.ContentPolicy)
		if err != nil {
			return nil, access.Invalid("content_policy", err.Error())
		}
		in.ContentPolicy, _ = json.Marshal(policy)
	} else {
		in.ContentPolicy = nil
	}
	var fidelityErr error
	in.Fidelity, fidelityErr = normalizedFidelity(in.Fidelity)
	if fidelityErr != nil {
		return nil, fidelityErr
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
		var providerProject *string
		var err error
		switch {
		case t.ProviderModelID != nil:
			if modelID, err = access.ParseUUID(*t.ProviderModelID); err != nil {
				return nil, access.Invalid("targets.provider_model_id", "Use a provider model identifier.")
			}
			err = q.QueryRow(ctx, "SELECT p.id::text,p.name,m.upstream_model,p.project_id::text FROM olp_go.provider_models m JOIN olp_go.providers p ON p.id=m.provider_id WHERE m.id=$1", modelID).Scan(&providerID, &providerName, &providerModel, &providerProject)
		case t.ProviderID != nil && t.ProviderModel != nil:
			if providerID, err = access.ParseUUID(*t.ProviderID); err != nil {
				return nil, access.Invalid("targets.provider_id", "Use a provider identifier.")
			}
			err = q.QueryRow(ctx, "SELECT m.id::text,p.name,m.upstream_model,p.project_id::text FROM olp_go.provider_models m JOIN olp_go.providers p ON p.id=m.provider_id WHERE p.id=$1 AND m.upstream_model=$2", providerID, *t.ProviderModel).Scan(&modelID, &providerName, &providerModel, &providerProject)
		default:
			return nil, access.Invalid("targets", "Name each target by provider_model_id or by provider_id and provider_model.")
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, access.Invalid("targets."+strconv.Itoa(i)+".provider_model", "Target "+strconv.Itoa(i)+" names a model this installation does not know.")
		}
		if err != nil {
			return nil, err
		}
		if !sameProject(providerProject, projectID) {
			return nil, access.Fail(422, "target_project_mismatch", "Target "+strconv.Itoa(i)+" belongs to a different project than the draft.")
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
	p, err := s.Access.Principal(r, s.Access.Pool, "read")
	if err != nil {
		return access.Reply{}, err
	}
	page, err := access.Page(r)
	if err != nil {
		return access.Reply{}, err
	}
	rows, err := s.Access.Pool.Query(r.Context(), "SELECT "+draftColumns+draftFrom+" WHERE d.id<$1 AND ($2 OR d.project_id=ANY($3::uuid[])) ORDER BY d.id DESC LIMIT $4", page.Before, p.AllProjects, p.ProjectIDs(), page.Limit+1)
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
	p, err := s.Access.Principal(r, s.Access.Pool, "read")
	if err != nil {
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
	if !p.CanProject(d.ProjectID, false) {
		return access.Reply{}, pgx.ErrNoRows
	}
	return s.draftDetail(r.Context(), s.Access.Pool, d)
}

func (s *Server) createDraft(r *http.Request) (access.Reply, error) {
	a := s.Access
	var input DraftInput
	if err := access.DecodeUnique(r, &input, 1<<20); err != nil {
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
	if err = a.RequireProject(r.Context(), tx, p, input.ProjectID, true); err != nil {
		return access.Reply{}, err
	}
	if len(input.Fidelity) == 0 {
		input.Fidelity, err = PublishedFidelity(r.Context(), tx, input.Slug, input.ProjectID)
		if err != nil {
			return access.Reply{}, err
		}
	}
	targets, err := ValidateDraftInput(r.Context(), tx, &input, input.ProjectID, nil)
	if err != nil {
		return access.Reply{}, err
	}
	id, etag := access.NewID(), access.NewID()
	operations, _ := json.Marshal(input.Operations)
	encoded, _ := json.Marshal(targets)
	if _, err = tx.Exec(r.Context(), "INSERT INTO olp_go.route_drafts(id,slug,state,operations,overall_timeout_ms,max_attempts,targets,content_policy,etag,created_by,project_id,fidelity) VALUES($1,$2,'draft',$3,$4,$5,$6,$7,$8,$9,$10,$11)", id, input.Slug, operations, input.OverallTimeoutMS, input.MaxAttempts, encoded, input.ContentPolicy, etag, p.UserID(), input.ProjectID, input.Fidelity); err != nil {
		return access.Reply{}, err
	}
	if err = access.Audit(r.Context(), tx, r, p.ID, "route_draft.create", "route_draft", id, "success"); err != nil {
		return access.Reply{}, err
	}
	result := access.Reply{Status: 201, Location: "/api/v3/route-drafts/" + id, ETag: etag, Body: map[string]any{"id": id, "slug": input.Slug, "state": "draft", "etag": etag, "project_id": input.ProjectID, "fidelity": json.RawMessage(input.Fidelity)}}
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
	var input DraftInput
	if err = access.DecodeUnique(r, &input, 1<<20); err != nil {
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
	if err := access.ProjectAccess(p, current.ProjectID, true); err != nil {
		return access.Reply{}, err
	}
	if err = access.Match(r, current.ETag); err != nil {
		return access.Reply{}, err
	}
	if input.ProjectID != nil && !sameProject(input.ProjectID, current.ProjectID) {
		return access.Reply{}, access.Invalid("project_id", "The draft's project is set at creation and cannot change.")
	}
	if len(input.Fidelity) == 0 {
		input.Fidelity = bytes.Clone(current.Fidelity)
	}
	if len(input.Fidelity) == 0 {
		input.Fidelity, err = PublishedFidelity(r.Context(), tx, input.Slug, current.ProjectID)
		if err != nil {
			return access.Reply{}, err
		}
	}
	targets, err := ValidateDraftInput(r.Context(), tx, &input, current.ProjectID, current.Targets)
	if err != nil {
		return access.Reply{}, err
	}
	etag := access.NewID()
	operations, _ := json.Marshal(input.Operations)
	encoded, _ := json.Marshal(targets)
	if _, err = tx.Exec(r.Context(), "UPDATE olp_go.route_drafts SET slug=$2,state='draft',operations=$3,overall_timeout_ms=$4,max_attempts=$5,targets=$6,content_policy=$7,etag=$8,fidelity=$9,updated_at=now() WHERE id=$1", id, input.Slug, operations, input.OverallTimeoutMS, input.MaxAttempts, encoded, input.ContentPolicy, etag, input.Fidelity); err != nil {
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
	if err := access.ProjectAccess(p, current.ProjectID, true); err != nil {
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

func registeredOperation(operation string) bool {
	for _, d := range operationregistry.Default.Dialects() {
		if d.Operation.ID == operation {
			return true
		}
	}
	return false
}
