package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/routing"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operationplan"
	"github.com/tyk-swe/olp/internal/operationregistry"
	"github.com/tyk-swe/olp/internal/operations"
	"github.com/tyk-swe/olp/internal/usage"
)

type unaryCall struct {
	source         oif.Request
	dialect        operations.Dialect
	route, surface string
}

func (x *call) operationName() string {
	if x.unary != nil {
		return x.unary.dialect.Operation.ID
	}
	return x.family.Operation()
}
func (x *call) surfaceName() string {
	if x.unary != nil {
		return x.unary.surface
	}
	return x.family.Surface()
}

// clientSurface is the wire surface the caller speaks, which shapes error
// envelopes and credential query parameters. It can differ from surfaceName,
// the capability surface: Gemini embeddings match OpenAI embedding targets.
func (x *call) clientSurface() string {
	if x.unary != nil && x.unary.surface == "native" {
		return "native"
	}
	if x.ingress != "" {
		return x.ingress
	}
	return x.surfaceName()
}
func (s *Server) selectUnary(x *call, family wire.Format, dialect string, body []byte, pathModel string) (bool, error) {
	explicit := dialect != ""
	if !explicit {
		ids := map[wire.Format]string{wire.Embeddings: "openai-embeddings", wire.Rerank: "rerank", wire.Moderation: "openai-moderation", wire.InputTokens: "openai-input-tokens", wire.AnthropicCount: "anthropic-count-tokens", wire.GeminiCount: "gemini-count-tokens", wire.GeminiEmbeddings: "gemini-embeddings", wire.GeminiEmbeddingsBatch: "gemini-batch-embeddings", wire.Format("bedrock_count"): "bedrock-count-tokens"}
		dialect = ids[family]
		if dialect == "" {
			return false, nil
		}
	}
	model := pathModel
	if model == "" {
		var header struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &header)
		model = header.Model
	}
	route, exists := x.request.release.Snapshot.Routes[model]
	// The native Gemini embedding methods exist only on strict routes, and the
	// parser that takes any other request cannot read their bodies, so an
	// unknown or transformed route is refused here with its own error.
	nativeOnly := family == wire.GeminiEmbeddings || family == wire.GeminiEmbeddingsBatch
	if !explicit && !nativeOnly && (!exists || !route.Fidelity.Strict()) {
		return false, nil
	}
	if !exists {
		return true, modelNotFound(model)
	}
	x.route = &route
	if !x.strict() {
		return true, operations.Error("target_capability", "/route", "strict_native_operation", "Registered native operation endpoints require a strict route.")
	}
	codec, ok := operationregistry.Lookup(dialect)
	if !ok {
		return true, operations.Error("target_capability", "/dialect", "registered_dialect", "The native dialect is not registered.")
	}
	surface := family.Surface()
	if explicit {
		surface = "native"
	}
	// Classify before the body can fail to parse: a malformed request is still
	// this dialect's operation on its native surface in the terminal envelope.
	x.unary = &unaryCall{dialect: codec, route: model, surface: surface}
	x.mode = "unary"
	source, err := operationplan.Parse(dialect, body, int(s.cfg.MaxBodyBytes))
	if err != nil {
		return true, err
	}
	x.unary.source = source
	return true, nil
}
func (x *call) unaryPlan(p *routing.Provider, targetID, model string) (*operationplan.Plan, error) {
	x.prepareSource()
	record := x.targetPreparation(p, targetID, model)
	if record.unary.plan != nil {
		return record.unary.plan, nil
	}
	template, _ := x.snapshot().OperationTemplate(x.route.Slug, targetID, x.operationName())
	if template == nil {
		return nil, operations.Error("target_capability", "/operation", "compiled_operation", "The selected target has no compiled native operation contract.")
	}
	clientValues := x.semanticHeaders.Values("X-Olp-Client-Contract")
	if len(clientValues) > 1 {
		return nil, operations.Invalid("/client_contract", "Provide one client contract.")
	}
	client := ""
	if len(clientValues) == 1 {
		client = clientValues[0]
	}
	plan, err := template.Bind(x.unary.source, operationplan.Context{Headers: x.semanticHeaders, Query: x.semanticQuery, Route: x.unary.route, ClientContract: client})
	if err != nil {
		return nil, err
	}
	decisions, err := plan.CheckInput()
	for _, decision := range decisions {
		recordDecision(x, decision)
	}
	if err != nil {
		return nil, err
	}
	record.unary = unaryAdmission{plan: plan, estimate: plan.Estimate(), receipt: plan.Receipt(), parameters: routing.Listed(plan.Parameters())}
	x.retainPreparation(record)
	return plan, nil
}
func (s *Server) prepareUnary(ctx context.Context, x *call) *Error {
	authorize := s.keyAuthorizer(x.authority)
	if e := authorize(x.route); e != nil {
		return e
	}
	if x.semanticQueryInvalid {
		return invalidRequest("invalid_request", "The query is malformed or ambiguous.", nil)
	}
	if e := x.dropIngressQuery(); e != nil {
		return e
	}
	x.bind(unaryPlanner, authorize, false)
	return s.planNamed(ctx, x)
}

// planUnary plans x.route for a registered native operation.
func (s *Server) planUnary(ctx context.Context, x *call) *Error {
	if e := s.checkRouteFidelity(ctx, x.route); e != nil {
		return e
	}
	var incompatible error
	options := s.selectionOptions(ctx, x)
	options.Accept = func(p routing.Provider, t routing.Target) error {
		_, err := x.unaryAdmission(&p, t.ID, t.ProviderModel)
		if err != nil {
			incompatible = err
		}
		return err
	}
	options.Effective = func(p routing.Provider, t routing.Target) (routing.Names, *routing.TokenDemand) {
		admission, err := x.unaryAdmission(&p, t.ID, t.ProviderModel)
		if err != nil {
			return nil, nil
		}
		return admission.parameters, nil
	}
	plan, err := routing.Select(x.request.release.Snapshot, x.route.Slug, x.operationName(), x.surfaceName(), "unary", x.affinity, options)
	if err != nil {
		return requestError(err)
	}
	if plan.Delegate != "" {
		return s.delegate(ctx, x, plan)
	}
	x.adoptPlan(plan)
	if len(plan.Attempts) == 0 {
		if incompatible != nil {
			return requestError(incompatible)
		}
		x.noteCostExceeded(plan)
		if x.costExcluded {
			return costLimitExceeded()
		}
		return selectionError(&routing.SelectionError{Code: routing.NoEligibleTargets}, x.route.Slug)
	}
	return nil
}
func (s *Server) serveUnary(w http.ResponseWriter, r *http.Request, x *call, body []byte) (*outcome, int) {
	fail := func(e *Error) (*outcome, int) {
		if x.dispatched && e.Status >= 500 {
			copy := *e
			copy.NoRetry = true
			e = &copy
		}
		// A cancelled client has no status to receive and nothing to read.
		if e.Status > 0 {
			writeSurfaceError(w, e, x.clientSurface())
		}
		return &outcome{err: e, cancelled: e.Status == 0}, e.Status
	}
	var e *Error
	x.preferences, x.priority, e = routingControls(r, x.authority.Policy)
	if e != nil {
		return fail(e)
	}
	if e = s.prepareUnary(r.Context(), x); e != nil {
		return fail(e)
	}
	x.estimate = requestEstimate(x)
	overall := time.Duration(x.primary.OverallTimeout) * time.Millisecond
	ctx, cancel := x.routeContext(r.Context())
	defer cancel()
	if e = s.awaitAdmission(ctx, x); e != nil {
		return fail(e)
	}
	admitted := x.admissionAuthority(x.authority)
	x.lease, e = s.Admission.reserveKeyCosted(ctx, &admitted, x.clientSurface(), keyReservationEstimate(x.estimate, s.dispatchableAttempts(x)), overall, s.costReservation(x, x.authority), x.limitRoute())
	if e != nil {
		return fail(e)
	}
	result := runAttempts(ctx, s, x, attemptAdapter[operationplan.Result]{estimate: x.attemptReservation, dispatch: func(ctx context.Context, a routing.Attempt, p *routing.Provider, slot routing.Slot, fact AttemptFact) (AttemptFact, operationplan.Result, *attemptFailure) {
		return s.unaryAttempt(ctx, x, a, p, slot, fact)
	}})
	if result.err != nil {
		out, status := fail(result.err)
		out.committed, out.cancelled = result.committed, out.cancelled || result.cancelled
		return out, status
	}
	rc := http.NewResponseController(w)
	if rc.SetWriteDeadline(time.Now().Add(responseWriteTimeout)) != nil {
		return fail(serverError(500, "internal_error", "The result could not be written."))
	}
	w.Header().Set("Content-Type", "application/json")
	x.responseHeaders(w.Header(), false)
	w.WriteHeader(http.StatusOK)
	out := &outcome{committed: true}
	fact := &x.facts[len(x.facts)-1]
	fact.Committed = true
	if fact.Interaction != nil {
		fact.Interaction.ClientState = usage.ClientPartial
	}
	_, err := w.Write(result.result.Body)
	if err == nil {
		err = rc.Flush()
	}
	if err != nil {
		out.err = (&attemptFailure{class: classCancelled}).toError()
		out.cancelled = true
	} else {
		x.delivered(s.now())
		if fact.Interaction != nil {
			fact.Interaction.ClientState = usage.ClientTerminal
		}
	}
	return out, http.StatusOK
}
