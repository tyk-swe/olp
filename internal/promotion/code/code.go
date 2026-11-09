package code

import (
	"cmp"
	"context"
	"maps"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/oapi-codegen/nullable"
	openapi_types "github.com/oapi-codegen/runtime/types"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/promotion/project"
	"github.com/tyk-swe/olp/internal/promotion/provider"
	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/release"
)

type Code struct {
	Accounts []Account
	Pools    []Pool
	Routes   []Route
	Budgets  []Budget
}

type Desired struct {
	Code     Code
	bindings bindings
}

type bindings struct {
	credentials map[string]string
	users       map[string]string
	keys        map[string]string
}

type change struct {
	item  promotion.Item
	write func(context.Context, pgx.Tx, access.Principal) error
}

type resolution struct {
	accounts []resolved[providers.CodeAccountInput]
	pools    []resolved[access.CodePoolInput]
	routes   []resolved[routeInput]
	budgets  []resolved[access.CodeBudgetInput]
}

func Parse(document *contract.ConfigurationCode, declared *contract.ConfigurationCodeBindings, names promotion.Names, deps provider.Deps) (Desired, error) {
	codeBindings := promotion.ValueOf(declared)
	values := bindings{credentials: uuids(codeBindings.Credentials), users: uuids(codeBindings.Users), keys: uuids(codeBindings.ApiKeys)}
	if document == nil {
		return Desired{}, onlyDeclared(values, Code{})
	}
	code, err := parseCode(*document, names, deps)
	if err != nil {
		return Desired{}, err
	}
	if err := onlyDeclared(values, code); err != nil {
		return Desired{}, err
	}
	return Desired{Code: code, bindings: values}, nil
}

func parseCode(document contract.ConfigurationCode, names promotion.Names, deps provider.Deps) (Code, error) {
	accountEntries, poolEntries, routeEntries, budgetEntries := document.Accounts.GetOrEmpty(), document.Pools.GetOrEmpty(), document.Routes.GetOrEmpty(), document.Budgets.GetOrEmpty()
	ids := slices.Concat(
		promotion.Map(accountEntries, func(e contract.ConfigurationCodeAccount) string { return e.Id.String() }),
		promotion.Map(poolEntries, func(e contract.ConfigurationCodePool) string { return e.Id.String() }),
		promotion.Map(routeEntries, func(e contract.ConfigurationCodeRoute) string { return e.Id.String() }),
		promotion.Map(budgetEntries, func(e contract.ConfigurationCodeBudget) string { return e.Id.String() }),
	)
	if len(slices.Compact(slices.Sorted(slices.Values(ids)))) != len(ids) {
		return Code{}, access.Invalid("code", "Use distinct artifact resource references.")
	}
	accounts, err := promotion.TryMapIndex(accountEntries, func(_ int, e contract.ConfigurationCodeAccount) (Account, error) { return parseAccount(e) })
	if err != nil {
		return Code{}, err
	}
	pools, err := promotion.TryMapIndex(poolEntries, func(_ int, e contract.ConfigurationCodePool) (Pool, error) { return parsePool(e, accounts) })
	if err != nil {
		return Code{}, err
	}
	routes, err := promotion.TryMapIndex(routeEntries, func(i int, e contract.ConfigurationCodeRoute) (Route, error) {
		return parseRoute(e, routeEntries[:i], accounts, pools, names, deps)
	})
	if err != nil {
		return Code{}, err
	}
	budgets, err := promotion.TryMapIndex(budgetEntries, func(_ int, e contract.ConfigurationCodeBudget) (Budget, error) { return parseBudget(e, routes) })
	if err != nil {
		return Code{}, err
	}
	return Code{Accounts: accounts, Pools: pools, Routes: routes, Budgets: budgets}, nil
}

func onlyDeclared(values bindings, code Code) error {
	groups := []struct {
		values map[string]string
		refs   []string
	}{
		{values.credentials, slices.Concat(promotion.Map(code.Accounts, func(a Account) string { return a.CredentialRef }), slices.Concat(promotion.Map(code.Routes, Route.credentialRefs)...))},
		{values.users, slices.Concat(promotion.Map(code.Pools, Pool.ownerRefs)...)},
		{values.keys, slices.Concat(slices.Concat(promotion.Map(code.Routes, Route.keyRefs)...), slices.Concat(promotion.Map(code.Pools, func(p Pool) []string { return p.APIKeyRefs })...), slices.Concat(promotion.Map(code.Budgets, Budget.keyRefs)...))},
	}
	for _, group := range groups {
		for ref := range group.values {
			if !slices.Contains(group.refs, ref) {
				return access.Invalid("code_bindings", "Bind only references declared in code configuration.")
			}
		}
	}
	return nil
}

func Read(ctx context.Context, q access.Queryer) (Code, error) {
	accounts, err := readAccounts(ctx, q)
	if err != nil {
		return Code{}, err
	}
	pools, err := readPools(ctx, q)
	if err != nil {
		return Code{}, err
	}
	routes, err := readRoutes(ctx, q)
	if err != nil {
		return Code{}, err
	}
	budgets, err := readBudgets(ctx, q)
	if err != nil {
		return Code{}, err
	}
	return Code{Accounts: accounts, Pools: pools, Routes: routes, Budgets: budgets}, nil
}

func Render(code Code) (*contract.ConfigurationCode, error) {
	if len(code.Accounts)+len(code.Pools)+len(code.Routes)+len(code.Budgets) == 0 {
		return nil, nil
	}
	accounts, err := promotion.TryMap(slices.SortedFunc(slices.Values(code.Accounts), func(x, y Account) int { return cmp.Compare(x.ID, y.ID) }), renderAccount)
	if err != nil {
		return nil, err
	}
	pools, err := promotion.TryMap(slices.SortedFunc(slices.Values(code.Pools), func(x, y Pool) int { return cmp.Compare(x.ID, y.ID) }), renderPool)
	if err != nil {
		return nil, err
	}
	routes, err := promotion.TryMap(slices.SortedFunc(slices.Values(code.Routes), func(x, y Route) int { return cmp.Compare(x.ID, y.ID) }), renderRoute)
	if err != nil {
		return nil, err
	}
	budgets, err := promotion.TryMap(slices.SortedFunc(slices.Values(code.Budgets), func(x, y Budget) int { return cmp.Compare(x.ID, y.ID) }), renderBudget)
	if err != nil {
		return nil, err
	}
	return &contract.ConfigurationCode{Accounts: nullable.NewNullableWithValue(accounts), Pools: nullable.NewNullableWithValue(pools), Routes: nullable.NewNullableWithValue(routes), Budgets: nullable.NewNullableWithValue(budgets)}, nil
}

func Planner(desired Desired, deps provider.Deps) promotion.Planner {
	return promotion.Planner{
		Authorize: func(ctx context.Context, q access.Queryer, p access.Principal) error {
			return authorize(ctx, q, p, desired.Code)
		},
		Plan: func(ctx context.Context, q access.Queryer) ([]promotion.Item, error) {
			changes, err := plan(ctx, q, desired, deps)
			if err != nil {
				return nil, err
			}
			return promotion.Map(changes, func(c change) promotion.Item { return c.item }), nil
		},
	}
}

func Applier(desired Desired, deps provider.Deps) promotion.Applier {
	return func(ctx context.Context, tx pgx.Tx, p access.Principal) error {
		changes, err := plan(ctx, tx, desired, deps)
		if err != nil {
			return err
		}
		pending := slices.Collect(func(yield func(change) bool) {
			for _, c := range changes {
				if c.item.Action != contract.ConfigurationPlanItemActionNoop && !yield(c) {
					return
				}
			}
		})
		for _, c := range pending {
			if err := c.write(ctx, tx, p); err != nil {
				return err
			}
		}
		if len(pending) == 0 {
			return nil
		}
		_, published := release.Publish(ctx, tx, p.UserID())
		return published
	}
}

func authorize(ctx context.Context, q access.Queryer, p access.Principal, code Code) error {
	directory, err := project.ReadDirectory(ctx, q)
	if err != nil {
		return err
	}
	names := slices.Concat(
		promotion.Map(code.Accounts, func(a Account) string { return a.Project }),
		promotion.Map(code.Pools, func(p Pool) string { return p.Project }),
		promotion.Map(code.Routes, func(r Route) string { return r.Project }),
		promotion.Map(code.Budgets, func(b Budget) string { return b.Project }),
	)
	for _, name := range names {
		id, err := projectOf(directory, name)
		if err != nil {
			return err
		}
		if err := p.Project(&id, access.Change); err != nil {
			return err
		}
	}
	return nil
}

func plan(ctx context.Context, q access.Queryer, desired Desired, deps provider.Deps) ([]change, error) {
	directory, err := project.ReadDirectory(ctx, q)
	if err != nil {
		return nil, err
	}
	r, err := resolve(ctx, q, desired, directory, deps)
	if err != nil {
		return nil, err
	}
	accounts, err := promotion.TryMapIndex(r.accounts, func(_ int, a resolved[providers.CodeAccountInput]) (change, error) { return accountChange(ctx, q, a) })
	if err != nil {
		return nil, err
	}
	pools, err := promotion.TryMapIndex(r.pools, func(_ int, p resolved[access.CodePoolInput]) (change, error) { return poolChange(ctx, q, p) })
	if err != nil {
		return nil, err
	}
	routes, err := promotion.TryMapIndex(r.routes, func(_ int, route resolved[routeInput]) (change, error) { return routeChange(ctx, q, route) })
	if err != nil {
		return nil, err
	}
	budgets, err := promotion.TryMapIndex(r.budgets, func(_ int, b resolved[access.CodeBudgetInput]) (change, error) { return budgetChange(ctx, q, b) })
	if err != nil {
		return nil, err
	}
	return slices.Concat(accounts, pools, routes, budgets), nil
}

func resolve(ctx context.Context, q access.Queryer, desired Desired, directory project.Directory, deps provider.Deps) (resolution, error) {
	accounts, err := promotion.TryMapIndex(desired.Code.Accounts, func(_ int, a Account) (resolved[providers.CodeAccountInput], error) {
		return resolveAccount(ctx, q, a, directory, desired.bindings.credentials)
	})
	if err != nil {
		return resolution{}, err
	}
	pools, err := promotion.TryMapIndex(desired.Code.Pools, func(_ int, p Pool) (resolved[access.CodePoolInput], error) {
		return resolvePool(ctx, q, p, desired, directory, destinations(accounts))
	})
	if err != nil {
		return resolution{}, err
	}
	in := scope{directory: directory, bindings: desired.bindings, deps: deps, accounts: accounts, pools: pools}
	routes, err := promotion.TryMapIndex(desired.Code.Routes, func(_ int, r Route) (resolved[routeInput], error) { return resolveRoute(ctx, q, r, in) })
	if err != nil {
		return resolution{}, err
	}
	budgets, err := promotion.TryMapIndex(desired.Code.Budgets, func(_ int, b Budget) (resolved[access.CodeBudgetInput], error) {
		return resolveBudget(ctx, q, b, desired, directory, destinations(routes))
	})
	if err != nil {
		return resolution{}, err
	}
	if err := cmp.Or(distinct(accounts), distinct(pools), distinct(budgets)); err != nil {
		return resolution{}, err
	}
	return resolution{accounts: accounts, pools: pools, routes: routes, budgets: budgets}, nil
}

func uuids(declared *map[string]openapi_types.UUID) map[string]string {
	return maps.Collect(func(yield func(string, string) bool) {
		for ref, id := range promotion.ValueOf(declared) {
			if !yield(ref, id.String()) {
				return
			}
		}
	})
}
