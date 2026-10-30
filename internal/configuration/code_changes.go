package configuration

import (
	"context"
	"maps"
	"slices"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/routing"
)

type codeChange struct {
	Kind               string
	Key                string
	ID                 string
	Action             string
	PublicationChanged bool
}

func codeChangeFor[T any](ctx context.Context, q access.Queryer, kind string, entry resolvedCodeResource[T], query string) (codeChange, error) {
	if !entry.Existing {
		return codeChange{Kind: kind, Key: entry.Ref, ID: entry.ID, Action: "create"}, nil
	}
	var current T
	if err := q.QueryRow(ctx, query, entry.ID).Scan(&current); err != nil {
		return codeChange{}, err
	}
	return codeChange{Kind: kind, Key: entry.Ref, ID: entry.ID, Action: codeReplacement(!codeJSONEqual(current, entry.Input))}, nil
}
func codeChanges(ctx context.Context, q access.Queryer, resolved *resolvedCode) ([]codeChange, error) {
	accounts, err := mapCodeResult(resolved.Accounts, func(e resolvedCodeResource[providers.CodeAccountInput]) (codeChange, error) {
		return codeChangeFor(ctx, q, "code_account", e, `SELECT jsonb_build_object('project_id',project_id,'provider_id',provider_id,'credential_id',credential_id,'name',name,'enabled',enabled,'models',models) FROM olp.code_accounts WHERE id=$1`)
	})
	if err != nil {
		return nil, err
	}
	pools, err := mapCodeResult(resolved.Pools, func(e resolvedCodeResource[access.CodePoolInput]) (codeChange, error) {
		input := e.Input
		normalized := access.CodePoolInput{ProjectID: input.ProjectID, Name: input.Name, Kind: input.Kind, OwnerUserID: input.OwnerUserID, AccountIDs: sortedCode(input.AccountIDs), AccountWeights: input.AccountWeights, APIKeyIDs: sortedCode(input.APIKeyIDs)}
		return codeChangeFor(ctx, q, "code_pool", resolvedCodeResource[access.CodePoolInput]{Ref: e.Ref, ID: e.ID, Existing: e.Existing, Input: normalized}, `SELECT jsonb_build_object('project_id',project_id,'name',name,'kind',kind,'owner_user_id',owner_user_id,'account_ids',ARRAY(SELECT account_id FROM olp.code_pool_accounts WHERE pool_id=x.id ORDER BY account_id),'account_weights',coalesce((SELECT jsonb_object_agg(account_id::text,weight) FROM olp.code_pool_accounts WHERE pool_id=x.id),'{}'),'api_key_ids',ARRAY(SELECT api_key_id FROM olp.code_pool_keys WHERE pool_id=x.id ORDER BY api_key_id)) FROM olp.code_pools x WHERE id=$1`)
	})
	if err != nil {
		return nil, err
	}
	routes, err := mapCodeResult(resolved.Routes, func(e resolvedCodeResource[CodeRouteEntry]) (codeChange, error) {
		if !e.Existing {
			return codeChange{Kind: "code_route", Key: e.Ref, ID: e.ID, Action: "create", PublicationChanged: e.Input.Published != nil}, nil
		}
		var draft codemode.Route
		var published *codemode.Route
		var connections map[string]providers.Configuration
		var paid map[string]routing.Provider
		if err := q.QueryRow(ctx, `SELECT x.draft,v.document,coalesce(v.connections,'{}'),coalesce(v.paid_connections,'{}') FROM olp.code_routes x LEFT JOIN olp.code_route_revisions v ON v.id=x.latest_revision_id WHERE x.id=$1`, e.ID).Scan(&draft, &published, &connections, &paid); err != nil {
			return codeChange{}, err
		}
		var publication *CodePublication
		if published != nil {
			publication = &CodePublication{Policy: codePolicy(*published), Connections: mapCodeConnections(connections), paidResolved: paid}
		}
		draftChanged := !codeJSONEqual(codePolicy(draft), e.Input.Draft)
		publishedChanged := !codePublicationEqual(publication, e.Input.Published)
		return codeChange{Kind: "code_route", Key: e.Ref, ID: e.ID, Action: codeReplacement(draftChanged || publishedChanged), PublicationChanged: publishedChanged}, nil
	})
	if err != nil {
		return nil, err
	}
	budgets, err := mapCodeResult(resolved.Budgets, func(e resolvedCodeResource[access.CodeBudgetInput]) (codeChange, error) {
		return codeChangeFor(ctx, q, "code_budget", e, `SELECT jsonb_build_object('project_id',project_id,'route_id',route_id,'api_key_id',api_key_id,'daily_tokens',daily_tokens,'monthly_tokens',monthly_tokens,'enabled',enabled) FROM olp.code_token_budgets WHERE id=$1`)
	})
	if err != nil {
		return nil, err
	}
	return slices.Concat(accounts, pools, routes, budgets), nil
}
func sortedCode(values []string) []string {
	if len(values) == 0 {
		return []string{}
	}
	out := slices.Clone(values)
	slices.Sort(out)
	return out
}

func planCode(ctx context.Context, q access.Queryer, doc *Document, result *planResult) error {
	if err := doc.codeBindings.validate(doc.Code); err != nil {
		return err
	}
	resolved, err := resolveCode(ctx, q, doc.Code, doc.codeBindings)
	if err != nil {
		return err
	}
	changes, err := codeChanges(ctx, q, resolved)
	if err != nil {
		return err
	}
	for _, change := range changes {
		result.item(change.Kind, change.Key, change.Action, "")
	}
	return nil
}

func mapCodeConnections(connections map[string]providers.Configuration) map[string]CodeConnectionEntry {
	return maps.Collect(func(yield func(string, CodeConnectionEntry) bool) {
		for id, cfg := range connections {
			if !yield(id, CodeConnectionEntry{Configuration: cfg}) {
				return
			}
		}
	})
}

func codePublicationEqual(left, right *CodePublication) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return codeJSONEqual(left.Policy, right.Policy) && codeJSONEqual(left.Connections, right.Connections) && codeJSONEqual(left.paidResolved, right.paidResolved)
}

func codeReplacement(changed bool) string {
	if changed {
		return "replace"
	}
	return "noop"
}
