package tasks

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/agentcontract"
)

var (
	ErrCapabilityInvalid     = errors.New("task capability unavailable")
	ErrCapabilityUnavailable = errors.New("task persistence unavailable")
	ErrCallbackLimit         = errors.New("agent task attempt limit reached")
)

type CapabilityDatabase interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Begin(context.Context) (pgx.Tx, error)
}
type Capabilities struct{ db CapabilityDatabase }

func NewCapabilities(db CapabilityDatabase) *Capabilities { return &Capabilities{db: db} }

type Capability struct {
	KeyID, ProjectID, PublicationID, RevisionID, WorkloadRevision, EndUser, Pair, DelegationRevision string
	CallerExpires                                                                                    *time.Time
	Expires                                                                                          time.Time
	Delegation                                                                                       []byte
	Definition                                                                                       agentcontract.Draft
}

func (s *Capabilities) Lookup(ctx context.Context, digest []byte) (Capability, error) {
	var record Capability
	err := s.db.QueryRow(ctx, `SELECT COALESCE(t.api_key_id,d.api_key_id)::text,d.project_id::text,c.publication_id::text,c.revision_id::text,COALESCE(CASE WHEN t.id IS NULL THEN c.caller_workload_revision_id ELSE t.caller_workload_revision_id END::text,''),CASE WHEN t.id IS NULL OR t.delegation_pair_digest<>'' THEN c.caller_expires_at ELSE t.caller_expires_at END,c.expires_at,CASE WHEN t.id IS NULL THEN c.end_user_digest ELSE original.end_user_digest END,c.delegation_token_digest,v.definition,COALESCE(t.delegation_pair_digest,''),COALESCE(original_delegation.issuer_revision::text,'') FROM olp.a2a_task_capabilities c JOIN olp.execution_debits d ON d.id=c.dispatch_id JOIN olp.agent_publication_revisions v ON v.id=c.revision_id LEFT JOIN olp.a2a_tasks t ON t.id=c.task_id LEFT JOIN olp.a2a_task_capabilities original ON original.dispatch_id=t.dispatch_id LEFT JOIN olp.agent_delegations original_delegation ON original_delegation.token_digest=decode(t.delegation_token_digest,'hex') WHERE c.token_digest=$1 AND c.expires_at>now() AND (t.id IS NULL OR original.dispatch_id IS NOT NULL) AND ((c.task_id IS NULL AND d.state='dispatched' AND EXISTS(SELECT 1 FROM olp.a2a_task_admissions a WHERE a.dispatch_id=c.dispatch_id)) OR (t.state='active' AND t.deadline_at>now()))`, digest).Scan(&record.KeyID, &record.ProjectID, &record.PublicationID, &record.RevisionID, &record.WorkloadRevision, &record.CallerExpires, &record.Expires, &record.EndUser, &record.Delegation, &record.Definition, &record.Pair, &record.DelegationRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return Capability{}, ErrCapabilityInvalid
	}
	if err != nil {
		return Capability{}, ErrCapabilityUnavailable
	}
	if record.Definition.Agent == nil {
		return Capability{}, ErrCapabilityInvalid
	}
	return record, nil
}
func (s *Capabilities) AdmitCallback(ctx context.Context, digest []byte, scope, accountingID string) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return ErrCapabilityUnavailable
	}
	defer tx.Rollback(ctx)
	var dispatch, task string
	var definition agentcontract.Draft
	err = tx.QueryRow(ctx, `SELECT c.dispatch_id::text,COALESCE(c.task_id::text,''),v.definition FROM olp.a2a_task_capabilities c JOIN olp.agent_publication_revisions v ON v.id=c.revision_id JOIN olp.execution_debits d ON d.id=c.dispatch_id LEFT JOIN olp.a2a_tasks t ON t.id=c.task_id WHERE c.token_digest=$1 AND c.expires_at>now() AND ((c.task_id IS NULL AND d.state='dispatched' AND EXISTS(SELECT 1 FROM olp.a2a_task_admissions a WHERE a.dispatch_id=c.dispatch_id)) OR (t.state='active' AND t.deadline_at>now())) FOR UPDATE OF c`, digest).Scan(&dispatch, &task, &definition)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrCapabilityInvalid
	}
	if err != nil {
		return ErrCapabilityUnavailable
	}
	if definition.Agent == nil {
		return ErrCapabilityInvalid
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
		return ErrCallbackLimit
	}
	if err != nil {
		return ErrCapabilityUnavailable
	}
	if _, err = tx.Exec(ctx, `INSERT INTO olp.a2a_task_callback_accounting(dispatch_id,kind,accounting_id) VALUES($1,$2,$3) ON CONFLICT(dispatch_id,kind,accounting_id) DO NOTHING`, dispatch, scope, accountingID); err != nil {
		return ErrCapabilityUnavailable
	}
	if tx.Commit(ctx) != nil {
		return ErrCapabilityUnavailable
	}
	return nil
}
