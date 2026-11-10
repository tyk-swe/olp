package session

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/attribution"
	"github.com/tyk-swe/olp/internal/code"
)

type authorityScope uint8

const (
	scopeInference authorityScope = iota
	scopeResearch
	scopeModels
	scopePaidInference
)

func validateAdmission(in Admission) error {
	if err := attribution.Validate(in.Attribution); err != nil {
		return code.Refuse(400, "code_invalid_attribution")
	}
	if in.EndUserDigest != "" && !keyauthority.ValidEndUserDigest(in.EndUserDigest) {
		return code.Refuse(400, "code_end_user_invalid")
	}
	if err := in.Operation.Identity.Validate(); err != nil {
		return err
	}
	if err := access.ValidText("operation", in.Operation.Name, 100); err != nil {
		return code.Refuse(400, "code_operation_invalid")
	}
	if err := access.ValidText("model", in.Operation.Model, 200); err != nil {
		return code.Refuse(400, "code_model_invalid")
	}
	return nil
}

func loadAuthority(ctx context.Context, tx pgx.Tx, in Admission, a *keyauthority.Authority, scope authorityScope) (code.Route, error) {
	var policy, document []byte
	var groups keyauthority.RouteGroups
	var templates keyauthority.LimitTemplates
	err := tx.QueryRow(ctx, `SELECT k.id::text,k.lookup_id,k.created_by::text,k.project_id::text,k.policy,k.expires_at,k.revoked_at,v.document,COALESCE(p.attribution_policy,'{}'::jsonb),COALESCE(p.route_groups,'{}'::jsonb),COALESCE(k.limit_lookup_id,k.lookup_id),COALESCE(p.limit_templates,'{}'::jsonb),p.end_user_policy,k.budget_group_id::text,g.limit_template,g.requests_per_minute,g.tokens_per_minute,g.max_concurrency,g.daily_cost_limit::text,g.monthly_cost_limit::text,g.weekly_cost_limit::text,COALESCE(g.cost_basis,'cost'),k.workload_issuer_id::text,COALESCE(k.workload_digest,''),COALESCE(k.workload_mapping,''),COALESCE(wi.etag::text,'')
		FROM olp.api_keys k LEFT JOIN olp.workload_issuers wi ON wi.id=k.workload_issuer_id LEFT JOIN olp.budget_groups g ON g.id=k.budget_group_id LEFT JOIN olp.projects p ON p.id=k.project_id JOIN olp.users u ON u.id=k.created_by JOIN olp.code_routes r ON r.id=$2 JOIN olp.code_route_revisions v ON v.id=r.latest_revision_id
		WHERE k.id=$1 AND (k.workload_issuer_id IS NULL OR wi.document->>'enabled'='true') AND u.active AND u.oidc_authorized AND
		(u.access_scope='global' OR EXISTS(SELECT 1 FROM olp.effective_project_members m WHERE m.user_id=u.id AND m.project_id=r.project_id))`,
		in.APIKeyID, in.Route.ID).Scan(
		&a.ID, &a.LookupID, &a.Issuer, &a.ProjectID, &policy, &a.ExpiresAt, &a.RevokedAt, &document,
		&a.ProjectAttributionPolicy, &groups, &a.LimitLookupID, &templates, &a.ProjectEndUserPolicy,
		&a.BudgetGroupID, &a.BudgetGroupTemplate, &a.BudgetGroupRPM, &a.BudgetGroupTPM, &a.BudgetGroupConcurrency,
		&a.BudgetGroupDailyCostLimit, &a.BudgetGroupMonthlyCostLimit, &a.BudgetGroupWeeklyCostLimit, &a.BudgetGroupCostBasis,
		&a.WorkloadIssuerID, &a.WorkloadDigest, &a.WorkloadMapping, &a.WorkloadRevision,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return code.Route{}, code.Refuse(403, "code_permission_denied")
	}
	if err != nil {
		return code.Route{}, err
	}
	if err = json.Unmarshal(policy, &a.Policy); err != nil {
		return code.Route{}, err
	}
	if err = a.BindLimitTemplates(templates); err != nil {
		return code.Route{}, err
	}
	if err = a.BindRouteGroups(groups); err != nil {
		return code.Route{}, err
	}
	var current code.Route
	if err = json.Unmarshal(document, &current); err != nil {
		return code.Route{}, err
	}
	if !in.Route.Enabled || !current.Enabled || current.ID != in.Route.ID || current.Slug != in.Route.Slug || current.ProjectID != in.Route.ProjectID || current.PoolID != in.Route.PoolID ||
		!a.Allows("inference", in.Route.Slug, &in.Route.ProjectID, time.Now()) {
		return code.Route{}, code.Refuse(403, "code_permission_denied")
	}
	if scope == scopeInference || scope == scopePaidInference {
		if _, err := current.ResolveModel(code.Operation{Model: in.Operation.PublicModel()}); err != nil {
			return code.Route{}, err
		}
		if !slices.Contains(in.Route.Models, in.Operation.Model) || !slices.Contains(current.Models, in.Operation.Model) {
			return code.Route{}, code.Refuse(403, "code_model_denied")
		}
	}
	if in.Recovery != nil && (current.RevisionID != in.Route.RevisionID || current.InitialRecoveryAttempts < in.Recovery.Number) {
		return code.Route{}, code.Refuse(409, "code_recovery_policy_changed")
	}
	if scope == scopeModels && current.RevisionID != in.Route.RevisionID {
		return code.Route{}, code.Refuse(409, "code_route_revision_changed")
	}
	if scope == scopeResearch && (current.RevisionID != in.Route.RevisionID || current.Tools == nil || in.Route.Tools == nil ||
		current.Tools.AccountID != in.Route.Tools.AccountID || !slices.Equal(current.Tools.Services, in.Route.Tools.Services)) {
		return code.Route{}, code.Refuse(409, "code_research_revision_changed")
	}
	var assigned, paid bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.code_pool_keys pk JOIN olp.code_pools p ON p.id=pk.pool_id JOIN olp.api_keys k ON k.id=pk.api_key_id
		WHERE p.id=$1 AND pk.api_key_id=$2 AND p.project_id=$3 AND (p.kind='shared' OR p.owner_user_id=k.created_by)),
		$6::bool AND EXISTS(SELECT 1 FROM olp.code_paid_bindings b WHERE b.route_id=$7 AND b.api_key_id=$2 AND b.conversation=$4 AND b.model=$5)`,
		in.Route.PoolID, in.APIKeyID, in.Route.ProjectID, in.Operation.Identity.Conversation, in.Operation.PublicModel(), scope == scopeInference, in.Route.ID).Scan(&assigned, &paid)
	if err != nil {
		return code.Route{}, err
	}
	if !assigned {
		return code.Route{}, code.Refuse(403, "code_pool_denied")
	}
	if paid {
		return code.Route{}, code.Refuse(409, "code_paid_target_conflict")
	}
	return current, nil
}
