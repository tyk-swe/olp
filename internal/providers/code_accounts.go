package providers

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
)

type CodeAccount struct {
	ID                string
	Project           string
	Provider          string
	Name              string
	Enabled           bool
	Models            []string
	ExtraUsageEnabled *bool
	CycleAnchor       *string
}

type GrantedCredential struct {
	ProviderID string
	Principal  string
}

func ReadCodeAccounts(ctx context.Context, q access.Queryer) ([]CodeAccount, error) {
	rows, err := q.Query(ctx, `SELECT a.id::text,p.name,v.name,a.name,a.enabled,a.models,a.extra_usage_enabled,a.cycle_anchor::text FROM olp.code_accounts a JOIN olp.projects p ON p.id=a.project_id JOIN olp.providers v ON v.id=a.provider_id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[CodeAccount])
}

func CodeAccountIDs(ctx context.Context, q access.Queryer, projectID, providerID, principal string) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT id::text FROM olp.code_accounts WHERE project_id=$1 AND provider_id=$2 AND principal=$3`, projectID, providerID, principal)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func ReadCodeAccountInput(ctx context.Context, q access.Queryer, id string) (CodeAccountInput, error) {
	var input CodeAccountInput
	err := q.QueryRow(ctx, `SELECT jsonb_build_object('project_id',project_id,'provider_id',provider_id,'credential_id',credential_id,'name',name,'enabled',enabled,'models',models,'extra_usage_enabled',extra_usage_enabled,'cycle_anchor',cycle_anchor) FROM olp.code_accounts WHERE id=$1`, id).Scan(&input)
	return input, err
}

func ReadGrantedCredential(ctx context.Context, q access.Queryer, providerName, projectID, credentialID string) (*GrantedCredential, error) {
	var granted GrantedCredential
	err := q.QueryRow(ctx, `SELECT p.id::text,c.principal FROM olp.providers p JOIN olp.provider_credentials c ON c.provider_id=p.id JOIN olp.provider_grants g ON g.credential_id=c.id WHERE lower(p.name)=lower($1) AND p.project_id=$2 AND c.id=$3 AND c.principal<>'' AND c.revoked_at IS NULL AND g.lapsed_at IS NULL AND (g.expires_at IS NULL OR g.expires_at>now())`, providerName, projectID, credentialID).Scan(&granted.ProviderID, &granted.Principal)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &granted, nil
}
