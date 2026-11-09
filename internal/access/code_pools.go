package access

import (
	"context"

	"github.com/jackc/pgx/v5"
)

type CodePool struct {
	ID             string
	Project        string
	Name           string
	OwnerUserID    *string
	AccountWeights map[string]int64
	APIKeyIDs      []string
}

type CodeCandidate struct {
	ID             string
	ProjectID      string
	ScopeMatches   bool
	NaturalMatches bool
}

type CodePoolMatch struct {
	ProjectID   string
	Name        string
	Kind        string
	OwnerUserID *string
}

func ReadCodePools(ctx context.Context, q Queryer) ([]CodePool, error) {
	rows, err := q.Query(ctx, `SELECT x.id::text,p.name,x.name,x.owner_user_id::text,coalesce((SELECT jsonb_object_agg(account_id::text,weight) FROM olp.code_pool_accounts WHERE pool_id=x.id),'{}'),ARRAY(SELECT api_key_id::text FROM olp.code_pool_keys WHERE pool_id=x.id ORDER BY api_key_id) FROM olp.code_pools x JOIN olp.projects p ON p.id=x.project_id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[CodePool])
}

func CodePoolCandidates(ctx context.Context, q Queryer, ref, derived string, match CodePoolMatch) ([]CodeCandidate, error) {
	rows, err := q.Query(ctx, `SELECT id::text,project_id::text,(kind=$3 AND owner_user_id IS NOT DISTINCT FROM $4::uuid),(name=$2 AND kind=$3 AND owner_user_id IS NOT DISTINCT FROM $4::uuid) FROM olp.code_pools WHERE id=$5 OR id=$6 OR (project_id=$1 AND name=$2 AND kind=$3 AND owner_user_id IS NOT DISTINCT FROM $4::uuid)`, match.ProjectID, match.Name, match.Kind, match.OwnerUserID, ref, derived)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[CodeCandidate])
}

func ReadCodePoolInput(ctx context.Context, q Queryer, id string) (CodePoolInput, error) {
	var input CodePoolInput
	err := q.QueryRow(ctx, `SELECT jsonb_build_object('project_id',project_id,'name',name,'kind',kind,'owner_user_id',owner_user_id,'account_ids',ARRAY(SELECT account_id FROM olp.code_pool_accounts WHERE pool_id=x.id ORDER BY account_id),'account_weights',coalesce((SELECT jsonb_object_agg(account_id::text,weight) FROM olp.code_pool_accounts WHERE pool_id=x.id),'{}'),'api_key_ids',ARRAY(SELECT api_key_id FROM olp.code_pool_keys WHERE pool_id=x.id ORDER BY api_key_id)) FROM olp.code_pools x WHERE id=$1`, id).Scan(&input)
	return input, err
}
