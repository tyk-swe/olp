package code

import (
	"context"
	"iter"
	"maps"
	"slices"

	"github.com/google/uuid"
	"github.com/oapi-codegen/nullable"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/routes"
)

type Policy struct {
	Pool                    string
	Models                  []string
	Enabled                 bool
	ModelPools              map[string][]string
	Aliases                 map[string]string
	InitialRecoveryAttempts int
	ModelRewriting          bool
	CodexCLIIdentity        bool
	ChatToCodex             bool
	MessagesToCodex         bool
	ChatToAntigravity       bool
	AllowanceOrdering       bool
	PaidParticipation       bool
	PaidTargets             map[string]PaidTarget
	PaidFallbackTargets     map[string]PaidTarget
	Tools                   *codemode.ResearchTools
	MaxBodyBytes            *int64
}

func policyOf(e contract.ConfigurationCodeRoutePolicy) Policy {
	return Policy{
		Pool: e.Pool.String(), Models: promotion.NonNil(slices.Clone(e.Models)), Enabled: e.Enabled,
		ModelPools: maps.Clone(orEmpty(e.ModelPools)), Aliases: maps.Clone(orEmpty(e.Aliases)), InitialRecoveryAttempts: promotion.ValueOf(e.InitialRecoveryAttempts),
		ModelRewriting: promotion.ValueOf(e.ModelRewriting), CodexCLIIdentity: promotion.ValueOf(e.CodexCliIdentity), ChatToCodex: promotion.ValueOf(e.ChatToCodex), MessagesToCodex: promotion.ValueOf(e.MessagesToCodex),
		ChatToAntigravity: promotion.ValueOf(e.ChatToAntigravity), AllowanceOrdering: promotion.ValueOf(e.AllowanceOrdering), PaidParticipation: promotion.ValueOf(e.PaidParticipation),
		PaidTargets: mappedValues(orEmpty(e.PaidTargets), targetOf), PaidFallbackTargets: mappedValues(orEmpty(e.PaidFallbackTargets), targetOf),
		Tools: toolsOf(promotion.Pointer(e.Tools)), MaxBodyBytes: widened(promotion.Pointer(e.MaxBodyBytes)),
	}
}

func toolsOf(tools *contract.CodeResearchTools) *codemode.ResearchTools {
	if tools == nil {
		return nil
	}
	return &codemode.ResearchTools{AccountID: tools.AccountId.String(), Services: promotion.Map(tools.Services, func(s contract.CodeResearchToolsServices) string { return string(s) })}
}

func widened(limit *int) *int64 {
	if limit == nil {
		return nil
	}
	return new(int64(*limit))
}

func (p Policy) input(project, slug, pool string, tools *codemode.ResearchTools, targets, fallback map[string]codemode.PaidTarget) routes.CodeRouteInput {
	return routes.CodeRouteInput{
		ProjectID: project, Slug: slug, PoolID: pool, Models: promotion.NonNil(slices.Clone(p.Models)), Enabled: p.Enabled,
		ModelPools: maps.Collect(maps.All(p.ModelPools)), Aliases: maps.Collect(maps.All(p.Aliases)), InitialRecoveryAttempts: p.InitialRecoveryAttempts,
		ModelRewriting: p.ModelRewriting, CodexCLIIdentity: p.CodexCLIIdentity, ChatToCodex: p.ChatToCodex, MessagesToCodex: p.MessagesToCodex,
		ChatToAntigravity: p.ChatToAntigravity, AllowanceOrdering: p.AllowanceOrdering, PaidParticipation: p.PaidParticipation,
		PaidTargets: maps.Collect(maps.All(targets)), PaidFallbackTargets: maps.Collect(maps.All(fallback)), Tools: cloned(tools), MaxBodyBytes: p.MaxBodyBytes,
	}
}

func inputOf(route codemode.Route) routes.CodeRouteInput {
	return stored(route, nil, nil).input(route.ProjectID, route.Slug, route.PoolID, route.Tools, route.PaidTargets, route.PaidFallbackTargets)
}

func stored(route codemode.Route, targets, fallback map[string]PaidTarget) Policy {
	return Policy{
		Pool: route.PoolID, Models: route.Models, Enabled: route.Enabled, ModelPools: route.ModelPools, Aliases: route.Aliases, InitialRecoveryAttempts: route.InitialRecoveryAttempts,
		ModelRewriting: route.ModelRewriting, CodexCLIIdentity: route.CodexCLIIdentity, ChatToCodex: route.ChatToCodex, MessagesToCodex: route.MessagesToCodex,
		ChatToAntigravity: route.ChatToAntigravity, AllowanceOrdering: route.AllowanceOrdering, PaidParticipation: route.PaidParticipation,
		PaidTargets: targets, PaidFallbackTargets: fallback, Tools: cloned(route.Tools), MaxBodyBytes: route.MaxBodyBytes,
	}
}

func portablePolicy(route codemode.Route, slots map[string]PaidTarget) (Policy, error) {
	targets, err := portableTargets(route.PaidTargets, slots)
	if err != nil {
		return Policy{}, err
	}
	fallback, err := portableTargets(route.PaidFallbackTargets, slots)
	return stored(route, targets, fallback), err
}

func (p Policy) resolve(ctx context.Context, q access.Queryer, slug, projectID string, in scope) (routes.CodeRouteInput, error) {
	targets, err := resolveTargets(ctx, q, p.PaidTargets, projectID, in.bindings.credentials)
	if err != nil {
		return routes.CodeRouteInput{}, err
	}
	fallback, err := resolveTargets(ctx, q, p.PaidFallbackTargets, projectID, in.bindings.credentials)
	if err != nil {
		return routes.CodeRouteInput{}, err
	}
	return p.input(projectID, slug, destinations(in.pools)[p.Pool], onAccount(p.Tools, destinations(in.accounts)), targets, fallback), nil
}

func onAccount(tools *codemode.ResearchTools, accounts map[string]string) *codemode.ResearchTools {
	if tools == nil {
		return nil
	}
	return &codemode.ResearchTools{AccountID: accounts[tools.AccountID], Services: tools.Services}
}

func cloned(tools *codemode.ResearchTools) *codemode.ResearchTools {
	if tools == nil {
		return nil
	}
	return &codemode.ResearchTools{AccountID: tools.AccountID, Services: promotion.NonNil(slices.Clone(tools.Services))}
}

func (p Policy) paidTargets() iter.Seq2[string, PaidTarget] {
	return func(yield func(string, PaidTarget) bool) {
		for _, targets := range []map[string]PaidTarget{p.PaidTargets, p.PaidFallbackTargets} {
			for _, model := range sortedKeys(targets) {
				if !yield(model, targets[model]) {
					return
				}
			}
		}
	}
}

func (p Policy) targets(provider string) bool {
	for _, target := range p.paidTargets() {
		if target.Provider == provider {
			return true
		}
	}
	return false
}

func renderPolicy(p Policy) (contract.ConfigurationCodeRoutePolicy, error) {
	pool, err := uuid.Parse(p.Pool)
	if err != nil {
		return contract.ConfigurationCodeRoutePolicy{}, err
	}
	tools, err := renderTools(p.Tools)
	if err != nil {
		return contract.ConfigurationCodeRoutePolicy{}, err
	}
	return contract.ConfigurationCodeRoutePolicy{
		Pool: pool, Models: promotion.NonNil(slices.Clone(p.Models)), Enabled: p.Enabled,
		ModelPools: new(maps.Collect(maps.All(p.ModelPools))), Aliases: new(maps.Collect(maps.All(p.Aliases))), InitialRecoveryAttempts: new(p.InitialRecoveryAttempts),
		ModelRewriting: new(p.ModelRewriting), CodexCliIdentity: new(p.CodexCLIIdentity), ChatToCodex: new(p.ChatToCodex), MessagesToCodex: new(p.MessagesToCodex),
		ChatToAntigravity: new(p.ChatToAntigravity), AllowanceOrdering: new(p.AllowanceOrdering), PaidParticipation: new(p.PaidParticipation),
		PaidTargets: new(mappedValues(p.PaidTargets, PaidTarget.render)), PaidFallbackTargets: new(mappedValues(p.PaidFallbackTargets, PaidTarget.render)),
		Tools: tools, MaxBodyBytes: narrowed(p.MaxBodyBytes),
	}, nil
}

func renderTools(tools *codemode.ResearchTools) (nullable.Nullable[contract.CodeResearchTools], error) {
	if tools == nil {
		return nullable.Nullable[contract.CodeResearchTools]{}, nil
	}
	account, err := uuid.Parse(tools.AccountID)
	services := promotion.Map(tools.Services, func(s string) contract.CodeResearchToolsServices { return contract.CodeResearchToolsServices(s) })
	return nullable.NewNullableWithValue(contract.CodeResearchTools{AccountId: account, Services: services}), err
}

func narrowed(limit *int64) nullable.Nullable[int] {
	if limit == nil {
		return nullable.Nullable[int]{}
	}
	return nullable.NewNullableWithValue(int(*limit))
}
