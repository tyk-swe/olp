package kernel

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/domain/priority"
	"github.com/tyk-swe/olp/internal/domain/protocols"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/anthropicbeta"
	"github.com/tyk-swe/olp/internal/gateway/codeforward"
	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/ingress"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/keyauthority"
	"github.com/tyk-swe/olp/internal/platform/concurrency"
	"github.com/tyk-swe/olp/internal/routing"
)

// routingHeader carries per-request routing preferences and the admission
// priority. Preferences can only lower the published attempt budget, and the
// priority can only stay within the key's ceiling.
const routingHeader = "X-OLP-Routing"

// routingControls reads the routing header and resolves the request's
// admission class: the key's own priority unless the header asks for another
// within the key's ceiling. Asking above it is refused like any other budget
// increase.
func routingControls(r *http.Request, policy keyauthority.KeyPolicy) (*routing.Preferences, string, *fault.Error) {
	preferences, e := routingPreferences(r)
	if e != nil {
		return nil, "", e
	}
	if preferences == nil || preferences.Priority == nil {
		return preferences, policy.DefaultPriority(), nil
	}
	requestedPriority, ceiling := *preferences.Priority, policy.PriorityCeiling()
	if priority.Rank(requestedPriority) < priority.Rank(ceiling) {
		param := routingHeader
		return nil, "", fault.InvalidRequest("priority_increase_forbidden", "This API key may request priority "+ceiling+" at most.", &param)
	}
	return preferences, requestedPriority, nil
}

func routingPreferences(r *http.Request) (*routing.Preferences, *fault.Error) {
	values := r.Header.Values("X-OLP-Routing")
	if len(values) == 0 {
		return nil, nil
	}
	param := "X-OLP-Routing"
	if len(values) != 1 {
		return nil, fault.InvalidRequest("invalid_request", "Provide exactly one X-OLP-Routing header.", &param)
	}
	p, err := routing.ParsePreferences([]byte(values[0]))
	if err != nil {
		return nil, fault.InvalidRequest("invalid_request", err.Error(), &param)
	}
	return p, nil
}

func RequestError(err error) *fault.Error {
	var guard guardrails.Failure
	if errors.As(err, &guard) {
		return guardrailError(guard)
	}
	if gatewayError, ok := errors.AsType[*fault.Error](err); ok {
		return gatewayError
	}
	var contract interface {
		Incompatibility() (code, field, requirement, message string)
	}
	if errors.As(err, &contract) {
		code, field, requirement, message := contract.Incompatibility()
		var param *string
		if field != "" {
			param = &field
		}
		if code == "target_capability" && requirement == "source_control_mapping" {
			// Preserve the established ingress error for an unmapped control;
			// the planner and inspector retain its precise requirement/category.
			return fault.InvalidRequest("unsupported_parameter", message, param)
		}
		return fault.InvalidRequest(code, message, param)
	}
	if re, ok := errors.AsType[*wire.RequestError](err); ok {
		var param *string
		if re.Param != "" {
			param = &re.Param
		}
		return fault.InvalidRequest(re.Code, re.Message, param)
	}
	return fault.InvalidRequest("invalid_request", err.Error(), nil)
}

// noteCostExceeded remembers that a leg this request planned excluded a
// non-shadow target for its maximum cost; when nothing serves, the refusal is
// 400 cost_limit_exceeded rather than route-unavailable.
func (x *Call) noteCostExceeded(plan routing.Plan) {
	for index := range plan.Decisions {
		if plan.Decisions[index].Reason != nil && *plan.Decisions[index].Reason == "cost_limit_exceeded" && !plan.Decisions[index].Shadow {
			x.costExcluded = true
			return
		}
	}
}

// costLimitExceeded is the refusal a request ends on when a leg it planned
// excluded a non-shadow target for its maximum cost.
func costLimitExceeded() *fault.Error {
	return fault.InvalidRequest("cost_limit_exceeded", "Every candidate exceeds the request's maximum cost.", func() *string { p := "max_cost"; return &p }())
}

func selectionError(err error, model string) *fault.Error {
	if se, ok := errors.AsType[*routing.SelectionError](err); ok {
		switch se.Code {
		case routing.RouteNotFound:
			return fault.ModelNotFound(model)
		case routing.OperationNotSupported:
			return fault.InvalidRequest("invalid_request", "The model `"+model+"` does not allow this operation.", nil)
		}
	}
	return fault.ServerError(http.StatusServiceUnavailable, "upstream_unavailable", "No provider is currently eligible to serve `"+model+"`.")
}

// prepare resolves the route, lets authorize refuse the caller's use of it in
// the caller's own terms, and ranks the eligible attempts against the pinned
// snapshot.
func (s *Server) prepare(ctx context.Context, x *Call, authorize func(*routing.Route) *fault.Error) *fault.Error {
	x.mode = "unary"
	if x.generation.parsed.Stream {
		x.mode = "streaming"
	}
	route, ok := x.Snapshot().FindRoute(x.generation.parsed.Route)
	if !ok {
		return fault.ModelNotFound(x.generation.parsed.Route)
	}
	x.Route = &route
	if e := authorize(&route); e != nil {
		return e
	}
	if route.ToolExecution != "gateway" && len(x.generation.approval) > 0 {
		return fault.PermissionError("tool_approval_invalid", "The workflow approval is invalid or expired.")
	}
	if route.ToolExecution == "gateway" && x.Strict() {
		return fault.InvalidRequest("tool_execution_strict_route", "Gateway tool execution requires a transformed route.", nil)
	}
	if route.ToolExecution == "gateway" && !x.generation.toolsetsSelected {
		x.mirrors = false
		bounds, callerBounds, bounded, boundErr := workflowBounds(route.RunLimits, x.authority.Policy.RunLimits, x.generation.parsed)
		if boundErr != nil {
			return boundErr
		}
		x.generation.runLimits = bounds
		x.generation.callerRunLimits = callerBounds
		x.generation.parsed = bounded
		selected, parsed, e := selectToolsets(route.Toolsets, x.generation.parsed)
		if e != nil {
			return e
		}
		x.generation.toolsets = selected
		x.generation.parsed = parsed
		x.generation.toolsetsSelected = true
	}
	if route.ToolExecution == "gateway" && (s.Deps.WorkflowRuns == nil || s.Deps.WorkflowTools == nil || s.Deps.Debits == nil) {
		return fault.ServerError(http.StatusServiceUnavailable, "tool_execution_unavailable", "Gateway tool execution requires durable accounting and tool services.")
	}
	if x.pin != nil {
		if e := s.pinAttempts(ctx, x); e != nil {
			return e
		}
	}
	// A pinned resource, a continuation and provider state belong to the
	// route that holds them, so those requests never leave it.
	x.bind(canonicalPlanner, authorize, x.pin != nil || x.generation.continuation != nil || x.providerState)
	return s.planNamed(ctx, x)
}

// planCanonical plans x.route for a canonical request. A selector that
// delegates moves the request on to its route, which is planned in turn.
func (s *Server) planCanonical(ctx context.Context, x *Call) *fault.Error {
	if x.named().ToolExecution == "gateway" && x.Route.Fidelity.Strict() {
		return fault.InvalidRequest("tool_execution_strict_route", "Gateway tool execution requires a transformed route.", nil)
	}
	if e := s.bindGuardrails(ctx, x); e != nil {
		return e
	}
	if x.prompt != nil && x.Route.Fidelity.Strict() {
		param := "olp_prompt"
		return fault.InvalidRequest("invalid_prompt", "Published prompts require a transformed route.", &param)
	}
	if e := s.CheckRouteFidelity(ctx, x.Route); e != nil {
		return e
	}
	if x.Strict() {
		if x.actor == "playground" {
			param := "client_contract"
			return fault.InvalidRequest("state_carrier", "This playground projection does not yet preserve the strict native interaction; use the admitted native API client.", &param)
		}
		if x.semanticQueryInvalid {
			param := "query"
			return fault.InvalidRequest("invalid_request", "The query must be unambiguous URL-encoded parameters.", &param)
		}
		if e := x.dropIngressQuery(); e != nil {
			return e
		}
	}
	if e := checkAnthropicBeta(x); e != nil {
		return e
	}
	session := s.planning(x, canonicalPlanning, x.Route, x.family.Operation(), x.family.Surface(), x.mode)
	source := x.summarizeSource()
	options := s.selectionOptions(ctx, x)
	// A target's context window is weighed against the request as the model that
	// would serve it counts it, so the demand follows the target.
	options.Parameters = source.parameters
	options.Demand = func(_ routing.Provider, t routing.Target) *routing.TokenDemand {
		if x.guard.engine.HasAction(guardrails.Input, guardrails.Redact) || x.guard.engine.HasAction(guardrails.Input, guardrails.Mask) {
			return nil
		}
		return x.sourceDemand(t.ProviderModel)
	}
	options.Prepare = func(p routing.Provider, t routing.Target) (*routing.CandidatePreparation, error) {
		cfg := p.Connector()
		if x.named().ToolExecution == "gateway" && !p.SupportsToolCalls(t.ProviderModel, x.family.Surface(), x.mode) {
			return nil, errors.New("model tool-call capability unavailable")
		}
		if p.Network != nil && p.Network.CredentialID != "" && s.Deps.Runtime.Eligibility(p.Network.CredentialID) != routing.Eligible {
			return nil, errors.New("provider network credential unavailable")
		}
		if !cfg.Supports(x.family.Operation(), x.family.Surface(), x.mode) {
			return nil, errors.New("connector capability unavailable")
		}
		if x.providerState && !stateQualified(&p, t.ProviderModel, x.family.Operation(), x.mode) {
			session.semantic = errors.New("provider-state capability unavailable")
			return nil, session.semantic
		}
		if !x.Strict() && protocols.StructuredOutputRequested(x.generation.parsed) {
			metadata, _ := p.Metadata(t.ProviderModel)
			if metadata.SupportedParameters == nil || !slices.Contains(*metadata.SupportedParameters, "response_format") {
				session.semantic = errors.New("model does not declare structured-output support")
				return nil, session.semantic
			}
		}
		prepared, err := x.prepareCandidate(&p, t.ID, t.ProviderModel)
		if err != nil {
			session.semantic = err
			return nil, err
		}
		return prepared, nil
	}
	options.Preferences = x.legPreferences()
	_, _, failure := session.selectPlan(ctx, options)
	return failure
}

func checkAnthropicBeta(x *Call) *fault.Error {
	if x.Strict() || len(x.semanticHeaders.Values("Anthropic-Beta")) == 0 {
		return nil
	}
	snapshot := x.Snapshot()
	targets := make([]connectors.Config, 0, len(x.Route.Targets))
	for _, target := range x.Route.Targets {
		provider := snapshot.Provider(target.ProviderID)
		targets = append(targets, provider.Connector())
	}
	return anthropicbeta.Check(false, x.generation.parsed.Family, x.semanticHeaders, targets)
}

// WriteAdmissionOverload renders the surface's 503 rejection. The public
// admission middleware calls it before routing, so the request context is the
// only state available — writeSurfaceError applies the write deadline itself.
func WriteAdmissionOverload(w http.ResponseWriter, r *http.Request) {
	fault.WriteSurfaceError(w, fault.Overloaded, fault.RequestSurface(r))
}

// WriteNotFound renders the surface's 404 for a request nothing routed.
// Process composition mounts it under reserved inference prefixes the gateway
// only partially claims, so an unknown endpoint or a method mismatch still
// answers in the envelope the surface's other errors use.
func WriteNotFound(w http.ResponseWriter, r *http.Request, surface string) {
	fault.WriteSurfaceError(w, fault.NotFoundError("not_found", "Unknown endpoint "+r.Method+" "+r.URL.Path+"."), surface)
}

// BindAuthority attaches the authenticated caller's authority to x and pins
// its consumer rate card when the routing inputs are fresh. Stale inputs leave
// the request unpinned so accounting resolves the card at fact time.
func (s *Server) BindAuthority(x *Call, authority keyauthority.Authority) {
	x.authority = authority
	x.authority.SecretOverlap = nil
	x.keyID, x.affinity = authority.ID, []byte(authority.ID)
	x.endUserDigest = authority.EndUserDigest
	x.budgetGroupID = authority.BudgetGroupID
	x.responseMetadata = authority.Policy.ResponseMetadata
	if s.Deps.Runtime == nil {
		return
	}
	inputs := s.Deps.Runtime.RoutingInputs()
	x.routingInputs = inputs
	if authority.ProjectID != nil && inputs != nil {
		if revision, known := inputs.RateCardRevision(authority.ProjectID, s.Now()); known {
			x.cardPinned = true
			if revision != nil {
				x.cardRevisionID = &revision.ID
				x.cardRev = revision
			}
		}
	}
}

func (s *Server) ObserveAdmittedGeneration(x *Call, authority keyauthority.Authority) {
	if s.Deps.ObserveOverlap != nil && authority.SecretOverlap != nil {
		s.Deps.ObserveOverlap(authority.ID, *authority.SecretOverlap, x.request.startedAt)
	}
}

func (s *Server) WorkflowResolver() WorkflowToolRunner {
	if s == nil || s.Deps.WorkflowRuns == nil || s.Deps.WorkflowTools == nil || s.Deps.Debits == nil {
		return nil
	}
	return s.Deps.WorkflowTools
}

func (s *Server) InlineMediaLimits() protocols.InlineMediaLimits {
	if s == nil {
		return normalizedInlineMedia(protocols.InlineMediaLimits{})
	}
	return s.Config.InlineMedia
}

func normalizedConfig(cfg Config) Config {
	return Config{
		Region: cfg.Region, MaxInFlight: cfg.MaxInFlight, MaxBodyBytes: cfg.MaxBodyBytes,
		MaxMediaBodyBytes: positiveOrDefault(cfg.MaxMediaBodyBytes, 64<<20), MaxResponseBytes: cfg.MaxResponseBytes, MaxEventBytes: cfg.MaxEventBytes,
		TrustedProxies: slices.Clone(cfg.TrustedProxies), CORSAllowedOrigins: slices.Clone(cfg.CORSAllowedOrigins),
		InlineMedia: normalizedInlineMedia(cfg.InlineMedia), AdmissionPool: cfg.AdmissionPool, Predicates: cfg.Predicates, Signer: cfg.Signer, Carrier: cfg.Carrier,
		UnconfinedPlugins: cfg.UnconfinedPlugins, MaxShadowInFlight: cfg.MaxShadowInFlight,
	}
}

func positiveOrDefault[T ~int64](value, fallback T) T {
	if value > 0 {
		return value
	}
	return fallback
}

func normalizedInlineMedia(value protocols.InlineMediaLimits) protocols.InlineMediaLimits {
	if value.Items != 0 {
		return value
	}
	return protocols.InlineMediaLimits{Items: 4, ItemBytes: 1 << 20, TotalBytes: 2 << 20}
}

func inferencePool(cfg Config) *concurrency.Pool {
	if cfg.AdmissionPool != nil {
		return cfg.AdmissionPool
	}
	return concurrency.NewPool(max(cfg.MaxInFlight, 1))
}

func codeForwarder(deps Dependencies, cfg Config, gate *ingress.Gate, auth *connectors.Auth) *codeforward.Forwarder {
	if deps.Code == nil || deps.Code.Ledger == nil || deps.Code.Authorizer == nil {
		return nil
	}
	return codeforward.New(codeforward.Dependencies{
		Services: *deps.Code,
		Runtime:  deps.Runtime, Sink: deps.Sink, Admission: deps.Admission, Gate: gate, Egress: deps.Egress, Auth: auth,
		Log: deps.Log, Now: time.Now, MaxBodyBytes: cfg.MaxBodyBytes, HeaderTimeout: UpstreamHeaderTimeout,
	})
}

func normalizedDependencies(deps Dependencies) Dependencies {
	return Dependencies{
		Egress: deps.Egress, Log: deps.Log, Cache: deps.Cache, Guardrails: guardrailRunner(deps),
		Code:    deps.Code,
		Runtime: deps.Runtime, Digester: deps.Digester, BudgetAuthority: deps.BudgetAuthority, TaskAttemptAdmitter: deps.TaskAttemptAdmitter,
		TaskCapabilityAuthenticator: deps.TaskCapabilityAuthenticator, DelegationReauthenticator: deps.DelegationReauthenticator,
		ObserveOverlap: deps.ObserveOverlap,
		Sink:           configuredSink(deps.Sink, deps.Log), Admission: deps.Admission, Debits: deps.Debits, WorkflowRuns: deps.WorkflowRuns, WorkflowTools: deps.WorkflowTools,
		Traces: deps.Traces, Media: deps.Media, Staging: deps.Staging, Uploads: deps.Uploads, Capture: deps.Capture, Business: deps.Business, Resources: deps.Resources, Batches: deps.Batches, Resolver: deps.Resolver,
	}
}

func configuredSink(sink events.Sink, log *slog.Logger) events.Sink {
	if sink != nil {
		return sink
	}
	return events.LogSink{Log: log}
}
