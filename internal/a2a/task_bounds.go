package a2a

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	protocol "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/agentcontract"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/secrets"
)

var errTaskLimit = errors.New("agent task limit reached")

func (l *Ledger) admitRate(ctx context.Context, authority access.Authority) ([]*limits.Lease, error) {
	if l == nil || l.Store == nil || l.Store.Limiter == nil {
		return nil, limits.ErrDebitAuthority
	}
	rate := int64(60)
	ttl := time.Hour + time.Minute
	requests := []limits.Request{
		{LookupID: "a2a_" + strings.ReplaceAll(authority.ID, "-", ""), CostOwnerID: authority.ID, RequestsPerMinute: &rate, LeaseTTL: ttl},
		{LookupID: authority.LimitsLookup(), CostOwnerID: authority.ID, RequestsPerMinute: authority.Policy.RequestsPerMinute, MaxConcurrency: authority.Policy.MaxConcurrency, LeaseTTL: ttl},
	}
	if agent := authority.DelegationAgentLimits; agent != nil {
		requests = append(requests, limits.Request{LookupID: agent.LookupID, CostOwnerID: agent.ID, RequestsPerMinute: agent.Limits.RequestsPerMinute, MaxConcurrency: agent.Limits.MaxConcurrency, LeaseTTL: ttl})
	}
	if authority.BudgetGroupID != nil {
		requests = append(requests, limits.Request{LookupID: limits.BudgetGroupLookup(*authority.BudgetGroupID), CostOwnerID: *authority.BudgetGroupID, RequestsPerMinute: authority.BudgetGroupRPM, MaxConcurrency: authority.BudgetGroupConcurrency, LeaseTTL: ttl})
	}
	merged := make([]limits.Request, 0, len(requests))
	for _, request := range requests {
		if !request.HasRateLimits() {
			continue
		}
		found := false
		for i := range merged {
			if merged[i].LookupID == request.LookupID {
				merged[i].RequestsPerMinute = lowerLimit(merged[i].RequestsPerMinute, request.RequestsPerMinute)
				merged[i].MaxConcurrency = lowerLimit(merged[i].MaxConcurrency, request.MaxConcurrency)
				found = true
				break
			}
		}
		if !found {
			merged = append(merged, request)
		}
	}
	leases := make([]*limits.Lease, 0, len(merged))
	for _, request := range merged {
		lease, err := l.Store.Limiter.Reserve(ctx, request)
		if err != nil {
			finishRate(ctx, leases, false)
			return nil, err
		}
		if lease != nil {
			leases = append(leases, lease)
		}
	}
	return leases, nil
}

func lowerLimit(a, b *int64) *int64 {
	if a == nil || b != nil && *b < *a {
		return b
	}
	return a
}

func finishRate(ctx context.Context, leases []*limits.Lease, dispatched bool) {
	for _, lease := range leases {
		if dispatched {
			_ = lease.Release(context.WithoutCancel(ctx))
		} else {
			_ = lease.Refund(context.WithoutCancel(ctx))
		}
	}
}

func maxTaskContinuations(definition *agentcontract.Definition) int {
	if definition.MaxTaskContinuations == nil {
		return 32
	}
	return *definition.MaxTaskContinuations
}

func taskDeadlineSeconds(definition *agentcontract.Definition) int {
	if definition.TaskDeadlineSeconds == nil {
		return 600
	}
	return *definition.TaskDeadlineSeconds
}

func (l *Ledger) admitTask(ctx context.Context, res reservation, authority access.Authority, taskID string, revision agentcontract.Revision) (string, error) {
	definition := revision.Definition.Agent
	token := "olpt_" + secrets.Token()
	delegationDigest, err := hex.DecodeString(authority.DelegationTokenDigest)
	if err != nil {
		return "", err
	}
	tx, err := l.Pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "a2a-task:"+authority.ID); err != nil {
		return "", err
	}
	if taskID == "" {
		var count int
		if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM olp.a2a_tasks WHERE api_key_id=$1 AND state IN ('active','outcome_unknown')) + (SELECT count(*) FROM olp.a2a_task_admissions a JOIN olp.execution_debits d ON d.id=a.dispatch_id WHERE a.api_key_id=$1 AND a.task_id IS NULL AND d.state NOT IN ('settled','released','not_accepted'))`, authority.ID).Scan(&count); err != nil {
			return "", err
		}
		if count >= 4 {
			return "", errTaskLimit
		}
	} else {
		tag, err := tx.Exec(ctx, `UPDATE olp.a2a_tasks SET continuations=continuations+1 WHERE id=$1 AND (($4='' AND api_key_id=$2 AND delegation_pair_digest='') OR ($4<>'' AND delegation_pair_digest=$4)) AND state='active' AND deadline_at>now() AND continuations<$3`, taskID, authority.ID, maxTaskContinuations(definition), authority.DelegationPairDigest)
		if err != nil {
			return "", err
		}
		if tag.RowsAffected() != 1 {
			return "", errTaskLimit
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO olp.a2a_task_admissions(dispatch_id,api_key_id,task_id) VALUES($1,$2,NULLIF($3,'')::uuid)`, res.DispatchID, authority.ID, taskID); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO olp.a2a_task_capabilities(dispatch_id,token_digest,publication_id,revision_id,task_id,expires_at,caller_workload_revision_id,caller_expires_at,end_user_digest,delegation_token_digest) SELECT $1,$2,$3,$4,NULLIF($5,'')::uuid,LEAST(a.created_at+$6*interval '1 second',COALESCE(t.deadline_at,'infinity'::timestamptz),COALESCE($8::timestamptz,'infinity'::timestamptz)),NULLIF($7,'')::uuid,$8,$9,$10 FROM olp.a2a_task_admissions a LEFT JOIN olp.a2a_tasks t ON t.id=a.task_id WHERE a.dispatch_id=$1`, res.DispatchID, l.Auth.Digest(secrets.TaskCapabilityDigest, token), revision.PublicationID, revision.ID, taskID, taskDeadlineSeconds(definition), authority.WorkloadRevision, authority.ExpiresAt, authority.EndUserDigest, delegationDigest); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return token, nil
}

func taskEventState(event protocol.Event) string {
	switch value := event.(type) {
	case *protocol.Task:
		if value != nil {
			return taskState(value.Status.State)
		}
	case *protocol.TaskStatusUpdateEvent:
		if value != nil {
			return taskState(value.Status.State)
		}
	}
	return "active"
}

func taskState(state protocol.TaskState) string {
	switch state {
	case protocol.TaskStateCompleted:
		return "completed"
	case protocol.TaskStateCanceled:
		return "canceled"
	case protocol.TaskStateFailed, protocol.TaskStateRejected:
		return "failed"
	case protocol.TaskStateSubmitted, protocol.TaskStateWorking, protocol.TaskStateInputRequired, protocol.TaskStateAuthRequired:
		return "active"
	default:
		return "outcome_unknown"
	}
}

func (l *Ledger) recordTaskState(ctx context.Context, taskID string, state string) error {
	if taskID == "" {
		return nil
	}
	tag, err := l.Pool.Exec(ctx, `UPDATE olp.a2a_tasks SET state=CASE WHEN state='outcome_unknown' AND $2='active' THEN state ELSE $2 END WHERE id=$1 AND (state IN ('active','outcome_unknown') OR state=$2)`, taskID, state)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return pgx.ErrNoRows
	}
	return nil
}
