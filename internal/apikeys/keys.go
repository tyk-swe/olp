package apikeys

import (
	"encoding/json"
	"net/http"
	"slices"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/domain/attribution"
	"github.com/tyk-swe/olp/internal/domain/priority"
	"github.com/tyk-swe/olp/internal/domain/routeidentity"
	"github.com/tyk-swe/olp/internal/keyauthority"
	"github.com/tyk-swe/olp/internal/limits/ledger"
)

type keyInput struct {
	Name          string  `json:"name"`
	ProjectID     *string `json:"project_id"`
	BudgetGroupID *string `json:"budget_group_id"`
	keyauthority.KeyPolicy
}

const maxAttributionKeys = 8

func validateAndNormalizeKey(input keyInput, expirationChanged bool) error {
	if days := input.RotationIntervalDays; days != nil && (*days < 1 || *days > 3650) {
		return httpkit.Invalid("rotation_interval_days", "Use a rotation interval of 1–3650 days, or null.")
	}
	if len(input.AllowedRouteGroups) > 64 {
		return httpkit.Invalid("allowed_route_groups", "Use at most 64 route groups.")
	}
	groupNames := map[string]bool{}
	for _, name := range input.AllowedRouteGroups {
		if !routeidentity.ValidSlug(name) || groupNames[name] {
			return httpkit.Invalid("allowed_route_groups", "Use unique valid group names.")
		}
		groupNames[name] = true
	}
	policy := input.KeyPolicy.AttributionPolicy()
	if err := access.ValidateAttributionPolicy(&policy); err != nil {
		return err
	}
	if err := keyauthority.ValidateTemplateName(input.LimitTemplate); err != nil {
		return err
	}
	if err := input.RouteLimits.ValidateAndNormalize(); err != nil {
		return err
	}
	if err := input.RegionalLimits.Validate(); err != nil {
		return err
	}
	if input.RunLimits != nil && !input.RunLimits.Valid() {
		return httpkit.Invalid("run_limits", "Use valid workflow run limits.")
	}
	if err := validateKeyCIDRs(input.AllowedCIDRs); err != nil {
		return err
	}
	if input.EndUserPolicy != nil && input.EndUserSource == nil {
		return httpkit.Invalid("end_user_source", "An end-user policy requires an identifier source.")
	}
	if err := input.EndUserPolicy.ValidateAndNormalize(); err != nil {
		return err
	}
	if input.EndUserSource != nil && *input.EndUserSource != "header" && *input.EndUserSource != "native" {
		return httpkit.Invalid("end_user_source", "Use header, native, or null to disable end-user identification.")
	}
	if err := httpkit.ValidText("name", input.Name, 100); err != nil {
		return err
	}
	if len(input.Scopes) < 1 || len(input.Scopes) > len(ConsumerScopes()) {
		return httpkit.Invalid("scopes", "Select at least one unique scope.")
	}
	seen := map[string]bool{}
	for _, scope := range input.Scopes {
		if !slices.Contains(ConsumerScopes(), scope) || seen[scope] {
			return httpkit.Invalid("scopes", "Use unique supported consumer scopes.")
		}
		seen[scope] = true
	}
	for field, ids := range map[string][]string{"allowed_toolsets": input.AllowedToolsets, "allowed_agents": input.AllowedAgents} {
		if err := validateResourceAllowlist(field, ids); err != nil {
			return err
		}
	}
	if input.AllowedRoutes == nil {
		return httpkit.Invalid("allowed_routes", "This field cannot be null.")
	}
	if len(input.AllowedRoutes) > 100 {
		return httpkit.Invalid("allowed_routes", "Use at most 100 route slugs.")
	}
	seen = map[string]bool{}
	for _, route := range input.AllowedRoutes {
		if !routeidentity.ValidSlug(route) || seen[route] {
			return httpkit.Invalid("allowed_routes", "Use unique valid route slugs.")
		}
		seen[route] = true
	}
	if len(input.AllowedAttributionKeys) > maxAttributionKeys {
		return httpkit.Invalid("allowed_attribution_keys", "Use at most 8 attribution keys.")
	}
	seen = map[string]bool{}
	for _, key := range input.AllowedAttributionKeys {
		if key == attribution.DelegationPairKey || !attribution.ValidKey(key) || seen[key] {
			return httpkit.Invalid("allowed_attribution_keys", "Use unique valid attribution keys.")
		}
		seen[key] = true
	}
	if err := input.KeyPolicy.AdmissionLimits().ValidateAndNormalize(); err != nil {
		return err
	}
	for field, value := range map[string]*string{"priority": input.Priority, "max_priority": input.MaxPriority} {
		if value != nil && priority.Rank(*value) < 0 {
			return httpkit.Invalid(field, "Use critical, high, normal or low.")
		}
	}
	if priority.Rank(input.DefaultPriority()) < priority.Rank(input.PriorityCeiling()) {
		return httpkit.Invalid("priority", "The default priority cannot exceed max_priority.")
	}
	if expirationChanged && input.ExpiresAt != nil && !input.ExpiresAt.After(time.Now()) {
		return httpkit.Invalid("expires_at", "Choose a future expiry.")
	}
	return nil
}

// AdvanceAuthority advances key authority, so every gateway reloads API keys
// and which credential versions may serve on its next poll.
const keyFields = `'run_limits',k.policy->'run_limits','allowed_toolsets',COALESCE(NULLIF(k.policy->'allowed_toolsets','null'::jsonb),'[]'::jsonb),
 'allowed_agents',COALESCE(NULLIF(k.policy->'allowed_agents','null'::jsonb),'[]'::jsonb),
 'workload_issuer_id',k.workload_issuer_id,'workload_digest',k.workload_digest,'workload_mapping',k.workload_mapping,'rotation_interval_days',k.policy->'rotation_interval_days',
 'rotation_due_at',CASE WHEN k.policy->>'rotation_interval_days' IS NOT NULL THEN COALESCE(k.rotated_at,k.created_at)+make_interval(secs=>86400.0*(k.policy->>'rotation_interval_days')::int) END,
 'id',k.id,'lookup_id',k.lookup_id,'name',k.name,'project_id',k.project_id,'project_name',pr.name,'budget_group_id',k.budget_group_id,'created_by',k.created_by,'created_by_email',u.email,'etag',k.etag,'created_at',k.created_at,'expires_at',k.expires_at,'revoked_at',k.revoked_at,'paused_at',k.paused_at,'rotated_at',k.rotated_at,'scopes',k.policy->'scopes','allowed_routes',k.policy->'allowed_routes',
 'regional_limits',COALESCE(NULLIF(k.policy->'regional_limits','null'::jsonb),'{}'::jsonb),'limit_template',k.policy->'limit_template','route_limits',COALESCE(NULLIF(k.policy->'route_limits','null'::jsonb),'{}'::jsonb),'allowed_route_groups',COALESCE(NULLIF(k.policy->'allowed_route_groups','null'::jsonb),'[]'::jsonb),'allowed_cidrs',COALESCE(NULLIF(k.policy->'allowed_cidrs','null'::jsonb),'[]'::jsonb),'requests_per_minute',k.policy->'requests_per_minute','tokens_per_minute',k.policy->'tokens_per_minute','max_concurrency',k.policy->'max_concurrency','allowed_attribution_keys',COALESCE(NULLIF(k.policy->'allowed_attribution_keys','null'::jsonb),'[]'::jsonb),'allow_provider_state',COALESCE(k.policy->'allow_provider_state','false'::jsonb),'forbid_response_cache',COALESCE(k.policy->'forbid_response_cache','false'::jsonb),'response_metadata',COALESCE(k.policy->'response_metadata','false'::jsonb),'required_attribution_keys',COALESCE(NULLIF(k.policy->'required_attribution_keys','null'::jsonb),'[]'::jsonb),'attribution_defaults',COALESCE(NULLIF(k.policy->'attribution_defaults','null'::jsonb),'{}'::jsonb),'end_user_policy',k.policy->'end_user_policy','end_user_source',k.policy->'end_user_source','priority',k.policy->'priority','max_priority',k.policy->'max_priority','effective_limits',k.effective_limits,'cost_basis',COALESCE(k.policy->>'cost_basis','cost')`
const keyFrom = " FROM olp.api_keys_with_limits k JOIN olp.users u ON u.id=k.created_by LEFT JOIN olp.projects pr ON pr.id=k.project_id"

const overlapErrorAt = `GREATEST(o.last_recording_error_at, ($2::jsonb->>o.lookup_id)::timestamptz)`
const overlapDetailFields = `,'last_observed_use',o.last_observed_use,
 'last_recording_error_at',` + overlapErrorAt + `,
 'observation_status',CASE WHEN ` + overlapErrorAt + ` IS NULL THEN 'ok' ELSE 'degraded' END`

func keyOverlapsSQL(detailFields string) string {
	return `,'active_overlaps',COALESCE((SELECT jsonb_agg(jsonb_build_object(
 'lookup_id',o.lookup_id,'expires_at',LEAST(o.expires_at,k.expires_at)` + detailFields + `) ORDER BY o.expires_at)
 FROM olp.api_key_overlaps o WHERE o.api_key_id=k.id AND LEAST(o.expires_at,k.expires_at)>now() AND k.revoked_at IS NULL),'[]'::jsonb)`
}

// keyJSON renders one API key row, whose alias must be k, as the management
// contract's key detail. The budget is live accounting: accrued spend and
// unpriced attempts are summed from the recorded facts and their retained
// rollups, the amounts they are measured against come from the stored policy,
// and enforcement_active tells the console whether this installation admits
// requests against them at all.
func (a *API) keyJSON(overlapFields string) string {
	enforcement := "false"
	if a.server.LimitsEnforced() {
		enforcement = "true"
	}
	return `jsonb_build_object(` + keyFields + keyOverlapsSQL(overlapFields) + `,'budget',olp.budget_allowances(k.id,k.effective_limits,jsonb_set(jsonb_set(jsonb_set(` + ledger.KeyBudgetSQL("k") +
		`||jsonb_build_object('enforcement_active',` + enforcement +
		`),'{daily,limit}',COALESCE(k.policy->'daily_cost_limit','null'::jsonb)),'{monthly,limit}',COALESCE(k.policy->'monthly_cost_limit','null'::jsonb)),'{weekly,limit}',COALESCE(k.policy->'weekly_cost_limit','null'::jsonb))))`
}

func (a *API) apiKeys(r *http.Request, principal authz.Principal) (httpkit.Reply, error) {
	p, err := httpkit.Page(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	var issuer any
	if raw := r.URL.Query().Get("created_by"); raw != "" {
		issuer, err = httpkit.ParseUUID(raw)
		if err != nil {
			return httpkit.Reply{}, err
		}
	}
	items, err := httpkit.QueryJSON(r.Context(), a.server.Pool, "SELECT "+a.keyJSON("")+keyFrom+" WHERE k.id<$1 AND ($2::uuid IS NULL OR k.created_by=$2) AND ($3 OR k.project_id=ANY($4::uuid[])) ORDER BY k.id DESC LIMIT $5", p.Before, issuer, principal.AllProjects, principal.ProjectIDs(), p.Limit+1)
	return httpkit.ListReply(items, p), err
}
func (a *API) apiKey(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "api_key_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	var data []byte
	var etag string
	var projectID *string
	failures := a.overlaps.pendingFailures(id)
	if err = a.server.Pool.QueryRow(r.Context(), "SELECT "+a.keyJSON(overlapDetailFields)+",k.etag::text,k.project_id::text"+keyFrom+" WHERE k.id=$1", id, failures).Scan(&data, &etag, &projectID); err != nil {
		return httpkit.Reply{}, err
	}
	if err := p.Project(projectID, authz.View); err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.Detail(json.RawMessage(data), etag), nil
}
