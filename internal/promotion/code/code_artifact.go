package code

import (
	"github.com/tyk-swe/olp/internal/promotion"
	"maps"
	"slices"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/routes"
)

func codePortablePaidTargets(targets map[string]codemode.PaidTarget) map[string]promotion.CodePaidTarget {
	return maps.Collect(func(yield func(string, promotion.CodePaidTarget) bool) {
		for model, target := range targets {
			if !yield(model, promotion.CodePaidTarget{Provider: target.ProviderID, Slot: target.SlotID}) {
				return
			}
		}
	})
}
func codeNativePaidTargets(targets map[string]promotion.CodePaidTarget) map[string]codemode.PaidTarget {
	return maps.Collect(func(yield func(string, codemode.PaidTarget) bool) {
		for model, target := range targets {
			if !yield(model, codemode.PaidTarget{ProviderID: target.Provider, SlotID: target.Slot}) {
				return
			}
		}
	})
}
func codePolicy(route codemode.Route) promotion.CodeRoutePolicy {
	return promotion.CodeRoutePolicy{ChatToAntigravity: route.ChatToAntigravity, AllowanceOrdering: route.AllowanceOrdering, PaidParticipation: route.PaidParticipation, PaidTargets: codePortablePaidTargets(route.PaidTargets), PaidFallbackTargets: codePortablePaidTargets(route.PaidFallbackTargets), Pool: route.PoolID, Models: route.Models, Enabled: route.Enabled, ModelPools: maps.Collect(maps.All(route.ModelPools)), InitialRecoveryAttempts: route.InitialRecoveryAttempts, ModelRewriting: route.ModelRewriting, Aliases: maps.Collect(maps.All(route.Aliases)), MessagesToGrok: route.MessagesToGrok, MessagesGrokSafetyIdentifier: route.MessagesGrokSafetyIdentifier, ResponsesToGrok: route.ResponsesToGrok, ChatToGrok: route.ChatToGrok, GrokCLIIdentity: route.GrokCLIIdentity, CodexCLIIdentity: route.CodexCLIIdentity, ChatToCodex: route.ChatToCodex, MessagesToCodex: route.MessagesToCodex, MessagesCodexSafetyIdentifier: route.MessagesCodexSafetyIdentifier, Tools: route.Tools, MaxBodyBytes: route.MaxBodyBytes}
}
func policyInput(policy promotion.CodeRoutePolicy, project, slug, pool string) routes.CodeRouteInput {
	return routes.CodeRouteInput{ChatToAntigravity: policy.ChatToAntigravity, AllowanceOrdering: policy.AllowanceOrdering, PaidParticipation: policy.PaidParticipation, PaidTargets: codeNativePaidTargets(policy.PaidTargets), PaidFallbackTargets: codeNativePaidTargets(policy.PaidFallbackTargets), ProjectID: project, Slug: slug, PoolID: pool, Models: policy.Models, Enabled: policy.Enabled, ModelPools: policy.ModelPools, InitialRecoveryAttempts: policy.InitialRecoveryAttempts, ModelRewriting: policy.ModelRewriting, Aliases: policy.Aliases, MessagesToGrok: policy.MessagesToGrok, MessagesGrokSafetyIdentifier: policy.MessagesGrokSafetyIdentifier, ResponsesToGrok: policy.ResponsesToGrok, ChatToGrok: policy.ChatToGrok, GrokCLIIdentity: policy.GrokCLIIdentity, CodexCLIIdentity: policy.CodexCLIIdentity, ChatToCodex: policy.ChatToCodex, MessagesToCodex: policy.MessagesToCodex, MessagesCodexSafetyIdentifier: policy.MessagesCodexSafetyIdentifier, Tools: policy.Tools, MaxBodyBytes: policy.MaxBodyBytes}
}
func allPaidTargets(policy promotion.CodeRoutePolicy) map[string]promotion.CodePaidTarget {
	return maps.Collect(func(yield func(string, promotion.CodePaidTarget) bool) {
		for _, targets := range []map[string]promotion.CodePaidTarget{policy.PaidTargets, policy.PaidFallbackTargets} {
			for model, target := range targets {
				if !yield(model, target) {
					return
				}
			}
		}
	})
}
func paidRoute(policy promotion.CodeRoutePolicy, project string) codemode.Route {
	return codemode.Route{Models: policy.Models, ProjectID: project, PaidParticipation: policy.PaidParticipation, PaidTargets: codeNativePaidTargets(policy.PaidTargets), PaidFallbackTargets: codeNativePaidTargets(policy.PaidFallbackTargets), InitialRecoveryAttempts: policy.InitialRecoveryAttempts}
}

func codeAccountRefs(pool promotion.CodePoolEntry) []string {
	return append([]string{}, slices.Sorted(maps.Keys(pool.Accounts))...)
}

type codeResult[T any] struct {
	value T
	err   error
}

func mapCodeResult[A, B any](values []A, project func(A) (B, error)) ([]B, error) {
	results := promotion.MapCode(values, func(value A) codeResult[B] { v, err := project(value); return codeResult[B]{v, err} })
	for _, result := range results {
		if result.err != nil {
			return nil, result.err
		}
	}
	return promotion.MapCode(results, func(result codeResult[B]) B { return result.value }), nil
}
