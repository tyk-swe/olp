package runtime

import (
	"fmt"
	"strconv"

	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/interaction"
	"github.com/tyk-swe/olp/internal/operationplan"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"slices"
)

// CompileRouteExecution validates the complete configured links of a route.
// Request values are bound later; no provider request or discovery runs here.
func (s *Snapshot) CompileRouteExecution(route Route) error {
	_, _, err := s.compileRouteExecution(route)
	if err == nil {
		_, err = s.compileOperations(route)
	}
	return err
}

func (s *Snapshot) compileRouteExecution(route Route) (map[string]*interaction.Template, *contentpolicy.Compiled, error) {
	if err := ValidateRouteFidelity(route.Fidelity, route.ContentPolicy); err != nil {
		return nil, nil, err
	}
	policy, err := contentpolicy.Compile(route.ContentPolicy)
	if err != nil {
		return nil, nil, err
	}
	if FidelityMode(route.Fidelity) != FidelityStrict {
		return nil, policy, nil
	}
	if !slices.Contains(route.Operations, "generation") {
		return nil, policy, nil
	}
	templates := make(map[string]*interaction.Template, len(route.Targets))
	for _, target := range route.Targets {
		provider, ok := s.Providers[target.ProviderID]
		if !ok {
			return nil, nil, fmt.Errorf("route target references an unavailable provider")
		}
		template, err := interaction.Compile(interaction.Config{Provider: provider.Connector(), ProviderID: provider.ID, RevisionID: provider.RevisionID, Model: target.ProviderModel, Policy: route.ContentPolicy})
		if err != nil {
			return nil, nil, err
		}
		templates[target.ID] = template
	}
	return templates, policy, nil
}

// InteractionTemplate is compiled with the pinned release, never cached by a
// caller-controlled prompt or schema. Its cardinality is bounded by route targets.
func (s *Snapshot) InteractionTemplate(slug, target string) (*interaction.Template, bool) {
	template, ok := s.interactions[slug][target]
	return template, ok
}

func (r *Route) CompiledContentPolicy() *contentpolicy.Compiled { return r.compiledContentPolicy }

// EffectiveOutputLimit reads the dialect-owned bound after omission defaults.
// It does not assert equivalence between reasoning and generation token scopes.
func EffectiveOutputLimit(request *openai.Request) *int64 {
	paths := []string{"/max_completion_tokens", "/max_tokens"}
	switch request.Family {
	case openai.FamilyResponses:
		paths = []string{"/max_output_tokens"}
	case openai.FamilyBedrock:
		paths = []string{"/inferenceConfig/maxTokens"}
	case openai.FamilyGemini, openai.FamilyGeminiStream:
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
	if FidelityMode(route.Fidelity) != FidelityStrict {
		return nil, nil
	}
	templates := map[string]*operationplan.Template{}
	for _, op := range route.Operations {
		if op == "generation" {
			continue
		}
		for _, target := range route.Targets {
			provider, ok := s.Providers[target.ProviderID]
			if !ok {
				return nil, fmt.Errorf("route target references an unavailable provider")
			}
			template, err := operationplan.Compile(operationplan.Config{Provider: provider.Connector(), ProviderID: provider.ID, RevisionID: provider.RevisionID, Model: target.ProviderModel, Operation: op, Policy: route.ContentPolicy})
			if err != nil {
				return nil, err
			}
			templates[op+"/"+target.ID] = template
		}
	}
	return templates, nil
}
func (s *Snapshot) OperationTemplate(slug, target, operation string) (*operationplan.Template, bool) {
	template, ok := s.operations[slug][operation+"/"+target]
	return template, ok
}
