package authority

import (
	"github.com/tyk-swe/olp/internal/attribution"
	"github.com/tyk-swe/olp/internal/limits"
	"maps"
	"slices"
	"time"
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
	ExpiresAt, RevokedAt                                   *time.Time
}

type TaskCapability struct {
	Digest           string
	RouteRevisions   []string
	ToolsetRevisions []string
}

func (a Authority) TaskAuthority(identity Authority, snapshot, workloadRevision string, expiry *time.Time, EndUser string, capability *TaskCapability) Authority {
	return Authority{
		TaskCapability:              capability,
		DelegationAgentLimits:       a.DelegationAgentLimits,
		SnapshotID:                  snapshot,
		AgentPrincipal:              a.AgentPrincipal,
		DelegationAgentID:           a.DelegationAgentID,
		DelegationMemberID:          a.DelegationMemberID,
		DelegationPairDigest:        a.DelegationPairDigest,
		DelegationTokenDigest:       a.DelegationTokenDigest,
		WorkloadIssuerID:            identity.WorkloadIssuerID,
		WorkloadDigest:              identity.WorkloadDigest,
		WorkloadRevision:            workloadRevision,
		WorkloadMapping:             identity.WorkloadMapping,
		BudgetIncreases:             a.BudgetIncreases,
		OrganizationID:              a.OrganizationID,
		OrganizationBudget:          a.OrganizationBudget,
		InstallationID:              a.InstallationID,
		InstallationBudget:          a.InstallationBudget,
		ProjectBudget:               a.ProjectBudget,
		ProjectAttributionBudgets:   a.ProjectAttributionBudgets,
		Attribution:                 a.Attribution,
		allowedGroupRoutes:          a.allowedGroupRoutes,
		ProjectAttributionPolicy:    a.ProjectAttributionPolicy,
		EndUserDigest:               EndUser,
		ProjectEndUserPolicy:        a.ProjectEndUserPolicy,
		ID:                          identity.ID,
		Issuer:                      a.Issuer,
		LookupID:                    identity.LookupID,
		LimitLookupID:               identity.LimitLookupID,
		ProjectID:                   a.ProjectID,
		BudgetGroupTemplate:         a.BudgetGroupTemplate,
		BudgetGroupRPM:              a.BudgetGroupRPM,
		BudgetGroupTPM:              a.BudgetGroupTPM,
		BudgetGroupConcurrency:      a.BudgetGroupConcurrency,
		BudgetGroupID:               a.BudgetGroupID,
		BudgetGroupCostBasis:        a.BudgetGroupCostBasis,
		BudgetGroupDailyCostLimit:   a.BudgetGroupDailyCostLimit,
		BudgetGroupMonthlyCostLimit: a.BudgetGroupMonthlyCostLimit,
		BudgetGroupWeeklyCostLimit:  a.BudgetGroupWeeklyCostLimit,
		Policy:                      a.Policy,
		ExpiresAt:                   expiry,
		RevokedAt:                   identity.RevokedAt,
	}
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

func (a Authority) Delegated(scopes, routes, toolsets, agents []string, agent Authority, memberID, pairDigest, tokenDigest string, expiresAt time.Time) *Authority {
	return &Authority{
		SnapshotID:            a.SnapshotID,
		AgentPrincipal:        a.AgentPrincipal,
		DelegationAgentID:     agent.ID,
		DelegationMemberID:    memberID,
		DelegationPairDigest:  pairDigest,
		DelegationTokenDigest: tokenDigest,
		DelegationAgentLimits: &DelegationAgentLimits{ID: agent.ID, LookupID: agent.LimitsLookup(), WorkloadIssuerID: agent.WorkloadIssuerID, WorkloadDigest: agent.WorkloadDigest, WorkloadRevision: agent.WorkloadRevision, WorkloadMapping: agent.WorkloadMapping, Limits: AdmissionLimits{
			RequestsPerMinute: agent.Policy.RequestsPerMinute,
			TokensPerMinute:   agent.Policy.TokensPerMinute,
			MaxConcurrency:    agent.Policy.MaxConcurrency,
			DailyCostLimit:    agent.Policy.DailyCostLimit,
			WeeklyCostLimit:   agent.Policy.WeeklyCostLimit,
			MonthlyCostLimit:  agent.Policy.MonthlyCostLimit,
			CostBasis:         agent.Policy.CostBasis,
		}},
		WorkloadIssuerID:          a.WorkloadIssuerID,
		WorkloadDigest:            a.WorkloadDigest,
		WorkloadRevision:          a.WorkloadRevision,
		WorkloadMapping:           a.WorkloadMapping,
		BudgetIncreases:           a.BudgetIncreases,
		OrganizationID:            a.OrganizationID,
		OrganizationBudget:        a.OrganizationBudget,
		InstallationID:            a.InstallationID,
		InstallationBudget:        a.InstallationBudget,
		ProjectBudget:             a.ProjectBudget,
		ProjectAttributionBudgets: a.ProjectAttributionBudgets,
		Attribution:               a.Attribution,
		ProjectAttributionPolicy:  a.ProjectAttributionPolicy,
		EndUserDigest:             a.EndUserDigest,
		ProjectEndUserPolicy:      a.ProjectEndUserPolicy,
		ID:                        a.ID, Issuer: a.Issuer, LookupID: a.LookupID, LimitLookupID: a.LimitLookupID,
		ProjectID:                   a.ProjectID,
		BudgetGroupTemplate:         a.BudgetGroupTemplate,
		BudgetGroupRPM:              a.BudgetGroupRPM,
		BudgetGroupTPM:              a.BudgetGroupTPM,
		BudgetGroupConcurrency:      a.BudgetGroupConcurrency,
		BudgetGroupID:               a.BudgetGroupID,
		BudgetGroupCostBasis:        a.BudgetGroupCostBasis,
		BudgetGroupDailyCostLimit:   a.BudgetGroupDailyCostLimit,
		BudgetGroupMonthlyCostLimit: a.BudgetGroupMonthlyCostLimit,
		BudgetGroupWeeklyCostLimit:  a.BudgetGroupWeeklyCostLimit,
		Policy: KeyPolicy{
			RunLimits: a.Policy.RunLimits,
			Scopes:    slices.Clone(scopes), AllowedRoutes: slices.Clone(routes), AllowedToolsets: slices.Clone(toolsets), AllowedAgents: slices.Clone(agents),
			ForbidResponseCache:     a.Policy.ForbidResponseCache || agent.Policy.ForbidResponseCache,
			LimitTemplate:           a.Policy.LimitTemplate,
			RouteLimits:             a.Policy.RouteLimits,
			RegionalLimits:          a.Policy.RegionalLimits,
			RotationIntervalDays:    a.Policy.RotationIntervalDays,
			RequiredAttributionKeys: a.Policy.RequiredAttributionKeys,
			AttributionDefaults:     a.Policy.AttributionDefaults,
			AllowedCIDRs:            a.Policy.AllowedCIDRs,
			AllowedAttributionKeys:  a.Policy.AllowedAttributionKeys,
			RequestsPerMinute:       a.Policy.RequestsPerMinute,
			TokensPerMinute:         a.Policy.TokensPerMinute,
			MaxConcurrency:          a.Policy.MaxConcurrency,
			DailyCostLimit:          a.Policy.DailyCostLimit,
			MonthlyCostLimit:        a.Policy.MonthlyCostLimit,
			WeeklyCostLimit:         a.Policy.WeeklyCostLimit,
			ExpiresAt:               a.Policy.ExpiresAt,
			CostBasis:               a.Policy.CostBasis,
			AllowProviderState:      a.Policy.AllowProviderState && agent.Policy.AllowProviderState,
			ResponseMetadata:        a.Policy.ResponseMetadata,
			EndUserSource:           a.Policy.EndUserSource,
			EndUserPolicy:           a.Policy.EndUserPolicy,
			Priority:                a.Policy.Priority, MaxPriority: a.Policy.MaxPriority,
		},
		ExpiresAt: &expiresAt, RevokedAt: a.RevokedAt,
	}
}

func (a Authority) Allows(scope, route string, projectID *string, now time.Time) bool {
	if (a.ProjectID == nil) != (projectID == nil) || (a.ProjectID != nil && *a.ProjectID != *projectID) {
		return false
	}
	if scope != "inference" && scope != "models_read" && scope != "guardrails" {
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

func (a Authority) KeyDebitOwner() limits.DebitOwner {
	return limits.DebitOwner{ID: a.ID, Kind: "key", Basis: limits.Basis(a.Policy.CostBasis)}
}

func (a Authority) KeyDebitRequest(leaseTTL time.Duration) limits.Request {
	return limits.Request{
		CostOwnerID: a.ID, CostBasis: limits.Basis(a.Policy.CostBasis), LookupID: a.LimitsLookup(),
		CostIncreases:    a.BudgetIncreases[a.ID],
		DailyCostLimit:   a.Policy.DailyCostLimit,
		WeeklyCostLimit:  a.Policy.WeeklyCostLimit,
		MonthlyCostLimit: a.Policy.MonthlyCostLimit,
		LeaseTTL:         leaseTTL,
	}
}

type ToolDebitBoundary struct {
	Owner          limits.DebitOwner
	Request        limits.Request
	AggregateLevel string
	AggregateKey   string
	AggregateValue string
}

func (a *DelegationAgentLimits) CostDebitBoundary(leaseTTL time.Duration, increases map[string]string) *ToolDebitBoundary {
	if a == nil {
		return nil
	}
	return &ToolDebitBoundary{
		Owner: limits.DebitOwner{ID: a.ID, Kind: "key", Basis: limits.Basis(a.Limits.CostBasis)},
		Request: limits.Request{
			CostOwnerID: a.ID, LookupID: a.LookupID, CostBasis: limits.Basis(a.Limits.CostBasis), CostIncreases: increases[a.ID],
			DailyCostLimit: a.Limits.DailyCostLimit, WeeklyCostLimit: a.Limits.WeeklyCostLimit,
			MonthlyCostLimit: a.Limits.MonthlyCostLimit, LeaseTTL: leaseTTL,
		},
	}
}

type ToolDebitBoundariesResult struct {
	Boundaries   []ToolDebitBoundary
	CostBudgeted bool
	Resolved     bool
}

func (r ToolDebitBoundariesResult) Owners() []limits.DebitOwner {
	owners := make([]limits.DebitOwner, 0, len(r.Boundaries))
	for _, boundary := range r.Boundaries {
		owners = append(owners, boundary.Owner)
	}
	return owners
}

func (r ToolDebitBoundariesResult) Requests() []limits.Request {
	requests := make([]limits.Request, 0, len(r.Boundaries))
	for _, boundary := range r.Boundaries {
		requests = append(requests, boundary.Request)
	}
	return requests
}

func (a Authority) ToolDebitBoundaries(leaseTTL time.Duration) ToolDebitBoundariesResult {
	boundaries := []ToolDebitBoundary{{Owner: a.KeyDebitOwner(), Request: a.KeyDebitRequest(leaseTTL)}}
	add := func(owner limits.DebitOwner, request limits.Request) {
		boundaries = append(boundaries, ToolDebitBoundary{Owner: owner, Request: request})
	}
	addAggregate := func(owner limits.DebitOwner, request limits.Request, level, key, value string) {
		boundaries = append(boundaries, ToolDebitBoundary{Owner: owner, Request: request, AggregateLevel: level, AggregateKey: key, AggregateValue: value})
	}
	if agent := a.DelegationAgentLimits.CostDebitBoundary(leaseTTL, a.BudgetIncreases); agent != nil {
		add(agent.Owner, agent.Request)
	}
	if a.BudgetGroupID != nil && a.GroupLimits().CostBudgeted() {
		add(limits.DebitOwner{ID: *a.BudgetGroupID, Kind: "group", Basis: limits.Basis(a.BudgetGroupCostBasis)}, limits.Request{
			CostOwnerID: *a.BudgetGroupID, CostBasis: limits.Basis(a.BudgetGroupCostBasis),
			LookupID: limits.BudgetGroupLookup(*a.BudgetGroupID), CostIncreases: a.BudgetIncreases[*a.BudgetGroupID],
			DailyCostLimit: a.BudgetGroupDailyCostLimit, WeeklyCostLimit: a.BudgetGroupWeeklyCostLimit,
			MonthlyCostLimit: a.BudgetGroupMonthlyCostLimit, LeaseTTL: leaseTTL,
		})
	}
	subject := func(value *string) string {
		if value == nil {
			return ""
		}
		return *value
	}
	for _, boundary := range [...]struct {
		level, subject string
		policy         *BudgetPolicy
	}{
		{"installation", a.InstallationID, a.InstallationBudget},
		{"organization", subject(a.OrganizationID), a.OrganizationBudget},
		{"project", subject(a.ProjectID), a.ProjectBudget},
	} {
		if !boundary.policy.Limited() {
			continue
		}
		if boundary.subject == "" {
			return ToolDebitBoundariesResult{}
		}
		owner := limits.AggregateBudgetID(boundary.level, boundary.subject)
		addAggregate(limits.DebitOwner{ID: owner, Kind: "aggregate", Basis: boundary.policy.Basis()}, limits.Request{
			CostOwnerID: owner, CostBasis: boundary.policy.Basis(), LookupID: limits.AggregateBudgetLookup(owner),
			CostIncreases:  a.BudgetIncreases[owner],
			DailyCostLimit: boundary.policy.DailyCostLimit, WeeklyCostLimit: boundary.policy.WeeklyCostLimit,
			MonthlyCostLimit: boundary.policy.MonthlyCostLimit, LeaseTTL: leaseTTL,
		}, boundary.level, boundary.subject, "")
	}
	labels := a.BudgetAttribution()
	for _, key := range slices.Sorted(maps.Keys(labels)) {
		policy, found := a.ProjectAttributionBudgets[key][labels[key]]
		if !found || !policy.Limited() {
			continue
		}
		if a.ProjectID == nil {
			return ToolDebitBoundariesResult{}
		}
		owner := limits.AttributionBudgetID(*a.ProjectID, key, labels[key])
		addAggregate(limits.DebitOwner{ID: owner, Kind: "aggregate", Basis: policy.Basis()}, limits.Request{
			CostOwnerID: owner, CostBasis: policy.Basis(), LookupID: limits.AggregateBudgetLookup(owner),
			CostIncreases:  a.BudgetIncreases[owner],
			DailyCostLimit: policy.DailyCostLimit, WeeklyCostLimit: policy.WeeklyCostLimit,
			MonthlyCostLimit: policy.MonthlyCostLimit, LeaseTTL: leaseTTL,
		}, "attribution", key, labels[key])
	}
	pairPolicy, hasPairPolicy := a.ProjectAttributionBudgets[attribution.DelegationPairKey][a.DelegationPairDigest]
	if a.DelegationPairDigest != "" && (!hasPairPolicy || !pairPolicy.Limited()) {
		if a.ProjectID == nil {
			return ToolDebitBoundariesResult{}
		}
		owner := limits.AttributionBudgetID(*a.ProjectID, attribution.DelegationPairKey, a.DelegationPairDigest)
		addAggregate(limits.DebitOwner{ID: owner, Kind: "aggregate", Basis: limits.BasisCost}, limits.Request{
			CostOwnerID: owner, CostBasis: limits.BasisCost, LookupID: limits.AggregateBudgetLookup(owner), LeaseTTL: leaseTTL,
		}, "attribution", attribution.DelegationPairKey, a.DelegationPairDigest)
	}
	if a.EndUserDigest != "" {
		for _, boundary := range [...]struct {
			policy  *EndUserPolicy
			project bool
		}{
			{a.ProjectEndUserPolicy, true}, {a.Policy.EndUserPolicy, false},
		} {
			policy := boundary.policy.Limits(a.EndUserDigest)
			if !policy.CostBudgeted() {
				continue
			}
			owner := limits.EndUserKeyID(a.ID, a.EndUserDigest)
			if boundary.project {
				if a.ProjectID == nil {
					return ToolDebitBoundariesResult{}
				}
				owner = limits.EndUserProjectID(*a.ProjectID, a.EndUserDigest)
			}
			add(limits.DebitOwner{ID: owner, Kind: "end_user", Basis: limits.Basis(policy.CostBasis)}, limits.Request{
				CostOwnerID: owner, CostBasis: limits.Basis(policy.CostBasis), LookupID: limits.EndUserLookup(owner),
				CostIncreases:  a.BudgetIncreases[owner],
				DailyCostLimit: policy.DailyCostLimit, WeeklyCostLimit: policy.WeeklyCostLimit,
				MonthlyCostLimit: policy.MonthlyCostLimit, LeaseTTL: leaseTTL,
			})
		}
	}
	costBudgeted := false
	for _, boundary := range boundaries {
		costBudgeted = costBudgeted || boundary.Request.HasCostBudget()
	}
	return ToolDebitBoundariesResult{Boundaries: boundaries, CostBudgeted: costBudgeted, Resolved: true}
}
