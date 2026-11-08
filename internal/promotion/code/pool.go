package code

import (
	"context"
	"maps"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/oapi-codegen/nullable"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/promotion/project"
)

type Pool struct {
	ID         string
	Project    string
	Name       string
	OwnerRef   *string
	Accounts   map[string]int64
	APIKeyRefs []string
}

func parsePool(e contract.ConfigurationCodePool, accounts []Account) (Pool, error) {
	if access.ValidText("code", e.Project, 100) != nil {
		return Pool{}, access.Invalid("code.pools.project", "Name the project.")
	}
	owner := promotion.Pointer(e.OwnerRef)
	if (e.Kind == contract.ConfigurationCodePoolKindPersonal) != (owner != nil) {
		return Pool{}, access.Invalid("code.pools.owner_ref", "Personal pools require an explicit owner reference.")
	}
	weights, err := poolAccounts(e, accounts)
	if err != nil {
		return Pool{}, err
	}
	keys := present(e.ApiKeyRefs)
	pool := Pool{ID: e.Id.String(), Project: e.Project, Name: e.Name, OwnerRef: owner, Accounts: weights, APIKeyRefs: promotion.NonNil(slices.Clone(keys))}
	placeholder := access.CodePoolInput{ProjectID: pool.ID, Name: pool.Name, Kind: string(e.Kind), OwnerUserID: placeholderOf(owner, pool.ID), AccountIDs: sortedKeys(weights), AccountWeights: weights, APIKeyIDs: promotion.Map(keys, func(string) string { return pool.ID })}
	if err := placeholder.Validate(); err != nil {
		return Pool{}, err
	}
	if slices.ContainsFunc(keys, func(ref string) bool { return access.ValidText("code", ref, 200) != nil }) {
		return Pool{}, access.Invalid("code.pools.api_key_refs", "Use nonempty destination key references.")
	}
	return pool, nil
}

func poolAccounts(e contract.ConfigurationCodePool, accounts []Account) (map[string]int64, error) {
	weights := maps.Collect(func(yield func(string, int64) bool) {
		for ref, weight := range e.Accounts {
			if !yield(canonical(ref), int64(weight)) {
				return
			}
		}
	})
	declared := func(id string) bool {
		return slices.ContainsFunc(accounts, func(a Account) bool { return a.ID == id && strings.EqualFold(a.Project, e.Project) })
	}
	if len(weights) != len(e.Accounts) || !all(slices.Collect(maps.Keys(weights)), declared) {
		return nil, access.Invalid("code.pools.accounts", "Every account must resolve within the pool project.")
	}
	return weights, nil
}

func canonical(ref string) string {
	id, err := access.ParseUUID(ref)
	if err != nil {
		return ""
	}
	return id
}

func all[T any](items []T, keep func(T) bool) bool {
	return !slices.ContainsFunc(items, func(item T) bool { return !keep(item) })
}

func placeholderOf(ref *string, id string) *string {
	if ref == nil {
		return nil
	}
	return &id
}

func (p Pool) kind() string {
	if p.OwnerRef != nil {
		return string(contract.ConfigurationCodePoolKindPersonal)
	}
	return string(contract.ConfigurationCodePoolKindShared)
}

func (p Pool) ownerRefs() []string {
	if p.OwnerRef == nil {
		return []string{}
	}
	return []string{*p.OwnerRef}
}

func readPools(ctx context.Context, q access.Queryer) ([]Pool, error) {
	return promotion.Rows(ctx, q, pgx.RowToStructByPos[Pool], `SELECT x.id::text,p.name,x.name,CASE WHEN x.owner_user_id IS NULL THEN NULL ELSE 'code/user/'||x.owner_user_id END,coalesce((SELECT jsonb_object_agg(account_id::text,weight) FROM olp.code_pool_accounts WHERE pool_id=x.id),'{}'),ARRAY(SELECT 'code/key/'||api_key_id FROM olp.code_pool_keys WHERE pool_id=x.id ORDER BY api_key_id) FROM olp.code_pools x JOIN olp.projects p ON p.id=x.project_id`)
}

func renderPool(p Pool) (contract.ConfigurationCodePool, error) {
	id, err := uuid.Parse(p.ID)
	if err != nil {
		return contract.ConfigurationCodePool{}, err
	}
	accounts := maps.Collect(func(yield func(string, int) bool) {
		for ref, weight := range p.Accounts {
			if !yield(ref, int(weight)) {
				return
			}
		}
	})
	keys := slices.Sorted(slices.Values(p.APIKeyRefs))
	return contract.ConfigurationCodePool{Id: id, Project: p.Project, Name: p.Name, Kind: contract.ConfigurationCodePoolKind(p.kind()), OwnerRef: promotion.Nullable(p.OwnerRef), Accounts: accounts, ApiKeyRefs: nullable.NewNullableWithValue(promotion.NonNil(slices.Clone(keys)))}, nil
}

func resolvePool(ctx context.Context, q access.Queryer, p Pool, desired Desired, directory project.Directory, accountIDs map[string]string) (resolved[access.CodePoolInput], error) {
	projectID, err := projectOf(directory, p.Project)
	if err != nil {
		return resolved[access.CodePoolInput]{}, err
	}
	ownerID, err := owner(ctx, q, p.OwnerRef, projectID, desired.bindings.users)
	if err != nil {
		return resolved[access.CodePoolInput]{}, err
	}
	keys, err := keysOf(ctx, q, p.APIKeyRefs, projectID, ownerID, desired.bindings.keys)
	if err != nil {
		return resolved[access.CodePoolInput]{}, err
	}
	derived, err := derivedID("pool", projectID, p.ID)
	if err != nil {
		return resolved[access.CodePoolInput]{}, err
	}
	unique := !slices.ContainsFunc(desired.Code.Pools, func(other Pool) bool {
		return other.ID != p.ID && strings.EqualFold(other.Project, p.Project) && other.Name == p.Name && other.kind() == p.kind() && boundOr(other.OwnerRef, desired.bindings.users) == boundOr(p.OwnerRef, desired.bindings.users)
	})
	to, err := mapped(ctx, q, p.ID, projectID, derived, unique, `SELECT id::text,project_id::text,(kind=$3 AND owner_user_id IS NOT DISTINCT FROM $4::uuid),(name=$2 AND kind=$3 AND owner_user_id IS NOT DISTINCT FROM $4::uuid) FROM olp.code_pools WHERE id=$5 OR id=$6 OR (project_id=$1 AND name=$2 AND kind=$3 AND owner_user_id IS NOT DISTINCT FROM $4::uuid)`, projectID, p.Name, p.kind(), ownerID, p.ID, derived)
	weights := maps.Collect(func(yield func(string, int64) bool) {
		for ref, weight := range p.Accounts {
			if !yield(accountIDs[ref], weight) {
				return
			}
		}
	})
	input := access.CodePoolInput{ProjectID: projectID, Name: p.Name, Kind: p.kind(), OwnerUserID: ownerID, AccountIDs: sortedKeys(weights), AccountWeights: weights, APIKeyIDs: promotion.NonNil(slices.Sorted(slices.Values(keys)))}
	return resolved[access.CodePoolInput]{ref: p.ID, to: to, input: input}, err
}

func poolChange(ctx context.Context, q access.Queryer, p resolved[access.CodePoolInput]) (change, error) {
	action, err := compared(ctx, q, p, `SELECT jsonb_build_object('project_id',project_id,'name',name,'kind',kind,'owner_user_id',owner_user_id,'account_ids',ARRAY(SELECT account_id FROM olp.code_pool_accounts WHERE pool_id=x.id ORDER BY account_id),'account_weights',coalesce((SELECT jsonb_object_agg(account_id::text,weight) FROM olp.code_pool_accounts WHERE pool_id=x.id),'{}'),'api_key_ids',ARRAY(SELECT api_key_id FROM olp.code_pool_keys WHERE pool_id=x.id ORDER BY api_key_id)) FROM olp.code_pools x WHERE id=$1`)
	return change{
		item: promotion.Item{Kind: contract.ConfigurationPlanItemKindCodePool, Key: p.ref, Action: action},
		write: func(ctx context.Context, tx pgx.Tx, principal access.Principal) error {
			_, err := access.SaveCodePool(ctx, tx, principal, p.to.id, access.NewID(), p.input, p.to.existing)
			return err
		},
	}, err
}
