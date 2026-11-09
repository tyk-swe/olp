package routing

import (
	"fmt"
	"maps"
	"slices"

	"github.com/tyk-swe/olp/internal/guardrails"
)

func (s *Snapshot) compileGuardrails() error {
	if s.Guardrails == nil {
		return nil
	}
	routes := map[string]*guardrails.Engine{}
	for slug, route := range s.Routes {
		engines, err := s.Guardrails.Engines(guardrails.Boundary{ProjectID: guardrails.ProjectOf(route.ProjectID), RouteID: route.ID})
		if err != nil {
			return err
		}
		for _, key := range slices.Sorted(maps.Keys(engines)) {
			e := engines[key]
			if err = e.ValidateRoute(route.Fidelity.Strict(), route.OverallTimeout); err != nil {
				return fmt.Errorf("route %q guardrails: %w", slug, err)
			}
			if route.IsPassthrough() {
				return &Refusal{Code: "passthrough_conflict", Message: "A pass-through route cannot carry guardrails."}
			}
			routes[slug+"\x00"+key] = e
		}
	}
	toolsets := map[string]string{}
	for _, revision := range s.AgentPublications {
		if revision.Kind == "toolset" {
			toolsets[revision.PublicationID] = revision.ProjectID
		}
	}
	tools := map[string]*guardrails.Engine{}
	for toolset, project := range toolsets {
		engines, err := s.Guardrails.Engines(guardrails.Boundary{ProjectID: project, ToolsetID: toolset})
		if err != nil {
			return fmt.Errorf("toolset %q guardrails: %w", toolset, err)
		}
		for key, e := range engines {
			tools[toolset+"\x00"+key] = e
		}
	}
	s.guardrailEngines, s.toolGuardrailEngines = routes, tools
	return nil
}

func (s *Snapshot) GuardrailEngine(slug, key string) *guardrails.Engine {
	if s == nil || s.Guardrails == nil {
		return nil
	}
	if e := s.guardrailEngines[slug+"\x00"+key]; e != nil {
		return e
	}
	return s.guardrailEngines[slug+"\x00"]
}

func (s *Snapshot) ToolGuardrailEngine(toolsetID, key string) *guardrails.Engine {
	if s == nil || s.Guardrails == nil {
		return nil
	}
	if e := s.toolGuardrailEngines[toolsetID+"\x00"+key]; e != nil {
		return e
	}
	return s.toolGuardrailEngines[toolsetID+"\x00"]
}
