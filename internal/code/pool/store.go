package pool

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/code"
)

const selectRows = `SELECT x.id::text, x.project_id::text, x.name, x.kind, x.owner_user_id::text,
	ARRAY(SELECT account_id::text FROM olp.code_pool_accounts WHERE pool_id=x.id ORDER BY account_id),
	coalesce((SELECT jsonb_object_agg(account_id::text,weight) FROM olp.code_pool_accounts WHERE pool_id=x.id),'{}'::jsonb),
	ARRAY(SELECT api_key_id::text FROM olp.code_pool_keys WHERE pool_id=x.id ORDER BY api_key_id),
	x.etag::text
	FROM olp.code_pools x`

func scanPool(row pgx.Row) (code.Pool, error) {
	var p code.Pool
	err := row.Scan(&p.ID, &p.ProjectID, &p.Name, &p.Kind, &p.OwnerUserID, &p.AccountIDs, &p.AccountWeights, &p.APIKeyIDs, &p.ETag)
	if p.AccountIDs == nil {
		p.AccountIDs = []string{}
	}
	if p.APIKeyIDs == nil {
		p.APIKeyIDs = []string{}
	}
	if p.AccountWeights == nil {
		p.AccountWeights = map[string]int64{}
	}
	return p, err
}

func Load(ctx context.Context, q access.Queryer, id string) (code.Pool, error) {
	return scanPool(q.QueryRow(ctx, selectRows+` WHERE x.id=$1`, id))
}

func Save(ctx context.Context, tx pgx.Tx, p access.Principal, id, etag string, d Draft, existing bool) (code.Pool, error) {
	project := d.projectID
	if err := p.Project(&project, access.Change); err != nil {
		return code.Pool{}, err
	}
	owner := d.ownership.Owner()
	if owner != nil {
		var member bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.effective_project_members WHERE project_id=$1 AND user_id=$2)`, d.projectID, *owner).Scan(&member); err != nil {
			return code.Pool{}, err
		}
		if !member {
			return code.Pool{}, access.Invalid("owner_user_id", "The owner must be a project member.")
		}
	}
	for _, accountID := range d.accountIDs {
		var matches bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.code_accounts WHERE id=$1 AND project_id=$2)`, accountID, d.projectID).Scan(&matches); err != nil {
			return code.Pool{}, err
		}
		if !matches {
			return code.Pool{}, access.NotFound
		}
	}
	for _, keyID := range d.apiKeyIDs {
		var keyOwner string
		var matches bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.api_keys WHERE id=$1 AND project_id=$2), coalesce((SELECT created_by::text FROM olp.api_keys WHERE id=$1),'')`, keyID, d.projectID).Scan(&matches, &keyOwner); err != nil {
			return code.Pool{}, err
		}
		if !matches {
			return code.Pool{}, access.NotFound
		}
		if owner != nil && keyOwner != *owner {
			return code.Pool{}, access.Invalid("api_key_ids", "Personal pools accept only their owner's keys.")
		}
	}
	if existing {
		var kind code.PoolKind
		var existingOwner *string
		if err := tx.QueryRow(ctx, `SELECT kind,owner_user_id::text FROM olp.code_pools WHERE id=$1`, id).Scan(&kind, &existingOwner); err != nil {
			return code.Pool{}, err
		}
		if kind != d.ownership.Kind() || (existingOwner == nil) != (owner == nil) || existingOwner != nil && *existingOwner != *owner {
			return code.Pool{}, access.Invalid("kind", "Pool ownership is immutable.")
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO olp.code_pools(id,project_id,name,kind,owner_user_id,etag,created_by) VALUES($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT(id) DO UPDATE SET name=excluded.name,etag=excluded.etag`,
		id, d.projectID, d.name, d.ownership.Kind(), owner, etag, p.UserID()); err != nil {
		return code.Pool{}, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM olp.code_pool_accounts WHERE pool_id=$1`, id); err != nil {
		return code.Pool{}, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM olp.code_pool_keys WHERE pool_id=$1`, id); err != nil {
		return code.Pool{}, err
	}
	for _, accountID := range d.accountIDs {
		if _, err := tx.Exec(ctx, `INSERT INTO olp.code_pool_accounts(pool_id,account_id,weight) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, id, accountID, max(1, d.accountWeights[accountID])); err != nil {
			return code.Pool{}, err
		}
	}
	for _, keyID := range d.apiKeyIDs {
		if _, err := tx.Exec(ctx, `INSERT INTO olp.code_pool_keys(pool_id,api_key_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, id, keyID); err != nil {
			return code.Pool{}, err
		}
	}
	return Load(ctx, tx, id)
}

type Stored struct {
	ID             string
	Project        string
	Name           string
	OwnerUserID    *string
	AccountWeights map[string]int64
	APIKeyIDs      []string
}

type Match struct {
	ProjectID   string
	Name        string
	Kind        code.PoolKind
	OwnerUserID *string
}

func ReadStored(ctx context.Context, q access.Queryer) ([]Stored, error) {
	rows, err := q.Query(ctx, `SELECT x.id::text,p.name,x.name,x.owner_user_id::text,coalesce((SELECT jsonb_object_agg(account_id::text,weight) FROM olp.code_pool_accounts WHERE pool_id=x.id),'{}'),ARRAY(SELECT api_key_id::text FROM olp.code_pool_keys WHERE pool_id=x.id ORDER BY api_key_id) FROM olp.code_pools x JOIN olp.projects p ON p.id=x.project_id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Stored])
}

func Candidates(ctx context.Context, q access.Queryer, ref, derived string, match Match) ([]code.CandidateMatch, error) {
	rows, err := q.Query(ctx, `SELECT id::text,project_id::text,(kind=$3 AND owner_user_id IS NOT DISTINCT FROM $4::uuid),(name=$2 AND kind=$3 AND owner_user_id IS NOT DISTINCT FROM $4::uuid) FROM olp.code_pools WHERE id=$5 OR id=$6 OR (project_id=$1 AND name=$2 AND kind=$3 AND owner_user_id IS NOT DISTINCT FROM $4::uuid)`, match.ProjectID, match.Name, match.Kind, match.OwnerUserID, ref, derived)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[code.CandidateMatch])
}

func ReadInput(ctx context.Context, q access.Queryer, id string) (Input, error) {
	var input Input
	err := q.QueryRow(ctx, `SELECT jsonb_build_object('project_id',project_id,'name',name,'kind',kind,'owner_user_id',owner_user_id,'account_ids',ARRAY(SELECT account_id FROM olp.code_pool_accounts WHERE pool_id=x.id ORDER BY account_id),'account_weights',coalesce((SELECT jsonb_object_agg(account_id::text,weight) FROM olp.code_pool_accounts WHERE pool_id=x.id),'{}'),'api_key_ids',ARRAY(SELECT api_key_id FROM olp.code_pool_keys WHERE pool_id=x.id ORDER BY api_key_id)) FROM olp.code_pools x WHERE id=$1`, id).Scan(&input)
	return input, err
}
