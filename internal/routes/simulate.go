package routes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/operationregistry"
	"github.com/tyk-swe/olp/internal/operations/tokenization/estimate"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/usage"
)

// simulationInput is the draft request and the normalized input used by both
// simulation endpoints after their own decoding and authorization.
type simulationInput struct {
	Operation            string               `json:"operation"`
	Surface              string               `json:"surface"`
	Mode                 string               `json:"mode"`
	Seed                 string               `json:"seed"`
	Preferences          *runtime.Preferences `json:"preferences"`
	EstimatedInputTokens *int64               `json:"estimated_input_tokens"`
	MaxOutputTokens      *int64               `json:"max_output_tokens"`
	Request              json.RawMessage      `json:"request"`
	Dialect              string               `json:"dialect"`
	ClientContract       string               `json:"client_contract"`
	SemanticHeaders      map[string]string    `json:"semantic_headers"`
	QuerySettings        map[string]string    `json:"query_settings"`
	APIKeyID             *string              `json:"api_key_id"`
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

// simulatedDemand is the token demand a simulation weighs each target's context
// window against. The caller's own estimate is one number for every target.
// Without one, and with a request to read, each target is weighed by the count
// of its own model, the input estimate the gateway's planning uses for that
// target; it is recorded by target, so the decision can say how it was made. A
// reply bound the caller names replaces the one the request names, whatever
// supplies the input.
type simulatedDemand struct {
	fixed     *runtime.TokenDemand
	prompt    *estimate.Prompt
	output    *int64
	estimates map[string]estimate.Estimate
}

func newSimulatedDemand(parsed *openai.Request, input simulationInput, fixed *runtime.TokenDemand) *simulatedDemand {
	d := &simulatedDemand{fixed: fixed, output: input.MaxOutputTokens}
	if parsed != nil && input.EstimatedInputTokens == nil {
		d.prompt, d.estimates = estimate.Walk(parsed), map[string]estimate.Estimate{}
	}
	return d
}

// outputBound is the reply bound a target's own request is weighed by: the one
// the caller named for the simulation, or else the one the request carries.
func (d *simulatedDemand) outputBound(request *openai.Request) *int64 {
	if d.output != nil {
		return d.output
	}
	return runtime.EffectiveOutputLimit(request)
}

// sourceDemand is the planner's per-target demand, or nil when the caller's
// number stands for every target.
func (d *simulatedDemand) sourceDemand() func(runtime.Provider, runtime.Target) *runtime.TokenDemand {
	if d.prompt == nil {
		return nil
	}
	return func(_ runtime.Provider, target runtime.Target) *runtime.TokenDemand { return d.source(target) }
}

// source is the demand of the caller's request on a target's model.
func (d *simulatedDemand) source(target runtime.Target) *runtime.TokenDemand {
	if d.prompt == nil {
		return d.fixed
	}
	return d.measure(target, d.prompt)
}

// prepared is the demand once the target has its own request, as a strict
// contract prepares it: that request's input, counted for the target's model.
func (d *simulatedDemand) prepared(target runtime.Target, request *openai.Request) *runtime.TokenDemand {
	if d.prompt == nil {
		return d.fixed
	}
	prompt := estimate.Walk(request)
	prompt.Follow(d.prompt)
	return d.measure(target, prompt)
}

func (d *simulatedDemand) measure(target runtime.Target, prompt *estimate.Prompt) *runtime.TokenDemand {
	e := prompt.Estimate(estimate.ForModel(target.ProviderModel), nil)
	d.estimates[target.ID] = e
	demand := &runtime.TokenDemand{EstimatedInputTokens: e.Input, MaxOutputTokens: e.Output}
	if d.output != nil {
		demand.MaxOutputTokens = d.output
	}
	return demand
}

func validTuple(operation, surface, mode string) error {
	if operationregistry.Default.Supports(operation, surface, mode) {
		return nil
	}
	if !slices.Contains(supportedOperations, operation) && !registeredOperation(operation) {
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

func (s *Server) simulateDraft(r *http.Request, p access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "draft_id")
	if err != nil {
		return access.Reply{}, err
	}
	var input simulationInput
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
	if err := p.Project(d.ProjectID, access.View); err != nil {
		return access.Reply{}, err
	}
	if err := ValidateFidelityPolicy(d.Fidelity, d.ContentPolicy); err != nil {
		return access.Reply{}, err
	}
	live, err := s.resolve(r.Context(), s.Access.Pool, d.Targets)
	if err != nil {
		return access.Reply{}, err
	}
	routingID := d.ID
	if err = s.Access.Pool.QueryRow(r.Context(), "SELECT id::text FROM olp.routes WHERE slug=$1", d.Slug).Scan(&routingID); err != nil && !isNoRows(err) {
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
	fidelity, err := runtime.DecodeFidelity(d.Fidelity)
	if err != nil {
		return access.Reply{}, err
	}
	route := simulationRoute(routingID, d.Slug, fidelity, d.Operations, d.OverallTimeoutMS, d.MaxAttempts, d.Targets)
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
	eligibility, err := routeCredentialEligibility(r.Context(), tx, snapshot, route)
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
	decisions, err := s.inspectSimulation(snapshot, d.Slug, input, key, inputs, demand, eligibility)
	if err != nil {
		return access.Reply{}, err
	}
	targets := []map[string]any{}
	for _, decision := range decisions {
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
	Preferences          *runtime.Preferences       `json:"preferences"`
	APIKeyID             *string                    `json:"api_key_id"`
	ClientContract       string                     `json:"client_contract"`
	Seed                 string                     `json:"seed"`
	EstimatedInputTokens *int64                     `json:"estimated_input_tokens"`
	MaxOutputTokens      *int64                     `json:"max_output_tokens"`
	Dialect              string                     `json:"dialect"`
	SemanticHeaders      map[string]string          `json:"semantic_headers"`
	QuerySettings        map[string]string          `json:"query_settings"`
}

// simulateRouting answers the console's routing simulator against the routes
// as currently published.
func (s *Server) simulateRouting(r *http.Request, p access.Principal) (access.Reply, error) {
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
		if err := p.Project(route.ProjectID, access.View); err != nil {
			return access.Reply{}, err
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
	eligibility, err := routeCredentialEligibility(r.Context(), tx, snapshot, snapshot.Routes[slug])
	if err != nil {
		return access.Reply{}, err
	}
	demand, err := tokenDemand(input.EstimatedInputTokens, input.MaxOutputTokens)
	if err != nil {
		return access.Reply{}, err
	}
	inspection := simulationInput{
		Operation: operation, Surface: input.Surface, Mode: input.Mode, Seed: input.Seed,
		Preferences: input.Preferences, Request: input.Operation["request"],
		EstimatedInputTokens: input.EstimatedInputTokens, MaxOutputTokens: input.MaxOutputTokens,
		Dialect: input.Dialect, ClientContract: input.ClientContract,
		SemanticHeaders: input.SemanticHeaders, QuerySettings: input.QuerySettings,
	}
	decisions, err := s.inspectSimulation(snapshot, slug, inspection, key, inputs, demand, eligibility)
	if err != nil {
		return access.Reply{}, err
	}
	return access.OK(decisions), nil
}

// inspectSimulation shares semantic inspection and routing explanations across
// draft and published routes. Callers own loading, validation and authorization;
// inspection never reserves state or dispatches an upstream request.
func (s *Server) inspectSimulation(snapshot *runtime.Snapshot, slug string, input simulationInput, key inspectionKeyContext, inputs *usage.RoutingInputs, demand *runtime.TokenDemand, eligibility func(string) runtime.Eligibility) ([]inspectedDecision, error) {
	route := snapshot.Routes[slug]
	context, err := inspectionContext(input.SemanticHeaders, input.QuerySettings, key.allowProviderState)
	if err != nil {
		return nil, err
	}
	parsed, unary, mediaRequest, err := inspectorAnyRequest(input.Request, input.Operation, input.Surface, input.Mode, input.Dialect, slug, route.Fidelity.Strict())
	if err != nil {
		return nil, err
	}
	if input.Operation == "generation" {
		if err = inspectionClientContract(input.ClientContract, &context, s.Access.Keys != nil); err != nil {
			return nil, err
		}
	}
	counted := newSimulatedDemand(parsed, input, demand)
	accept, effective, inspections := inspectionAccept(route, parsed, context, counted)
	if unary != nil {
		accept, effective, inspections = inspectionUnaryAccept(route, *unary, context, input.ClientContract, demand)
	}
	if mediaRequest != nil {
		accept, effective, inspections = inspectionMediaAccept(route, mediaRequest, input.Dialect, context, input.ClientContract, demand)
	}
	options := runtime.SelectionOptions{
		KeyID: key.id, Preferences: input.Preferences, Inputs: inputs, TokenDemand: counted.fixed, Demand: counted.sourceDemand(),
		CheckSlots: true, CredentialEligibility: eligibility, UnconfinedPlugins: s.UnconfinedPlugins,
		Accept: accept, Effective: effective,
	}
	if key.reason != "" {
		options.Accept = nil
		options.Effective = nil
	}
	if parsed != nil {
		options.Parameters = sync.OnceValue(func() []string { return protocols.ParameterNames(parsed) })
	}

	plan, err := runtime.PlanRequest(snapshot, slug, input.Operation, input.Surface, input.Mode, []byte(input.Seed), options)
	if err != nil {
		return nil, err
	}
	applyInspectionKeyReason(plan.Decisions, key.reason)
	return inspectedDecisions(plan.Decisions, route, parsed != nil || unary != nil || mediaRequest != nil, inspections, counted.estimates), nil
}

// Register mounts the route surface.
func (s *Server) Register(mux *http.ServeMux) {
	s.registerCodeMode(mux)
	s.Access.Route(mux, "GET /api/v1/route-drafts", s.drafts)
	s.Access.Route(mux, "POST /api/v1/route-drafts", s.createDraft)
	s.Access.Route(mux, "GET /api/v1/route-drafts/{draft_id}", s.draft)
	s.Access.Route(mux, "PUT /api/v1/route-drafts/{draft_id}", s.replaceDraft)
	s.Access.Route(mux, "DELETE /api/v1/route-drafts/{draft_id}", s.deleteDraft)
	s.Access.Route(mux, "POST /api/v1/route-drafts/{draft_id}/validate", s.validateDraft)
	s.Access.Route(mux, "POST /api/v1/route-drafts/{draft_id}/activate", s.activateDraft)
	s.Access.Route(mux, "POST /api/v1/route-drafts/{draft_id}/simulate", s.simulateDraft, access.MaxBody(1<<20))
	s.Access.Route(mux, "GET /api/v1/routes", s.routes)
	s.Access.Route(mux, "GET /api/v1/routes/{route_id}", s.route)
	s.Access.Route(mux, "POST /api/v1/routes/{route_id}/retire", s.retireRoute)
	s.Access.Route(mux, "GET /api/v1/routes/{route_id}/revisions", s.revisions)
	s.Access.Route(mux, "GET /api/v1/routes/{route_id}/revisions/diff", s.revisionDiff)
	s.Access.Route(mux, "GET /api/v1/routes/{route_id}/revisions/{revision_id}", s.revision)
	s.Access.Route(mux, "POST /api/v1/routes/{route_id}/revisions/{revision_id}/restore-as-draft", s.restoreRevision)
	s.Access.Route(mux, "GET /api/v1/routing-policies/{scope}/{id}", s.policy)
	s.Access.Route(mux, "PUT /api/v1/routing-policies/{scope}/{id}", s.putPolicy)
	s.Access.Route(mux, "POST /api/v1/routing/simulate", s.simulateRouting, access.MaxBody(1<<20))
}

func simulationRoute(id, slug string, fidelity runtime.RouteFidelity, operations []string, timeout, budget int, targets []runtime.PublishedTarget) runtime.Route {
	r := runtime.Route{ID: id, RoutingID: id, Slug: slug, Fidelity: fidelity, Operations: operations, OverallTimeout: int64(timeout), MaxAttempts: budget}
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

// Credential revocation and grant lapse are authoritative without publishing a
// new route or provider revision. Apply them to previews just as the executor
// does at dispatch.
func routeCredentialEligibility(ctx context.Context, q access.Queryer, snapshot *runtime.Snapshot, route runtime.Route) (func(string) runtime.Eligibility, error) {
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
	ineligible, err := runtime.ReadIneligible(ctx, q, ids)
	if err != nil {
		return nil, err
	}
	return func(id string) runtime.Eligibility { return ineligible[id] }, nil
}
