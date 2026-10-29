package runtime

import (
	"context"
	"encoding/hex"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/agentcontract"
	"github.com/tyk-swe/olp/internal/secrets"
)

var ErrTaskAttemptLimit = errors.New("agent task attempt limit reached")

type taskCapabilityStore interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Begin(context.Context) (pgx.Tx, error)
}

func (m *Manager) taskCapabilityStore() taskCapabilityStore {
	if m.taskStore != nil {
		return m.taskStore
	}
	return m.pool
}

func (m *Manager) AuthenticateTaskCapability(ctx context.Context, token string) (*access.Authority, error) {
	return m.authenticateTaskCapability(ctx, m.auth.Digest(secrets.TaskCapabilityDigest, token))
}

func (m *Manager) authenticateTaskCapability(ctx context.Context, digest []byte) (*access.Authority, error) {
	var keyID, project, publication, revision, workloadRevision, endUser string
	var callerExpires *time.Time
	var expires time.Time
	var delegation []byte
	var definition agentcontract.Draft
	err := m.taskCapabilityStore().QueryRow(ctx, `SELECT d.api_key_id::text,d.project_id::text,c.publication_id::text,c.revision_id::text,COALESCE(c.caller_workload_revision_id::text,''),c.caller_expires_at,c.expires_at,c.end_user_digest,c.delegation_token_digest,v.definition FROM olp.a2a_task_capabilities c JOIN olp.execution_debits d ON d.id=c.dispatch_id JOIN olp.agent_publication_revisions v ON v.id=c.revision_id LEFT JOIN olp.a2a_tasks t ON t.id=c.task_id WHERE c.token_digest=$1 AND c.expires_at>now() AND ((c.task_id IS NULL AND d.state='dispatched' AND EXISTS(SELECT 1 FROM olp.a2a_task_admissions a WHERE a.dispatch_id=c.dispatch_id)) OR (t.state='active' AND t.deadline_at>now()))`, digest).Scan(&keyID, &project, &publication, &revision, &workloadRevision, &callerExpires, &expires, &endUser, &delegation, &definition)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrInvalidKey
	}
	if err != nil {
		return nil, ErrStaleAuthority
	}
	if len(delegation) != 0 || definition.Agent == nil {
		return nil, ErrInvalidKey
	}
	if err := m.AuthorizeAgentTask(keyID, project, publication, revision, workloadRevision, callerExpires); err != nil {
		return nil, err
	}
	m.mu.RLock()
	principal, ok := m.authority.taskPrincipal(keyID)
	if ok && principal.WorkloadIssuerID != nil {
		principal, ok = m.authority.taskWorkloadPrincipal(principal, workloadRevision, callerExpires)
	}
	m.mu.RUnlock()
	if !ok {
		return nil, ErrInvalidKey
	}
	expires = boundDelegationExpiry(expires, principal.ExpiresAt)
	principal.ExpiresAt = &expires
	if endUser == "" && (principal.Policy.EndUserSource != nil || principal.Policy.EndUserPolicy != nil || principal.ProjectEndUserPolicy != nil) {
		return nil, ErrInvalidKey
	}
	principal.EndUserDigest = endUser
	if !principal.AllowsEndUser(endUser) {
		return nil, ErrInvalidKey
	}
	principal.TaskCapability = &access.TaskCapability{Digest: hex.EncodeToString(digest), RouteRevisions: definition.Agent.RouteRevisions, ToolsetRevisions: definition.Agent.ToolsetRevisions}
	return &principal, nil
}

func (state authorityState) taskWorkloadPrincipal(principal access.Authority, revision string, expiry *time.Time) (access.Authority, bool) {
	if principal.WorkloadIssuerID == nil || expiry == nil {
		return access.Authority{}, false
	}
	for _, issuer := range state.workloadIssuers {
		if issuer.ID != *principal.WorkloadIssuerID || !issuer.Config.Enabled || issuer.Revision != revision {
			continue
		}
		base, ok := issuer.bindings[principal.WorkloadMapping]
		if !ok {
			return access.Authority{}, false
		}
		base.ID = principal.ID
		base.LookupID = principal.LookupID
		base.LimitLookupID = principal.LimitLookupID
		base.RevokedAt = principal.RevokedAt
		base.WorkloadIssuerID = principal.WorkloadIssuerID
		base.WorkloadDigest = principal.WorkloadDigest
		base.WorkloadMapping = principal.WorkloadMapping
		base.WorkloadRevision = revision
		base.SnapshotID = state.id
		base.ExpiresAt = expiry
		return base, true
	}
	return access.Authority{}, false
}

func (m *Manager) AdmitTaskCapabilityAttempt(ctx context.Context, authority access.Authority, scope, resource, revision string, project *string) error {
	if authority.TaskCapability == nil {
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
	tx, err := m.taskCapabilityStore().Begin(ctx)
	if err != nil {
		return ErrStaleAuthority
	}
	defer tx.Rollback(ctx)
	var dispatch, task string
	var definition agentcontract.Draft
	err = tx.QueryRow(ctx, `SELECT c.dispatch_id::text,COALESCE(c.task_id::text,''),v.definition FROM olp.a2a_task_capabilities c JOIN olp.agent_publication_revisions v ON v.id=c.revision_id JOIN olp.execution_debits d ON d.id=c.dispatch_id LEFT JOIN olp.a2a_tasks t ON t.id=c.task_id WHERE c.token_digest=$1 AND c.expires_at>now() AND ((c.task_id IS NULL AND d.state='dispatched' AND EXISTS(SELECT 1 FROM olp.a2a_task_admissions a WHERE a.dispatch_id=c.dispatch_id)) OR (t.state='active' AND t.deadline_at>now())) FOR UPDATE OF c`, digest).Scan(&dispatch, &task, &definition)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrInvalidKey
	}
	if err != nil {
		return ErrStaleAuthority
	}
	if definition.Agent == nil {
		return ErrInvalidKey
	}
	limit := 32
	if definition.Agent.MaxTaskContinuations != nil {
		limit = *definition.Agent.MaxTaskContinuations
	}
	var count int
	if task == "" {
		err = tx.QueryRow(ctx, `UPDATE olp.a2a_task_admissions SET callback_attempts=callback_attempts+1 WHERE dispatch_id=$1 AND callback_attempts<$2 RETURNING callback_attempts`, dispatch, limit).Scan(&count)
	} else {
		err = tx.QueryRow(ctx, `UPDATE olp.a2a_tasks SET continuations=continuations+1 WHERE id=$1 AND state='active' AND deadline_at>now() AND continuations<$2 RETURNING continuations`, task, limit).Scan(&count)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrTaskAttemptLimit
	}
	if err != nil || tx.Commit(ctx) != nil {
		return ErrStaleAuthority
	}
	return nil
}
