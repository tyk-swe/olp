package routes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/usage"
)

type simulateDraftRequest struct {
	Operation   string       `json:"operation"`
	Surface     string       `json:"surface"`
	Mode        string       `json:"mode"`
	Seed        string       `json:"seed"`
	Preferences *Preferences `json:"preferences"`
}

func validTuple(operation, surface, mode string) error {
	if !slices.Contains(supportedOperations, operation) {
		return access.Fail(422, "operation_unavailable", "The "+operation+" operation is not available in this release.")
	}
	if !slices.Contains([]string{"openai", "anthropic", "gemini"}, surface) {
		return access.Fail(422, "surface_unavailable", "Use the openai, anthropic, or gemini surface.")
	}
	if !connectors.Supports("openai", "openai", operation, surface, mode) {
		return access.Fail(422, "mode_unavailable", "This operation does not support the requested surface and mode.")
	}
	return nil
}

func (s *Server) simulateDraft(r *http.Request) (access.Reply, error) {
	if _, err := s.Access.Principal(r, s.Access.Pool, "read"); err != nil {
		return access.Reply{}, err
	}
	id, err := access.IDParam(r, "draft_id")
	if err != nil {
		return access.Reply{}, err
	}
	var input simulateDraftRequest
	if err = access.Decode(r, &input); err != nil {
		return access.Reply{}, err
	}
	if err = validTuple(input.Operation, input.Surface, input.Mode); err != nil {
		return access.Reply{}, err
	}
	if err = input.Preferences.Validate("preferences"); err != nil {
		return access.Reply{}, err
	}
	if len(input.Seed) > 256 {
		return access.Reply{}, access.Invalid("seed", "Use at most 256 characters.")
	}
	d, err := loadDraft(r.Context(), s.Access.Pool, id, false)
	if err != nil {
		return access.Reply{}, err
	}
	live, err := resolve(r.Context(), s.Access.Pool, d.Targets)
	if err != nil {
		return access.Reply{}, err
	}
	routingID := d.ID
	if err = s.Access.Pool.QueryRow(r.Context(), "SELECT id::text FROM olp_go.routes WHERE slug=$1", d.Slug).Scan(&routingID); err != nil && !isNoRows(err) {
		return access.Reply{}, err
	}
	tx, err := s.Access.Pool.Begin(r.Context())
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	snapshot, err := runtime.Compile(r.Context(), tx)
	if err != nil {
		return access.Reply{}, err
	}
	route := simulationRoute(routingID, d.Slug, d.Operations, d.OverallTimeoutMS, d.MaxAttempts, d.Targets)
	route.Policy, _, err = loadPolicy(r.Context(), tx, "route-draft", d.ID, false)
	if err != nil {
		return access.Reply{}, err
	}
	snapshot.Routes[d.Slug] = route
	inputs, err := s.routingInputs(r, tx)
	if err != nil {
		return access.Reply{}, err
	}
	revoked, err := routeRevocations(r.Context(), tx, snapshot, route)
	if err != nil {
		return access.Reply{}, err
	}
	plan, err := runtime.PlanRequest(snapshot, d.Slug, input.Operation, input.Surface, input.Mode, []byte(input.Seed), runtime.SelectionOptions{Preferences: input.Preferences, Inputs: inputs, CheckSlots: true, CredentialRevoked: revoked})
	if err != nil {
		return access.Reply{}, err
	}
	targets := []map[string]any{}
	for _, decision := range plan.Decisions {
		var name string
		for _, t := range d.Targets {
			if t.ID == decision.TargetID {
				name = t.ProviderName
				if l := live[t.ProviderModelID]; l != nil {
					name = l.ProviderName
				}
				break
			}
		}
		targets = append(targets, map[string]any{"target_id": decision.TargetID, "provider_id": decision.ProviderID, "provider_name": name, "provider_model": decision.UpstreamModel, "priority": decision.Priority, "eligible": decision.Eligible, "attempt": decision.Attempt, "reason": decision.Reason, "decision": decision})
	}
	return access.OK(map[string]any{"deterministic_seed": input.Seed, "operation": input.Operation, "surface": input.Surface, "mode": input.Mode, "targets": targets}), nil
}

func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

type simulationRequest struct {
	Operation   map[string]json.RawMessage `json:"operation"`
	Surface     string                     `json:"surface"`
	Mode        string                     `json:"mode"`
	Preferences *Preferences               `json:"preferences"`
	APIKeyID    *string                    `json:"api_key_id"`
	Seed        string                     `json:"seed"`
}

// simulateRouting answers the console's routing simulator against the routes
// as currently published.
func (s *Server) simulateRouting(r *http.Request) (access.Reply, error) {
	if _, err := s.Access.Principal(r, s.Access.Pool, "read"); err != nil {
		return access.Reply{}, err
	}
	var input simulationRequest
	if err := access.Decode(r, &input); err != nil {
		return access.Reply{}, err
	}
	operation := "generation"
	if raw, ok := input.Operation["operation"]; ok {
		if err := json.Unmarshal(raw, &operation); err != nil {
			return access.Reply{}, access.Invalid("operation.operation", "Use an operation name.")
		}
	}
	if err := validTuple(operation, input.Surface, input.Mode); err != nil {
		return access.Reply{}, err
	}
	if err := input.Preferences.Validate("preferences"); err != nil {
		return access.Reply{}, err
	}
	if len(input.Seed) > 256 {
		return access.Reply{}, access.Invalid("seed", "Use at most 256 characters.")
	}
	var request struct {
		Route string `json:"route"`
		Model string `json:"model"`
	}
	if raw, ok := input.Operation["request"]; ok {
		if err := json.Unmarshal(raw, &request); err != nil {
			return access.Reply{}, access.Invalid("operation.request", "Use a request object.")
		}
	}
	slug := request.Route
	if slug == "" {
		slug = request.Model
	}
	if !access.RouteSlug.MatchString(slug) {
		return access.Reply{}, access.Invalid("operation.request.route", "Name the route to simulate.")
	}
	var keyReason string
	var keyID string
	if input.APIKeyID != nil {
		var err error
		keyID, err = access.ParseUUID(*input.APIKeyID)
		if err != nil {
			return access.Reply{}, access.Invalid("api_key_id", "Use an API key identifier.")
		}
		var authority access.Authority
		var raw []byte
		if err = s.Access.Pool.QueryRow(r.Context(), "SELECT policy,expires_at,revoked_at FROM olp_go.api_keys WHERE id=$1", keyID).Scan(&raw, &authority.ExpiresAt, &authority.RevokedAt); err != nil {
			return access.Reply{}, err
		}
		if err = json.Unmarshal(raw, &authority.Policy); err != nil {
			return access.Reply{}, err
		}
		if !authority.Allows("inference", slug, time.Now()) {
			keyReason = "api_key_not_authorized"
			if len(authority.Policy.AllowedRoutes) > 0 && !slices.Contains(authority.Policy.AllowedRoutes, slug) {
				keyReason = "route_not_allowed_for_key"
			}
		}
	}
	tx, err := s.Access.Pool.Begin(r.Context())
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	snapshot, err := runtime.Compile(r.Context(), tx)
	if err != nil {
		return access.Reply{}, err
	}
	inputs, err := s.routingInputs(r, tx)
	if err != nil {
		return access.Reply{}, err
	}
	revoked, err := routeRevocations(r.Context(), tx, snapshot, snapshot.Routes[slug])
	if err != nil {
		return access.Reply{}, err
	}
	options := runtime.SelectionOptions{KeyID: keyID, Preferences: input.Preferences, Inputs: inputs, CheckSlots: true, CredentialRevoked: revoked}
	parsed, err := protocols.SimulationRequest(input.Operation["request"], operation, input.Surface, input.Mode, slug)
	if err != nil {
		return access.Reply{}, access.Invalid("operation.request", err.Error())
	}
	options.Parameters = protocols.ParameterNames(parsed)
	options.Accept = func(p runtime.Provider, t runtime.Target) error {
		_, _, err := protocols.Encode(parsed, p.Kind, p.VendorID, p.Connector().Model(t.ProviderModel), p.ParameterDefaults)
		return err
	}

	plan, err := runtime.PlanRequest(snapshot, slug, operation, input.Surface, input.Mode, []byte(input.Seed), options)
	if err != nil {
		return access.Reply{}, err
	}
	decisions := plan.Decisions
	if keyReason != "" {
		for i := range decisions {
			d := &decisions[i]
			d.Eligible = false
			d.Attempt = nil
			d.Reason = &keyReason
		}
	}
	return access.OK(decisions), nil
}

// Register mounts the route surface.
func (s *Server) Register(mux *http.ServeMux) {
	h := s.Access.Handle
	mux.HandleFunc("GET /api/v3/route-drafts", h(s.drafts))
	mux.HandleFunc("POST /api/v3/route-drafts", h(s.createDraft))
	mux.HandleFunc("GET /api/v3/route-drafts/{draft_id}", h(s.draft))
	mux.HandleFunc("PUT /api/v3/route-drafts/{draft_id}", h(s.replaceDraft))
	mux.HandleFunc("DELETE /api/v3/route-drafts/{draft_id}", h(s.deleteDraft))
	mux.HandleFunc("POST /api/v3/route-drafts/{draft_id}/validate", h(s.validateDraft))
	mux.HandleFunc("POST /api/v3/route-drafts/{draft_id}/activate", h(s.activateDraft))
	mux.HandleFunc("POST /api/v3/route-drafts/{draft_id}/simulate", h(s.simulateDraft))
	mux.HandleFunc("GET /api/v3/routes", h(s.routes))
	mux.HandleFunc("GET /api/v3/routes/{route_id}", h(s.route))
	mux.HandleFunc("GET /api/v3/routes/{route_id}/revisions", h(s.revisions))
	mux.HandleFunc("GET /api/v3/routes/{route_id}/revisions/diff", h(s.revisionDiff))
	mux.HandleFunc("GET /api/v3/routes/{route_id}/revisions/{revision_id}", h(s.revision))
	mux.HandleFunc("POST /api/v3/routes/{route_id}/revisions/{revision_id}/restore-as-draft", h(s.restoreRevision))
	mux.HandleFunc("GET /api/v3/routing-policies/{scope}/{id}", h(s.policy))
	mux.HandleFunc("PUT /api/v3/routing-policies/{scope}/{id}", h(s.putPolicy))
	mux.HandleFunc("POST /api/v3/routing/simulate", s.Access.HandleWith(1<<20, s.simulateRouting))
}

func simulationRoute(id, slug string, operations []string, timeout, budget int, targets []runtime.PublishedTarget) runtime.Route {
	r := runtime.Route{ID: id, RoutingID: id, Slug: slug, Operations: operations, OverallTimeout: int64(timeout), MaxAttempts: budget}
	for _, t := range targets {
		r.Targets = append(r.Targets, runtime.Target{ID: t.ID, ProviderID: t.ProviderID, ProviderModel: t.ProviderModel, Priority: t.Priority, Weight: t.Weight, Timeout: t.TimeoutMS, RoutingID: t.ProviderModelID})
	}
	return r
}
func (s *Server) routingInputs(r *http.Request, q access.Queryer) (*usage.RoutingInputs, error) {
	if s.Inputs != nil {
		return s.Inputs(), nil
	}
	return usage.LoadRoutingInputs(r.Context(), q, time.Now())
}

// A credential revocation is authoritative without publishing a new route or
// provider revision. Apply it to previews just as the executor does at dispatch.
func routeRevocations(ctx context.Context, q access.Queryer, snapshot *runtime.Snapshot, route runtime.Route) (func(string) bool, error) {
	ids := []string{}
	for _, target := range route.Targets {
		for _, slot := range snapshot.Providers[target.ProviderID].Slots {
			if slot.CredentialID != nil {
				ids = append(ids, *slot.CredentialID)
			}
		}
	}
	revoked := map[string]bool{}
	if len(ids) > 0 {
		rows, err := q.Query(ctx, "SELECT id::text FROM olp_go.provider_credentials WHERE id=ANY($1::uuid[]) AND revoked_at IS NOT NULL", ids)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				return nil, err
			}
			revoked[id] = true
		}
		if err = rows.Err(); err != nil {
			return nil, err
		}
	}
	return func(id string) bool { return revoked[id] }, nil
}
