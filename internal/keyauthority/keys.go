package keyauthority

import (
	"maps"
	"slices"
	"time"

	"github.com/tyk-swe/olp/internal/domain/attribution"
	"github.com/tyk-swe/olp/internal/limits"
)

type KeyPolicy struct {
	RunLimits               *limits.WorkflowBounds `json:"run_limits,omitempty"`
	AllowedToolsets         []string               `json:"allowed_toolsets,omitempty"`
	AllowedAgents           []string               `json:"allowed_agents,omitempty"`
	ForbidResponseCache     bool                   `json:"forbid_response_cache"`
	LimitTemplate           *string                `json:"limit_template"`
	RouteLimits             RouteLimits            `json:"route_limits"`
	RegionalLimits          RegionalLimits         `json:"regional_limits"`
	RotationIntervalDays    *int                   `json:"rotation_interval_days"`
	AllowedRouteGroups      []string               `json:"allowed_route_groups"`
	RequiredAttributionKeys []string               `json:"required_attribution_keys"`
	AttributionDefaults     map[string]string      `json:"attribution_defaults"`
	Scopes                  []string               `json:"scopes"`
	AllowedRoutes           []string               `json:"allowed_routes"`
	AllowedCIDRs            []string               `json:"allowed_cidrs"`
	AllowedAttributionKeys  []string               `json:"allowed_attribution_keys"`
	RequestsPerMinute       *int64                 `json:"requests_per_minute"`
	TokensPerMinute         *int64                 `json:"tokens_per_minute"`
	MaxConcurrency          *int64                 `json:"max_concurrency"`
	DailyCostLimit          *string                `json:"daily_cost_limit"`
	MonthlyCostLimit        *string                `json:"monthly_cost_limit"`
	WeeklyCostLimit         *string                `json:"weekly_cost_limit"`
	ExpiresAt               *time.Time             `json:"expires_at"`
	CostBasis               string                 `json:"cost_basis,omitempty"`
	AllowProviderState      bool                   `json:"allow_provider_state"`
	ResponseMetadata        bool                   `json:"response_metadata"`
	EndUserSource           *string                `json:"end_user_source"`
	EndUserPolicy           *EndUserPolicy         `json:"end_user_policy"`
	// Priority is the admission class the key's requests queue in, normal
	// when unset; MaxPriority is the highest class a request may choose
	// through the routing header, the key's own priority when unset.
	Priority    *string `json:"priority,omitempty"`
	MaxPriority *string `json:"max_priority,omitempty"`
}

func (p KeyPolicy) DefaultPriority() string {
	if p.Priority != nil {
		return *p.Priority
	}
	return "normal"
}

func (p KeyPolicy) PriorityCeiling() string {
	if p.MaxPriority != nil {
		return *p.MaxPriority
	}
	return p.DefaultPriority()
}

type DelegationAgentLimits struct {
	ID               string
	LookupID         string
	WorkloadIssuerID *string
	WorkloadDigest   string
	WorkloadRevision string
	WorkloadMapping  string
	Limits           AdmissionLimits
}

// Authority is durable input for the independent runtime authority refresh. Lookup never
// grants one positive scope through the other or applies unenforced limits.
// LookupID is the public lookup segment of the secret, which identifies the key
// in shared state that must never carry the key's internal identifier.
type Authority struct {
	SecretOverlap             *SecretOverlap `json:"-"`
	TaskCapability            *TaskCapability
	DelegationAgentLimits     *DelegationAgentLimits
	SnapshotID                string
	AgentPrincipal            bool
	DelegationAgentID         string
	DelegationMemberID        string
	DelegationPairDigest      string
	DelegationTokenDigest     string
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
	BudgetGroupCostBasis                                   string
	BudgetGroupDailyCostLimit                              *string
	BudgetGroupMonthlyCostLimit                            *string
	BudgetGroupWeeklyCostLimit                             *string
	Policy                                                 KeyPolicy
	ExpiresAt, RevokedAt, PausedAt                         *time.Time
}

type SecretOverlap struct {
	LookupID  string
	ExpiresAt time.Time
}

type TaskCapability struct {
	Digest           string
	RouteRevisions   []string
	ToolsetRevisions []string
}

type TaskDerivation struct {
	Identity         Authority
	SnapshotID       string
	WorkloadRevision string
	ExpiresAt        *time.Time
	EndUserDigest    string
	Capability       *TaskCapability
}

func (a Authority) TaskAuthority(spec TaskDerivation) Authority {
	next := a
	next.SecretOverlap = nil
	next.TaskCapability = spec.Capability
	next.SnapshotID = spec.SnapshotID
	next.WorkloadIssuerID = spec.Identity.WorkloadIssuerID
	next.WorkloadDigest = spec.Identity.WorkloadDigest
	next.WorkloadRevision = spec.WorkloadRevision
	next.WorkloadMapping = spec.Identity.WorkloadMapping
	next.EndUserDigest = spec.EndUserDigest
	next.ID = spec.Identity.ID
	next.LookupID = spec.Identity.LookupID
	next.LimitLookupID = spec.Identity.LimitLookupID
	next.ExpiresAt = spec.ExpiresAt
	next.RevokedAt = spec.Identity.RevokedAt
	next.PausedAt = spec.Identity.PausedAt
	return next
}

func (a Authority) BudgetAttribution() map[string]string {
	if a.DelegationPairDigest == "" {
		return a.Attribution
	}
	return maps.Collect(func(yield func(string, string) bool) {
		for key, value := range a.Attribution {
			if key != attribution.DelegationPairKey && !yield(key, value) {
				return
			}
		}
		yield(attribution.DelegationPairKey, a.DelegationPairDigest)
	})
}

type DelegationDerivation struct {
	Scopes      []string
	Routes      []string
	Toolsets    []string
	Agents      []string
	Agent       Authority
	MemberID    string
	PairDigest  string
	TokenDigest string
	ExpiresAt   time.Time
}

func (a Authority) Delegated(spec DelegationDerivation) *Authority {
	next := a
	next.SecretOverlap = nil
	next.TaskCapability = nil
	next.allowedGroupRoutes = nil
	next.DelegationAgentID = spec.Agent.ID
	next.DelegationMemberID = spec.MemberID
	next.DelegationPairDigest = spec.PairDigest
	next.DelegationTokenDigest = spec.TokenDigest
	next.DelegationAgentLimits = &DelegationAgentLimits{
		ID:               spec.Agent.ID,
		LookupID:         spec.Agent.LimitsLookup(),
		WorkloadIssuerID: spec.Agent.WorkloadIssuerID,
		WorkloadDigest:   spec.Agent.WorkloadDigest,
		WorkloadRevision: spec.Agent.WorkloadRevision,
		WorkloadMapping:  spec.Agent.WorkloadMapping,
		Limits: AdmissionLimits{
			RequestsPerMinute: spec.Agent.Policy.RequestsPerMinute,
			TokensPerMinute:   spec.Agent.Policy.TokensPerMinute,
			MaxConcurrency:    spec.Agent.Policy.MaxConcurrency,
			DailyCostLimit:    spec.Agent.Policy.DailyCostLimit,
			WeeklyCostLimit:   spec.Agent.Policy.WeeklyCostLimit,
			MonthlyCostLimit:  spec.Agent.Policy.MonthlyCostLimit,
			CostBasis:         spec.Agent.Policy.CostBasis,
		},
	}
	next.Policy.Scopes = slices.Clone(spec.Scopes)
	next.Policy.AllowedRoutes = slices.Clone(spec.Routes)
	next.Policy.AllowedRouteGroups = nil
	next.Policy.AllowedToolsets = slices.Clone(spec.Toolsets)
	next.Policy.AllowedAgents = slices.Clone(spec.Agents)
	next.Policy.ForbidResponseCache = a.Policy.ForbidResponseCache || spec.Agent.Policy.ForbidResponseCache
	next.Policy.AllowProviderState = a.Policy.AllowProviderState && spec.Agent.Policy.AllowProviderState
	expiresAt := spec.ExpiresAt
	next.ExpiresAt = &expiresAt
	return &next
}

func (a Authority) Allows(scope, route string, projectID *string, now time.Time) bool {
	if (a.ProjectID == nil) != (projectID == nil) || (a.ProjectID != nil && *a.ProjectID != *projectID) {
		return false
	}
	if scope != "inference" && scope != "models_read" && scope != "guardrails" {
		return false
	}
	if a.RevokedAt != nil || a.PausedAt != nil || (a.ExpiresAt != nil && !a.ExpiresAt.After(now)) {
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

type AggregateAccount struct{ Level, Key, Value string }

type CostBoundary struct {
	Owner     limits.Owner
	Lookup    string
	Budget    *limits.CostBudget
	Aggregate *AggregateAccount
}

type costLimits struct{ daily, weekly, monthly *string }

func (a Authority) costBoundary(owner limits.Owner, lookup string, l costLimits) CostBoundary {
	return CostBoundary{
		Owner: owner, Lookup: lookup,
		Budget: limits.Budget(owner, l.daily, l.weekly, l.monthly, a.BudgetIncreases[owner.ID]),
	}
}

func (a Authority) aggregateBoundary(account AggregateAccount, owner string, basis limits.Basis, l costLimits) CostBoundary {
	boundary := a.costBoundary(limits.Owner{ID: owner, Kind: limits.OwnerAggregate, Basis: basis}, limits.AggregateBudgetLookup(owner), l)
	return CostBoundary{Owner: boundary.Owner, Lookup: boundary.Lookup, Budget: boundary.Budget, Aggregate: &account}
}

func (a Authority) KeyCostBoundary() CostBoundary {
	return a.costBoundary(limits.Owner{ID: a.ID, Kind: limits.OwnerKey, Basis: limits.Basis(a.Policy.CostBasis)}, a.LimitsLookup(),
		costLimits{a.Policy.DailyCostLimit, a.Policy.WeeklyCostLimit, a.Policy.MonthlyCostLimit})
}

func (a Authority) delegationAgentBoundary() []CostBoundary {
	agent := a.DelegationAgentLimits
	if agent == nil {
		return nil
	}
	return []CostBoundary{a.costBoundary(limits.Owner{ID: agent.ID, Kind: limits.OwnerKey, Basis: limits.Basis(agent.Limits.CostBasis)}, agent.LookupID,
		costLimits{agent.Limits.DailyCostLimit, agent.Limits.WeeklyCostLimit, agent.Limits.MonthlyCostLimit})}
}

func (a Authority) groupBoundary() []CostBoundary {
	if a.BudgetGroupID == nil || !a.GroupLimits().CostBudgeted() {
		return nil
	}
	return []CostBoundary{a.costBoundary(limits.Owner{ID: *a.BudgetGroupID, Kind: limits.OwnerGroup, Basis: limits.Basis(a.BudgetGroupCostBasis)}, limits.BudgetGroupLookup(*a.BudgetGroupID),
		costLimits{a.BudgetGroupDailyCostLimit, a.BudgetGroupWeeklyCostLimit, a.BudgetGroupMonthlyCostLimit})}
}

func text(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func (p *BudgetPolicy) costLimits() costLimits {
	return costLimits{p.DailyCostLimit, p.WeeklyCostLimit, p.MonthlyCostLimit}
}

func (a Authority) aggregateBoundaries() ([]CostBoundary, bool) {
	boundaries := []CostBoundary{}
	for _, level := range [...]struct {
		name, subject string
		policy        *BudgetPolicy
	}{
		{"installation", a.InstallationID, a.InstallationBudget},
		{"organization", text(a.OrganizationID), a.OrganizationBudget},
		{"project", text(a.ProjectID), a.ProjectBudget},
	} {
		if !level.policy.Limited() {
			continue
		}
		if level.subject == "" {
			return nil, false
		}
		boundaries = append(boundaries, a.aggregateBoundary(AggregateAccount{Level: level.name, Key: level.subject},
			limits.AggregateBudgetID(level.name, level.subject), level.policy.Basis(), level.policy.costLimits()))
	}
	return boundaries, true
}

func (a Authority) attributionBoundary(key, value string, basis limits.Basis, l costLimits) CostBoundary {
	return a.aggregateBoundary(AggregateAccount{Level: "attribution", Key: key, Value: value},
		limits.AttributionBudgetID(*a.ProjectID, key, value), basis, l)
}

func (a Authority) attributionBoundaries() ([]CostBoundary, bool) {
	boundaries := []CostBoundary{}
	labels := a.BudgetAttribution()
	for _, key := range slices.Sorted(maps.Keys(labels)) {
		policy, found := a.ProjectAttributionBudgets[key][labels[key]]
		if !found || !policy.Limited() {
			continue
		}
		if a.ProjectID == nil {
			return nil, false
		}
		boundaries = append(boundaries, a.attributionBoundary(key, labels[key], policy.Basis(), policy.costLimits()))
	}
	pairPolicy, hasPairPolicy := a.ProjectAttributionBudgets[attribution.DelegationPairKey][a.DelegationPairDigest]
	if a.DelegationPairDigest == "" || hasPairPolicy && pairPolicy.Limited() {
		return boundaries, true
	}
	if a.ProjectID == nil {
		return nil, false
	}
	return append(boundaries, a.attributionBoundary(attribution.DelegationPairKey, a.DelegationPairDigest, limits.BasisCost, costLimits{})), true
}

func (a Authority) endUserBoundaries() ([]CostBoundary, bool) {
	boundaries := []CostBoundary{}
	if a.EndUserDigest == "" {
		return boundaries, true
	}
	projectPolicy, keyPolicy := a.ProjectEndUserPolicy.Limits(a.EndUserDigest), a.Policy.EndUserPolicy.Limits(a.EndUserDigest)
	if projectPolicy.CostBudgeted() {
		if a.ProjectID == nil {
			return nil, false
		}
		boundaries = append(boundaries, a.endUserBoundary(limits.EndUserProjectID(*a.ProjectID, a.EndUserDigest), projectPolicy))
	}
	if keyPolicy.CostBudgeted() {
		boundaries = append(boundaries, a.endUserBoundary(limits.EndUserKeyID(a.ID, a.EndUserDigest), keyPolicy))
	}
	return boundaries, true
}

type CostBoundaries struct {
	Boundaries []CostBoundary
	Resolved   bool
}

func (r CostBoundaries) CostBudgeted() bool {
	return slices.ContainsFunc(r.Boundaries, func(b CostBoundary) bool { return b.Budget != nil })
}

func (r CostBoundaries) Owners() []limits.Owner {
	owners := make([]limits.Owner, 0, len(r.Boundaries))
	for _, boundary := range r.Boundaries {
		owners = append(owners, boundary.Owner)
	}
	return owners
}

func (r CostBoundaries) Admissions() []limits.Boundary {
	admissions := make([]limits.Boundary, 0, len(r.Boundaries))
	for _, boundary := range r.Boundaries {
		admissions = append(admissions, limits.Boundary{Owner: boundary.Owner, Budget: boundary.Budget})
	}
	return admissions
}

func (a Authority) routeBoundary(route string) []CostBoundary {
	policy := a.Policy.RouteLimits[route]
	if route == "" || !policy.CostBudgeted() {
		return nil
	}
	owner := limits.KeyRouteBudgetID(a.ID, route)
	return []CostBoundary{a.costBoundary(limits.Owner{ID: owner, Kind: limits.OwnerAggregate, Basis: limits.Basis(policy.CostBasis)}, limits.AggregateBudgetLookup(owner),
		costLimits{policy.DailyCostLimit, policy.WeeklyCostLimit, policy.MonthlyCostLimit})}
}

func (a Authority) CostBoundaries(route string) CostBoundaries {
	aggregates, resolved := a.aggregateBoundaries()
	if !resolved {
		return CostBoundaries{}
	}
	attributed, resolved := a.attributionBoundaries()
	if !resolved {
		return CostBoundaries{}
	}
	endUsers, resolved := a.endUserBoundaries()
	if !resolved {
		return CostBoundaries{}
	}
	return CostBoundaries{
		Boundaries: slices.Concat([]CostBoundary{a.KeyCostBoundary()}, a.delegationAgentBoundary(), a.groupBoundary(), a.routeBoundary(route), aggregates, attributed, endUsers),
		Resolved:   true,
	}
}

func (a Authority) endUserBoundary(owner string, policy AdmissionLimits) CostBoundary {
	return a.costBoundary(limits.Owner{ID: owner, Kind: limits.OwnerEndUser, Basis: limits.Basis(policy.CostBasis)}, limits.EndUserLookup(owner),
		costLimits{policy.DailyCostLimit, policy.WeeklyCostLimit, policy.MonthlyCostLimit})
}
