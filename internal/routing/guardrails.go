package routing

import (
	"fmt"

	"github.com/tyk-swe/olp/internal/guardrails"
)

func (s *Snapshot) compileGuardrails() error {
	if s.Guardrails == nil {
		return nil
	}
	s.guardrailEngines = map[string]*guardrails.Engine{}
	for slug, route := range s.Routes {
		project := "installation"
		if route.ProjectID != nil {
			project = *route.ProjectID
		}
		boundary := guardrails.Boundary{RouteID: route.ID, ProjectID: project, OrganizationID: s.Guardrails.ProjectOrganizations[project]}
		keys := []string{""}
		for _, a := range s.Guardrails.Attachments {
			if a.Scope == "api-key" && s.Guardrails.Policies[a.PolicyRevisionID].ProjectID == project {
				keys = append(keys, a.ScopeID)
			}
		}
		for _, key := range keys {
			boundary.KeyID = key
			e, err := s.Guardrails.Compile(boundary, nil)
			if err != nil {
				return err
			}
			if err = e.ValidateRoute(route.Fidelity.Strict(), route.OverallTimeout); err != nil {
				return fmt.Errorf("route %q guardrails: %w", slug, err)
			}
			if e != nil && route.IsPassthrough() {
				return &Refusal{Code: "passthrough_conflict", Message: "A pass-through route cannot carry guardrails."}
			}
			if e != nil {
				s.guardrailEngines[slug+"\x00"+key] = e
			}
		}
	}
	return s.compileToolGuardrails()
}

func (s *Snapshot) compileToolGuardrails() error {
	s.toolGuardrailEngines = map[string]*guardrails.Engine{}
	projects := map[string]string{}
	for _, revision := range s.AgentPublications {
		if revision.Kind == "toolset" {
			projects[revision.PublicationID] = revision.ProjectID
		}
	}
	for toolsetID, project := range projects {
		boundary := guardrails.Boundary{ToolsetID: toolsetID, ProjectID: project, OrganizationID: s.Guardrails.ProjectOrganizations[project]}
		keys := []string{""}
		for _, a := range s.Guardrails.Attachments {
			if a.Scope == "api-key" && s.Guardrails.Policies[a.PolicyRevisionID].ProjectID == project {
				keys = append(keys, a.ScopeID)
			}
		}
		for _, key := range keys {
			boundary.KeyID = key
			e, err := s.Guardrails.Compile(boundary, nil)
			if err != nil {
				return fmt.Errorf("toolset %q guardrails: %w", toolsetID, err)
			}
			if e != nil {
				s.toolGuardrailEngines[toolsetID+"\x00"+key] = e
			}
		}
	}
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
