package release

import (
	"time"

	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/keyauthority"
)

type agentWorkAdmission uint8

const (
	newAgentWork agentWorkAdmission = iota
	admittedAgentWork
)

func (admission agentWorkAdmission) authority(a keyauthority.Authority) keyauthority.Authority {
	if admission == newAgentWork {
		return a
	}
	current := a
	current.PausedAt = nil
	return current
}

func (m *Manager) AuthorizeAgentTask(keyID, projectID, publicationID, revisionID, workloadRevision string, callerExpiresAt *time.Time) error {
	return m.authorizeAgentTask(keyID, projectID, publicationID, revisionID, workloadRevision, callerExpiresAt, admittedAgentWork)
}

func (m *Manager) authorizeAgentTask(keyID, projectID, publicationID, revisionID, workloadRevision string, callerExpiresAt *time.Time, admission agentWorkAdmission) error {
	now := time.Now()
	m.mu.RLock()
	defer m.mu.RUnlock()
	if !m.authority.loaded || now.Sub(m.authority.readAt) > AuthorityStaleAfter {
		return ErrStaleAuthority
	}
	principal, ok := m.authority.taskPrincipal(keyID)
	if !ok || principal.ID != keyID || principal.ProjectID == nil || *principal.ProjectID != projectID || principal.RevokedAt != nil || principal.ExpiresAt != nil && !principal.ExpiresAt.After(now) || callerExpiresAt != nil && !callerExpiresAt.After(now) {
		return httpkit.Forbidden()
	}
	if principal.WorkloadIssuerID != nil && (principal.WorkloadDigest == "" || callerExpiresAt == nil || workloadRevision == "") {
		return httpkit.Forbidden()
	}
	identity := keyauthority.Authority{
		ID:               principal.ID,
		WorkloadDigest:   principal.WorkloadDigest,
		WorkloadIssuerID: principal.WorkloadIssuerID,
		WorkloadMapping:  principal.WorkloadMapping,
		WorkloadRevision: workloadRevision,
	}
	if !m.authority.currentAgentPrincipal(identity, "agents", publicationID, projectID, now, admission) {
		return httpkit.Forbidden()
	}
	return m.authority.authorizeTaskPublication(projectID, publicationID, revisionID)
}

func (state authorityState) taskPrincipal(keyID string) (keyauthority.Authority, bool) {
	if principal, ok := state.agentPrincipals[keyID]; ok {
		return principal, true
	}
	for _, principal := range state.workloadPrincipals {
		if principal.ID == keyID {
			return principal, true
		}
	}
	return keyauthority.Authority{}, false
}

func (state authorityState) authorizeTaskPublication(projectID, publicationID, revisionID string) error {
	resource, ok := state.agentResources[publicationID]
	if !ok || resource.Kind != "agent" || resource.ProjectID != projectID || !resource.Revisions[revisionID] {
		return httpkit.Forbidden()
	}
	return nil
}
