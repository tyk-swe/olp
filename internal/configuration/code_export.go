package configuration

import (
	"context"
	"encoding/json"
	"github.com/jackc/pgx/v5"
	"maps"
	"slices"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/routing"
)

func exportCode(ctx context.Context, q access.Queryer) (*CodeConfiguration, error) {
	var accounts []CodeAccountEntry
	if err := q.QueryRow(ctx, `SELECT coalesce(jsonb_agg(jsonb_build_object('id',a.id,'project',p.name,'provider',v.name,'credential_ref','code/account/'||a.id,'name',a.name,'enabled',a.enabled,'models',a.models)),'[]') FROM olp.code_accounts a JOIN olp.projects p ON p.id=a.project_id JOIN olp.providers v ON v.id=a.provider_id`).Scan(&accounts); err != nil {
		return nil, err
	}
	var pools []CodePoolEntry
	if err := q.QueryRow(ctx, `SELECT coalesce(jsonb_agg(jsonb_build_object('id',x.id,'project',p.name,'name',x.name,'kind',x.kind,'owner_ref',CASE WHEN x.owner_user_id IS NULL THEN NULL ELSE 'code/user/'||x.owner_user_id END,'accounts',coalesce((SELECT jsonb_object_agg(account_id::text,weight) FROM olp.code_pool_accounts WHERE pool_id=x.id),'{}'),'api_key_refs',ARRAY(SELECT 'code/key/'||api_key_id FROM olp.code_pool_keys WHERE pool_id=x.id ORDER BY api_key_id))),'[]') FROM olp.code_pools x JOIN olp.projects p ON p.id=x.project_id`).Scan(&pools); err != nil {
		return nil, err
	}
	type storedRoute struct {
		ID              string
		Project         string
		Slug            string
		Draft           codemode.Route
		Published       *codemode.Route
		Connections     map[string]providers.Configuration
		PaidConnections map[string]routing.Provider
	}
	rows, err := q.Query(ctx, `SELECT x.id::text,p.name,x.slug,x.draft,v.document,coalesce((SELECT jsonb_object_agg(p.name,c.value) FROM jsonb_each(v.connections) c JOIN olp.providers p ON p.id::text=c.key),'{}'),coalesce(v.paid_connections,'{}') FROM olp.code_routes x JOIN olp.projects p ON p.id=x.project_id LEFT JOIN olp.code_route_revisions v ON v.id=x.latest_revision_id`)
	if err != nil {
		return nil, err
	}
	stored, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (storedRoute, error) {
		var out storedRoute
		err := row.Scan(&out.ID, &out.Project, &out.Slug, &out.Draft, &out.Published, &out.Connections, &out.PaidConnections)
		return out, err
	})
	if err != nil {
		return nil, err
	}
	refs, err := exportCodePaidReferences(ctx, q)
	if err != nil {
		return nil, err
	}
	routes, err := mapCodeResult(stored, func(row storedRoute) (CodeRouteEntry, error) {
		draft, err := exportCodePaidPolicy(row.Draft, refs)
		if err != nil {
			return CodeRouteEntry{}, err
		}
		if row.Published == nil {
			return CodeRouteEntry{ID: row.ID, Project: row.Project, Slug: row.Slug, Draft: draft}, nil
		}
		published, err := exportCodePaidPolicy(*row.Published, refs)
		if err != nil {
			return CodeRouteEntry{}, err
		}
		paid, err := exportCodePaid(ctx, q, row.PaidConnections)
		if err != nil {
			return CodeRouteEntry{}, err
		}
		return CodeRouteEntry{ID: row.ID, Project: row.Project, Slug: row.Slug, Draft: draft, Published: &CodePublication{Policy: published, PaidConnections: paid, Connections: maps.Collect(func(yield func(string, CodeConnectionEntry) bool) {
			for name, cfg := range row.Connections {
				entry := ProviderEntry{Name: name, Configuration: cfg}
				portableNetwork(&entry)
				if !yield(name, CodeConnectionEntry{Configuration: entry.Configuration, NetworkCredentialRef: entry.NetworkCredentialRef}) {
					return
				}
			}
		})}}, nil
	})
	if err != nil {
		return nil, err
	}
	var budgets []CodeBudgetEntry
	if err := q.QueryRow(ctx, `SELECT coalesce(jsonb_agg(jsonb_build_object('id',x.id,'project',p.name,'route',x.route_id,'api_key_ref',CASE WHEN x.api_key_id IS NULL THEN NULL ELSE 'code/key/'||x.api_key_id END,'daily_tokens',x.daily_tokens,'monthly_tokens',x.monthly_tokens,'enabled',x.enabled)),'[]') FROM olp.code_token_budgets x JOIN olp.projects p ON p.id=x.project_id`).Scan(&budgets); err != nil {
		return nil, err
	}
	if len(accounts)+len(pools)+len(routes)+len(budgets) == 0 {
		return nil, nil
	}
	return &CodeConfiguration{Accounts: accounts, Pools: pools, Routes: routes, Budgets: budgets}, nil
}

func mapCode[A, B any](values []A, project func(A) B) []B {
	return append([]B{}, slices.Collect(func(yield func(B) bool) {
		for _, value := range values {
			if !yield(project(value)) {
				return
			}
		}
	})...)
}

func codeJSONEqual[A any](a, b A) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return string(left) == string(right)
}
