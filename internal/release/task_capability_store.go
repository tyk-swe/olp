package release

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/domain/agentcontract"
)

type taskCapabilityDatabase interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Begin(context.Context) (pgx.Tx, error)
}

type TaskCallbackCounter func(context.Context, pgx.Tx, agentcontract.CallbackAttempt) (bool, error)

type taskCapabilityRecord struct {
	KeyID, ProjectID, PublicationID, RevisionID, WorkloadRevision, EndUser, Pair, DelegationRevision string
	CallerExpires                                                                                    *time.Time
	Expires                                                                                          time.Time
	Delegation                                                                                       []byte
	Definition                                                                                       agentcontract.Draft
}

func lookupTaskCapability(ctx context.Context, db taskCapabilityDatabase, digest []byte) (taskCapabilityRecord, error) {
	var record taskCapabilityRecord
	err := db.QueryRow(ctx, `SELECT COALESCE(t.api_key_id,d.api_key_id)::text,d.project_id::text,c.publication_id::text,c.revision_id::text,COALESCE(CASE WHEN t.id IS NULL THEN c.caller_workload_revision_id ELSE t.caller_workload_revision_id END::text,''),CASE WHEN t.id IS NULL OR t.delegation_pair_digest<>'' THEN c.caller_expires_at ELSE t.caller_expires_at END,c.expires_at,CASE WHEN t.id IS NULL THEN c.end_user_digest ELSE original.end_user_digest END,c.delegation_token_digest,v.definition,COALESCE(t.delegation_pair_digest,''),COALESCE(original_delegation.issuer_revision::text,'') FROM olp.a2a_task_capabilities c JOIN olp.execution_debits d ON d.id=c.dispatch_id JOIN olp.agent_publication_revisions v ON v.id=c.revision_id LEFT JOIN olp.a2a_tasks t ON t.id=c.task_id LEFT JOIN olp.a2a_task_capabilities original ON original.dispatch_id=t.dispatch_id LEFT JOIN olp.agent_delegations original_delegation ON original_delegation.token_digest=decode(t.delegation_token_digest,'hex') WHERE c.token_digest=$1 AND c.expires_at>now() AND (t.id IS NULL OR original.dispatch_id IS NOT NULL) AND ((c.task_id IS NULL AND d.state='dispatched' AND EXISTS(SELECT 1 FROM olp.a2a_task_admissions a WHERE a.dispatch_id=c.dispatch_id)) OR (t.state='active' AND t.deadline_at>now()))`, digest).Scan(&record.KeyID, &record.ProjectID, &record.PublicationID, &record.RevisionID, &record.WorkloadRevision, &record.CallerExpires, &record.Expires, &record.EndUser, &record.Delegation, &record.Definition, &record.Pair, &record.DelegationRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return taskCapabilityRecord{}, ErrInvalidKey
	}
	if err != nil {
		return taskCapabilityRecord{}, ErrStaleAuthority
	}
	if record.Definition.Agent == nil {
		return taskCapabilityRecord{}, ErrInvalidKey
	}
	return record, nil
}

func admitTaskCallback(ctx context.Context, db taskCapabilityDatabase, count TaskCallbackCounter, digest []byte, scope, accountingID string) error {
	tx, err := db.Begin(ctx)
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
	counted, err := count(ctx, tx, agentcontract.CallbackAttempt{DispatchID: dispatch, TaskID: task, Kind: scope, AccountingID: accountingID, Limit: limit})
	if err != nil {
		return ErrStaleAuthority
	}
	if !counted {
		return ErrTaskAttemptLimit
	}
	if tx.Commit(ctx) != nil {
		return ErrStaleAuthority
	}
	return nil
}
