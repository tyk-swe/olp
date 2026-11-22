package quota

import (
	"context"
	"errors"
	"maps"
	"slices"
	"time"

	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/limits/admission"
	"github.com/tyk-swe/olp/internal/routing"
)

type sharedAssignment struct {
	groupID         string
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
		groupID:         groupID,
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

func sharedAssignments(view *routing.View) map[string]sharedAssignment {
	assignments := map[string]sharedAssignment{}
	if view == nil {
		return assignments
	}
	for _, provider := range view.Providers() {
		for _, quota := range provider.SharedTokenQuotas {
			if assignment, ok := sharedQuotaFor(&provider, quota.Model); ok {
				assignments[assignment.member] = assignment
			}
		}
	}
	return assignments
}

func SyncSharedQuotas(ctx context.Context, limiter *admission.Limiter, previous, next *routing.View) error {
	if limiter == nil || next == nil {
		return nil
	}
	before := sharedAssignments(previous)
	after := sharedAssignments(next)
	departed := map[string][]admission.MembershipEntry{}
	joined := map[string][]admission.MembershipEntry{}
	for member, assignment := range before {
		if current, ok := after[member]; !ok || current.lookup != assignment.lookup {
			departed[assignment.lookup] = append(departed[assignment.lookup], admission.MembershipEntry{Member: member})
		}
	}
	for member, assignment := range after {
		if prior, ok := before[member]; !ok || prior.lookup != assignment.lookup || prior.tokensPerMinute != assignment.tokensPerMinute {
			joined[assignment.lookup] = append(joined[assignment.lookup], admission.MembershipEntry{Member: member, Active: true})
		}
	}
	generation := next.Generation().Ordinal
	var failures []error
	for _, step := range []map[string][]admission.MembershipEntry{departed, joined} {
		for _, lookup := range slices.Sorted(maps.Keys(step)) {
			syncCtx, cancel := context.WithTimeout(ctx, ReserveTimeout)
			err := limiter.SyncSharedMembership(syncCtx, lookup, generation, step[lookup])
			cancel()
			if err != nil {
				failures = append(failures, err)
			}
		}
	}
	return errors.Join(failures...)
}
