package configuration

import (
	"maps"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/routes"
	"github.com/tyk-swe/olp/internal/routing"
)

type CodeConfiguration struct {
	Accounts []CodeAccountEntry `json:"accounts"`
	Pools    []CodePoolEntry    `json:"pools"`
	Routes   []CodeRouteEntry   `json:"routes"`
	Budgets  []CodeBudgetEntry  `json:"budgets"`
}

type CodeBindings struct {
	Credentials map[string]string `json:"credentials"`
	Users       map[string]string `json:"users"`
	APIKeys     map[string]string `json:"api_keys"`
}

type CodeAccountEntry struct {
	ID            string   `json:"id"`
	Project       string   `json:"project"`
	Provider      string   `json:"provider"`
	CredentialRef string   `json:"credential_ref"`
	Name          string   `json:"name"`
	Enabled       bool     `json:"enabled"`
	Models        []string `json:"models"`
}

type CodePoolEntry struct {
	ID         string           `json:"id"`
	Project    string           `json:"project"`
	Name       string           `json:"name"`
	Kind       string           `json:"kind"`
	OwnerRef   *string          `json:"owner_ref"`
	Accounts   map[string]int64 `json:"accounts"`
	APIKeyRefs []string         `json:"api_key_refs"`
}

type CodePaidTarget struct {
	Provider      string `json:"provider"`
	Slot          string `json:"slot"`
	CredentialRef string `json:"credential_ref,omitempty"`
}
type CodePaidConnection struct {
	Provider    ProviderEntry       `json:"provider"`
	SlotKeyRefs map[string][]string `json:"slot_key_refs"`
}
type CodeRoutePolicy struct {
	ChatToAntigravity       bool                      `json:"chat_to_antigravity,omitempty"`
	PaidParticipation       bool                      `json:"paid_participation"`
	PaidTargets             map[string]CodePaidTarget `json:"paid_targets,omitempty"`
	Pool                    string                    `json:"pool"`
	Models                  []string                  `json:"models"`
	Enabled                 bool                      `json:"enabled"`
	ModelPools              map[string][]string       `json:"model_pools"`
	InitialRecoveryAttempts int                       `json:"initial_recovery_attempts"`
	ModelRewriting          bool                      `json:"model_rewriting"`
	Aliases                 map[string]string         `json:"aliases"`
	MessagesToCodex         bool                      `json:"messages_to_codex"`
	Tools                   *codemode.ResearchTools   `json:"tools"`
	MaxBodyBytes            *int64                    `json:"max_body_bytes"`
}

type CodeConnectionEntry struct {
	Configuration        providers.Configuration `json:"configuration"`
	NetworkCredentialRef *string                 `json:"network_credential_ref,omitempty"`
}

type CodePublication struct {
	PaidConnections map[string]CodePaidConnection `json:"paid_connections,omitempty"`
	paidResolved    map[string]routing.Provider
	Policy          CodeRoutePolicy                `json:"policy"`
	Connections     map[string]CodeConnectionEntry `json:"connections"`
}

type CodeRouteEntry struct {
	ID        string           `json:"id"`
	Project   string           `json:"project"`
	Slug      string           `json:"slug"`
	Draft     CodeRoutePolicy  `json:"draft"`
	Published *CodePublication `json:"published,omitempty"`
}

type CodeBudgetEntry struct {
	ID            string  `json:"id"`
	Project       string  `json:"project"`
	Route         *string `json:"route"`
	APIKeyRef     *string `json:"api_key_ref"`
	DailyTokens   *int64  `json:"daily_tokens"`
	MonthlyTokens *int64  `json:"monthly_tokens"`
	Enabled       bool    `json:"enabled"`
}

func codePolicy(route codemode.Route) CodeRoutePolicy {
	return CodeRoutePolicy{ChatToAntigravity: route.ChatToAntigravity, PaidParticipation: route.PaidParticipation, PaidTargets: maps.Collect(func(yield func(string, CodePaidTarget) bool) {
		for model, target := range route.PaidTargets {
			if !yield(model, CodePaidTarget{Provider: target.ProviderID, Slot: target.SlotID}) {
				return
			}
		}
	}), Pool: route.PoolID, Models: route.Models, Enabled: route.Enabled, ModelPools: route.ModelPools, InitialRecoveryAttempts: route.InitialRecoveryAttempts, ModelRewriting: route.ModelRewriting, Aliases: route.Aliases, MessagesToCodex: route.MessagesToCodex, Tools: route.Tools, MaxBodyBytes: route.MaxBodyBytes}
}

func (policy CodeRoutePolicy) input(project, slug, pool string) routes.CodeRouteInput {
	return routes.CodeRouteInput{ChatToAntigravity: policy.ChatToAntigravity, PaidParticipation: policy.PaidParticipation, PaidTargets: maps.Collect(func(yield func(string, codemode.PaidTarget) bool) {
		for model, target := range policy.PaidTargets {
			if !yield(model, codemode.PaidTarget{ProviderID: target.Provider, SlotID: target.Slot}) {
				return
			}
		}
	}), ProjectID: project, Slug: slug, PoolID: pool, Models: policy.Models, Enabled: policy.Enabled, ModelPools: policy.ModelPools, InitialRecoveryAttempts: policy.InitialRecoveryAttempts, ModelRewriting: policy.ModelRewriting, Aliases: policy.Aliases, MessagesToCodex: policy.MessagesToCodex, Tools: policy.Tools, MaxBodyBytes: policy.MaxBodyBytes}
}

func canonicalCode(code *CodeConfiguration) *CodeConfiguration {
	if code == nil {
		return nil
	}
	pools := mapCode(code.Pools, func(pool CodePoolEntry) CodePoolEntry {
		return CodePoolEntry{ID: pool.ID, Project: pool.Project, Name: pool.Name, Kind: pool.Kind, OwnerRef: pool.OwnerRef, Accounts: pool.Accounts, APIKeyRefs: slices.Sorted(slices.Values(pool.APIKeyRefs))}
	})
	return &CodeConfiguration{Accounts: slices.SortedFunc(slices.Values(code.Accounts), func(a, b CodeAccountEntry) int { return strings.Compare(a.ID, b.ID) }), Pools: slices.SortedFunc(slices.Values(pools), func(a, b CodePoolEntry) int { return strings.Compare(a.ID, b.ID) }), Routes: slices.SortedFunc(slices.Values(code.Routes), func(a, b CodeRouteEntry) int { return strings.Compare(a.ID, b.ID) }), Budgets: slices.SortedFunc(slices.Values(code.Budgets), func(a, b CodeBudgetEntry) int { return strings.Compare(a.ID, b.ID) })}
}
func codeAccountRefs(pool CodePoolEntry) []string {
	return append([]string{}, slices.Sorted(maps.Keys(pool.Accounts))...)
}

type codeResult[T any] struct {
	value T
	err   error
}

func mapCodeResult[A, B any](values []A, project func(A) (B, error)) ([]B, error) {
	results := mapCode(values, func(value A) codeResult[B] { v, err := project(value); return codeResult[B]{v, err} })
	for _, result := range results {
		if result.err != nil {
			return nil, result.err
		}
	}
	return mapCode(results, func(result codeResult[B]) B { return result.value }), nil
}
