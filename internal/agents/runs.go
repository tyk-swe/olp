package agents

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/secrets"
)

// StartRunTx deduplicates a correlation record. It grants no permission to
// resend a step and does not itself reserve or charge money.
func StartRunTx(ctx context.Context, tx pgx.Tx, key *secrets.AuthKey, a access.Authority, id, scope, resource, revision string) error {
	if key == nil || a.ProjectID == nil || !validApprovalID(id) || !a.AllowsResource(scope, resource, *a.ProjectID, time.Now()) {
		return limits.ErrDebitAuthority
	}
	kind := "toolset"
	if scope == "agents" {
		kind = "agent"
	} else if scope != "tools" {
		return limits.ErrDebitAuthority
	}
	principal, _ := json.Marshal(struct{ Key, Project, Workload string }{a.ID, *a.ProjectID, a.WorkloadDigest})
	digest := key.Digest(secrets.AgentPrincipalDigest, string(principal))
	var allowed bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.agent_publications p JOIN olp.agent_publication_revisions v ON v.publication_id=p.id JOIN olp.api_keys k ON k.id=$4 WHERE p.id=$1 AND v.id=$2 AND p.project_id=$3 AND p.kind=$5 AND k.project_id=$3 AND NOT p.retired AND k.revoked_at IS NULL AND (k.expires_at IS NULL OR k.expires_at>clock_timestamp()))`, resource, revision, *a.ProjectID, a.ID, kind).Scan(&allowed); err != nil {
		return err
	}
	if !allowed {
		return limits.ErrDebitAuthority
	}
	_, err := tx.Exec(ctx, `INSERT INTO olp.agent_runs(id,api_key_id,project_id,revision_id,principal_digest) VALUES($1,$2,$3,$4,$5) ON CONFLICT(id) DO NOTHING`, id, a.ID, *a.ProjectID, revision, digest)
	if err != nil {
		return err
	}
	if err = tx.QueryRow(ctx, `SELECT api_key_id=$2 AND project_id=$3 AND revision_id=$4 AND principal_digest=$5 FROM olp.agent_runs WHERE id=$1`, id, a.ID, *a.ProjectID, revision, digest).Scan(&allowed); err != nil {
		return err
	}
	if !allowed {
		return limits.ErrDebitConflict
	}
	return nil
}

// PrepareStepTx persists the run/step link and shared reservation intent in
// one transaction. Deferred foreign keys fence orphaned financial references.
func PrepareStepTx(ctx context.Context, tx pgx.Tx, store *limits.DebitStore, a access.Authority, intent limits.DebitIntent, ordinal int, owners []limits.DebitOwner) (limits.DebitState, error) {
	if a.ProjectID == nil || intent.APIKeyID != a.ID || intent.ProjectID != *a.ProjectID || ordinal < 0 || intent.RunID == "" || intent.StepID == "" {
		return "", limits.ErrDebitAuthority
	}
	var allowed bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.agent_runs r JOIN olp.agent_publication_revisions v ON v.id=r.revision_id JOIN olp.agent_publications p ON p.id=v.publication_id WHERE r.id=$1 AND r.api_key_id=$2 AND r.project_id=$3 AND NOT p.retired)`, intent.RunID, a.ID, *a.ProjectID).Scan(&allowed); err != nil {
		return "", err
	}
	if !allowed {
		return "", limits.ErrDebitAuthority
	}
	state, err := store.PrepareTx(ctx, tx, intent, owners)
	if err != nil {
		return "", err
	}
	_, err = tx.Exec(ctx, `INSERT INTO olp.agent_steps(id,run_id,dispatch_id,ordinal) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, intent.StepID, intent.RunID, intent.ID, ordinal)
	if err != nil {
		return "", err
	}
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.agent_steps WHERE id=$1 AND run_id=$2 AND dispatch_id=$3 AND ordinal=$4)`, intent.StepID, intent.RunID, intent.ID, ordinal).Scan(&allowed); err != nil {
		return "", err
	}
	if !allowed {
		return "", limits.ErrDebitConflict
	}
	return state, nil
}
