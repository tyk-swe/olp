package routes

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/textproto"
	"net/url"
	"slices"
	"strings"

	"golang.org/x/net/http/httpguts"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/interaction"
	"github.com/tyk-swe/olp/internal/operationregistry"
	"github.com/tyk-swe/olp/internal/operations/generation"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/providerinvoke"
	"github.com/tyk-swe/olp/internal/runtime"
)

type inspectedDecision struct {
	runtime.Decision
	Interaction *interactionInspection `json:"interaction"`
}

type interactionInspection struct {
	Status         string `json:"status"`
	Fidelity       string `json:"fidelity"`
	Class          string `json:"class,omitempty"`
	Operation      string `json:"operation,omitempty"`
	IngressDialect string `json:"ingress_dialect,omitempty"`
	EgressDialect  string `json:"egress_dialect,omitempty"`
	ReturnDialect  string `json:"return_dialect,omitempty"`
	// HostedTools are the provider-hosted tool families the admitted plan
	// covers, bounded by the caller's authorization and the bound profile's
	// qualified lifecycle contracts. It is planner evidence — no tool ran —
	// and is omitted when the plan admitted none.
	HostedTools         []string               `json:"hosted_tools,omitempty"`
	Representation      string                 `json:"representation,omitempty"`
	ProfileID           string                 `json:"profile_id,omitempty"`
	ProfileRevision     string                 `json:"profile_revision,omitempty"`
	EffectiveRequest    *inspectedRequest      `json:"effective_request,omitempty"`
	SemanticContext     []inspectedField       `json:"semantic_context,omitempty"`
	Dispositions        []inspectedDisposition `json:"dispositions,omitempty"`
	OmittedDispositions int                    `json:"omitted_dispositions,omitempty"`
	Obligations         *inspectedObligations  `json:"obligations,omitempty"`
	Evidence            []string               `json:"evidence"`
	Serving             *inspectedServing      `json:"serving,omitempty"`
}

type inspectedServing struct {
	ProviderRevisionID    string `json:"provider_revision_id"`
	Model                 string `json:"model"`
	PrincipalDeclared     bool   `json:"principal_declared"`
	SnapshotDeclared      bool   `json:"snapshot_declared"`
	RegionDeclared        bool   `json:"region_declared"`
	ResourceScopeDeclared bool   `json:"resource_scope_declared"`
}

type inspectedDisposition struct {
	Field       string `json:"field"`
	Disposition string `json:"disposition"`
	Rule        string `json:"rule"`
	Evidence    string `json:"evidence"`
}

type inspectedObligations struct {
	Delivery                string   `json:"delivery"`
	Lifetime                string   `json:"lifetime"`
	Submission              string   `json:"submission"`
	Effects                 []string `json:"effects"`
	Continuation            string   `json:"continuation"`
	Retry                   string   `json:"retry"`
	MaxBodyBytes            int      `json:"max_body_bytes"`
	MaxEventBytes           int      `json:"max_event_bytes"`
	MaxContinuationBytes    int      `json:"max_continuation_bytes,omitempty"`
	Actionability           string   `json:"actionability,omitempty"`
	RejectAmbiguousFailover bool     `json:"reject_ambiguous_failover"`
	GuardResults            bool     `json:"guard_results"`
}

type inspectionDiagnostic struct{ code, field, requirement, message string }

func (e *inspectionDiagnostic) Error() string { return e.message }
func (e *inspectionDiagnostic) Incompatibility() (string, string, string, string) {
	return e.code, e.field, e.requirement, e.message
}

func safeInspectionError(err error) error {
	var diagnostic interface {
		Incompatibility() (code, field, requirement, message string)
	}
	if errors.As(err, &diagnostic) {
		code, field, requirement, message := diagnostic.Incompatibility()
		return &inspectionDiagnostic{code, inspectionField(field), requirement, message}
	}
	return &inspectionDiagnostic{"target_capability", "/", "prepared_invocation", "The selected target cannot prepare this invocation."}
}

func inspectionContext(headers, query map[string]string, allowState, allowHosted bool) (interaction.Context, error) {
	context := interaction.Context{Headers: http.Header{}, Query: url.Values{}, AllowProviderState: allowState, AllowHostedTools: allowHosted}
	if len(headers) > 16 || len(query) > 16 {
		return context, access.Invalid("semantic_headers", "Use at most 16 semantic headers and query settings.")
	}
	allowedHeaders, allowedQuery := map[string]bool{}, map[string]bool{}
	for _, profile := range connectors.Profiles() {
		for _, name := range profile.SemanticHeaders {
			allowedHeaders[textproto.CanonicalMIMEHeaderKey(name)] = true
		}
		for _, name := range profile.QuerySettings {
			allowedQuery[name] = true
		}
	}
	for name, value := range headers {
		canonical := textproto.CanonicalMIMEHeaderKey(name)
		_, duplicate := context.Headers[canonical]
		if !allowedHeaders[canonical] || len(value) > 2048 || !httpguts.ValidHeaderFieldValue(value) || duplicate {
			return context, access.Invalid("semantic_headers", "Use distinct profile-owned semantic header names and bounded values; authentication headers are not accepted.")
		}
		context.Headers[canonical] = []string{value}
	}
	for name, value := range query {
		if !allowedQuery[name] || len(value) > 2048 || strings.ContainsAny(value, "\r\n\x00") {
			return context, access.Invalid("query_settings", "Use bounded profile-owned semantic query settings; credentials and transport overrides are not accepted.")
		}
		context.Query[name] = []string{value}
	}
	return context, nil
}

func inspectorRequest(raw json.RawMessage, operation, surface, mode, dialect, slug string, strict bool) (*openai.Request, generation.Source, error) {
	if len(raw) == 0 {
		return nil, generation.Source{}, nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil, generation.Source{}, access.Invalid("request", "Use a native request object.")
	}
	if model, present := fields["model"]; present {
		var name string
		if json.Unmarshal(model, &name) != nil || name != slug {
			return nil, generation.Source{}, access.Invalid("request.model", "The request must name the route being inspected.")
		}
	}
	tupleOnly := len(fields) > 0
	for field := range fields {
		tupleOnly = tupleOnly && (field == "route" || field == "model")
	}
	if tupleOnly {
		return nil, generation.Source{}, nil
	}
	if !strict && dialect == "" {
		parsed, err := protocols.SimulationRequest(raw, operation, surface, mode, slug)
		if err != nil {
			return nil, generation.Source{}, access.Invalid("request", "The request is not valid for the selected operation and surface.")
		}
		return parsed, generation.Source{}, nil
	}
	if _, canonical := fields["route"]; canonical {
		return nil, generation.Source{}, access.Invalid("request", "Actual strict inspection requires a native request; set its model to the selected route and choose its dialect.")
	}
	families := []openai.Family{openai.FamilyChat, openai.FamilyResponses, openai.FamilyAnthropic, openai.FamilyGemini, openai.FamilyGeminiStream, openai.FamilyInputTokens, openai.FamilyAnthropicCount, openai.FamilyGeminiCount, openai.FamilyEmbeddings, openai.FamilyModeration, openai.FamilyRerank}
	var family openai.Family
	for _, candidate := range families {
		if candidate.Operation() != operation || candidate.Surface() != surface {
			continue
		}
		if candidate == openai.FamilyGemini && mode == "streaming" || candidate == openai.FamilyGeminiStream && mode != "streaming" {
			continue
		}
		if dialect == "" || openai.Descriptor(candidate, mode == "streaming").Dialect.ID == dialect {
			family = candidate
			break
		}
	}
	if family != "" {
		parsed, err := protocols.Parse(family, raw, slug)
		if err != nil {
			return nil, generation.Source{}, access.Invalid("request", "The request is not valid for the selected native dialect.")
		}
		if parsed.Route != slug || parsed.Stream != (mode == "streaming") {
			return nil, generation.Source{}, access.Invalid("request", "The native request must name the selected route and match the selected delivery mode.")
		}
		return parsed, generation.Source{}, nil
	}
	// A registered generation dialect carries no wire family; its own Lift
	// produces the neutral source contract inspection binds directly.
	if operation == "generation" && dialect != "" {
		if gen, ok := operationregistry.Generation.DialectLabel(dialect); ok && gen.Lift != nil {
			if gen.Surface != surface && surface != "native" {
				return nil, generation.Source{}, access.Invalid("dialect", "Choose a dialect belonging to this operation, surface and mode.")
			}
			source, err := gen.Lift(raw, slug, false, 1<<20)
			if err != nil {
				return nil, generation.Source{}, access.Invalid("request", "The request is not valid for the selected native dialect.")
			}
			if source.Route != slug || source.Stream != (mode == "streaming") {
				return nil, generation.Source{}, access.Invalid("request", "The native request must name the selected route and match the selected delivery mode.")
			}
			return nil, source, nil
		}
	}
	return nil, generation.Source{}, access.Invalid("dialect", "Choose a dialect belonging to this operation, surface and mode.")
}

func inspectionAccept(route runtime.Route, parsed *openai.Request, source generation.Source, context interaction.Context, demand *runtime.TokenDemand) (func(runtime.Provider, runtime.Target) error, func(runtime.Provider, runtime.Target) ([]string, *runtime.TokenDemand), map[string]*interactionInspection) {
	inspections := map[string]*interactionInspection{}
	if parsed == nil && !source.Request.Document().Valid() {
		return nil, nil, inspections
	}
	fidelity := runtime.FidelityMode(route.Fidelity)
	effectivePlans := map[string]*interaction.Plan{}
	var effective func(runtime.Provider, runtime.Target) ([]string, *runtime.TokenDemand)
	if fidelity == runtime.FidelityStrict {
		effective = func(_ runtime.Provider, target runtime.Target) ([]string, *runtime.TokenDemand) {
			plan := effectivePlans[target.ID]
			delete(effectivePlans, target.ID)
			if plan == nil {
				return nil, demand
			}
			// The plan's dialect owns the effective request's reservation and
			// routing shapes; a registration-only dialect has no legacy view.
			output := plan.OutputLimit()
			var resolved *runtime.TokenDemand
			if demand != nil || output != nil {
				resolved = &runtime.TokenDemand{MaxOutputTokens: output}
				if demand != nil {
					resolved.EstimatedInputTokens = demand.EstimatedInputTokens
					if output == nil {
						resolved.MaxOutputTokens = demand.MaxOutputTokens
					}
				}
			}
			return plan.Parameters(), resolved
		}
	}
	accept := func(provider runtime.Provider, target runtime.Target) error {
		result := &interactionInspection{Status: "incompatible", Fidelity: fidelity, Evidence: []string{}}
		inspections[target.ID] = result
		config := provider.Connector()
		if fidelity != runtime.FidelityStrict {
			// Legacy/transformed previews retain their explicit old semantics and
			// never acquire a strict qualification class from a successful encode.
			if len(context.Headers) > 0 || len(context.Query) > 0 || context.ContinuationVersion != "" {
				return &inspectionDiagnostic{"target_capability", "/", "semantic_context", "Inspect caller semantic context on an explicit strict route."}
			}
			if parsed == nil {
				return &inspectionDiagnostic{"target_capability", "/dialect", "registered_dialect", "A registration-only generation dialect inspects on an explicit strict route."}
			}
			invocation, err := providerinvoke.Prepare(parsed, config, target.ProviderModel, provider.ParameterDefaults)
			if err != nil {
				return err
			}
			result.Status, result.Class = "legacy", fidelity
			result.Operation = parsed.Family.Operation()
			result.IngressDialect = openai.Descriptor(parsed.Family, parsed.Stream).Dialect.ID
			result.EgressDialect = openai.Descriptor(invocation.Wire, parsed.Stream).Dialect.ID
			result.ReturnDialect = result.IngressDialect
			result.Representation = "oif"
			summary := inspectRequest(invocation.Prepared.Document(), invocation.Prepared.Provenance())
			result.EffectiveRequest = &summary
			return nil
		}
		template, err := interaction.Compile(interaction.Config{Provider: config, ProviderID: provider.ID, RevisionID: provider.RevisionID, Model: target.ProviderModel, Policy: route.ContentPolicy})
		if err != nil {
			return safeInspectionError(err)
		}
		binding := context
		binding.RetainedResponses = context.DurableContinuation && config.SupportsRetainedResponses()
		var plan *interaction.Plan
		if source.Request.Document().Valid() {
			// A registered generation source binds through the neutral
			// contract directly; only legacy family requests take the
			// compatibility adapter.
			plan, err = template.Bind(source, binding)
		} else {
			plan, err = template.BindRequest(parsed, binding)
		}
		if err != nil {
			return safeInspectionError(err)
		}
		receipt, obligations := plan.Receipt(), plan.Obligations()
		result.Class, result.Operation = receipt.Class, receipt.Operation
		result.IngressDialect, result.EgressDialect, result.ReturnDialect = receipt.SourceDialect, receipt.TargetDialect, receipt.SourceDialect
		result.Representation = "oif"
		result.HostedTools = plan.Hosted()
		result.ProfileID, result.ProfileRevision = receipt.ProfileID, receipt.ProfileRevision
		serving := plan.Serving()
		result.Serving = &inspectedServing{ProviderRevisionID: serving.RevisionID, Model: serving.Model, PrincipalDeclared: serving.PrincipalID != "", SnapshotDeclared: serving.Snapshot != "", RegionDeclared: serving.Region != "", ResourceScopeDeclared: serving.ResourceScope != ""}
		result.Evidence = append([]string{}, receipt.Evidence...)
		result.Obligations = &inspectedObligations{
			Delivery: obligations.Delivery, Lifetime: obligations.Lifetime, Continuation: obligations.Continuation, Retry: obligations.Retry,
			Submission: obligations.Submission, Effects: append([]string{}, obligations.Effects...),
			MaxBodyBytes: obligations.MaxBodyBytes, MaxEventBytes: obligations.MaxEventBytes, MaxContinuationBytes: obligations.MaxContinuationBytes, Actionability: obligations.Actionability, RejectAmbiguousFailover: obligations.RejectAmbiguousFailover, GuardResults: obligations.GuardResults,
		}
		seen := map[inspectedDisposition]bool{}
		for _, disposition := range receipt.Dispositions {
			entry := inspectedDisposition{inspectionField(disposition.Field), disposition.Disposition, disposition.Rule, disposition.Evidence}
			if !seen[entry] && len(result.Dispositions) < 64 {
				result.Dispositions = append(result.Dispositions, entry)
				seen[entry] = true
			} else {
				result.OmittedDispositions++
			}
		}
		effectiveRequest := plan.Effective()
		summary := inspectRequest(effectiveRequest, plan.Prepared().Provenance())
		result.EffectiveRequest = &summary
		preparedConfig := plan.Config()
		for name := range preparedConfig.SemanticHeaders {
			origin := "provider_default"
			if _, supplied := context.Headers[textproto.CanonicalMIMEHeaderKey(name)]; supplied {
				origin = "caller"
			}
			result.SemanticContext = append(result.SemanticContext, inspectedField{Field: "/headers/" + textproto.CanonicalMIMEHeaderKey(name), Kind: "string", Origin: origin, Redacted: true})
		}
		for name := range preparedConfig.QuerySettings {
			origin := "provider_default"
			if _, supplied := context.Query[name]; supplied {
				origin = "caller"
			}
			result.SemanticContext = append(result.SemanticContext, inspectedField{Field: "/query/" + name, Kind: "string", Origin: origin, Redacted: true})
		}
		slices.SortFunc(result.SemanticContext, func(a, b inspectedField) int { return strings.Compare(a.Field, b.Field) })
		if _, err := plan.CheckInput(); err != nil {
			safe := safeInspectionError(err)
			if diagnostic, ok := safe.(*inspectionDiagnostic); ok && diagnostic.code == "content_policy_blocked" {
				result.Status = "blocked"
			}
			return safe
		}
		result.Status = "admitted"
		effectivePlans[target.ID] = plan
		return nil
	}
	return accept, effective, inspections
}

func inspectedDecisions(decisions []runtime.Decision, route runtime.Route, inspected bool, details map[string]*interactionInspection) []inspectedDecision {
	result := make([]inspectedDecision, 0, len(decisions))
	for _, decision := range decisions {
		inspection := details[decision.TargetID]
		if inspection == nil {
			status := "not_evaluated"
			if !inspected {
				status = "not_inspected"
			}
			inspection = &interactionInspection{Status: status, Fidelity: runtime.FidelityMode(route.Fidelity), Evidence: []string{}}
		}
		result = append(result, inspectedDecision{Decision: decision, Interaction: inspection})
	}
	return result
}

// Capability is supplied by server composition, never asserted by raw request
// fields. Inspection binds the same versioned planner without claiming a state
// reservation, creating a handle, or dispatching an inference request.
func inspectionClientContract(version string, context *interaction.Context, encrypted bool) error {
	if version != "" && !operationregistry.Generation.KnownContract(version) {
		return access.Invalid("client_contract", "Use a registered generation client contract.")
	}
	context.ContinuationVersion = version
	context.DurableContinuation = encrypted
	return nil
}
