package codeadmin

import (
	"context"
	"encoding/json"
	"maps"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/platform/database"
)

type Pool struct {
	ID             string
	Project        string
	Name           string
	OwnerUserID    *string
	AccountWeights map[string]int64
	APIKeyIDs      []string
}

type Candidate struct {
	ID             string
	ProjectID      string
	ScopeMatches   bool
	NaturalMatches bool
}

type PoolMatch struct {
	ProjectID   string
	Name        string
	Kind        string
	OwnerUserID *string
}

func ReadPools(ctx context.Context, q database.Queryer) ([]Pool, error) {
	rows, err := q.Query(ctx, `SELECT x.id::text,p.name,x.name,x.owner_user_id::text,coalesce((SELECT jsonb_object_agg(account_id::text,weight) FROM olp.code_pool_accounts WHERE pool_id=x.id),'{}'),ARRAY(SELECT api_key_id::text FROM olp.code_pool_keys WHERE pool_id=x.id ORDER BY api_key_id) FROM olp.code_pools x JOIN olp.projects p ON p.id=x.project_id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Pool])
}

func PoolCandidates(ctx context.Context, q database.Queryer, ref, derived string, match PoolMatch) ([]Candidate, error) {
	rows, err := q.Query(ctx, `SELECT id::text,project_id::text,(kind=$3 AND owner_user_id IS NOT DISTINCT FROM $4::uuid),(name=$2 AND kind=$3 AND owner_user_id IS NOT DISTINCT FROM $4::uuid) FROM olp.code_pools WHERE id=$5 OR id=$6 OR (project_id=$1 AND name=$2 AND kind=$3 AND owner_user_id IS NOT DISTINCT FROM $4::uuid)`, match.ProjectID, match.Name, match.Kind, match.OwnerUserID, ref, derived)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Candidate])
}

func ReadPoolInput(ctx context.Context, q database.Queryer, id string) (PoolInput, error) {
	var input PoolInput
	err := q.QueryRow(ctx, `SELECT jsonb_build_object('project_id',project_id,'name',name,'kind',kind,'owner_user_id',owner_user_id,'account_ids',ARRAY(SELECT account_id FROM olp.code_pool_accounts WHERE pool_id=x.id ORDER BY account_id),'account_weights',coalesce((SELECT jsonb_object_agg(account_id::text,weight) FROM olp.code_pool_accounts WHERE pool_id=x.id),'{}'),'api_key_ids',ARRAY(SELECT api_key_id FROM olp.code_pool_keys WHERE pool_id=x.id ORDER BY api_key_id)) FROM olp.code_pools x WHERE id=$1`, id).Scan(&input)
	return input, err
}

const poolJSON = `SELECT jsonb_build_object('id',x.id,'project_id',x.project_id,'name',x.name,'kind',x.kind,'owner_user_id',x.owner_user_id,'etag',x.etag,
	'account_weights',coalesce((SELECT jsonb_object_agg(account_id::text,weight) FROM olp.code_pool_accounts WHERE pool_id=x.id),'{}'::jsonb),
	'account_ids',ARRAY(SELECT account_id FROM olp.code_pool_accounts WHERE pool_id=x.id ORDER BY account_id),
	'api_key_ids',ARRAY(SELECT api_key_id FROM olp.code_pool_keys WHERE pool_id=x.id ORDER BY api_key_id)) FROM olp.code_pools x`

type PoolInput struct {
	ProjectID      string           `json:"project_id"`
	Name           string           `json:"name"`
	Kind           string           `json:"kind"`
	OwnerUserID    *string          `json:"owner_user_id"`
	AccountIDs     []string         `json:"account_ids"`
	AccountWeights map[string]int64 `json:"account_weights"`
	APIKeyIDs      []string         `json:"api_key_ids"`
}

func SavePool(ctx context.Context, tx pgx.Tx, p authz.Principal, id, etag string, raw PoolInput, existing bool) (json.RawMessage, error) {
	in, validationError := raw.Validated()
	if validationError != nil {
		return nil, validationError
	}
	if err := p.Project(&in.ProjectID, authz.Change); err != nil {
		return nil, err
	}
	if in.OwnerUserID != nil {
		var member bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.effective_project_members WHERE project_id=$1 AND user_id=$2)`, in.ProjectID, *in.OwnerUserID).Scan(&member); err != nil {
			return nil, err
		}
		if !member {
			return nil, httpkit.Invalid("owner_user_id", "The owner must be a project member.")
		}
	}
	for _, account := range in.AccountIDs {
		if err := scoped(ctx, tx, "code_accounts", account, in.ProjectID); err != nil {
			return nil, err
		}
	}
	for _, key := range in.APIKeyIDs {
		if err := scoped(ctx, tx, "api_keys", key, in.ProjectID); err != nil {
			return nil, err
		}
		if in.OwnerUserID != nil {
			var owner string
			if err := tx.QueryRow(ctx, `SELECT created_by::text FROM olp.api_keys WHERE id=$1`, key).Scan(&owner); err != nil {
				return nil, err
			}
			if owner != *in.OwnerUserID {
				return nil, httpkit.Invalid("api_key_ids", "Personal pools accept only their owner's keys.")
			}
		}
	}
	if existing {
		var kind string
		var owner *string
		if err := tx.QueryRow(ctx, `SELECT kind,owner_user_id::text FROM olp.code_pools WHERE id=$1`, id).Scan(&kind, &owner); err != nil {
			return nil, err
		}
		if kind != in.Kind || (owner == nil) != (in.OwnerUserID == nil) || owner != nil && *owner != *in.OwnerUserID {
			return nil, httpkit.Invalid("kind", "Pool ownership is immutable.")
		}
	}
	_, err := tx.Exec(ctx, `INSERT INTO olp.code_pools(id,project_id,name,kind,owner_user_id,etag,created_by) VALUES($1,$2,$3,$4,$5,$6,$7)
			ON CONFLICT(id) DO UPDATE SET name=excluded.name,etag=excluded.etag`, id, in.ProjectID, in.Name, in.Kind, in.OwnerUserID, etag, p.UserID())
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM olp.code_pool_accounts WHERE pool_id=$1`, id); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM olp.code_pool_keys WHERE pool_id=$1`, id); err != nil {
		return nil, err
	}
	for _, account := range in.AccountIDs {
		if _, err = tx.Exec(ctx, `INSERT INTO olp.code_pool_accounts(pool_id,account_id,weight) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, id, account, max(1, in.AccountWeights[account])); err != nil {
			return nil, err
		}
	}
	for _, key := range in.APIKeyIDs {
		if _, err = tx.Exec(ctx, `INSERT INTO olp.code_pool_keys(pool_id,api_key_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, id, key); err != nil {
			return nil, err
		}
	}
	var result json.RawMessage
	queryErr := tx.QueryRow(ctx, poolJSON+` WHERE x.id=$1`, id).Scan(&result)
	return result, queryErr
}

func (in PoolInput) validate() error {
	if err := httpkit.ValidText("name", in.Name, 100); err != nil {
		return err
	}
	if in.Kind != "personal" && in.Kind != "shared" || (in.Kind == "personal") != (in.OwnerUserID != nil) {
		return httpkit.Invalid("kind", "Personal pools require an owner; shared pools have none.")
	}
	for account, weight := range in.AccountWeights {
		if !slices.Contains(in.AccountIDs, account) || weight < 1 || weight > 2147483647 {
			return httpkit.Invalid("account_weights", "Weights must be between 1 and 2147483647 and belong to assigned accounts.")
		}
	}
	return nil
}

func (in PoolInput) Validated() (PoolInput, error) {
	if in.AccountIDs == nil || in.APIKeyIDs == nil || len(in.AccountIDs) > 100 || len(in.APIKeyIDs) > 100 {
		return PoolInput{}, httpkit.Invalid("assignments", "Use at most 100 accounts and keys.")
	}
	weightKeys := slices.Sorted(maps.Keys(in.AccountWeights))
	parsed, err := httpkit.ParseUUIDs(slices.Concat([]string{in.ProjectID}, in.AccountIDs, in.APIKeyIDs, weightKeys))
	if err != nil {
		return PoolInput{}, err
	}
	owner, ownerError := httpkit.ParseOptionalUUID(in.OwnerUserID)
	if ownerError != nil {
		return PoolInput{}, ownerError
	}
	accountEnd := 1 + len(in.AccountIDs)
	keyEnd := accountEnd + len(in.APIKeyIDs)
	canonicalWeights := parsed[keyEnd:]
	if len(slices.Compact(slices.Sorted(slices.Values(canonicalWeights)))) != len(weightKeys) {
		return PoolInput{}, httpkit.Invalid("account_weights", "Assign each account weight once.")
	}
	weights := maps.Collect(func(yield func(string, int64) bool) {
		for index, key := range weightKeys {
			if !yield(canonicalWeights[index], in.AccountWeights[key]) {
				return
			}
		}
	})
	canonical := PoolInput{ProjectID: parsed[0], Name: in.Name, Kind: in.Kind, OwnerUserID: owner, AccountIDs: parsed[1:accountEnd], APIKeyIDs: parsed[accountEnd:keyEnd], AccountWeights: weights}
	return canonical, canonical.validate()
}

func (in PoolInput) Validate() error { _, err := in.Validated(); return err }
