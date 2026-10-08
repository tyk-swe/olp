package access

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/attribution"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/secrets"
)

type KeyPolicy struct {
	LimitTemplate           *string           `json:"limit_template"`
	RouteLimits             RouteLimits       `json:"route_limits"`
	RotationIntervalDays    *int              `json:"rotation_interval_days"`
	AllowedRouteGroups      []string          `json:"allowed_route_groups"`
	RequiredAttributionKeys []string          `json:"required_attribution_keys"`
	AttributionDefaults     map[string]string `json:"attribution_defaults"`
	Scopes                  []string          `json:"scopes"`
	AllowedRoutes           []string          `json:"allowed_routes"`
	AllowedCIDRs            []string          `json:"allowed_cidrs"`
	AllowedAttributionKeys  []string          `json:"allowed_attribution_keys"`
	RequestsPerMinute       *int64            `json:"requests_per_minute"`
	TokensPerMinute         *int64            `json:"tokens_per_minute"`
	MaxConcurrency          *int64            `json:"max_concurrency"`
	DailyCostLimit          *string           `json:"daily_cost_limit"`
	MonthlyCostLimit        *string           `json:"monthly_cost_limit"`
	WeeklyCostLimit         *string           `json:"weekly_cost_limit"`
	ExpiresAt               *time.Time        `json:"expires_at"`
	AllowProviderState      bool              `json:"allow_provider_state"`
	ResponseMetadata        bool              `json:"response_metadata"`
	EndUserSource           *string           `json:"end_user_source"`
	EndUserPolicy           *EndUserPolicy    `json:"end_user_policy"`
	// Priority is the admission class the key's requests queue in, normal
	// when unset; MaxPriority is the highest class a request may choose
	// through the routing header, the key's own priority when unset.
	Priority    *string `json:"priority,omitempty"`
	MaxPriority *string `json:"max_priority,omitempty"`
}

// Priorities are the admission classes, most urgent first.
var Priorities = []string{"critical", "high", "normal", "low"}

// PriorityRank orders admission classes from zero, the most urgent; it is
// -1 for a name that is no class.
func PriorityRank(name string) int { return slices.Index(Priorities, name) }

// DefaultPriority is the class the key's requests take unless they ask for
// another.
func (p KeyPolicy) DefaultPriority() string {
	if p.Priority != nil {
		return *p.Priority
	}
	return "normal"
}

// PriorityCeiling is the most urgent class a request may ask for.
func (p KeyPolicy) PriorityCeiling() string {
	if p.MaxPriority != nil {
		return *p.MaxPriority
	}
	return p.DefaultPriority()
}

type keyInput struct {
	Name          string  `json:"name"`
	ProjectID     *string `json:"project_id"`
	BudgetGroupID *string `json:"budget_group_id"`
	KeyPolicy
}

var RouteSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,99}$`)
var decimal = regexp.MustCompile(`^[0-9]{1,12}(\.[0-9]{1,12})?$`)
var attributionKey = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]{0,31}$`)

const maxAttributionKeys = 8

func validateKey(input keyInput, expirationChanged bool) error {
	if days := input.RotationIntervalDays; days != nil && (*days < 1 || *days > 3650) {
		return Invalid("rotation_interval_days", "Use a rotation interval of 1–3650 days, or null.")
	}
	if len(input.AllowedRouteGroups) > 64 {
		return Invalid("allowed_route_groups", "Use at most 64 route groups.")
	}
	groupNames := map[string]bool{}
	for _, name := range input.AllowedRouteGroups {
		if !RouteSlug.MatchString(name) || groupNames[name] {
			return Invalid("allowed_route_groups", "Use unique valid group names.")
		}
		groupNames[name] = true
	}
	policy := input.KeyPolicy.AttributionPolicy()
	if err := validateAttributionPolicy(&policy); err != nil {
		return err
	}
	if err := validTemplateName(input.LimitTemplate); err != nil {
		return err
	}
	if err := input.RouteLimits.Validate(); err != nil {
		return err
	}
	if err := validateKeyCIDRs(input.AllowedCIDRs); err != nil {
		return err
	}
	if input.EndUserPolicy != nil && input.EndUserSource == nil {
		return Invalid("end_user_source", "An end-user policy requires an identifier source.")
	}
	if err := input.EndUserPolicy.validate(); err != nil {
		return err
	}
	if input.EndUserSource != nil && *input.EndUserSource != "header" && *input.EndUserSource != "native" {
		return Invalid("end_user_source", "Use header, native, or null to disable end-user identification.")
	}
	if err := ValidText("name", input.Name, 100); err != nil {
		return err
	}
	if len(input.Scopes) < 1 || len(input.Scopes) > 2 {
		return Invalid("scopes", "Select at least one unique scope.")
	}
	seen := map[string]bool{}
	for _, scope := range input.Scopes {
		if (scope != "inference" && scope != "models_read") || seen[scope] {
			return Invalid("scopes", "Use unique inference and models_read scopes.")
		}
		seen[scope] = true
	}
	if input.AllowedRoutes == nil {
		return Invalid("allowed_routes", "This field cannot be null.")
	}
	if len(input.AllowedRoutes) > 100 {
		return Invalid("allowed_routes", "Use at most 100 route slugs.")
	}
	seen = map[string]bool{}
	for _, route := range input.AllowedRoutes {
		if !RouteSlug.MatchString(route) || seen[route] {
			return Invalid("allowed_routes", "Use unique valid route slugs.")
		}
		seen[route] = true
	}
	if input.AllowedAttributionKeys == nil {
		input.AllowedAttributionKeys = []string{}
	}
	if len(input.AllowedAttributionKeys) > maxAttributionKeys {
		return Invalid("allowed_attribution_keys", "Use at most 8 attribution keys.")
	}
	seen = map[string]bool{}
	for _, key := range input.AllowedAttributionKeys {
		if !attributionKey.MatchString(key) || seen[key] {
			return Invalid("allowed_attribution_keys", "Use unique valid attribution keys.")
		}
		seen[key] = true
	}
	for field, value := range map[string]*int64{"requests_per_minute": input.RequestsPerMinute, "max_concurrency": input.MaxConcurrency} {
		if value != nil && (*value < 1 || *value > 2147483647) {
			return Invalid(field, "Use a positive integer of at most 2147483647.")
		}
	}
	if input.TokensPerMinute != nil && (*input.TokensPerMinute < 1 || *input.TokensPerMinute > limits.MaxCounter) {
		return Invalid("tokens_per_minute", "Use a positive integer of at most 9007199254740991.")
	}
	for field, value := range map[string]*string{"daily_cost_limit": input.DailyCostLimit, "monthly_cost_limit": input.MonthlyCostLimit, "weekly_cost_limit": input.WeeklyCostLimit} {
		if value != nil {
			amount := strings.TrimSpace(*value)
			if !decimal.MatchString(amount) || !strings.ContainsAny(amount, "123456789") {
				return Invalid(field, "Use a positive decimal amount with at most 12 integer and 12 fractional digits.")
			}
			*value = amount
		}
	}
	for field, value := range map[string]*string{"priority": input.Priority, "max_priority": input.MaxPriority} {
		if value != nil && PriorityRank(*value) < 0 {
			return Invalid(field, "Use critical, high, normal or low.")
		}
	}
	if PriorityRank(input.DefaultPriority()) < PriorityRank(input.PriorityCeiling()) {
		return Invalid("priority", "The default priority cannot exceed max_priority.")
	}
	if expirationChanged && input.ExpiresAt != nil && !input.ExpiresAt.After(time.Now()) {
		return Invalid("expires_at", "Choose a future expiry.")
	}
	return nil
}
func ParseUUID(value string) (string, error) {
	id, err := uuid.Parse(value)
	if err != nil {
		return "", Invalid("id", "Use a valid UUID.")
	}
	return id.String(), nil
}

// parseProjectID canonicalises an optional project_id, so project lookups,
// ownership comparisons and idempotency fingerprints see one spelling.
func parseProjectID(value *string) (*string, error) {
	if value == nil {
		return nil, nil
	}
	id, err := uuid.Parse(*value)
	if err != nil {
		return nil, Invalid("project_id", "Use a valid UUID.")
	}
	canonical := id.String()
	return &canonical, nil
}

// AdvanceAuthority advances key authority, so every gateway reloads API keys
// and which credential versions may serve on its next poll.
func AdvanceAuthority(ctx context.Context, tx pgx.Tx) (any, error) {
	var id string
	var sequence int64
	err := tx.QueryRow(ctx, "UPDATE olp.installation SET authority_id=$1,authority_sequence=authority_sequence+1 WHERE singleton RETURNING authority_id::text,authority_sequence", NewID()).Scan(&id, &sequence)
	return map[string]any{"id": id, "sequence": sequence}, err
}

const keyFields = `'workload_issuer_id',k.workload_issuer_id,'workload_digest',k.workload_digest,'workload_mapping',k.workload_mapping,'rotation_interval_days',k.policy->'rotation_interval_days',
 'rotation_due_at',CASE WHEN k.policy->>'rotation_interval_days' IS NOT NULL THEN COALESCE(k.rotated_at,k.created_at)+make_interval(secs=>86400.0*(k.policy->>'rotation_interval_days')::int) END,
 'active_overlaps',COALESCE((SELECT jsonb_agg(jsonb_build_object('lookup_id',o.lookup_id,'expires_at',LEAST(o.expires_at,k.expires_at)) ORDER BY o.expires_at) FROM olp.api_key_overlaps o WHERE o.api_key_id=k.id AND LEAST(o.expires_at,k.expires_at)>now() AND k.revoked_at IS NULL),'[]'::jsonb),
 'id',k.id,'lookup_id',k.lookup_id,'name',k.name,'project_id',k.project_id,'project_name',pr.name,'budget_group_id',k.budget_group_id,'created_by',k.created_by,'created_by_email',u.email,'etag',k.etag,'created_at',k.created_at,'expires_at',k.expires_at,'revoked_at',k.revoked_at,'rotated_at',k.rotated_at,'scopes',k.policy->'scopes','allowed_routes',k.policy->'allowed_routes',
 'limit_template',k.policy->'limit_template','route_limits',COALESCE(NULLIF(k.policy->'route_limits','null'::jsonb),'{}'::jsonb),'allowed_route_groups',COALESCE(NULLIF(k.policy->'allowed_route_groups','null'::jsonb),'[]'::jsonb),'allowed_cidrs',COALESCE(NULLIF(k.policy->'allowed_cidrs','null'::jsonb),'[]'::jsonb),'requests_per_minute',k.policy->'requests_per_minute','tokens_per_minute',k.policy->'tokens_per_minute','max_concurrency',k.policy->'max_concurrency','allowed_attribution_keys',COALESCE(k.policy->'allowed_attribution_keys','[]'::jsonb),'allow_provider_state',COALESCE(k.policy->'allow_provider_state','false'::jsonb),'response_metadata',COALESCE(k.policy->'response_metadata','false'::jsonb),'required_attribution_keys',COALESCE(NULLIF(k.policy->'required_attribution_keys','null'::jsonb),'[]'::jsonb),'attribution_defaults',COALESCE(NULLIF(k.policy->'attribution_defaults','null'::jsonb),'{}'::jsonb),'end_user_policy',k.policy->'end_user_policy','end_user_source',k.policy->'end_user_source','priority',k.policy->'priority','max_priority',k.policy->'max_priority','effective_limits',k.effective_limits`
const keyFrom = " FROM olp.api_keys_with_limits k JOIN olp.users u ON u.id=k.created_by LEFT JOIN olp.projects pr ON pr.id=k.project_id"

// keyJSON renders one API key row, whose alias must be k, as the management
// contract's key detail. The budget is live accounting: accrued spend and
// unpriced attempts are summed from the recorded facts and their retained
// rollups, the amounts they are measured against come from the stored policy,
// and enforcement_active tells the console whether this installation admits
// requests against them at all.
func (s *Server) keyJSON() string {
	enforcement := "false"
	if s.LimitsEnforced {
		enforcement = "true"
	}
	return `jsonb_build_object(` + keyFields + `,'budget',olp.budget_allowances(k.id,k.effective_limits,jsonb_set(jsonb_set(jsonb_set(` + limits.BudgetSQL +
		`||jsonb_build_object('enforcement_active',` + enforcement +
		`),'{daily,limit}',COALESCE(k.policy->'daily_cost_limit','null'::jsonb)),'{monthly,limit}',COALESCE(k.policy->'monthly_cost_limit','null'::jsonb)),'{weekly,limit}',COALESCE(k.policy->'weekly_cost_limit','null'::jsonb))))`
}

func (s *Server) apiKeys(r *http.Request, principal Principal) (Reply, error) {
	p, err := Page(r)
	if err != nil {
		return Reply{}, err
	}
	var issuer any
	if raw := r.URL.Query().Get("created_by"); raw != "" {
		issuer, err = ParseUUID(raw)
		if err != nil {
			return Reply{}, err
		}
	}
	rows, err := s.Pool.Query(r.Context(), "SELECT "+s.keyJSON()+keyFrom+" WHERE k.id<$1 AND ($2::uuid IS NULL OR k.created_by=$2) AND ($3 OR k.project_id=ANY($4::uuid[])) ORDER BY k.id DESC LIMIT $5", p.Before, issuer, principal.AllProjects, principal.ProjectIDs(), p.Limit+1)
	if err != nil {
		return Reply{}, err
	}
	items, err := JSONRows(rows)
	return ListReply(items, p), err
}
func (s *Server) apiKey(r *http.Request, p Principal) (Reply, error) {
	id, err := IDParam(r, "api_key_id")
	if err != nil {
		return Reply{}, err
	}
	var data []byte
	var etag string
	var projectID *string
	if err = s.Pool.QueryRow(r.Context(), "SELECT "+s.keyJSON()+",k.etag::text,k.project_id::text"+keyFrom+" WHERE k.id=$1", id).Scan(&data, &etag, &projectID); err != nil {
		return Reply{}, err
	}
	if err := p.Project(projectID, View); err != nil {
		return Reply{}, err
	}
	return Detail(json.RawMessage(data), etag), nil
}
func (s *Server) createAPIKey(r *http.Request, _ Principal) (Reply, error) {
	input := keyInput{KeyPolicy: KeyPolicy{Scopes: []string{"inference"}, AllowedRoutes: []string{}, AllowedAttributionKeys: []string{}}}
	if err := Decode(r, &input); err != nil {
		return Reply{}, err
	}
	projectID, err := parseProjectID(input.ProjectID)
	if err != nil {
		return Reply{}, err
	}
	input.ProjectID = projectID
	tx, err := s.Begin(r)
	if err != nil {
		return Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.Reauthorize(r, tx)
	if err != nil {
		return Reply{}, err
	}
	claim, replayed, err := s.Replay(r, tx, p, input)
	if err != nil {
		return Reply{}, err
	}
	if replayed != nil {
		// The stored reply carries the plaintext secret, so the caller must
		// still reach the target project to receive it.
		if err := s.RequireProject(r.Context(), tx, p, input.ProjectID); err != nil {
			return Reply{}, err
		}
		return Commit(r, tx, *replayed)
	}
	// A replay returns the original result even if its key has since expired.
	if err := validateKey(input, true); err != nil {
		return Reply{}, err
	}
	if err = s.RequireProject(r.Context(), tx, p, input.ProjectID); err != nil {
		return Reply{}, err
	}
	if input.BudgetGroupID != nil {
		var parsed string
		if parsed, err = ParseUUID(*input.BudgetGroupID); err != nil {
			return Reply{}, err
		}
		input.BudgetGroupID = &parsed
	}
	if err = checkBudgetGroup(r.Context(), tx, input.BudgetGroupID, input.ProjectID); err != nil {
		return Reply{}, err
	}
	if err = checkLimitTemplates(r.Context(), tx, input.ProjectID, input.LimitTemplate, endUserTemplate(input.EndUserPolicy)); err != nil {
		return Reply{}, err
	}
	if err = checkKeyRouteGroups(r, tx, input.AllowedRouteGroups, input.ProjectID); err != nil {
		return Reply{}, err
	}
	if err = checkKeyRoutes(r, tx, input.AllowedRoutes, input.ProjectID); err != nil {
		return Reply{}, err
	}
	id, lookup, etag := NewID(), secrets.Token(), NewID()
	secret := "olp_" + lookup + "_" + secrets.Token()
	policy, err := json.Marshal(input.KeyPolicy)
	if err != nil {
		return Reply{}, err
	}
	if _, err = tx.Exec(r.Context(), "INSERT INTO olp.api_keys(id,lookup_id,digest,name,created_by,project_id,budget_group_id,policy,etag,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)", id, lookup, s.Auth.Digest(secrets.APIKeyDigest, secret), strings.TrimSpace(input.Name), p.UserID(), input.ProjectID, input.BudgetGroupID, policy, etag, input.ExpiresAt); err != nil {
		return Reply{}, err
	}
	if err = ensureRouteBudgetAccounts(r.Context(), tx, id, input.RouteLimits); err != nil {
		return Reply{}, err
	}
	generation, err := AdvanceAuthority(r.Context(), tx)
	if err != nil {
		return Reply{}, err
	}
	result := Reply{Status: 201, ETag: etag, Location: "/api/v1/api-keys/" + id, Body: map[string]any{"id": id, "lookup_id": lookup, "secret": secret, "runtime_generation": generation}}
	if err = Audit(r.Context(), tx, r, p.Actor(), "api_key.create", "api_key", id, "success"); err != nil {
		return Reply{}, err
	}
	if err = s.CompleteReplay(r, tx, claim, result); err != nil {
		return Reply{}, err
	}
	return Commit(r, tx, result)
}
func (s *Server) updateAPIKey(r *http.Request, _ Principal) (Reply, error) {
	var patch map[string]json.RawMessage
	if err := Decode(r, &patch); err != nil {
		return Reply{}, err
	}
	if patch == nil {
		return Reply{}, Invalid("policy", "Send a policy object.")
	}
	allowed := []string{"route_limits", "limit_template", "name", "scopes", "rotation_interval_days", "allowed_routes", "allowed_route_groups", "allowed_cidrs", "allowed_attribution_keys", "required_attribution_keys", "attribution_defaults", "requests_per_minute", "tokens_per_minute", "max_concurrency", "daily_cost_limit", "monthly_cost_limit", "weekly_cost_limit", "expires_at", "budget_group_id", "allow_provider_state", "response_metadata", "end_user_source", "end_user_policy", "priority", "max_priority"}
	for field, value := range patch {
		if !slices.Contains(allowed, field) {
			return Reply{}, Invalid(field, "Unknown policy field.")
		}
		if string(value) == "null" && (field == "name" || field == "scopes" || field == "allowed_routes") {
			return Reply{}, Invalid(field, "This field cannot be null.")
		}
	}
	id, err := IDParam(r, "api_key_id")
	if err != nil {
		return Reply{}, err
	}
	tx, err := s.Begin(r)
	if err != nil {
		return Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.Reauthorize(r, tx)
	if err != nil {
		return Reply{}, err
	}
	var data []byte
	var etag string
	var revoked *time.Time
	var projectID *string
	var groupID *string
	// Seed expiry from the authoritative column, so an omitted patch field
	// preserves it even when the policy JSON is missing or has a stale value.
	if err = tx.QueryRow(r.Context(), "SELECT policy||jsonb_build_object('name',name,'expires_at',expires_at),etag::text,revoked_at,project_id::text,budget_group_id::text FROM olp.api_keys WHERE id=$1", id).Scan(&data, &etag, &revoked, &projectID, &groupID); err != nil {
		return Reply{}, err
	}
	if err := p.Project(projectID, Change); err != nil {
		return Reply{}, err
	}
	var managed bool
	if err = tx.QueryRow(r.Context(), "SELECT workload_issuer_id IS NOT NULL FROM olp.api_keys WHERE id=$1", id).Scan(&managed); err != nil {
		return Reply{}, err
	}
	if managed {
		return Reply{}, Fail(409, "workload_managed", "Edit this principal's workload issuer mapping instead.")
	}
	if err = Match(r, etag); err != nil {
		return Reply{}, err
	}
	if revoked != nil {
		return Reply{}, Fail(409, "api_key_revoked", "A revoked key cannot be edited.")
	}
	var merged map[string]json.RawMessage
	if err = json.Unmarshal(data, &merged); err != nil {
		return Reply{}, err
	}
	maps.Copy(merged, patch)
	data, err = json.Marshal(merged)
	if err != nil {
		return Reply{}, err
	}
	var input keyInput
	if err = json.Unmarshal(data, &input); err != nil {
		return Reply{}, Invalid("policy", "Invalid policy value.")
	}
	_, changed := patch["expires_at"]
	if err = validateKey(input, changed); err != nil {
		return Reply{}, err
	}
	if raw, ok := patch["budget_group_id"]; ok {
		if err = json.Unmarshal(raw, &groupID); err != nil {
			return Reply{}, Invalid("budget_group_id", "Use a budget group UUID or null.")
		}
		if groupID != nil {
			var parsed string
			if parsed, err = ParseUUID(*groupID); err != nil {
				return Reply{}, err
			}
			groupID = &parsed
		}
		if err = checkBudgetGroup(r.Context(), tx, groupID, projectID); err != nil {
			return Reply{}, err
		}
	}
	if err = checkLimitTemplates(r.Context(), tx, projectID, input.LimitTemplate, endUserTemplate(input.EndUserPolicy)); err != nil {
		return Reply{}, err
	}
	if err = checkKeyRouteGroups(r, tx, input.AllowedRouteGroups, projectID); err != nil {
		return Reply{}, err
	}
	if err = checkKeyRoutes(r, tx, input.AllowedRoutes, projectID); err != nil {
		return Reply{}, err
	}
	data, err = json.Marshal(input.KeyPolicy)
	if err != nil {
		return Reply{}, err
	}
	etag = NewID()
	if _, err = tx.Exec(r.Context(), "UPDATE olp.api_keys SET name=$1,policy=$2,expires_at=$3,budget_group_id=$4,etag=$5 WHERE id=$6", strings.TrimSpace(input.Name), data, input.ExpiresAt, groupID, etag, id); err != nil {
		return Reply{}, err
	}
	if err = ensureRouteBudgetAccounts(r.Context(), tx, id, input.RouteLimits); err != nil {
		return Reply{}, err
	}
	generation, err := AdvanceAuthority(r.Context(), tx)
	if err != nil {
		return Reply{}, err
	}
	if err = Audit(r.Context(), tx, r, p.Actor(), "api_key.update", "api_key", id, "success"); err != nil {
		return Reply{}, err
	}
	return Commit(r, tx, Detail(map[string]any{"etag": etag, "runtime_generation": generation}, etag))
}
func (s *Server) revokeAPIKey(r *http.Request, _ Principal) (Reply, error) {
	return s.transitionKey(r, false)
}
func (s *Server) rotateAPIKey(r *http.Request, _ Principal) (Reply, error) {
	return s.transitionKey(r, true)
}
func (s *Server) transitionKey(r *http.Request, rotate bool) (Reply, error) {
	var overlapSeconds int
	var overlapUntil *time.Time
	var input map[string]json.RawMessage
	if rotate && r.ContentLength != 0 {
		if err := Decode(r, &input); err != nil {
			return Reply{}, err
		}
		for field := range input {
			if field != "daily_cost_limit" && field != "monthly_cost_limit" && field != "weekly_cost_limit" && field != "budget_group_id" && field != "overlap_seconds" {
				return Reply{}, Invalid(field, "Only cost budgets and overlap_seconds may accompany rotation.")
			}
		}
	}
	if raw, ok := input["overlap_seconds"]; ok {
		if string(raw) == "null" || json.Unmarshal(raw, &overlapSeconds) != nil || overlapSeconds < 0 || overlapSeconds > 86400 {
			return Reply{}, Invalid("overlap_seconds", "Use 0–86400 seconds of overlap.")
		}
	}
	id, err := IDParam(r, "api_key_id")
	if err != nil {
		return Reply{}, err
	}
	tx, err := s.Begin(r)
	if err != nil {
		return Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.Reauthorize(r, tx)
	if err != nil {
		return Reply{}, err
	}
	var etag, name string
	var data []byte
	var revoked *time.Time
	var projectID *string
	var groupID *string
	if err = tx.QueryRow(r.Context(), "SELECT etag::text,name,policy,revoked_at,project_id::text,budget_group_id::text FROM olp.api_keys WHERE id=$1 FOR UPDATE", id).Scan(&etag, &name, &data, &revoked, &projectID, &groupID); err != nil {
		return Reply{}, err
	}
	// A stored replay returns the rotated secret, so the caller must still
	// reach this key's project before it is accepted.
	if err := p.Project(projectID, Change); err != nil {
		return Reply{}, err
	}
	if rotate {
		var managed bool
		if err = tx.QueryRow(r.Context(), "SELECT workload_issuer_id IS NOT NULL FROM olp.api_keys WHERE id=$1", id).Scan(&managed); err != nil {
			return Reply{}, err
		}
		if managed {
			return Reply{}, Fail(409, "workload_managed", "Workload principals do not have API secrets to rotate.")
		}
	}
	claim, replayed, err := s.Replay(r, tx, p, input)
	if err != nil {
		return Reply{}, err
	}
	if replayed != nil {
		return Commit(r, tx, *replayed)
	}
	if err = Match(r, etag); err != nil {
		return Reply{}, err
	}
	if revoked != nil {
		return Reply{}, Fail(409, "api_key_revoked", "This key is already revoked.")
	}
	etag = NewID()
	action := "api_key.revoke"
	var secret, lookup string
	if rotate {
		if raw, ok := input["budget_group_id"]; ok {
			delete(input, "budget_group_id")
			if err = json.Unmarshal(raw, &groupID); err != nil {
				return Reply{}, Invalid("budget_group_id", "Use a budget group UUID or null.")
			}
			if groupID != nil {
				var parsed string
				if parsed, err = ParseUUID(*groupID); err != nil {
					return Reply{}, err
				}
				groupID = &parsed
			}
			if err = checkBudgetGroup(r.Context(), tx, groupID, projectID); err != nil {
				return Reply{}, err
			}
		}
		delete(input, "overlap_seconds")
		if _, err = tx.Exec(r.Context(), "DELETE FROM olp.api_key_overlaps WHERE api_key_id=$1 AND (expires_at<=now() OR $2=0)", id, overlapSeconds); err != nil {
			return Reply{}, err
		}
		if overlapSeconds > 0 {
			var count int
			if err = tx.QueryRow(r.Context(), "SELECT count(*) FROM olp.api_key_overlaps WHERE api_key_id=$1", id).Scan(&count); err != nil {
				return Reply{}, err
			}
			if count >= 8 {
				return Reply{}, Fail(409, "key_overlap_limit", "Eight previous secrets are still active. Wait for an overlap to end, or rotate with zero overlap.")
			}
			if err = tx.QueryRow(r.Context(), `INSERT INTO olp.api_key_overlaps(lookup_id,api_key_id,digest,expires_at)
     SELECT lookup_id,id,digest,LEAST(now()+make_interval(secs=>$2),expires_at) FROM olp.api_keys WHERE id=$1 RETURNING expires_at`, id, overlapSeconds).Scan(&overlapUntil); err != nil {
				return Reply{}, err
			}
		}
		var policy map[string]json.RawMessage
		if err = json.Unmarshal(data, &policy); err != nil {
			return Reply{}, err
		}
		maps.Copy(policy, input)
		data, err = json.Marshal(policy)
		if err != nil {
			return Reply{}, err
		}
		var normalized KeyPolicy
		if err = json.Unmarshal(data, &normalized); err != nil {
			return Reply{}, Invalid("policy", "Invalid cost budget.")
		}
		if err = validateKey(keyInput{Name: name, KeyPolicy: normalized}, false); err != nil {
			return Reply{}, err
		}
		data, err = json.Marshal(normalized)
		if err != nil {
			return Reply{}, err
		}
		action = "api_key.rotate"
		lookup = secrets.Token()
		secret = "olp_" + lookup + "_" + secrets.Token()
		_, err = tx.Exec(r.Context(), "UPDATE olp.api_keys SET limit_lookup_id=COALESCE(limit_lookup_id,lookup_id),lookup_id=$1,digest=$2,etag=$3,rotated_at=now(),policy=$4,budget_group_id=$5 WHERE id=$6", lookup, s.Auth.Digest(secrets.APIKeyDigest, secret), etag, data, groupID, id)
	} else {
		_, err = tx.Exec(r.Context(), "UPDATE olp.api_keys SET revoked_at=now(),etag=$1 WHERE id=$2", etag, id)
	}
	if err != nil {
		return Reply{}, err
	}
	generation, err := AdvanceAuthority(r.Context(), tx)
	if err != nil {
		return Reply{}, err
	}
	result := Detail(generation, etag)
	if rotate {
		result.Body = map[string]any{"id": id, "etag": etag, "lookup_id": lookup, "secret": secret, "runtime_generation": generation, "overlap_expires_at": overlapUntil}
	}
	if err = Audit(r.Context(), tx, r, p.Actor(), action, "api_key", id, "success"); err != nil {
		return Reply{}, err
	}
	if err = s.CompleteReplay(r, tx, claim, result); err != nil {
		return Reply{}, err
	}
	return Commit(r, tx, result)
}

func checkKeyRoutes(r *http.Request, q Queryer, routes []string, projectID *string) error {
	if len(routes) == 0 {
		return nil
	}
	var mismatched bool
	if err := q.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM (SELECT slug,project_id FROM olp.routes UNION ALL SELECT slug,project_id FROM olp.code_routes) r WHERE slug=ANY($1::text[]) AND project_id IS DISTINCT FROM $2::uuid)", routes, projectID).Scan(&mismatched); err != nil {
		return err
	}
	if mismatched {
		return Invalid("allowed_routes", "Allowed routes must belong to the key's project.")
	}
	return nil
}

// Authority is durable input for the independent runtime authority refresh. Lookup never
// grants one positive scope through the other or applies unenforced limits.
// LookupID is the public lookup segment of the secret, which identifies the key
// in shared state that must never carry the key's internal identifier.
type Authority struct {
	WorkloadIssuerID          *string
	WorkloadDigest            string
	WorkloadRevision          string
	WorkloadMapping           string
	BudgetIncreases           map[string]string
	OrganizationID            *string
	OrganizationBudget        *BudgetPolicy
	InstallationID            string
	InstallationBudget        *BudgetPolicy
	ProjectBudget             *BudgetPolicy
	ProjectAttributionBudgets AttributionBudgets
	// Attribution is resolved request-local metadata, never cached key authority.
	Attribution map[string]string

	allowedGroupRoutes       map[string]struct{}
	ProjectAttributionPolicy attribution.Policy
	// EndUserDigest is request-local, never part of the cached key authority.
	EndUserDigest                                          string
	ProjectEndUserPolicy                                   *EndUserPolicy
	ID, Issuer, LookupID                                   string
	LimitLookupID                                          string
	ProjectID                                              *string
	BudgetGroupTemplate                                    *string
	BudgetGroupRPM, BudgetGroupTPM, BudgetGroupConcurrency *int64
	BudgetGroupID                                          *string
	BudgetGroupDailyCostLimit                              *string
	BudgetGroupMonthlyCostLimit                            *string
	BudgetGroupWeeklyCostLimit                             *string
	Policy                                                 KeyPolicy
	ExpiresAt, RevokedAt                                   *time.Time
}

func (a Authority) Allows(scope, route string, projectID *string, now time.Time) bool {
	if (a.ProjectID == nil) != (projectID == nil) || (a.ProjectID != nil && *a.ProjectID != *projectID) {
		return false
	}
	if scope != "inference" && scope != "models_read" {
		return false
	}
	if a.RevokedAt != nil || (a.ExpiresAt != nil && !a.ExpiresAt.After(now)) {
		return false
	}
	return slices.Contains(a.Policy.Scopes, scope) &&
		a.allowsRoute(route)
}

// LimitsLookup remains stable across secret rotations, sharing counters with
// requests already in flight and every still-valid secret for this key.
func (a Authority) LimitsLookup() string {
	if a.LimitLookupID != "" {
		return a.LimitLookupID
	}
	return a.LookupID
}
