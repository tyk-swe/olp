package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operationplan"
	"github.com/tyk-swe/olp/internal/operationregistry"
	"github.com/tyk-swe/olp/internal/operations"
	"github.com/tyk-swe/olp/internal/operations/generation"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/usage"
)

type unaryExecution struct {
	source         oif.Request
	dialect        operations.Dialect
	route, surface string
	plans          map[string]*operationplan.Plan
}

func (x *execution) operationName() string {
	if x.unary != nil {
		return x.unary.dialect.Operation.ID
	}
	if x.gen != nil {
		return x.gen.Operation.ID
	}
	return x.family.Operation()
}
func (x *execution) surfaceName() string {
	if x.unary != nil {
		return x.unary.surface
	}
	if x.gen != nil {
		return x.gen.Surface
	}
	return x.family.Surface()
}
func (s *Server) selectUnary(x *execution, family openai.Family, dialect string, body []byte, pathModel string) (bool, error) {
	explicit := dialect != ""
	if !explicit {
		ids := map[openai.Family]string{openai.FamilyEmbeddings: "openai-embeddings", openai.FamilyRerank: "rerank", openai.FamilyModeration: "openai-moderation", openai.FamilyInputTokens: "openai-input-tokens", openai.FamilyAnthropicCount: "anthropic-count-tokens", openai.FamilyGeminiCount: "gemini-count-tokens", openai.FamilyGeminiEmbeddings: "gemini-embeddings", openai.FamilyGeminiEmbeddingsBatch: "gemini-batch-embeddings", openai.Family("bedrock_count"): "bedrock-count-tokens"}
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
	if !explicit && (!exists || runtime.FidelityMode(route.Fidelity) != runtime.FidelityStrict) {
		return false, nil
	}
	if !exists {
		return true, modelNotFound(model)
	}
	x.route = &route
	if !x.strict() {
		return true, operations.Error("target_capability", "/route", "strict_native_operation", "Registered native operation endpoints require a strict route.")
	}
	// Generation dialects are operation-owned registrations, not unary codecs:
	// the dialect lifts the body into the neutral source contract and the
	// strict interaction path plans it. An explicit /native/ route always
	// admits on the native surface. A false selection lets the inference flow
	// continue on the lifted source.
	if gen, ok := operationregistry.Generation.DialectLabel(dialect); ok {
		return s.selectNativeGeneration(x, gen, body, model)
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
	x.unary = &unaryExecution{dialect: codec, route: model, surface: surface, plans: map[string]*operationplan.Plan{}}
	x.mode = "unary"
	source, err := operationplan.Parse(dialect, body, int(s.cfg.MaxBodyBytes))
	if err != nil {
		return true, err
	}
	x.unary.source = source
	return true, nil
}

// selectNativeGeneration admits a registered generation dialect on a strict
// /native/ route: the dialect lifts the caller body into the immutable source
// envelope and generic planning continues from the neutral source. A dialect
// that predates registration keeps a legacy envelope view for compatibility
// callers; registration-only dialects carry no wire family at all.
func (s *Server) selectNativeGeneration(x *execution, dialect generation.Dialect, body []byte, model string) (bool, error) {
	transportStream := dialect.DeliveryKey != "" && len(x.semanticQuery[dialect.DeliveryKey]) == 1 && x.semanticQuery[dialect.DeliveryKey][0] == dialect.DeliveryValue
	source, err := dialect.Lift(body, model, transportStream, int(s.cfg.MaxBodyBytes))
	if err != nil {
		return false, err
	}
	if source.Request.Descriptor().Dialect != dialect.Identity {
		return false, operations.Error("target_capability", "/dialect", "source_identity_consistency", "The dialect adapter disagrees with its immutable source contract.")
	}
	x.source, x.gen = source, &dialect
	if dialect.Address.LegacyPath != "" {
		family := openai.Family(dialect.Address.LegacyPath)
		if source.Stream && family == openai.FamilyGemini {
			family = openai.FamilyGeminiStream
		}
		x.family = family
		x.parsed = openai.NewSourceEnvelope(family, model, source.Stream, source.Request.Document())
		if err := protocols.ValidateInlineMedia(x.parsed, s.cfg.InlineMedia); err != nil {
			return false, err
		}
	}
	return false, nil
}
func (x *execution) unaryPlan(p *runtime.Provider, model string) (*operationplan.Plan, error) {
	key := p.ID + "/" + p.RevisionID + "/" + model
	if plan := x.unary.plans[key]; plan != nil {
		return plan, nil
	}
	var template *operationplan.Template
	for _, target := range x.route.Targets {
		if target.ProviderID == p.ID && target.ProviderModel == model {
			template, _ = x.request.release.Snapshot.OperationTemplate(x.route.Slug, target.ID, x.operationName())
			break
		}
	}
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
	plan, err := template.Bind(x.unary.source, operationplan.Context{Headers: x.semanticHeaders, Query: x.semanticQuery, Route: x.route.Slug, ClientContract: client})
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
	x.unary.plans[key] = plan
	return plan, nil
}
func (s *Server) prepareUnary(x *execution) *Error {
	if !x.authority.Allows("inference", x.route.Slug, x.route.ProjectID, s.now()) {
		return permissionError("route_forbidden", "This key cannot use the requested route.")
	}
	if x.semanticQueryInvalid {
		return invalidRequest("invalid_request", "The query is malformed or ambiguous.", nil)
	}
	if x.surfaceName() == "gemini" {
		if values, ok := x.semanticQuery["key"]; ok {
			if len(values) != 1 || values[0] == "" {
				return invalidRequest("invalid_request", "Provide one non-empty API key query parameter.", nil)
			}
			delete(x.semanticQuery, "key")
		}
	}
	var incompatible error
	options := runtime.SelectionOptions{KeyID: x.keyID, Preferences: x.preferences, Inputs: s.routingInputs(), Now: s.now(), CheckSlots: true, CredentialRevoked: s.Runtime.Revoked,
		Accept: func(p runtime.Provider, t runtime.Target) error {
			_, err := x.unaryPlan(&p, t.ProviderModel)
			if err != nil {
				incompatible = err
			}
			return err
		},
		Effective: func(p runtime.Provider, t runtime.Target) ([]string, *runtime.TokenDemand) {
			plan, err := x.unaryPlan(&p, t.ProviderModel)
			if err != nil {
				return nil, nil
			}
			return plan.Parameters(), nil
		}}
	plan, err := runtime.PlanRequest(x.request.release.Snapshot, x.route.Slug, x.operationName(), x.surfaceName(), "unary", x.affinity, options)
	if err != nil {
		return requestError(err)
	}
	x.decisions, x.policy, x.attempts, x.budget = plan.Decisions, plan.Policy, plan.Attempts, plan.Budget
	if len(plan.Attempts) == 0 {
		if incompatible != nil {
			return requestError(incompatible)
		}
		return selectionError(&runtime.SelectionError{Code: runtime.NoEligibleTargets}, x.route.Slug)
	}
	return nil
}
func (s *Server) serveUnary(w http.ResponseWriter, r *http.Request, x *execution, body []byte) (*outcome, int) {
	fail := func(e *Error) (*outcome, int) {
		if x.dispatched && e.Status >= 500 {
			copy := *e
			copy.NoRetry = true
			e = &copy
		}
		writeSurfaceError(w, e, x.surfaceName())
		return &outcome{err: e}, e.Status
	}
	var e *Error
	x.preferences, e = routingPreferences(r)
	if e != nil {
		return fail(e)
	}
	if e = s.prepareUnary(x); e != nil {
		return fail(e)
	}
	x.estimate = requestEstimate(x)
	overall := time.Duration(x.route.OverallTimeout) * time.Millisecond
	ctx, cancel := context.WithTimeout(r.Context(), overall)
	defer cancel()
	x.lease, e = s.Admission.reserveKey(ctx, x.authority, keyReservationEstimate(x.estimate, s.dispatchableAttempts(x)), overall)
	if e != nil {
		return fail(e)
	}
	result := runAttempts(ctx, s, x, attemptAdapter[operationplan.Result]{estimate: x.providerEstimate, dispatch: func(ctx context.Context, a runtime.Attempt, p *runtime.Provider, slot runtime.Slot, n int) (AttemptFact, operationplan.Result, *attemptFailure) {
		return s.unaryAttempt(ctx, x, a, p, slot, n)
	}})
	if result.err != nil {
		return fail(result.err)
	}
	rc := http.NewResponseController(w)
	if rc.SetWriteDeadline(time.Now().Add(responseWriteTimeout)) != nil {
		return fail(serverError(500, "internal_error", "The result could not be written."))
	}
	w.Header().Set("Content-Type", "application/json")
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
