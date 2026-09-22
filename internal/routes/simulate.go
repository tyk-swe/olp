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
	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/usage"
)

type simulateDraftRequest struct {
	Operation            string            `json:"operation"`
	Surface              string            `json:"surface"`
	Mode                 string            `json:"mode"`
	Seed                 string            `json:"seed"`
	Preferences          *Preferences      `json:"preferences"`
	EstimatedInputTokens *int64            `json:"estimated_input_tokens"`
	MaxOutputTokens      *int64            `json:"max_output_tokens"`
	Request              json.RawMessage   `json:"request"`
	Dialect              string            `json:"dialect"`
	ClientContract       string            `json:"client_contract"`
	SemanticHeaders      map[string]string `json:"semantic_headers"`
	QuerySettings        map[string]string `json:"query_settings"`
	APIKeyID             *string           `json:"api_key_id"`
}

func tokenDemand(estimated, output *int64) (*runtime.TokenDemand, error) {
	if estimated != nil && *estimated < 0 {
		return nil, access.Invalid("estimated_input_tokens", "Use a non-negative token estimate.")
	}
	if output != nil && *output < 0 {
		return nil, access.Invalid("max_output_tokens", "Use a non-negative output bound.")
	}
	if estimated == nil && output == nil {
		return nil, nil
	}
	demand := &runtime.TokenDemand{MaxOutputTokens: output}
	if estimated != nil {
		demand.EstimatedInputTokens = *estimated
	}
	return demand, nil
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
	p, err := s.Access.Principal(r, s.Access.Pool, "read")
	if err != nil {
		return access.Reply{}, err
	}
	id, err := access.IDParam(r, "draft_id")
	if err != nil {
		return access.Reply{}, err
	}
	var input simulateDraftRequest
	if err = access.DecodeUnique(r, &input, 1<<20); err != nil {
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
	if !p.CanProject(d.ProjectID, false) {
		return access.Reply{}, pgx.ErrNoRows
	}
	if err := ValidateFidelityPolicy(d.Fidelity, d.ContentPolicy); err != nil {
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
	route.Fidelity, err = runtime.DecodeFidelity(d.Fidelity)
	if err != nil {
		return access.Reply{}, err
	}
	route.ProjectID = d.ProjectID
	if len(d.ContentPolicy) > 0 && string(d.ContentPolicy) != "null" {
		route.ContentPolicy, err = contentpolicy.Decode(d.ContentPolicy)
		if err != nil {
			return access.Reply{}, err
		}
	}
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
	demand, err := tokenDemand(input.EstimatedInputTokens, input.MaxOutputTokens)
	if err != nil {
		return access.Reply{}, err
	}
	key, err := s.inspectionKey(r, tx, p, input.APIKeyID, route)
	if err != nil {
		return access.Reply{}, err
	}
	context, err := inspectionContext(input.SemanticHeaders, input.QuerySettings, key.allowProviderState)
	if err != nil {
		return access.Reply{}, err
	}
	if err = inspectionClientContract(input.ClientContract, &context, s.Access.Keys != nil); err != nil {
		return access.Reply{}, err
	}

	parsed, err := inspectorRequest(input.Request, input.Operation, input.Surface, input.Mode, input.Dialect, d.Slug, runtime.FidelityMode(route.Fidelity) == runtime.FidelityStrict)
	if err != nil {
		return access.Reply{}, err
	}
	accept, effective, inspections := inspectionAccept(route, parsed, context, demand)
	options := runtime.SelectionOptions{KeyID: key.id, Preferences: input.Preferences, Inputs: inputs, TokenDemand: demand, CheckSlots: true, CredentialRevoked: revoked, Accept: accept, Effective: effective}
	if key.reason != "" {
		options.Accept = nil
		options.Effective = nil
	}
	if parsed != nil {
		options.Parameters = protocols.ParameterNames(parsed)
	}
	plan, err := runtime.PlanRequest(snapshot, d.Slug, input.Operation, input.Surface, input.Mode, []byte(input.Seed), options)
	if err != nil {
		return access.Reply{}, err
	}
	targets := []map[string]any{}
	applyInspectionKeyReason(plan.Decisions, key.reason)
	for _, decision := range inspectedDecisions(plan.Decisions, route, parsed, inspections) {
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
	Operation            map[string]json.RawMessage `json:"operation"`
	Surface              string                     `json:"surface"`
	Mode                 string                     `json:"mode"`
	Preferences          *Preferences               `json:"preferences"`
	APIKeyID             *string                    `json:"api_key_id"`
	Seed                 string                     `json:"seed"`
	EstimatedInputTokens *int64                     `json:"estimated_input_tokens"`
	MaxOutputTokens      *int64                     `json:"max_output_tokens"`
	Dialect              string                     `json:"dialect"`
	ClientContract       string                     `json:"client_contract"`
	SemanticHeaders      map[string]string          `json:"semantic_headers"`
	QuerySettings        map[string]string          `json:"query_settings"`
}

// simulateRouting answers the console's routing simulator against the routes
// as currently published.
func (s *Server) simulateRouting(r *http.Request) (access.Reply, error) {
	p, err := s.Access.Principal(r, s.Access.Pool, "read")
	if err != nil {
		return access.Reply{}, err
	}
	var input simulationRequest
	if err := access.DecodeUnique(r, &input, 1<<20); err != nil {
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
	var slug string
	if raw, present := input.Operation["route"]; present {
		if json.Unmarshal(raw, &slug) != nil {
			return access.Reply{}, access.Invalid("operation.route", "Name the route to simulate.")
		}
	}
	if slug == "" {
		slug = request.Route
	}
	if slug == "" {
		slug = request.Model
	}
	if !access.RouteSlug.MatchString(slug) {
		return access.Reply{}, access.Invalid("operation.request.route", "Name the route to simulate.")
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
	route := snapshot.Routes[slug]
	if _, ok := snapshot.Routes[slug]; ok {
		if !p.CanProject(route.ProjectID, false) {
			return access.Reply{}, pgx.ErrNoRows
		}
	}
	key, err := s.inspectionKey(r, tx, p, input.APIKeyID, route)
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
	options := runtime.SelectionOptions{KeyID: key.id, Preferences: input.Preferences, Inputs: inputs, CheckSlots: true, CredentialRevoked: revoked}
	options.TokenDemand, err = tokenDemand(input.EstimatedInputTokens, input.MaxOutputTokens)
	if err != nil {
		return access.Reply{}, err
	}
	context, err := inspectionContext(input.SemanticHeaders, input.QuerySettings, key.allowProviderState)
	if err != nil {
		return access.Reply{}, err
	}
	if err = inspectionClientContract(input.ClientContract, &context, s.Access.Keys != nil); err != nil {
		return access.Reply{}, err
	}

	parsed, err := inspectorRequest(input.Operation["request"], operation, input.Surface, input.Mode, input.Dialect, slug, runtime.FidelityMode(route.Fidelity) == runtime.FidelityStrict)
	if err != nil {
		return access.Reply{}, err
	}
	if parsed != nil {
		options.Parameters = protocols.ParameterNames(parsed)
	}
	accept, effective, inspections := inspectionAccept(route, parsed, context, options.TokenDemand)
	options.Accept = accept
	options.Effective = effective
	if key.reason != "" {
		options.Accept = nil
		options.Effective = nil
	}

	plan, err := runtime.PlanRequest(snapshot, slug, operation, input.Surface, input.Mode, []byte(input.Seed), options)
	if err != nil {
		return access.Reply{}, err
	}
	applyInspectionKeyReason(plan.Decisions, key.reason)
	return access.OK(inspectedDecisions(plan.Decisions, route, parsed, inspections)), nil
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
	mux.HandleFunc("POST /api/v3/route-drafts/{draft_id}/simulate", s.Access.HandleWith(1<<20, s.simulateDraft))
	mux.HandleFunc("GET /api/v3/routes", h(s.routes))
	mux.HandleFunc("GET /api/v3/routes/{route_id}", h(s.route))
	mux.HandleFunc("POST /api/v3/routes/{route_id}/retire", h(s.retireRoute))
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
		provider := snapshot.Providers[target.ProviderID]
		if provider.Network != nil && provider.Network.CredentialID != "" {
			ids = append(ids, provider.Network.CredentialID)
		}
		for _, slot := range provider.Slots {
			if slot.CredentialID != nil {
				ids = append(ids, *slot.CredentialID)
			}
		}
	}
	revoked := map[string]bool{}
	if len(ids) > 0 {
		rows, err := q.Query(ctx, "SELECT id::text FROM olp_go.provider_credentials WHERE id=ANY($1::uuid[]) AND revoked_at IS NOT NULL UNION SELECT id::text FROM olp_go.provider_network_credentials WHERE id=ANY($1::uuid[]) AND revoked_at IS NOT NULL", ids)
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
