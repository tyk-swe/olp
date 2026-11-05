package release

import (
	"context"
	"encoding/hex"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/agents/tasks"
	"github.com/tyk-swe/olp/internal/secrets"
)

var ErrTaskAttemptLimit = errors.New("agent task attempt limit reached")

type taskCapabilityStore interface {
	Lookup(context.Context, []byte) (tasks.Capability, error)
	AdmitCallback(context.Context, []byte, string, string) error
}

func (m *Manager) taskCapabilities() taskCapabilityStore {
	if m.taskStore != nil {
		return m.taskStore
	}
	return tasks.NewCapabilities(m.pool)
}
func taskCapabilityError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, tasks.ErrCapabilityInvalid):
		return ErrInvalidKey
	case errors.Is(err, tasks.ErrCallbackLimit):
		return ErrTaskAttemptLimit
	default:
		return ErrStaleAuthority
	}
}

func (m *Manager) AuthenticateTaskCapability(ctx context.Context, token string) (*access.Authority, error) {
	return m.authenticateTaskCapability(ctx, m.auth.Digest(secrets.TaskCapabilityDigest, token))
}

func (m *Manager) authenticateTaskCapability(ctx context.Context, digest []byte) (*access.Authority, error) {
	record, lookupErr := m.taskCapabilities().Lookup(ctx, digest)
	if lookupErr != nil {
		return nil, taskCapabilityError(lookupErr)
	}
	if err := m.AuthorizeAgentTask(record.KeyID, record.ProjectID, record.PublicationID, record.RevisionID, record.WorkloadRevision, record.CallerExpires); err != nil {
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
	capability := &access.TaskCapability{Digest: hex.EncodeToString(digest), RouteRevisions: slices.Clone(record.Definition.Agent.RouteRevisions), ToolsetRevisions: slices.Clone(record.Definition.Agent.ToolsetRevisions)}
	callback := currentPolicy.TaskAuthority(principal, snapshot, record.WorkloadRevision, &boundedExpiry, record.EndUser, capability)
	return &callback, nil
}

func (m *Manager) taskCapabilityPolicy(ctx context.Context, policy access.Authority, delegation []byte, keyID, project, publication, pair, revision string) (*access.Authority, error) {
	if len(delegation) == 0 {
		if pair != "" {
			return nil, ErrInvalidKey
		}
		return &policy, nil
	}
	if pair != "" && revision == "" {
		return nil, ErrInvalidKey
	}
	return m.authorizeDelegatedTask(ctx, delegation, keyID, project, publication, pair, revision)
}

func (state authorityState) taskCapabilityPrincipal(keyID, revision string, expiry *time.Time) (access.Authority, access.Authority, string, *time.Time, bool) {
	principal, ok := state.taskPrincipal(keyID)
	if !ok {
		return access.Authority{}, access.Authority{}, "", nil, false
	}
	if principal.WorkloadIssuerID == nil {
		return principal, principal, principal.SnapshotID, principal.ExpiresAt, true
	}
	if expiry == nil {
		return access.Authority{}, access.Authority{}, "", nil, false
	}
	for _, issuer := range state.workloadIssuers {
		if issuer.ID != *principal.WorkloadIssuerID || !issuer.Config.Enabled || issuer.Revision != revision {
			continue
		}
		base, ok := issuer.bindings[principal.WorkloadMapping]
		if !ok {
			return access.Authority{}, access.Authority{}, "", nil, false
		}
		return base, principal, state.id, expiry, true
	}
	return access.Authority{}, access.Authority{}, "", nil, false
}

func (m *Manager) AdmitTaskCapabilityAttempt(ctx context.Context, authority access.Authority, scope, resource, revision string, project *string, accountingID string) error {
	if authority.TaskCapability == nil {
		return ErrInvalidKey
	}
	id, err := uuid.Parse(accountingID)
	if err != nil || id == uuid.Nil || id.String() != accountingID {
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
		return access.Forbidden()
	}
	if scope == "inference" && (!current.Allows(scope, resource, project, time.Now()) || !slices.Contains(current.TaskCapability.RouteRevisions, revision)) {
		return access.Forbidden()
	}
	if scope == "tools" && (!current.AllowsResource(scope, resource, *project, time.Now()) || !slices.Contains(current.TaskCapability.ToolsetRevisions, revision)) {
		return access.Forbidden()
	}
	if scope != "inference" && scope != "tools" {
		return access.Forbidden()
	}
	return taskCapabilityError(m.taskCapabilities().AdmitCallback(ctx, digest, scope, accountingID))
}
