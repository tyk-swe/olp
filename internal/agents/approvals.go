package agents

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/secrets"
)

var ErrApproval = errors.New("approval is unavailable, expired, changed or already consumed")

// ApprovalBinding accepts only scoped IDs, digests and a finite allowance.
// Arguments, messages, rendered prompts and raw claims never reach this store.
type ApprovalBinding struct {
	RunID             string `json:"run_id"`
	ToolsetID         string `json:"toolset_id"`
	ToolsetRevisionID string `json:"toolset_revision_id"`
	RouteRevisionID   string `json:"route_revision_id"`
	ToolAlias         string `json:"tool_alias"`
	PrincipalDigest   string `json:"principal_digest"`
	ArgumentDigest    string `json:"argument_digest"`
	TranscriptDigest  string `json:"transcript_digest"`
	Currency          string `json:"currency"`
	MaxCost           string `json:"max_cost"`
}

func validApprovalID(raw string) bool {
	id, err := uuid.Parse(raw)
	return err == nil && id != uuid.Nil && id.String() == raw
}

func approvalDigest(key *secrets.AuthKey, b ApprovalBinding) ([]byte, error) {
	for _, id := range []string{b.RunID, b.ToolsetID, b.ToolsetRevisionID} {
		v, e := uuid.Parse(id)
		if e != nil || v == uuid.Nil || v.String() != id {
			return nil, ErrApproval
		}
	}
	for _, raw := range []string{b.PrincipalDigest, b.ArgumentDigest, b.TranscriptDigest} {
		data, e := hex.DecodeString(raw)
		if e != nil || len(data) != 32 {
			return nil, ErrApproval
		}
	}
	if b.RouteRevisionID != "" && !validApprovalID(b.RouteRevisionID) {
		return nil, ErrApproval
	}
	if key == nil || !regexp.MustCompile(`^t_[a-f0-9]{32}_[a-f0-9]{32}$`).MatchString(b.ToolAlias) || !limits.ValidCostLimit(b.MaxCost) || !regexp.MustCompile(`^[A-Z]{3}$`).MatchString(b.Currency) {
		return nil, ErrApproval
	}
	data, _ := json.Marshal(b)
	return key.Digest(secrets.ApprovalDigest, string(data)), nil
}

func CreateApprovalTx(ctx context.Context, tx pgx.Tx, key *secrets.AuthKey, a access.Authority, b ApprovalBinding, expires time.Time) (string, error) {
	now := time.Now()
	if a.ProjectID == nil || !a.AllowsResource("tools", b.ToolsetID, *a.ProjectID, now) {
		return "", ErrApproval
	}
	if expires.IsZero() {
		expires = now.Add(5 * time.Minute)
	}
	if !expires.After(now) || expires.After(now.Add(5*time.Minute)) {
		return "", ErrApproval
	}
	digest, err := approvalDigest(key, b)
	if err != nil {
		return "", err
	}
	id := access.NewID()
	_, err = tx.Exec(ctx, `INSERT INTO olp.agent_approvals(id,api_key_id,project_id,run_id,binding_digest,expires_at) SELECT $1,$2,$3,r.id,$5,$6 FROM olp.agent_runs r JOIN olp.agent_publications p ON p.id=$7 AND p.project_id=r.project_id AND NOT p.retired JOIN olp.live_agent_publication_revisions v ON v.id=$8 AND v.publication_id=p.id WHERE r.id=$4 AND r.api_key_id=$2 AND r.project_id=$3 AND encode(r.principal_digest,'hex')=$9 AND EXISTS(SELECT 1 FROM jsonb_array_elements(v.definition->'toolset'->'tools') t WHERE t->>'alias'=$10)`, id, a.ID, *a.ProjectID, b.RunID, digest, expires, b.ToolsetID, b.ToolsetRevisionID, b.PrincipalDigest, b.ToolAlias)
	if err != nil {
		return "", err
	}
	var exists bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.agent_approvals WHERE id=$1)`, id).Scan(&exists); err != nil {
		return "", err
	}
	if !exists {
		return "", ErrApproval
	}
	return id, nil
}

// ConsumeApprovalTx must share a transaction with limits.MarkDispatchedTx.
// The supplied authority must be freshly resolved by runtime; the database also
// refuses revoked/expired principals and retired resources at consumption.
func ConsumeApprovalTx(ctx context.Context, tx pgx.Tx, key *secrets.AuthKey, a access.Authority, id, dispatch string, b ApprovalBinding) error {
	if a.ProjectID == nil || !a.AllowsResource("tools", b.ToolsetID, *a.ProjectID, time.Now()) {
		return ErrApproval
	}
	digest, err := approvalDigest(key, b)
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE olp.agent_approvals x SET consumed_at=clock_timestamp(),dispatch_id=$2 FROM olp.api_keys k,olp.agent_publications p,olp.live_agent_publication_revisions v,olp.execution_debits d WHERE x.id=$1 AND d.id=$2 AND x.api_key_id=$3 AND x.project_id=$4 AND x.binding_digest=$5 AND x.consumed_at IS NULL AND x.expires_at>clock_timestamp() AND k.id=x.api_key_id AND k.revoked_at IS NULL AND (k.expires_at IS NULL OR k.expires_at>clock_timestamp()) AND p.id=$6 AND v.publication_id=p.id AND v.id=$9 AND NOT p.retired AND p.project_id=x.project_id AND d.api_key_id=x.api_key_id AND d.run_id=x.run_id AND d.state='ready' AND d.kind='tool' AND d.cost_bound<=$7::numeric AND d.currency=$8 AND d.affinity->>'revision_id'=$9::text AND COALESCE(d.affinity->>'route_revision_id','')=$10 AND d.affinity->>'tool_alias'=$11 AND d.affinity->>'input_digest'=$12 AND d.affinity->>'transcript_digest'=$13`, id, dispatch, a.ID, *a.ProjectID, digest, b.ToolsetID, b.MaxCost, b.Currency, b.ToolsetRevisionID, b.RouteRevisionID, b.ToolAlias, b.ArgumentDigest, b.TranscriptDigest)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrApproval
	}
	return nil
}
