package agents

import (
	"context"
	"crypto/hmac"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/secrets"
)

type WorkflowApprovalCall struct {
	CallID            string          `json:"call_id"`
	ToolsetID         string          `json:"toolset_id"`
	ToolsetRevisionID string          `json:"toolset_revision_id"`
	ToolAlias         string          `json:"tool_alias"`
	Arguments         json.RawMessage `json:"arguments"`
	MaxCost           string          `json:"max_cost"`
}

type WorkflowApprovalRequest struct {
	RunID           string                 `json:"run_id"`
	RouteRevisionID string                 `json:"route_revision_id"`
	StartOrdinal    int                    `json:"start_ordinal"`
	Transcript      json.RawMessage        `json:"transcript"`
	State           json.RawMessage        `json:"state"`
	Calls           []WorkflowApprovalCall `json:"calls"`
	ExpiresAt       time.Time              `json:"expires_at"`
}

type WorkflowApproval struct {
	CallID  string          `json:"call_id"`
	ID      string          `json:"id"`
	Binding ApprovalBinding `json:"binding"`
}

type WorkflowContinuation struct {
	RunID           string             `json:"run_id"`
	RouteRevisionID string             `json:"route_revision_id"`
	StartOrdinal    int                `json:"start_ordinal"`
	NextOrdinal     int                `json:"-"`
	State           json.RawMessage    `json:"state"`
	Approvals       []WorkflowApproval `json:"approvals"`
	ExpiresAt       time.Time          `json:"expires_at"`
}

func (s *WorkflowStore) Pause(ctx context.Context, authority access.Authority, request WorkflowApprovalRequest, maxBytes int) (string, error) {
	if s == nil || s.Pool == nil || s.Auth == nil || s.Keys == nil || authority.ProjectID == nil || request.StartOrdinal < 0 || len(request.Calls) == 0 || len(request.Calls) > 32 || !json.Valid(request.Transcript) || !json.Valid(request.State) {
		return "", ErrApproval
	}
	for i, call := range request.Calls {
		if call.CallID == "" || !json.Valid(call.Arguments) || slices.ContainsFunc(request.Calls[:i], func(previous WorkflowApprovalCall) bool { return previous.CallID == call.CallID }) {
			return "", ErrApproval
		}
	}
	now := time.Now()
	expires := func() time.Time {
		latest := now.Add(5 * time.Minute)
		if request.ExpiresAt.IsZero() || request.ExpiresAt.After(latest) {
			return latest
		}
		return request.ExpiresAt
	}()
	if !expires.After(now) {
		return "", ErrApproval
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	var currency string
	if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT btrim(currency) FROM olp.pricing_currency WHERE singleton),'USD')`).Scan(&currency); err != nil {
		return "", err
	}
	principal := hex.EncodeToString(PrincipalDigest(s.Auth, authority))
	transcript := hex.EncodeToString(s.Auth.Digest(secrets.ApprovalDigest, "workflow-transcript:"+string(request.Transcript)))
	type approvalResult struct {
		approval WorkflowApproval
		err      error
	}
	results := slices.Collect(func(yield func(approvalResult) bool) {
		for _, call := range request.Calls {
			binding := ApprovalBinding{RunID: request.RunID, ToolsetID: call.ToolsetID, ToolsetRevisionID: call.ToolsetRevisionID, RouteRevisionID: request.RouteRevisionID, ToolAlias: call.ToolAlias, PrincipalDigest: principal, ArgumentDigest: hex.EncodeToString(s.Auth.Digest(secrets.ToolInputDigest, string(call.Arguments))), TranscriptDigest: transcript, Currency: currency, MaxCost: call.MaxCost}
			id, err := CreateApprovalTx(ctx, tx, s.Auth, authority, binding, expires)
			if !yield(approvalResult{approval: WorkflowApproval{CallID: call.CallID, ID: id, Binding: binding}, err: err}) || err != nil {
				return
			}
		}
	})
	for _, result := range results {
		if result.err != nil {
			return "", result.err
		}
	}
	approvals := slices.Collect(func(yield func(WorkflowApproval) bool) {
		for _, result := range results {
			if !yield(result.approval) {
				return
			}
		}
	})
	continuation := WorkflowContinuation{RunID: request.RunID, RouteRevisionID: request.RouteRevisionID, StartOrdinal: request.StartOrdinal, State: request.State, Approvals: approvals, ExpiresAt: expires}
	payload, err := json.Marshal(continuation)
	if err != nil {
		return "", err
	}
	id := approvals[0].ID
	sealed, err := s.Keys.Seal(s.Installation, secrets.WorkflowContinuation, id, payload)
	if err != nil {
		return "", err
	}
	token := fmt.Sprintf("olpa_%d_%s_%s", s.Keys.Active, id, base64.RawURLEncoding.EncodeToString(sealed))
	if maxBytes <= 0 || len(token) > maxBytes {
		return "", ErrApproval
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return token, nil
}

func (s *WorkflowStore) Continue(ctx context.Context, authority access.Authority, token string, transcript json.RawMessage, arguments map[string]json.RawMessage, maxBytes int) (WorkflowContinuation, error) {
	if s == nil || s.Pool == nil || s.Auth == nil || s.Keys == nil || authority.ProjectID == nil || maxBytes <= 0 || len(token) > maxBytes || !json.Valid(transcript) {
		return WorkflowContinuation{}, ErrApproval
	}
	parts := strings.SplitN(token, "_", 4)
	if len(parts) != 4 || parts[0] != "olpa" || !validApprovalID(parts[2]) {
		return WorkflowContinuation{}, ErrApproval
	}
	version, err := strconv.Atoi(parts[1])
	if err != nil {
		return WorkflowContinuation{}, ErrApproval
	}
	sealed, err := base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil {
		return WorkflowContinuation{}, ErrApproval
	}
	payload, err := s.Keys.Open(s.Installation, secrets.WorkflowContinuation, parts[2], version, sealed)
	if err != nil {
		return WorkflowContinuation{}, ErrApproval
	}
	var continuation WorkflowContinuation
	if json.Unmarshal(payload, &continuation) != nil || !continuation.ExpiresAt.After(time.Now()) || len(continuation.Approvals) == 0 || len(continuation.Approvals) != len(arguments) {
		return WorkflowContinuation{}, ErrApproval
	}
	principal := PrincipalDigest(s.Auth, authority)
	transcriptDigest := s.Auth.Digest(secrets.ApprovalDigest, "workflow-transcript:"+string(transcript))
	for _, approval := range continuation.Approvals {
		argument, ok := arguments[approval.CallID]
		if !ok || !json.Valid(argument) {
			return WorkflowContinuation{}, ErrApproval
		}
		binding := approval.Binding
		principalDigest, e1 := hex.DecodeString(binding.PrincipalDigest)
		transcript, e2 := hex.DecodeString(binding.TranscriptDigest)
		argumentDigest, e3 := hex.DecodeString(binding.ArgumentDigest)
		if e1 != nil || e2 != nil || e3 != nil || !hmac.Equal(principal, principalDigest) || !hmac.Equal(transcriptDigest, transcript) || !hmac.Equal(s.Auth.Digest(secrets.ToolInputDigest, string(argument)), argumentDigest) {
			return WorkflowContinuation{}, ErrApproval
		}
		var active bool
		if err := s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.agent_approvals WHERE id=$1 AND api_key_id=$2 AND project_id=$3 AND run_id=$4 AND consumed_at IS NULL AND expires_at>clock_timestamp())`, approval.ID, authority.ID, *authority.ProjectID, continuation.RunID).Scan(&active); err != nil || !active {
			return WorkflowContinuation{}, ErrApproval
		}
	}
	var next int
	var dispatched bool
	if err := s.Pool.QueryRow(ctx, `SELECT GREATEST(COALESCE(max(step.ordinal)+1,0),$2),COALESCE(bool_or(debit.state NOT IN ('released','not_accepted')),false) FROM olp.agent_steps step JOIN olp.execution_debits debit ON debit.id=step.dispatch_id WHERE step.run_id=$1 AND step.ordinal>=$2`, continuation.RunID, continuation.StartOrdinal).Scan(&next, &dispatched); err != nil || dispatched {
		return WorkflowContinuation{}, ErrApproval
	}
	return WorkflowContinuation{RunID: continuation.RunID, RouteRevisionID: continuation.RouteRevisionID, StartOrdinal: continuation.StartOrdinal, NextOrdinal: next, State: continuation.State, Approvals: continuation.Approvals, ExpiresAt: continuation.ExpiresAt}, nil
}
