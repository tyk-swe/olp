package tasks

import (
	"context"
	"encoding/hex"
	"slices"
	"strings"
	"time"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/agentcontract"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/limits/admission"
	limitdebits "github.com/tyk-swe/olp/internal/limits/debits"
	"github.com/tyk-swe/olp/internal/secrets"
)

func rateRequest(lookup string, requests, concurrency *int64) admission.Request {
	return admission.Request{Lookup: lookup, LeaseTTL: time.Hour + time.Minute, Rate: admission.RateLimits{RequestsPerMinute: requests, MaxConcurrency: concurrency}}
}

func rateRequests(authority keyauthority.Authority) []admission.Request {
	rate := int64(60)
	requests := []admission.Request{
		rateRequest("a2a_"+strings.ReplaceAll(authority.ID, "-", ""), &rate, nil),
		rateRequest(authority.LimitsLookup(), authority.Policy.RequestsPerMinute, authority.Policy.MaxConcurrency),
	}
	if agent := authority.DelegationAgentLimits; agent != nil {
		requests = append(requests, rateRequest(agent.LookupID, agent.Limits.RequestsPerMinute, agent.Limits.MaxConcurrency))
	}
	if authority.BudgetGroupID != nil {
		requests = append(requests, rateRequest(limits.BudgetGroupLookup(*authority.BudgetGroupID), authority.BudgetGroupRPM, authority.BudgetGroupConcurrency))
	}
	merged := make([]admission.Request, 0, len(requests))
	for _, request := range requests {
		if !request.Rate.Limited() {
			continue
		}
		i := slices.IndexFunc(merged, func(m admission.Request) bool { return m.Lookup == request.Lookup })
		if i < 0 {
			merged = append(merged, request)
			continue
		}
		merged[i] = rateRequest(request.Lookup,
			lowerLimit(merged[i].Rate.RequestsPerMinute, request.Rate.RequestsPerMinute),
			lowerLimit(merged[i].Rate.MaxConcurrency, request.Rate.MaxConcurrency))
	}
	return merged
}

func (l *Store) admitRate(ctx context.Context, authority keyauthority.Authority) ([]*admission.Lease, error) {
	if l == nil || l.limiter == nil {
		return nil, limitdebits.ErrAuthority
	}
	requests := rateRequests(authority)
	leases := make([]*admission.Lease, 0, len(requests))
	for _, request := range requests {
		lease, err := l.limiter.Reserve(ctx, request)
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

func finishRate(ctx context.Context, leases []*admission.Lease, dispatched bool) {
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

func (l *Store) admitTask(ctx context.Context, res reservation, authority keyauthority.Authority, taskID string, revision agentcontract.Revision) (string, error) {
	definition := revision.Definition.Agent
	token := "olpt_" + secrets.Token()
	delegationDigest, err := hex.DecodeString(authority.DelegationTokenDigest)
	if err != nil {
		return "", err
	}
	tx, err := l.pool.Begin(ctx)
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
			return "", ErrTaskLimit
		}
	} else {
		tag, err := tx.Exec(ctx, `UPDATE olp.a2a_tasks SET continuations=continuations+1 WHERE id=$1 AND (($4='' AND api_key_id=$2 AND delegation_pair_digest='') OR ($4<>'' AND delegation_pair_digest=$4)) AND state='active' AND deadline_at>now() AND continuations<$3`, taskID, authority.ID, maxTaskContinuations(definition), authority.DelegationPairDigest)
		if err != nil {
			return "", err
		}
		if tag.RowsAffected() != 1 {
			return "", ErrTaskLimit
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO olp.a2a_task_admissions(dispatch_id,api_key_id,task_id) VALUES($1,$2,NULLIF($3,'')::uuid)`, res.DispatchID, authority.ID, taskID); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO olp.a2a_task_capabilities(dispatch_id,token_digest,publication_id,revision_id,task_id,expires_at,caller_workload_revision_id,caller_expires_at,end_user_digest,delegation_token_digest) SELECT $1,$2,$3,$4,NULLIF($5,'')::uuid,LEAST(a.created_at+$6*interval '1 second',COALESCE(t.deadline_at,'infinity'::timestamptz),COALESCE($8::timestamptz,'infinity'::timestamptz)),NULLIF($7,'')::uuid,$8,$9,$10 FROM olp.a2a_task_admissions a LEFT JOIN olp.a2a_tasks t ON t.id=a.task_id WHERE a.dispatch_id=$1`, res.DispatchID, l.auth.Digest(secrets.TaskCapabilityDigest, token), revision.PublicationID, revision.ID, taskID, taskDeadlineSeconds(definition), authority.WorkloadRevision, authority.ExpiresAt, authority.EndUserDigest, delegationDigest); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return token, nil
}

func (l *Store) RecordState(ctx context.Context, taskID string, state State) error {
	if taskID == "" {
		return nil
	}
	tag, err := l.pool.Exec(ctx, `UPDATE olp.a2a_tasks SET state=CASE WHEN state='outcome_unknown' AND $2='active' THEN state ELSE $2 END WHERE id=$1 AND (state IN ('active','outcome_unknown') OR state=$2)`, taskID, state)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrNotFound
	}
	return nil
}
