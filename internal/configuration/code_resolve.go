package configuration

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/providers"
)

type resolvedCodeResource[T any] struct {
	Ref      string
	ID       string
	Existing bool
	Input    T
	Identity string
}
type resolvedCode struct {
	Accounts []resolvedCodeResource[providers.CodeAccountInput]
	Pools    []resolvedCodeResource[access.CodePoolInput]
	Routes   []resolvedCodeResource[CodeRouteEntry]
	Budgets  []resolvedCodeResource[access.CodeBudgetInput]
}

func codeIdentity(ctx context.Context, q access.Queryer, query string, args ...any) (string, bool, error) {
	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return "", false, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return "", false, err
	}
	if len(ids) > 1 {
		return "", false, access.Invalid("code", "The destination has ambiguous code resource identities.")
	}
	if len(ids) == 1 {
		return ids[0], true, nil
	}
	return access.NewID(), false, nil
}

func codeProject(ctx context.Context, q access.Queryer, name string) (string, error) {
	var id string
	if err := q.QueryRow(ctx, `SELECT id::text FROM olp.projects WHERE lower(name)=lower($1)`, name).Scan(&id); errors.Is(err, pgx.ErrNoRows) {
		return "", access.Invalid("code.project", "Create the destination project and enroll its credentials before promoting code configuration.")
	} else if err != nil {
		return "", err
	}
	return id, nil
}

func codeBound(ref string, bindings map[string]string) (string, error) {
	value, ok := bindings[ref]
	if !ok {
		return "", access.Invalid("code_bindings", "Bind every referenced destination credential, owner and API key explicitly.")
	}
	return access.ParseUUID(value)
}

func resolveCode(ctx context.Context, q access.Queryer, code *CodeConfiguration, bindings CodeBindings) (*resolvedCode, error) {
	if code == nil {
		return &resolvedCode{}, nil
	}
	accounts, err := mapCodeResult(code.Accounts, func(entry CodeAccountEntry) (resolvedCodeResource[providers.CodeAccountInput], error) {
		project, err := codeProject(ctx, q, entry.Project)
		if err != nil {
			return resolvedCodeResource[providers.CodeAccountInput]{}, err
		}
		credential, err := codeBound(entry.CredentialRef, bindings.Credentials)
		if err != nil {
			return resolvedCodeResource[providers.CodeAccountInput]{}, err
		}
		var provider, principal string
		err = q.QueryRow(ctx, `SELECT p.id::text,c.principal FROM olp.providers p JOIN olp.provider_credentials c ON c.provider_id=p.id JOIN olp.provider_grants g ON g.credential_id=c.id WHERE lower(p.name)=lower($1) AND p.project_id=$2 AND c.id=$3 AND c.principal<>'' AND c.revoked_at IS NULL AND g.lapsed_at IS NULL AND (g.expires_at IS NULL OR g.expires_at>now())`, entry.Provider, project, credential).Scan(&provider, &principal)
		if errors.Is(err, pgx.ErrNoRows) {
			return resolvedCodeResource[providers.CodeAccountInput]{}, access.Invalid("code_bindings.credentials", "Choose a current enrolled credential belonging to the destination provider and project.")
		}
		if err != nil {
			return resolvedCodeResource[providers.CodeAccountInput]{}, err
		}
		id, exists, err := codeIdentity(ctx, q, `SELECT id::text FROM olp.code_accounts WHERE project_id=$1 AND provider_id=$2 AND principal=$3`, project, provider, principal)
		return resolvedCodeResource[providers.CodeAccountInput]{Ref: entry.ID, ID: id, Existing: exists, Identity: codeIdentityKey(project, provider, principal), Input: providers.CodeAccountInput{ProjectID: project, ProviderID: provider, CredentialID: credential, Name: entry.Name, Enabled: entry.Enabled, Models: entry.Models}}, err
	})
	if err != nil {
		return nil, err
	}
	accountIDs := maps.Collect(func(yield func(string, string) bool) {
		for _, a := range accounts {
			if !yield(a.Ref, a.ID) {
				return
			}
		}
	})
	pools, err := mapCodeResult(code.Pools, func(entry CodePoolEntry) (resolvedCodeResource[access.CodePoolInput], error) {
		project, err := codeProject(ctx, q, entry.Project)
		if err != nil {
			return resolvedCodeResource[access.CodePoolInput]{}, err
		}
		owner, err := resolveCodeOwner(ctx, q, entry.OwnerRef, project, bindings.Users)
		if err != nil {
			return resolvedCodeResource[access.CodePoolInput]{}, err
		}
		keys, err := mapCodeResult(entry.APIKeyRefs, func(ref string) (string, error) { return resolveCodeKey(ctx, q, ref, project, owner, bindings.APIKeys) })
		if err != nil {
			return resolvedCodeResource[access.CodePoolInput]{}, err
		}
		id, exists, err := codeIdentity(ctx, q, `SELECT id::text FROM olp.code_pools WHERE project_id=$1 AND name=$2 AND kind=$3 AND owner_user_id IS NOT DISTINCT FROM $4::uuid`, project, entry.Name, entry.Kind, owner)
		return resolvedCodeResource[access.CodePoolInput]{Ref: entry.ID, ID: id, Existing: exists, Identity: codeIdentityKey(project, entry.Name, entry.Kind, codePointer(owner)), Input: access.CodePoolInput{ProjectID: project, Name: entry.Name, Kind: entry.Kind, OwnerUserID: owner, AccountIDs: mapCode(codeAccountRefs(entry), func(ref string) string { return accountIDs[ref] }), AccountWeights: maps.Collect(func(yield func(string, int64) bool) {
			for ref, weight := range entry.Accounts {
				if !yield(accountIDs[ref], weight) {
					return
				}
			}
		}), APIKeyIDs: keys}}, err
	})
	if err != nil {
		return nil, err
	}
	poolIDs := maps.Collect(func(yield func(string, string) bool) {
		for _, p := range pools {
			if !yield(p.Ref, p.ID) {
				return
			}
		}
	})
	routes, err := mapCodeResult(code.Routes, func(entry CodeRouteEntry) (resolvedCodeResource[CodeRouteEntry], error) {
		project, err := codeProject(ctx, q, entry.Project)
		if err != nil {
			return resolvedCodeResource[CodeRouteEntry]{}, err
		}
		var ordinary bool
		if err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.routes WHERE slug=$1) OR EXISTS(SELECT 1 FROM olp.route_drafts WHERE slug=$1) OR EXISTS(SELECT 1 FROM olp.code_routes WHERE slug=$1 AND project_id<>$2)`, entry.Slug, project).Scan(&ordinary); err != nil {
			return resolvedCodeResource[CodeRouteEntry]{}, err
		}
		if ordinary {
			return resolvedCodeResource[CodeRouteEntry]{}, access.Invalid("code.routes.slug", "Choose an unused slug or a code route in the destination project.")
		}
		id, exists, err := codeIdentity(ctx, q, `SELECT id::text FROM olp.code_routes WHERE slug=$1 AND project_id=$2`, entry.Slug, project)
		if err != nil {
			return resolvedCodeResource[CodeRouteEntry]{}, err
		}
		publication, err := resolveCodePublication(ctx, q, entry.Published, poolIDs, accountIDs, project)
		if err != nil {
			return resolvedCodeResource[CodeRouteEntry]{}, err
		}
		return resolvedCodeResource[CodeRouteEntry]{Ref: entry.ID, ID: id, Existing: exists, Input: CodeRouteEntry{ID: id, Project: project, Slug: entry.Slug, Draft: resolveCodePolicy(entry.Draft, poolIDs, accountIDs), Published: publication}}, err
	})
	if err != nil {
		return nil, err
	}
	routeIDs := maps.Collect(func(yield func(string, string) bool) {
		for _, r := range routes {
			if !yield(r.Ref, r.ID) {
				return
			}
		}
	})
	budgets, err := mapCodeResult(code.Budgets, func(entry CodeBudgetEntry) (resolvedCodeResource[access.CodeBudgetInput], error) {
		project, err := codeProject(ctx, q, entry.Project)
		if err != nil {
			return resolvedCodeResource[access.CodeBudgetInput]{}, err
		}
		key, err := resolveCodeOptionalKey(ctx, q, entry.APIKeyRef, project, bindings.APIKeys)
		if err != nil {
			return resolvedCodeResource[access.CodeBudgetInput]{}, err
		}
		route := codeOptionalRef(entry.Route, routeIDs)
		id, exists, err := codeIdentity(ctx, q, `SELECT id::text FROM olp.code_token_budgets WHERE project_id=$1 AND route_id IS NOT DISTINCT FROM $2::uuid AND api_key_id IS NOT DISTINCT FROM $3::uuid AND (daily_tokens IS NOT NULL)=$4 AND (monthly_tokens IS NOT NULL)=$5`, project, route, key, entry.DailyTokens != nil, entry.MonthlyTokens != nil)
		return resolvedCodeResource[access.CodeBudgetInput]{Ref: entry.ID, ID: id, Existing: exists, Identity: codeIdentityKey(project, codePointer(route), codePointer(key), strconv.FormatBool(entry.DailyTokens != nil), strconv.FormatBool(entry.MonthlyTokens != nil)), Input: access.CodeBudgetInput{ProjectID: project, RouteID: route, APIKeyID: key, DailyTokens: entry.DailyTokens, MonthlyTokens: entry.MonthlyTokens, Enabled: entry.Enabled}}, err
	})
	if err != nil {
		return nil, err
	}
	if err := uniqueCodeDestinations(accounts); err != nil {
		return nil, err
	}
	if err := uniqueCodeDestinations(pools); err != nil {
		return nil, err
	}
	if err := uniqueCodeDestinations(budgets); err != nil {
		return nil, err
	}
	return &resolvedCode{Accounts: accounts, Pools: pools, Routes: routes, Budgets: budgets}, nil
}

func resolveCodeOwner(ctx context.Context, q access.Queryer, ref *string, project string, bindings map[string]string) (*string, error) {
	if ref == nil {
		return nil, nil
	}
	id, err := codeBound(*ref, bindings)
	if err != nil {
		return nil, err
	}
	var member bool
	if err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.effective_project_members m JOIN olp.users u ON u.id=m.user_id WHERE m.project_id=$1 AND m.user_id=$2 AND u.active)`, project, id).Scan(&member); err != nil {
		return nil, err
	}
	if !member {
		return nil, access.Invalid("code_bindings.users", "The personal pool owner must be an active destination project member.")
	}
	return &id, nil
}
func resolveCodeKey(ctx context.Context, q access.Queryer, ref, project string, owner *string, bindings map[string]string) (string, error) {
	id, err := codeBound(ref, bindings)
	if err != nil {
		return "", err
	}
	var allowed bool
	if err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.api_keys WHERE id=$1 AND project_id=$2 AND revoked_at IS NULL AND ($3::uuid IS NULL OR created_by=$3))`, id, project, owner).Scan(&allowed); err != nil {
		return "", err
	}
	if !allowed {
		return "", access.Invalid("code_bindings.api_keys", "The destination key must belong to the project and personal pool owner.")
	}
	return id, nil
}
func resolveCodeOptionalKey(ctx context.Context, q access.Queryer, ref *string, project string, bindings map[string]string) (*string, error) {
	if ref == nil {
		return nil, nil
	}
	id, err := resolveCodeKey(ctx, q, *ref, project, nil, bindings)
	return &id, err
}
func codeOptionalRef(ref *string, ids map[string]string) *string {
	if ref == nil {
		return nil
	}
	return new(ids[*ref])
}
func resolveCodePolicy(policy CodeRoutePolicy, pools, accounts map[string]string) CodeRoutePolicy {
	tools := func() *codemode.ResearchTools {
		if policy.Tools == nil {
			return nil
		}
		return &codemode.ResearchTools{AccountID: accounts[policy.Tools.AccountID], Services: policy.Tools.Services}
	}()
	return CodeRoutePolicy{Pool: pools[policy.Pool], Models: policy.Models, Enabled: policy.Enabled, ModelPools: policy.ModelPools, InitialRecoveryAttempts: policy.InitialRecoveryAttempts, ModelRewriting: policy.ModelRewriting, Aliases: policy.Aliases, MessagesToCodex: policy.MessagesToCodex, Tools: tools, MaxBodyBytes: policy.MaxBodyBytes}
}

func resolveCodePublication(ctx context.Context, q access.Queryer, publication *CodePublication, pools, accounts map[string]string, project string) (*CodePublication, error) {
	if publication == nil {
		return nil, nil
	}
	type connection struct {
		ID            string
		Configuration providers.Configuration
	}
	connections, err := mapCodeResult(slices.Sorted(maps.Keys(publication.Connections)), func(name string) (connection, error) {
		entry := publication.Connections[name]
		cfg := entry.Configuration
		var id string
		var current providers.Configuration
		if err := q.QueryRow(ctx, `SELECT id::text,configuration FROM olp.providers WHERE lower(name)=lower($1) AND project_id=$2`, name, project).Scan(&id, &current); err != nil {
			return connection{}, err
		}
		if cfg.Kind != current.Kind || cfg.AuthMode != current.AuthMode || cfg.ProfileID != current.ProfileID {
			return connection{}, access.Invalid("code.routes.published.connections", "Frozen connections must retain the destination provider's kind, authentication and profile.")
		}
		if entry.NetworkCredentialRef != nil {
			if *entry.NetworkCredentialRef != networkRef(name) || cfg.Options.Network == nil {
				return connection{}, access.Invalid("code.routes.published.connections", "Use the named provider network credential reference.")
			}
			if current.Options.Network == nil || current.Options.Network.CredentialID == "" {
				return connection{}, access.Invalid("code.routes.published.connections", "Bind the destination provider network credential first.")
			}
			network := *cfg.Options.Network
			network.CredentialID = current.Options.Network.CredentialID
			options := cfg.Options
			options.Network = &network
			cfg.Options = options
		}
		return connection{id, cfg}, nil
	})
	if err != nil {
		return nil, err
	}
	return &CodePublication{Policy: resolveCodePolicy(publication.Policy, pools, accounts), Connections: maps.Collect(func(yield func(string, CodeConnectionEntry) bool) {
		for _, c := range connections {
			if !yield(c.ID, CodeConnectionEntry{Configuration: c.Configuration}) {
				return
			}
		}
	})}, nil
}
func uniqueCodeDestinations[T any](entries []resolvedCodeResource[T]) error {
	for i, e := range entries {
		if slices.ContainsFunc(entries[:i], func(other resolvedCodeResource[T]) bool {
			return other.ID == e.ID || e.Identity != "" && other.Identity == e.Identity
		}) {
			return access.Invalid("code", "Multiple artifact resources resolve to the same destination identity.")
		}
	}
	return nil
}

func authorizeCodePromotion(ctx context.Context, q access.Queryer, p access.Principal, doc *Document) error {
	if doc.Code == nil {
		return nil
	}
	projects := slices.Concat(mapCode(doc.Code.Accounts, func(e CodeAccountEntry) string { return e.Project }), mapCode(doc.Code.Pools, func(e CodePoolEntry) string { return e.Project }), mapCode(doc.Code.Routes, func(e CodeRouteEntry) string { return e.Project }), mapCode(doc.Code.Budgets, func(e CodeBudgetEntry) string { return e.Project }))
	for _, project := range projects {
		id, err := codeProject(ctx, q, project)
		if err != nil {
			return err
		}
		if err := p.Project(&id, access.Change); err != nil {
			return err
		}
	}
	return nil
}

func codeRefProject[T any](ref, project string, entries []T, get func(T) (string, string)) bool {
	return slices.ContainsFunc(entries, func(e T) bool { id, p := get(e); return id == ref && strings.EqualFold(p, project) })
}

func codeIdentityKey(values ...string) string {
	encoded, _ := json.Marshal(values)
	return string(encoded)
}
func codePointer(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
