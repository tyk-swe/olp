package runtime

import (
	"fmt"

	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/interaction"
)

// CompileRouteExecution validates the complete configured links of a route.
// Request values are bound later; no provider request or discovery runs here.
func (s *Snapshot) CompileRouteExecution(route Route) error {
	_, _, err := s.compileRouteExecution(route)
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
	for _, operation := range route.Operations {
		if operation != "generation" {
			return nil, nil, &interaction.Error{Code: "target_capability", Field: "operations", Requirement: "operation_contract", Message: "This operation does not yet have a registered strict interaction runner."}
		}
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
