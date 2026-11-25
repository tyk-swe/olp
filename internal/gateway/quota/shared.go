package quota

import (
	"context"
	"iter"
	"maps"
	"slices"
	"time"

	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/limits/admission"
	"github.com/tyk-swe/olp/internal/routing"
)

type sharedAssignment struct {
	lookup          string
	member          string
	tokensPerMinute int64
}

func sharedQuotaFor(provider *routing.Provider, model string) (sharedAssignment, bool) {
	quota, groupID, ok := provider.SharedTokenQuotaFor(model)
	if !ok {
		return sharedAssignment{}, false
	}
	return sharedAssignment{
		lookup:          limits.SharedQuotaLookup(groupID),
		member:          limits.SharedQuotaMember(provider.ID, quota.Model),
		tokensPerMinute: quota.TokensPerMinute,
	}, true
}

func sharedRequest(assignment sharedAssignment, generation int64, demand TokenDemand, ttl time.Duration) admission.Request {
	return admission.Request{
		Lookup:   assignment.lookup,
		LeaseTTL: ttl,
		Rate: admission.RateLimits{
			TokensPerMinute: &assignment.tokensPerMinute,
			Tokens:          demand.Total,
			CreditCombined:  true,
		},
		Membership: &admission.Membership{Member: assignment.member, Generation: generation},
	}
}

func sharedAssignments(view *routing.View) iter.Seq[sharedAssignment] {
	return func(yield func(sharedAssignment) bool) {
		if view == nil {
			return
		}
		for _, provider := range view.Providers() {
			if provider.ProjectID == nil {
				continue
			}
			for _, quota := range provider.SharedTokenQuotas {
				assignment := sharedAssignment{
					lookup:          limits.SharedQuotaLookup(routing.SharedTokenQuotaGroupID(*provider.ProjectID, quota.Group)),
					member:          limits.SharedQuotaMember(provider.ID, quota.Model),
					tokensPerMinute: quota.TokensPerMinute,
				}
				if !yield(assignment) {
					return
				}
			}
		}
	}
}

type sharedPolicy struct {
	ceiling int64
	members []string
}

func sharedPolicies(view *routing.View) map[string]sharedPolicy {
	groups := make(map[string]sharedPolicy)
	for assignment := range sharedAssignments(view) {
		groups[assignment.lookup] = sharedPolicy{
			ceiling: assignment.tokensPerMinute,
			members: append(groups[assignment.lookup].members, assignment.member),
		}
	}
	return groups
}

func retiredSharedLookups(previous *routing.View, current map[string]sharedPolicy, skippedGroupIDs []string) []string {
	return slices.Compact(slices.Sorted(func(yield func(string) bool) {
		for assignment := range sharedAssignments(previous) {
			if _, kept := current[assignment.lookup]; !kept && !yield(assignment.lookup) {
				return
			}
		}
		for _, groupID := range skippedGroupIDs {
			lookup := limits.SharedQuotaLookup(groupID)
			if _, kept := current[lookup]; !kept && !yield(lookup) {
				return
			}
		}
	}))
}

func SyncSharedQuotas(ctx context.Context, limiter *admission.Limiter, previous, next *routing.View, skippedGroupIDs ...string) error {
	if limiter == nil || next == nil {
		return nil
	}
	current := sharedPolicies(next)
	retired := retiredSharedLookups(previous, current, skippedGroupIDs)
	updates := make([]limits.SharedMembership, len(retired)+len(current))
	for index, lookup := range retired {
		updates[index] = limits.SharedMembership{Lookup: lookup}
	}
	for index, lookup := range slices.Sorted(maps.Keys(current)) {
		policy := current[lookup]
		updates[len(retired)+index] = limits.SharedMembership{Lookup: lookup, Ceiling: policy.ceiling, Members: policy.members}
	}
	return limiter.SyncSharedMemberships(ctx, next.Generation().Ordinal, updates)
}
