package runtime

import (
	"time"

	"github.com/tyk-swe/olp/internal/access"
)

func (m *Manager) AuthorizeAgentTask(keyID, projectID, publicationID, revisionID, workloadRevision string, callerExpiresAt *time.Time) error {
	now := time.Now()
	m.mu.RLock()
	defer m.mu.RUnlock()
	if !m.authority.loaded || now.Sub(m.authority.readAt) > AuthorityStaleAfter {
		return ErrStaleAuthority
	}
	principal, ok := m.authority.agentPrincipals[keyID]
	if !ok || principal.ID != keyID || principal.ProjectID == nil || *principal.ProjectID != projectID || principal.RevokedAt != nil || principal.ExpiresAt != nil && !principal.ExpiresAt.After(now) || callerExpiresAt != nil && !callerExpiresAt.After(now) {
		return access.Forbidden()
	}
	if principal.WorkloadIssuerID != nil {
		if callerExpiresAt == nil || workloadRevision == "" {
			return access.Forbidden()
		}
		return m.authority.authorizeWorkloadAgentTask(principal, projectID, publicationID, revisionID, workloadRevision, now)
	}
	if !principal.AllowsResource("agents", publicationID, projectID, now) {
		return access.Forbidden()
	}
	return m.authority.authorizeTaskPublication(projectID, publicationID, revisionID)
}

func (state authorityState) authorizeWorkloadAgentTask(principal access.Authority, projectID, publicationID, revisionID, workloadRevision string, now time.Time) error {
	for _, issuer := range state.workloadIssuers {
		if issuer.ID != *principal.WorkloadIssuerID {
			continue
		}
		binding, ok := issuer.bindings[principal.WorkloadMapping]
		if !issuer.Config.Enabled || issuer.Revision != workloadRevision || !ok || !binding.AllowsResource("agents", publicationID, projectID, now) {
			return access.Forbidden()
		}
		return state.authorizeTaskPublication(projectID, publicationID, revisionID)
	}
	return access.Forbidden()
}

func (state authorityState) authorizeTaskPublication(projectID, publicationID, revisionID string) error {
	resource, ok := state.agentResources[publicationID]
	if !ok || resource.Kind != "agent" || resource.ProjectID != projectID || !resource.Revisions[revisionID] {
		return access.Forbidden()
	}
	return nil
}
