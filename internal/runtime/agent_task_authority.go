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
	principal, ok := m.authority.taskPrincipal(keyID)
	if !ok || principal.ID != keyID || principal.ProjectID == nil || *principal.ProjectID != projectID || principal.RevokedAt != nil || principal.ExpiresAt != nil && !principal.ExpiresAt.After(now) || callerExpiresAt != nil && !callerExpiresAt.After(now) {
		return access.Forbidden()
	}
	if principal.WorkloadIssuerID != nil && (principal.WorkloadDigest == "" || callerExpiresAt == nil || workloadRevision == "") {
		return access.Forbidden()
	}
	identity := access.Authority{
		ID:               principal.ID,
		WorkloadDigest:   principal.WorkloadDigest,
		WorkloadIssuerID: principal.WorkloadIssuerID,
		WorkloadMapping:  principal.WorkloadMapping,
		WorkloadRevision: workloadRevision,
	}
	if !m.authority.currentAgentPrincipal(identity, "agents", publicationID, projectID, now) {
		return access.Forbidden()
	}
	return m.authority.authorizeTaskPublication(projectID, publicationID, revisionID)
}

func (state authorityState) taskPrincipal(keyID string) (access.Authority, bool) {
	if principal, ok := state.agentPrincipals[keyID]; ok {
		return principal, true
	}
	for _, principal := range state.workloadPrincipals {
		if principal.ID == keyID {
			return principal, true
		}
	}
	return access.Authority{}, false
}

func (state authorityState) authorizeTaskPublication(projectID, publicationID, revisionID string) error {
	resource, ok := state.agentResources[publicationID]
	if !ok || resource.Kind != "agent" || resource.ProjectID != projectID || !resource.Revisions[revisionID] {
		return access.Forbidden()
	}
	return nil
}
