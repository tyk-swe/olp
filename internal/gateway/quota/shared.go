package quota

import (
	"context"
	"errors"
	"iter"
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
	if view == nil {
		return map[string]sharedAssignment{}
	}
	return maps.Collect(func(yield func(string, sharedAssignment) bool) {
		for _, provider := range view.Providers() {
			for _, quota := range provider.SharedTokenQuotas {
				assignment, ok := sharedQuotaFor(&provider, quota.Model)
				if ok && !yield(assignment.member, assignment) {
					return
				}
			}
		}
	})
}

type membershipChange struct {
	lookup string
	entry  admission.MembershipEntry
}

func membershipChanges(from, to map[string]sharedAssignment, active bool) []membershipChange {
	return slices.Collect(func(yield func(membershipChange) bool) {
		for member, assignment := range from {
			other, kept := to[member]
			changed := !kept || other.lookup != assignment.lookup || active && other.tokensPerMinute != assignment.tokensPerMinute
			if changed && !yield(membershipChange{lookup: assignment.lookup, entry: admission.MembershipEntry{Member: member, Active: active}}) {
				return
			}
		}
	})
}

func membershipByLookup(changes []membershipChange) iter.Seq2[string, []admission.MembershipEntry] {
	lookups := slices.Compact(slices.Sorted(func(yield func(string) bool) {
		for _, change := range changes {
			if !yield(change.lookup) {
				return
			}
		}
	}))
	return func(yield func(string, []admission.MembershipEntry) bool) {
		for _, lookup := range lookups {
			entries := slices.Collect(func(yield func(admission.MembershipEntry) bool) {
				for _, change := range changes {
					if change.lookup == lookup && !yield(change.entry) {
						return
					}
				}
			})
			if !yield(lookup, entries) {
				return
			}
		}
	}
}

func syncLookup(ctx context.Context, limiter *admission.Limiter, lookup string, generation int64, entries []admission.MembershipEntry) error {
	syncCtx, cancel := context.WithTimeout(ctx, ReserveTimeout)
	defer cancel()
	return limiter.SyncSharedMembership(syncCtx, lookup, generation, entries)
}

func SyncSharedQuotas(ctx context.Context, limiter *admission.Limiter, previous, next *routing.View) error {
	if limiter == nil || next == nil {
		return nil
	}
	before, after := sharedAssignments(previous), sharedAssignments(next)
	generation := next.Generation().Ordinal
	steps := [][]membershipChange{membershipChanges(before, after, false), membershipChanges(after, before, true)}
	return errors.Join(slices.Collect(func(yield func(error) bool) {
		for _, step := range steps {
			for lookup, entries := range membershipByLookup(step) {
				if !yield(syncLookup(ctx, limiter, lookup, generation, entries)) {
					return
				}
			}
		}
	})...)
}
