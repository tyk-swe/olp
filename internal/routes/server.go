// Package routes serves route drafts, published routes, revision history, and
// routing simulation on the management surface.
package routes

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/authz"
	"github.com/tyk-swe/olp/internal/catalog"
	"github.com/tyk-swe/olp/internal/domain/protocols"
	"github.com/tyk-swe/olp/internal/domain/vendors"
	"github.com/tyk-swe/olp/internal/mcp"
	"github.com/tyk-swe/olp/internal/platform/database"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage/routinginputs"
)

// Server serves the route management surface.
type Server struct {
	Region          string
	Access          *access.Server
	Inputs          func() *routinginputs.Inputs
	Workflows       mcp.WorkflowResolver
	WorkflowRelease func() *release.Release
	InlineMedia     protocols.InlineMediaLimits
	// Catalog is the reference catalog this release ships, whose documented
	// retirements targets carry.
	Catalog *catalog.Signed
	// UnconfinedPlugins is set where the deployment enables unconfined
	// plugins; previews plan as its gateways do.
	UnconfinedPlugins bool
	// Supply and Fleet give simulation the capacity, spend-cap and shared
	// circuit state the gateways plan with; nil without shared state.
	Supply routing.SupplyReader
	Fleet  interface {
		Circuits(ctx context.Context, now time.Time) (map[string]time.Time, error)
	}
}

const (
	maxTargets      = 64
	maxTimeoutMS    = 3600000
	maxAttempts     = 32767
	maxPriority     = 32767
	maxTargetWeight = 1000000
)

var supportedOperations = slices.Concat(
	[]string{"generation", "decision", "token_count", "embeddings", "moderation", "rerank", "ocr", "search"},
	vendors.MediaOperations(),
	[]string{"batch", "realtime", "bedrock_invoke", "passthrough"},
)

type draft struct {
	ID               string
	Slug             string
	State            string
	Operations       []string
	OverallTimeoutMS int
	MaxAttempts      int
	Targets          []routing.PublishedTarget
	Fidelity         []byte
	Behavior         []byte
	BasedOnRevision  *string
	ETag             string
	CreatedBy        string
	CreatedAt        time.Time
	UpdatedAt        time.Time
	CreatedByEmail   *string
	ProjectID        *string
	ProjectName      *string
}

const draftColumns = "d.id::text,d.slug,d.state,d.operations,d.overall_timeout_ms,d.max_attempts,d.targets,d.based_on_revision_id::text,d.etag::text,d.created_by::text,d.created_at,d.updated_at,u.email,d.project_id::text,pr.name,d.fidelity,d.behavior"
const draftFrom = " FROM olp.route_drafts d JOIN olp.users u ON u.id=d.created_by LEFT JOIN olp.projects pr ON pr.id=d.project_id"

func scanDraft(row pgx.Row) (*draft, error) {
	var d draft
	var operations, targets []byte
	if err := row.Scan(&d.ID, &d.Slug, &d.State, &operations, &d.OverallTimeoutMS, &d.MaxAttempts, &targets, &d.BasedOnRevision, &d.ETag, &d.CreatedBy, &d.CreatedAt, &d.UpdatedAt, &d.CreatedByEmail, &d.ProjectID, &d.ProjectName, &d.Fidelity, &d.Behavior); err != nil {
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

func loadDraft(ctx context.Context, q database.Queryer, id string, lock bool) (*draft, error) {
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
	AuthMode      string
	Slots         []routing.Slot
	// Lifecycle is the target model's documented deprecation and retirement.
	Lifecycle *catalog.LifecycleView
}

func (r *resolved) available() bool {
	return r != nil && r.ProviderState == "active" && r.Published && len(r.Certified) > 0
}

// resolve looks up every provider model referenced by the targets against the
// providers' activated revisions, which is what the gateway will serve, and
// the reference catalog's documented lifecycle of each.
func (s *Server) resolve(ctx context.Context, q database.Queryer, targets []routing.PublishedTarget) (map[string]*resolved, error) {
	ids := make([]string, 0, len(targets))
	for _, t := range targets {
		ids = append(ids, t.ProviderModelID)
	}
	rows, err := q.Query(ctx, `SELECT m.id::text,p.id::text,p.name,m.upstream_model,p.state,
		coalesce(r.models,'[]'::jsonb),coalesce(r.configuration->>'auth_mode','none'),
		coalesce(r.slots,'[]'::jsonb),
		ARRAY(SELECT c.id::text FROM olp.provider_credentials c WHERE c.provider_id=p.id AND c.revoked_at IS NULL),
		coalesce(coalesce(r.configuration,p.configuration)->'options'->>'vendor_id',''),coalesce(r.configuration->>'kind',p.kind),
		coalesce(coalesce(r.configuration,p.configuration)->'options'->'models'->m.upstream_model->>'canonical_model','')
		FROM olp.provider_models m JOIN olp.providers p ON p.id=m.provider_id
		LEFT JOIN olp.provider_revisions r ON r.id=p.active_revision_id
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
		var vendorID, kind, canonical string
		r := &resolved{Certified: map[string]bool{}}
		if err = rows.Scan(&id, &r.ProviderID, &r.ProviderName, &r.ProviderModel, &r.ProviderState, &models, &r.AuthMode, &slots, &credentials, &vendorID, &kind, &canonical); err != nil {
			return nil, err
		}
		if vendorID == "" {
			vendorID = vendors.DefaultFor(kind)
		}
		r.Lifecycle = s.Catalog.Lifecycle(vendorID, r.ProviderModel, canonical)
		if err = json.Unmarshal(slots, &r.Slots); err != nil {
			return nil, err
		}
		for i := range r.Slots {
			slot := &r.Slots[i]
			if slot.CredentialID != nil && !slices.Contains(credentials, *slot.CredentialID) {
				slot.CredentialID = nil
			}
		}
		var published []routing.RevisionModel
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

func targetJSON(t routing.PublishedTarget, live *resolved) map[string]any {
	name, model, providerID := t.ProviderName, t.ProviderModel, t.ProviderID
	if live != nil {
		name, model, providerID = live.ProviderName, live.ProviderModel, live.ProviderID
	}
	var lifecycle *catalog.LifecycleView
	if live != nil {
		lifecycle = live.Lifecycle
	}
	return map[string]any{"id": t.ID, "provider_model_id": t.ProviderModelID, "provider_id": providerID, "provider_name": name, "provider_model": model, "available": live.available(), "priority": t.Priority, "weight": t.Weight, "timeout_ms": t.TimeoutMS, "position": t.Position, "lifecycle": lifecycle, "tags": orEmpty(t.Tags), "shadow": t.Shadow}
}

// behaviorJSON flattens a stored route behavior into a draft or revision
// representation, with empty collections rather than absent fields.
func behaviorJSON(out map[string]any, raw []byte) map[string]any {
	behavior, _ := routing.DecodeBehavior(raw)
	retry := behavior.Retry
	if retry == nil {
		retry = routing.Retry{}
	}
	out["fallbacks"], out["selectors"], out["retry"] = orEmpty(behavior.Fallbacks), orEmpty(behavior.Selectors), retry
	out["affinity"], out["budget"] = behavior.Affinity, behavior.Budget
	out["max_body_bytes"] = behavior.MaxBodyBytes
	out["caller_cost_exempt"] = behavior.CallerCostExempt
	out["provider_fetched_documents"] = behavior.ProviderFetchedDocuments
	out["cache"], out["prompt_cache"] = behavior.Cache, behavior.PromptCache
	out["passthrough"] = behavior.Passthrough
	out["tool_execution"], out["toolsets"] = behavior.ToolExecution, orEmpty(behavior.Toolsets)
	out["responses_mcp_items"] = behavior.ResponsesMCPItems
	out["run_limits"] = behavior.RunLimits
	certification := "certified"
	if behavior.Passthrough != nil {
		certification = "uncertified"
	}
	out["certification"] = certification
	return out
}

func orEmpty[T any](values []T) []T {
	if values == nil {
		return []T{}
	}
	return values
}

func targetsJSON(targets []routing.PublishedTarget, live map[string]*resolved) []map[string]any {
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
	return behaviorJSON(map[string]any{"id": d.ID, "slug": d.Slug, "state": d.State, "overall_timeout_ms": d.OverallTimeoutMS, "max_attempts": d.MaxAttempts, "etag": d.ETag, "operations": d.Operations, "targets": targetsJSON(d.Targets, live), "created_at": d.CreatedAt, "updated_at": d.UpdatedAt, "based_on_revision_id": d.BasedOnRevision, "created_by_email": d.CreatedByEmail, "project_id": d.ProjectID, "project_name": d.ProjectName, "fidelity": json.RawMessage(d.Fidelity)}, d.Behavior)
}

func (s *Server) draftDetail(ctx context.Context, q database.Queryer, d *draft) (httpkit.Reply, error) {
	live, err := s.resolve(ctx, q, d.Targets)
	if err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.Detail(d.detail(live), d.ETag), nil
}

func (s *Server) drafts(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	page, err := httpkit.Page(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	rows, err := s.Access.Pool.Query(r.Context(), "SELECT "+draftColumns+draftFrom+" WHERE d.id<$1 AND ($2 OR d.project_id=ANY($3::uuid[])) ORDER BY d.id DESC LIMIT $4", page.Before, p.AllProjects, p.ProjectIDs(), page.Limit+1)
	if err != nil {
		return httpkit.Reply{}, err
	}
	defer rows.Close()
	var drafts []*draft
	var all []routing.PublishedTarget
	for rows.Next() {
		d, err := scanDraft(rows)
		if err != nil {
			return httpkit.Reply{}, err
		}
		drafts = append(drafts, d)
		all = append(all, d.Targets...)
	}
	if err = rows.Err(); err != nil {
		return httpkit.Reply{}, err
	}
	rows.Close()
	live, err := s.resolve(r.Context(), s.Access.Pool, all)
	if err != nil {
		return httpkit.Reply{}, err
	}
	items := []map[string]any{}
	for _, d := range drafts {
		items = append(items, d.detail(live))
	}
	return httpkit.ListReply(items, page), nil
}

func (s *Server) draft(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "draft_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	d, err := loadDraft(r.Context(), s.Access.Pool, id, false)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if err := p.Project(d.ProjectID, authz.View); err != nil {
		return httpkit.Reply{}, err
	}
	return s.draftDetail(r.Context(), s.Access.Pool, d)
}

func (s *Server) createDraft(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	a := s.Access
	var input DraftInput
	if err := httpkit.DecodeUnique(r, &input, 1<<20); err != nil {
		return httpkit.Reply{}, err
	}
	return access.Mutate(a, r, input, func(tx pgx.Tx, p authz.Principal) (struct{}, error) {
		// The stored reply carries what was created, so the caller must reach
		// its project before a replay is served.
		return struct{}{}, a.RequireProject(r.Context(), tx, p, input.ProjectID)
	}, func(tx pgx.Tx, p authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		targets, err := ValidateAndNormalizeDraftInput(r.Context(), tx, &input, input.ProjectID, nil)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		id, etag := httpkit.NewID(), httpkit.NewID()
		if err = StageDraft(r.Context(), tx, NewDraft{ID: id, CreatedBy: p.UserID(), ProjectID: input.ProjectID}, etag, input.Content(targets, nil)); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		result := httpkit.Reply{Status: 201, Location: "/api/v1/route-drafts/" + id, ETag: etag, Body: map[string]any{"id": id, "slug": input.Slug, "state": "draft", "etag": etag, "project_id": input.ProjectID, "fidelity": input.Fidelity}}
		return result, access.Event{Action: "route_draft.create", Resource: "route_draft", ID: id}, nil
	})
}

func (s *Server) replaceDraft(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	a := s.Access
	id, err := httpkit.IDParam(r, "draft_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	var input DraftInput
	if err = httpkit.DecodeUnique(r, &input, 1<<20); err != nil {
		return httpkit.Reply{}, err
	}
	return access.Transact(a, r, access.Unlocked, func(tx pgx.Tx, p authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		current, err := loadDraft(r.Context(), tx, id, true)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if err := p.Project(current.ProjectID, authz.Change); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if err = httpkit.Match(r, current.ETag); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if input.ProjectID != nil && !sameProject(input.ProjectID, current.ProjectID) {
			return httpkit.Reply{}, access.Event{}, httpkit.Invalid("project_id", "The draft's project is set at creation and cannot change.")
		}
		targets, err := ValidateAndNormalizeDraftInput(r.Context(), tx, &input, current.ProjectID, current.Targets)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		etag := httpkit.NewID()
		if err = StageDraft(r.Context(), tx, ExistingDraft{ID: id}, etag, input.Content(targets, nil)); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		updated, err := loadDraft(r.Context(), tx, id, false)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		result, err := s.draftDetail(r.Context(), tx, updated)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		return result, access.Event{Action: "route_draft.update", Resource: "route_draft", ID: id}, nil
	})
}

func (s *Server) deleteDraft(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	a := s.Access
	id, err := httpkit.IDParam(r, "draft_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	return access.Transact(a, r, access.Unlocked, func(tx pgx.Tx, p authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		current, err := loadDraft(r.Context(), tx, id, true)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if err := p.Project(current.ProjectID, authz.Change); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if err = httpkit.Match(r, current.ETag); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if _, err = tx.Exec(r.Context(), "DELETE FROM olp.route_drafts WHERE id=$1", id); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		return httpkit.Reply{Status: 204}, access.Event{Action: "route_draft.delete", Resource: "route_draft", ID: id}, nil
	})
}
