package configuration

import (
	"maps"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/providers"
)

func (code CodeConfiguration) Validate() error {
	ids := slices.Concat(mapCode(code.Accounts, func(e CodeAccountEntry) string { return e.ID }), mapCode(code.Pools, func(e CodePoolEntry) string { return e.ID }), mapCode(code.Routes, func(e CodeRouteEntry) string { return e.ID }), mapCode(code.Budgets, func(e CodeBudgetEntry) string { return e.ID }))
	for i, id := range ids {
		if _, err := access.ParseUUID(id); err != nil {
			return err
		}
		if slices.Contains(ids[:i], id) {
			return access.Invalid("code", "Use distinct artifact resource references.")
		}
	}
	for _, e := range code.Accounts {
		if access.ValidText("code", e.Project, 100) != nil || access.ValidText("code", e.Provider, 100) != nil || access.ValidText("code", e.CredentialRef, 200) != nil {
			return access.Invalid("code.accounts", "Name the project, provider and destination credential reference.")
		}
		if err := (providers.CodeAccountInput{ProjectID: e.ID, ProviderID: e.ID, CredentialID: e.ID, Name: e.Name, Enabled: e.Enabled, Models: e.Models}).Validate(); err != nil {
			return err
		}
	}
	for _, e := range code.Pools {
		if access.ValidText("code", e.Project, 100) != nil {
			return access.Invalid("code.pools.project", "Name the project.")
		}
		if e.Kind == "personal" != (e.OwnerRef != nil) {
			return access.Invalid("code.pools.owner_ref", "Personal pools require an explicit owner reference.")
		}
		for ref := range e.Accounts {
			if !codeRefProject(ref, e.Project, code.Accounts, func(a CodeAccountEntry) (string, string) { return a.ID, a.Project }) {
				return access.Invalid("code.pools.accounts", "Every account must resolve within the pool project.")
			}
		}
		if err := (access.CodePoolInput{ProjectID: e.ID, Name: e.Name, Kind: e.Kind, OwnerUserID: placeholderOwner(e.OwnerRef, e.ID), AccountIDs: codeAccountRefs(e), AccountWeights: e.Accounts, APIKeyIDs: mapCode(e.APIKeyRefs, func(string) string { return e.ID })}).Validate(); err != nil {
			return err
		}
		for _, ref := range e.APIKeyRefs {
			if access.ValidText("code", ref, 200) != nil {
				return access.Invalid("code.pools.api_key_refs", "Use nonempty destination key references.")
			}
		}
	}
	for i, e := range code.Routes {
		if slices.ContainsFunc(code.Routes[:i], func(other CodeRouteEntry) bool { return other.Slug == e.Slug }) {
			return access.Invalid("code.routes.slug", "Use distinct route slugs.")
		}
		if err := code.validatePolicy(e, e.Draft); err != nil {
			return err
		}
		if e.Published == nil {
			continue
		}
		if err := code.validatePolicy(e, e.Published.Policy); err != nil {
			return err
		}
		if len(e.Published.Connections) == 0 {
			return access.Invalid("code.routes.published.connections", "Published routes require frozen provider connections.")
		}
		pool := slices.IndexFunc(code.Pools, func(p CodePoolEntry) bool { return p.ID == e.Published.Policy.Pool })
		for name := range e.Published.Connections {
			if !slices.ContainsFunc(code.Accounts, func(a CodeAccountEntry) bool {
				_, member := code.Pools[pool].Accounts[a.ID]
				return member && strings.EqualFold(a.Provider, name)
			}) {
				return access.Invalid("code.routes.published.connections", "Frozen providers must resolve through published pool accounts.")
			}
		}
		for ref := range code.Pools[pool].Accounts {
			account := code.Accounts[slices.IndexFunc(code.Accounts, func(a CodeAccountEntry) bool { return a.ID == ref })]
			if !slices.ContainsFunc(slices.Collect(maps.Keys(e.Published.Connections)), func(name string) bool { return strings.EqualFold(name, account.Provider) }) {
				return access.Invalid("code.routes.published.connections", "Freeze every published pool provider.")
			}
		}
	}
	for _, e := range code.Budgets {
		if access.ValidText("code", e.Project, 100) != nil {
			return access.Invalid("code.budgets.project", "Name the project.")
		}
		if e.Route != nil && !codeRefProject(*e.Route, e.Project, code.Routes, func(r CodeRouteEntry) (string, string) { return r.ID, r.Project }) {
			return access.Invalid("code.budgets.route", "The route must resolve within the budget project.")
		}
		if err := (access.CodeBudgetInput{ProjectID: e.ID, RouteID: e.Route, APIKeyID: placeholderOwner(e.APIKeyRef, e.ID), DailyTokens: e.DailyTokens, MonthlyTokens: e.MonthlyTokens, Enabled: e.Enabled}).Validate(); err != nil {
			return err
		}
	}
	return nil
}
func (code CodeConfiguration) validatePolicy(route CodeRouteEntry, policy CodeRoutePolicy) error {
	if access.ValidText("code", route.Project, 100) != nil || !codeRefProject(policy.Pool, route.Project, code.Pools, func(p CodePoolEntry) (string, string) { return p.ID, p.Project }) {
		return access.Invalid("code.routes.pool", "Every pool must resolve within the route project.")
	}
	if policy.Tools != nil && !codeRefProject(policy.Tools.AccountID, route.Project, code.Accounts, func(a CodeAccountEntry) (string, string) { return a.ID, a.Project }) {
		return access.Invalid("code.routes.tools", "Research accounts must resolve within the route project.")
	}
	return policy.input(route.ID, route.Slug, policy.Pool).Validate()
}
func placeholderOwner(ref *string, id string) *string {
	if ref == nil {
		return nil
	}
	return &id
}

func (bindings CodeBindings) validate(code *CodeConfiguration) error {
	if code == nil {
		if len(bindings.Credentials)+len(bindings.Users)+len(bindings.APIKeys) > 0 {
			return access.Invalid("code_bindings", "Bind only references declared in code configuration.")
		}
		return nil
	}
	credentials := mapCode(code.Accounts, func(a CodeAccountEntry) string { return a.CredentialRef })
	users := slices.Collect(func(yield func(string) bool) {
		for _, p := range code.Pools {
			if p.OwnerRef != nil && !yield(*p.OwnerRef) {
				return
			}
		}
	})
	keys := slices.Concat(slices.Collect(func(yield func(string) bool) {
		for _, p := range code.Pools {
			for _, ref := range p.APIKeyRefs {
				if !yield(ref) {
					return
				}
			}
		}
	}), slices.Collect(func(yield func(string) bool) {
		for _, b := range code.Budgets {
			if b.APIKeyRef != nil && !yield(*b.APIKeyRef) {
				return
			}
		}
	}))
	for _, group := range []struct {
		Values map[string]string
		Refs   []string
	}{{bindings.Credentials, credentials}, {bindings.Users, users}, {bindings.APIKeys, keys}} {
		for ref, value := range group.Values {
			if !slices.Contains(group.Refs, ref) {
				return access.Invalid("code_bindings", "Bind only references declared in code configuration.")
			}
			if _, err := access.ParseUUID(value); err != nil {
				return err
			}
		}
	}
	return nil
}
