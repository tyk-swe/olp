package routing

import (
	"fmt"
	"slices"
	"strconv"

	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/domain/vendors"

	"github.com/tyk-swe/olp/internal/domain/operationregistry"
	"github.com/tyk-swe/olp/internal/duplexcontract"
	"github.com/tyk-swe/olp/internal/interaction"
	"github.com/tyk-swe/olp/internal/media/mediacontract"
	"github.com/tyk-swe/olp/internal/operationplan"
	"github.com/tyk-swe/olp/internal/retainedapi"
)

// ValidateRouteContracts validates the complete configured links of a route.
// Request values are bound later; no provider request or discovery runs here.
func (s *Snapshot) ValidateRouteContracts(route Route) error {
	if err := route.ValidatePassthrough(); err != nil {
		return err
	}
	if route.IsPassthrough() {
		if s.Guardrails != nil {
			view := *s
			view.Routes = map[string]Route{route.Slug: route}
			if err := view.compileGuardrails(); err != nil {
				return err
			}
			if view.GuardrailEngine(route.Slug, "") != nil {
				return &Refusal{Code: "passthrough_conflict", Message: "A pass-through route cannot carry guardrails."}
			}
		}
		return nil
	}
	if s.Guardrails != nil {
		view := *s
		view.Routes = map[string]Route{route.Slug: route}
		if err := view.compileGuardrails(); err != nil {
			return err
		}
	}
	_, err := s.compileRouteExecution(route)
	if err == nil {
		_, err = s.compileOperations(route)
	}
	if err == nil {
		_, err = s.compileMedia(route)
	}
	if err == nil {
		_, err = s.compileDurable(route)
	}
	if err == nil {
		_, err = s.compileRealtime(route)
	}
	if err == nil {
		_, err = s.compileWebRTC(route)
	}
	if err == nil {
		_, err = s.compileDuplex(route)
	}
	return err
}

func (s *Snapshot) compileRouteExecution(route Route) (map[string]*interaction.Template, error) {
	if route.Cache != nil && route.Cache.Mode == "semantic" {
		embedding, ok := s.Routes[route.Cache.EmbeddingRoute]
		if !ok || !slices.Contains(embedding.Operations, "embeddings") {
			return nil, fmt.Errorf("semantic cache requires a published embeddings route")
		}
	}
	if err := route.ValidateCaching(); err != nil {
		return nil, err
	}
	if err := validateFidelity(route.Fidelity); err != nil {
		return nil, err
	}
	if !route.Fidelity.Strict() || !slices.Contains(route.Operations, "generation") {
		return nil, nil
	}
	templates := make(map[string]*interaction.Template, len(route.Targets))
	for _, target := range route.Targets {
		provider, ok := s.Providers[target.ProviderID]
		if !ok {
			return nil, fmt.Errorf("route target references an unavailable provider")
		}
		// Interactions has its own step and resource contract. Its public runner
		// compiles that contract at admission and never uses GenerateContent.
		if provider.ProfileID == "gemini-interactions" {
			continue
		}
		template, err := interaction.Compile(interaction.Config{Provider: provider.Connector(), ProviderID: provider.ID, RevisionID: provider.RevisionID, Model: target.ProviderModel})
		if err != nil {
			return nil, err
		}
		templates[target.ID] = template
	}
	return templates, nil
}

// InteractionTemplate is compiled with the pinned release, never cached by a
// caller-controlled prompt or schema. Its cardinality is bounded by route targets.
func (s *Snapshot) InteractionTemplate(slug, target string) (*interaction.Template, bool) {
	template, ok := s.interactions[slug][target]
	return template, ok
}

// Shadowed reports whether any of the route's targets mirrors traffic.
func (r *Route) Shadowed() bool {
	for _, target := range r.Targets {
		if target.Shadow != nil {
			return true
		}
	}
	return false
}

// EffectiveOutputLimit reads the dialect-owned bound after omission defaults.
// It does not assert equivalence between reasoning and generation token scopes.
func EffectiveOutputLimit(request *wire.Request) *int64 {
	paths := []string{"/max_completion_tokens", "/max_tokens"}
	switch request.Family {
	case wire.Completions:
		paths = []string{"/max_tokens"}
	case wire.Responses:
		paths = []string{"/max_output_tokens"}
	case wire.Bedrock:
		paths = []string{"/inferenceConfig/maxTokens"}
	case wire.Gemini, wire.GeminiStream:
		paths = []string{"/generationConfig/maxOutputTokens"}
	}
	for _, path := range paths {
		if value, ok := request.OIF().Document().Lookup(path); ok {
			if number, err := strconv.ParseInt(value.Raw(), 10, 64); err == nil && number >= 0 {
				return &number
			}
		}
	}
	return nil
}

func (s *Snapshot) compileOperations(route Route) (map[string]*operationplan.Template, error) {
	templates := map[string]*operationplan.Template{}
	for _, op := range route.Operations {
		if op == PassthroughOperation {
			continue
		}
		if op == "agent_resource" {
			if !route.Fidelity.Strict() {
				return nil, fmt.Errorf("agent resources require a strict route")
			}
			for _, target := range route.Targets {
				provider, ok := s.Providers[target.ProviderID]
				if !ok || !provider.Connector().Supports(op, "gemini", "unary") {
					return nil, fmt.Errorf("route target cannot serve the agent resource contract")
				}
			}
			continue
		}
		if retainedapi.Operation(op) {
			// Retained families serve through their own pinned
			// handlers, not an operationplan dialect. Compilation
			// only proves every target can serve the contract.
			for _, target := range route.Targets {
				provider, ok := s.Providers[target.ProviderID]
				if !ok {
					return nil, fmt.Errorf("route target references an unavailable provider")
				}
				served := op == "retrieval" && provider.Connector().Supports(op, "openai", "unary")
				for _, family := range retainedapi.Families() {
					if family.Operation == op && provider.Connector().Supports(op, family.Surface, "unary") {
						served = true
					}
				}
				if !served {
					return nil, fmt.Errorf("route target cannot serve the %s contract", op)
				}
			}
			continue
		}
		if !route.Fidelity.Strict() && !operationregistry.PolicyFor(op).TransformedUnary {
			continue
		}
		if op == "generation" || op == "batch" || op == "realtime" || vendors.IsMediaOperation(op) {
			continue
		}
		for _, target := range route.Targets {
			provider, ok := s.Providers[target.ProviderID]
			if !ok {
				return nil, fmt.Errorf("route target references an unavailable provider")
			}
			template, err := operationplan.Compile(operationplan.Config{Provider: provider.Connector(), ProviderID: provider.ID, RevisionID: provider.RevisionID, Model: target.ProviderModel, Operation: op, AllowProviderFetchedDocuments: route.ProviderFetchedDocuments})
			if err != nil {
				return nil, err
			}
			templates[op+"/"+target.ID] = template
		}
	}
	return templates, nil
}

func (s *Snapshot) compileRealtime(route Route) (map[string]*RealtimeTemplate, error) {
	if !route.Fidelity.Strict() || !slices.Contains(route.Operations, "realtime") {
		return nil, nil
	}
	templates := make(map[string]*RealtimeTemplate, len(route.Targets))
	for _, target := range route.Targets {
		provider, ok := s.Providers[target.ProviderID]
		if !ok {
			return nil, fmt.Errorf("route target references an unavailable provider")
		}
		// Gemini Live has its own setup-first contract on a distinct ingress.
		if provider.ProfileID == "gemini-live" {
			continue
		}
		if duplexcontract.IsProfile(provider.ProfileID) {
			continue
		}
		if !provider.Supports(target.ProviderModel, "realtime", "openai", "realtime") {
			if provider.Supports(target.ProviderModel, "realtime", "openai", "webrtc") {
				continue
			}
			return nil, fmt.Errorf("target %s has no certified OpenAI realtime capability", target.ID)
		}
		if len(provider.ParameterDefaults) > 0 {
			return nil, fmt.Errorf("target %s has unqualified realtime parameter defaults", target.ID)
		}
		template, err := compileRealtimeContract(realtimeContractConfig{Provider: provider.Connector(), Model: target.ProviderModel})
		if err != nil {
			return nil, err
		}
		templates[target.ID] = template
	}
	return templates, nil
}

func (s *Snapshot) RealtimeTemplate(slug, target string) (*RealtimeTemplate, bool) {
	template, ok := s.realtime[slug][target]
	return template, ok
}

func (s *Snapshot) compileWebRTC(route Route) (map[string]*RealtimeTemplate, error) {
	if !route.Fidelity.Strict() || !slices.Contains(route.Operations, "realtime") {
		return nil, nil
	}
	templates := make(map[string]*RealtimeTemplate, len(route.Targets))
	for _, target := range route.Targets {
		provider, ok := s.Providers[target.ProviderID]
		if !ok {
			return nil, fmt.Errorf("route target references an unavailable provider")
		}
		if provider.ProfileID == "gemini-live" {
			continue
		}
		if !provider.Supports(target.ProviderModel, "realtime", "openai", "webrtc") {
			continue
		}
		if len(provider.ParameterDefaults) > 0 {
			return nil, fmt.Errorf("target %s has unqualified realtime parameter defaults", target.ID)
		}
		template, err := compileWebRTCContract(realtimeContractConfig{Provider: provider.Connector(), Model: target.ProviderModel})
		if err != nil {
			return nil, err
		}
		templates[target.ID] = template
	}
	return templates, nil
}

func (s *Snapshot) WebRTCTemplate(slug, target string) (*RealtimeTemplate, bool) {
	template, ok := s.webrtc[slug][target]
	return template, ok
}

func (s *Snapshot) compileDuplex(route Route) (map[string]*duplexcontract.Template, error) {
	if !route.Fidelity.Strict() || !slices.Contains(route.Operations, "realtime") {
		return nil, nil
	}
	templates := make(map[string]*duplexcontract.Template)
	for _, target := range route.Targets {
		provider, ok := s.Providers[target.ProviderID]
		if !ok {
			return nil, fmt.Errorf("route target references an unavailable provider")
		}
		if !duplexcontract.IsProfile(provider.ProfileID) {
			continue
		}
		if !provider.Supports(target.ProviderModel, "realtime", "native", "realtime") {
			return nil, fmt.Errorf("target %s has no certified native duplex capability", target.ID)
		}
		if len(provider.ParameterDefaults) > 0 {
			return nil, fmt.Errorf("target %s has unqualified duplex parameter defaults", target.ID)
		}
		template, err := duplexcontract.Compile(duplexcontract.Config{Provider: provider.Connector(), Model: target.ProviderModel})
		if err != nil {
			return nil, err
		}
		templates[target.ID] = template
	}
	return templates, nil
}

func (s *Snapshot) DuplexTemplate(slug, target string) (*duplexcontract.Template, bool) {
	template, ok := s.duplex[slug][target]
	return template, ok
}

func (s *Snapshot) compileMedia(route Route) (map[string]*mediacontract.Template, error) {
	if !route.Fidelity.Strict() {
		return nil, nil
	}
	templates := map[string]*mediacontract.Template{}
	for _, op := range route.Operations {
		if !vendors.IsMediaOperation(op) {
			continue
		}
		for _, target := range route.Targets {
			provider, ok := s.Providers[target.ProviderID]
			if !ok {
				return nil, fmt.Errorf("route target references an unavailable provider")
			}
			template, err := mediacontract.Compile(mediacontract.Config{Provider: provider.Connector(), ProviderID: provider.ID, RevisionID: provider.RevisionID, Model: target.ProviderModel, Operation: op})
			if err != nil {
				return nil, err
			}
			templates[op+"/"+target.ID] = template
		}
	}
	return templates, nil
}

func (s *Snapshot) MediaTemplate(slug, target, operation string) (*mediacontract.Template, bool) {
	template, ok := s.media[slug][operation+"/"+target]
	return template, ok
}

func (s *Snapshot) compileDurable(route Route) (map[string]*DurableTemplate, error) {
	if !route.Fidelity.Strict() || !slices.Contains(route.Operations, "batch") {
		return nil, nil
	}
	templates := make(map[string]*DurableTemplate, len(route.Targets))
	for _, target := range route.Targets {
		provider, ok := s.Providers[target.ProviderID]
		if !ok {
			return nil, fmt.Errorf("route target references an unavailable provider")
		}
		// The durable contract binds OpenAI batch requests only, and strict
		// files/batches serve only OpenAI and Azure targets. Any other
		// target (Anthropic, Gemini, native cloud, compatible vendors) has
		// no template: its own handler refuses strict batch at request
		// time, so activation must not fail here.
		if provider.Kind != "openai" && provider.Kind != "azure_openai" {
			continue
		}
		if !slices.ContainsFunc(provider.Capabilities, func(c Capability) bool {
			return c.Model == target.ProviderModel && c.Operation == "batch" && c.Surface == "openai" && c.Mode == "unary"
		}) {
			continue
		}
		template, err := compileDurableContract(durableContractConfig{Provider: provider.Connector(), ProviderID: provider.ID, RevisionID: provider.RevisionID, Model: target.ProviderModel})
		if err != nil {
			return nil, err
		}
		templates[target.ID] = template
	}
	return templates, nil
}

func (s *Snapshot) DurableTemplate(slug, target string) (*DurableTemplate, bool) {
	template, ok := s.durable[slug][target]
	return template, ok
}
func (s *Snapshot) OperationTemplate(slug, target, operation string) (*operationplan.Template, bool) {
	template, ok := s.operations[slug][operation+"/"+target]
	return template, ok
}
