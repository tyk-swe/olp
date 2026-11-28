package release

import (
	"cmp"
	"net/http"
	"slices"
	"time"

	"github.com/tyk-swe/olp/internal/domain/problem"
	"github.com/tyk-swe/olp/internal/keyauthority"
)

type delegationOption struct {
	ID              string   `json:"id"`
	AllowedRoutes   []string `json:"allowed_routes"`
	AllowedToolsets []string `json:"allowed_toolsets"`
	AllowedAgents   []string `json:"allowed_agents"`
}

func (m *Manager) memberConsentOptions(authorityID, projectID, keyID string, resources delegatableResources) ([]delegationOption, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	state := m.authority
	if err := state.consentable(authorityID); err != nil {
		return nil, err
	}
	key, ok := state.agentPrincipals[keyID]
	if !ok || key.WorkloadIssuerID != nil || key.ProjectID == nil || *key.ProjectID != projectID || !slices.Contains(key.Policy.Scopes, "delegate") {
		return nil, problem.Forbidden()
	}
	now := time.Now()
	options := slices.SortedFunc(func(yield func(delegationOption) bool) {
		for _, registered := range state.workloadPrincipals {
			if registered.ProjectID == nil || *registered.ProjectID != projectID {
				continue
			}
			agent, _, ok := state.memberConsentAgent(registered)
			if !ok {
				continue
			}
			routes := permitted(resources.routes, func(route string) bool {
				return delegationResourcesAllowed(agent, key, []string{route}, nil, nil, now)
			})
			toolsets := permitted(resources.toolsets, func(toolset string) bool {
				return delegationResourcesAllowed(agent, key, nil, []string{toolset}, nil, now)
			})
			agents := permitted(resources.agents, func(target string) bool {
				return delegationResourcesAllowed(agent, key, nil, nil, []string{target}, now)
			})
			if (len(routes) != 0 || len(toolsets) != 0 || len(agents) != 0) && !yield(delegationOption{ID: registered.ID, AllowedRoutes: routes, AllowedToolsets: toolsets, AllowedAgents: agents}) {
				return
			}
		}
	}, func(left, right delegationOption) int { return cmp.Compare(left.ID, right.ID) })
	return append([]delegationOption{}, options...), nil
}

func permitted(resources []string, allows func(string) bool) []string {
	return slices.AppendSeq([]string{}, func(yield func(string) bool) {
		for _, resource := range resources {
			if allows(resource) && !yield(resource) {
				return
			}
		}
	})
}

func consentScopes(scopes []string) []string {
	if len(scopes) == 0 {
		return []string{"inference"}
	}
	return scopes
}

type memberConsentTarget struct {
	agent    keyauthority.Authority
	key      keyauthority.Authority
	revision string
}

func (m *Manager) memberConsentTarget(authorityID, agentID, keyID string) (memberConsentTarget, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	state := m.authority
	if err := state.consentable(authorityID); err != nil {
		return memberConsentTarget{}, err
	}
	key, ok := state.agentPrincipals[keyID]
	if !ok || key.WorkloadIssuerID != nil {
		return memberConsentTarget{}, problem.NotFound
	}
	for _, principal := range state.workloadPrincipals {
		if principal.ID != agentID {
			continue
		}
		agent, revision, exists := state.memberConsentAgent(principal)
		if exists {
			return memberConsentTarget{agent: agent, key: key, revision: revision}, nil
		}
	}
	return memberConsentTarget{}, problem.NotFound
}

func (state authorityState) consentable(authorityID string) error {
	if !state.loaded || state.id != authorityID || time.Since(state.readAt) > AuthorityStaleAfter {
		return problem.Fail(http.StatusServiceUnavailable, "authority_unavailable", "Delegation is unavailable.")
	}
	return nil
}

func (state authorityState) memberConsentAgent(principal keyauthority.Authority) (keyauthority.Authority, string, bool) {
	if principal.WorkloadIssuerID == nil || principal.RevokedAt != nil || principal.ProjectID == nil {
		return keyauthority.Authority{}, "", false
	}
	for _, issuer := range state.workloadIssuers {
		if issuer.ID != *principal.WorkloadIssuerID || !issuer.Config.Enabled {
			continue
		}
		for _, mapping := range issuer.Config.Mappings {
			if mapping.Name != principal.WorkloadMapping || !mapping.AgentPrincipal || mapping.ProjectID != *principal.ProjectID {
				continue
			}
			agent, exists := issuer.bindings[mapping.Name]
			if exists {
				agent.PausedAt = principal.PausedAt
				return agent, issuer.Revision, true
			}
		}
	}
	return keyauthority.Authority{}, "", false
}
