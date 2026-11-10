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
	domain "github.com/tyk-swe/olp/internal/code"
	codepool "github.com/tyk-swe/olp/internal/code/pool"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion"
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
	keys := e.ApiKeyRefs.GetOrEmpty()
	p := Pool{ID: e.Id.String(), Project: e.Project, Name: e.Name, OwnerRef: owner, Accounts: weights, APIKeyRefs: promotion.NonNil(slices.Clone(keys))}
	placeholder := codepool.Input{ProjectID: p.ID, Name: p.Name, Kind: domain.PoolKind(e.Kind), OwnerUserID: placeholderOf(owner, p.ID), AccountIDs: promotion.NonNil(slices.Sorted(maps.Keys(weights))), AccountWeights: weights, APIKeyIDs: promotion.Map(keys, func(string) string { return p.ID })}
	if err := placeholder.Validate(); err != nil {
		return Pool{}, err
	}
	if slices.ContainsFunc(keys, func(ref string) bool { return access.ValidText("code", ref, 200) != nil }) {
		return Pool{}, access.Invalid("code.pools.api_key_refs", "Use nonempty destination key references.")
	}
	return p, nil
}

func poolAccounts(e contract.ConfigurationCodePool, accounts []Account) (map[string]int64, error) {
	weights := maps.Collect(func(yield func(string, int64) bool) {
		for ref, weight := range e.Accounts {
			if !yield(canonical(ref), int64(weight)) {
				return
			}
		}
	})
	undeclared := func(id string) bool {
		return !slices.ContainsFunc(accounts, func(a Account) bool { return a.ID == id && strings.EqualFold(a.Project, e.Project) })
	}
	if len(weights) != len(e.Accounts) || slices.ContainsFunc(slices.Collect(maps.Keys(weights)), undeclared) {
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

func placeholderOf(ref *string, id string) *string {
	if ref == nil {
		return nil
	}
	return &id
}

func (p Pool) kind() domain.PoolKind {
	if p.OwnerRef != nil {
		return domain.PoolPersonal
	}
	return domain.PoolShared
}

func (p Pool) ownerRefs() []string {
	if p.OwnerRef == nil {
		return []string{}
	}
	return []string{*p.OwnerRef}
}

func readPools(ctx context.Context, q access.Queryer) ([]Pool, error) {
	pools, err := codepool.ReadStored(ctx, q)
	if err != nil {
		return nil, err
	}
	return promotion.Map(pools, func(p codepool.Stored) Pool {
		keys := promotion.Map(p.APIKeyIDs, func(id string) string { return codeRef("key", id) })
		return Pool{ID: p.ID, Project: p.Project, Name: p.Name, OwnerRef: optionalRef("user", p.OwnerUserID), Accounts: p.AccountWeights, APIKeyRefs: keys}
	}), nil
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

func resolvePool(ctx context.Context, q access.Queryer, p Pool, desired Desired, directory promotion.Directory, accountIDs map[string]string) (resolved[codepool.Input], error) {
	projectID, err := projectOf(directory, p.Project)
	if err != nil {
		return resolved[codepool.Input]{}, err
	}
	ownerID, err := owner(ctx, q, p.OwnerRef, projectID, desired.bindings.users)
	if err != nil {
		return resolved[codepool.Input]{}, err
	}
	keys, err := keysOf(ctx, q, p.APIKeyRefs, projectID, ownerID, desired.bindings.keys)
	if err != nil {
		return resolved[codepool.Input]{}, err
	}
	derived, err := derivedID("pool", projectID, p.ID)
	if err != nil {
		return resolved[codepool.Input]{}, err
	}
	unique := !slices.ContainsFunc(desired.Code.Pools, func(other Pool) bool {
		return other.ID != p.ID && strings.EqualFold(other.Project, p.Project) && other.Name == p.Name && other.kind() == p.kind() && boundOr(other.OwnerRef, desired.bindings.users) == boundOr(p.OwnerRef, desired.bindings.users)
	})
	candidates, err := codepool.Candidates(ctx, q, p.ID, derived, codepool.Match{ProjectID: projectID, Name: p.Name, Kind: p.kind(), OwnerUserID: ownerID})
	if err != nil {
		return resolved[codepool.Input]{}, err
	}
	to, err := mapped(p.ID, projectID, derived, unique, candidates)
	weights := maps.Collect(func(yield func(string, int64) bool) {
		for ref, weight := range p.Accounts {
			if !yield(accountIDs[ref], weight) {
				return
			}
		}
	})
	input := codepool.Input{ProjectID: projectID, Name: p.Name, Kind: p.kind(), OwnerUserID: ownerID, AccountIDs: promotion.NonNil(slices.Sorted(maps.Keys(weights))), AccountWeights: weights, APIKeyIDs: promotion.NonNil(slices.Sorted(slices.Values(keys)))}
	return resolved[codepool.Input]{ref: p.ID, to: to, input: input}, err
}

func poolChange(ctx context.Context, q access.Queryer, p resolved[codepool.Input]) (change, error) {
	action, err := compared(ctx, q, p, codepool.ReadInput)
	return change{
		item: promotion.Item{Kind: contract.ConfigurationPlanItemKindCodePool, Key: p.ref, Action: action},
		write: func(ctx context.Context, tx pgx.Tx, principal access.Principal) error {
			draft, err := codepool.ParseDraft(p.input)
			if err != nil {
				return err
			}
			_, err = codepool.Save(ctx, tx, principal, p.to.id, access.NewID(), draft, p.to.existing)
			return err
		},
	}, err
}
