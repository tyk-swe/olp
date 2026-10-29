package runtime

import (
	"context"
	"encoding/hex"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"
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
	if definition.Agent == nil {
		return nil, ErrInvalidKey
	}
	if err := m.AuthorizeAgentTask(keyID, project, publication, revision, workloadRevision, callerExpires); err != nil {
		return nil, err
	}
	m.mu.RLock()
	policy, principal, snapshot, principalExpires, ok := m.authority.taskCapabilityPrincipal(keyID, workloadRevision, callerExpires)
	m.mu.RUnlock()
	if !ok {
		return nil, ErrInvalidKey
	}
	currentPolicy, err := m.taskCapabilityPolicy(ctx, policy, delegation, keyID, project, publication)
	if err != nil {
		return nil, err
	}
	if endUser == "" && (currentPolicy.Policy.EndUserSource != nil || currentPolicy.Policy.EndUserPolicy != nil || currentPolicy.ProjectEndUserPolicy != nil) {
		return nil, ErrInvalidKey
	}
	if !currentPolicy.AllowsEndUser(endUser) {
		return nil, ErrInvalidKey
	}
	boundedExpiry := boundDelegationExpiry(expires, principalExpires)
	capability := &access.TaskCapability{Digest: hex.EncodeToString(digest), RouteRevisions: slices.Clone(definition.Agent.RouteRevisions), ToolsetRevisions: slices.Clone(definition.Agent.ToolsetRevisions)}
	callback := currentPolicy.TaskAuthority(principal, snapshot, workloadRevision, &boundedExpiry, endUser, capability)
	return &callback, nil
}

func (m *Manager) taskCapabilityPolicy(ctx context.Context, policy access.Authority, delegation []byte, keyID, project, publication string) (*access.Authority, error) {
	if len(delegation) == 0 {
		return &policy, nil
	}
	return m.authorizeDelegatedTask(ctx, delegation, keyID, project, publication)
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
		err = tx.QueryRow(ctx, `UPDATE olp.a2a_tasks SET continuations=continuations+1,callback_attempts=callback_attempts+1 WHERE id=$1 AND state='active' AND deadline_at>now() AND continuations<$2 RETURNING continuations`, task, limit).Scan(&count)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrTaskAttemptLimit
	}
	if err != nil {
		return ErrStaleAuthority
	}
	if _, err = tx.Exec(ctx, `INSERT INTO olp.a2a_task_callback_accounting(dispatch_id,kind,accounting_id) VALUES($1,$2,$3) ON CONFLICT(dispatch_id,kind,accounting_id) DO NOTHING`, dispatch, scope, accountingID); err != nil {
		return ErrStaleAuthority
	}
	if tx.Commit(ctx) != nil {
		return ErrStaleAuthority
	}
	return nil
}
