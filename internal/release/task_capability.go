package release

import (
	"context"
	"encoding/hex"
	"errors"
	"slices"
	"time"

	"github.com/tyk-swe/olp/internal/domain/ids"
	"github.com/tyk-swe/olp/internal/domain/problem"
	"github.com/tyk-swe/olp/internal/keyauthority"
	"github.com/tyk-swe/olp/internal/platform/secrets"
)

var ErrTaskAttemptLimit = errors.New("agent task attempt limit reached")

func (m *Manager) taskCapabilities() taskCapabilityDatabase {
	if m.taskStore != nil {
		return m.taskStore
	}
	return m.pool
}

func (m *Manager) AuthenticateTaskCapability(ctx context.Context, token string) (*keyauthority.Authority, error) {
	return m.authenticateTaskCapability(ctx, m.auth.Digest(secrets.TaskCapabilityDigest, token))
}

func (m *Manager) authenticateTaskCapability(ctx context.Context, digest []byte) (*keyauthority.Authority, error) {
	record, err := lookupTaskCapability(ctx, m.taskCapabilities(), digest)
	if err != nil {
		return nil, err
	}
	if err := m.authorizeAgentTask(record.KeyID, record.ProjectID, record.PublicationID, record.RevisionID, record.WorkloadRevision, record.CallerExpires, newAgentWork); err != nil {
		return nil, err
	}
	m.mu.RLock()
	policy, principal, snapshot, principalExpires, ok := m.authority.taskCapabilityPrincipal(record.KeyID, record.WorkloadRevision, record.CallerExpires)
	m.mu.RUnlock()
	if !ok {
		return nil, ErrInvalidKey
	}
	currentPolicy, err := m.taskCapabilityPolicy(ctx, policy, record.Delegation, record.KeyID, record.ProjectID, record.PublicationID, record.Pair, record.DelegationRevision)
	if err != nil {
		return nil, err
	}
	if record.EndUser == "" && (currentPolicy.Policy.EndUserSource != nil || currentPolicy.Policy.EndUserPolicy != nil || currentPolicy.ProjectEndUserPolicy != nil) {
		return nil, ErrInvalidKey
	}
	if !currentPolicy.AllowsEndUser(record.EndUser) {
		return nil, ErrInvalidKey
	}
	boundedExpiry := boundDelegationExpiry(record.Expires, principalExpires)
	capability := &keyauthority.TaskCapability{Digest: hex.EncodeToString(digest), RouteRevisions: slices.Clone(record.Definition.Agent.RouteRevisions), ToolsetRevisions: slices.Clone(record.Definition.Agent.ToolsetRevisions)}
	callback := currentPolicy.TaskAuthority(keyauthority.TaskDerivation{
		Identity:         principal,
		SnapshotID:       snapshot,
		WorkloadRevision: record.WorkloadRevision,
		ExpiresAt:        &boundedExpiry,
		EndUserDigest:    record.EndUser,
		Capability:       capability,
	})
	return &callback, nil
}

func (m *Manager) taskCapabilityPolicy(ctx context.Context, policy keyauthority.Authority, delegation []byte, keyID, project, publication, pair, revision string) (*keyauthority.Authority, error) {
	if len(delegation) == 0 {
		if pair != "" {
			return nil, ErrInvalidKey
		}
		return &policy, nil
	}
	if pair != "" && revision == "" {
		return nil, ErrInvalidKey
	}
	return m.authorizeDelegatedTask(ctx, delegation, keyID, project, publication, pair, revision, newAgentWork)
}

func (state authorityState) taskCapabilityPrincipal(keyID, revision string, expiry *time.Time) (keyauthority.Authority, keyauthority.Authority, string, *time.Time, bool) {
	principal, ok := state.taskPrincipal(keyID)
	if !ok {
		return keyauthority.Authority{}, keyauthority.Authority{}, "", nil, false
	}
	if principal.WorkloadIssuerID == nil {
		return principal, principal, principal.SnapshotID, principal.ExpiresAt, true
	}
	if expiry == nil {
		return keyauthority.Authority{}, keyauthority.Authority{}, "", nil, false
	}
	for _, issuer := range state.workloadIssuers {
		if issuer.ID != *principal.WorkloadIssuerID || !issuer.Config.Enabled || issuer.Revision != revision {
			continue
		}
		base, ok := issuer.bindings[principal.WorkloadMapping]
		if !ok {
			return keyauthority.Authority{}, keyauthority.Authority{}, "", nil, false
		}
		return base, principal, state.id, expiry, true
	}
	return keyauthority.Authority{}, keyauthority.Authority{}, "", nil, false
}

func (m *Manager) AdmitTaskCapabilityAttempt(ctx context.Context, authority keyauthority.Authority, scope, resource, revision string, project *string, accountingID string) error {
	if authority.TaskCapability == nil {
		return ErrInvalidKey
	}
	if !ids.Canonical(accountingID) {
		return ErrInvalidKey
	}
	digest, err := hex.DecodeString(authority.TaskCapability.Digest)
	if err != nil {
		return ErrInvalidKey
	}
	current, err := m.authenticateTaskCapability(ctx, digest)
	if err != nil {
		return err
	}
	if current.ID != authority.ID || project == nil {
		return problem.Forbidden()
	}
	if scope == "inference" && (!current.Allows(scope, resource, project, time.Now()) || !slices.Contains(current.TaskCapability.RouteRevisions, revision)) {
		return problem.Forbidden()
	}
	if scope == "tools" && (!current.AllowsResource(scope, resource, *project, time.Now()) || !slices.Contains(current.TaskCapability.ToolsetRevisions, revision)) {
		return problem.Forbidden()
	}
	if scope != "inference" && scope != "tools" {
		return problem.Forbidden()
	}
	return admitTaskCallback(ctx, m.taskCapabilities(), digest, scope, accountingID)
}
