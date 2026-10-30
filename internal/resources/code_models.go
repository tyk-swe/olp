package resources

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codeclient"
	"github.com/tyk-swe/olp/internal/codemode"
)

func (s *CodeStore) Models(ctx context.Context, route codemode.Route, key string) (codeclient.Offers, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var authority access.Authority
	if err := codeAuthorityFor(ctx, tx, CodeAdmission{Route: route, APIKeyID: key}, &authority, codeModels); err != nil {
		return nil, err
	}
	return CodeOffers(ctx, tx, route.RevisionID)
}

func CodeOffers(ctx context.Context, db access.Queryer, revision string) (codeclient.Offers, error) {
	rows, err := db.Query(ctx, `SELECT DISTINCT a.models,`+codeadapter.SQL("(v.connections->(a.provider_id::text))")+`
		FROM olp.code_route_revisions v JOIN olp.code_pool_accounts pa ON pa.pool_id=(v.document->>'pool_id')::uuid
		JOIN olp.code_accounts a ON a.id=pa.account_id
		JOIN olp.provider_credentials c ON c.id=a.credential_id JOIN olp.provider_grants g ON g.credential_id=c.id JOIN olp.providers p ON p.id=a.provider_id
		WHERE v.id=$1 AND v.connections ? a.provider_id::text
		AND a.project_id=(v.document->>'project_id')::uuid AND a.enabled AND olp.code_account_available(a)
		AND p.state<>'disabled' AND p.project_id=a.project_id
		AND c.plugin_digest=v.connections->(a.provider_id::text)->>'profile_revision'
		AND c.profile_id=v.connections->(a.provider_id::text)->>'profile_id'
		AND c.provider_id=a.provider_id AND c.principal=a.principal AND c.revoked_at IS NULL AND g.lapsed_at IS NULL AND (g.expires_at IS NULL OR g.expires_at>now())`, revision)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	offers := codeclient.Offers{}
	for rows.Next() {
		var models []string
		var adapter *string
		if err := rows.Scan(&models, &adapter); err != nil {
			return nil, err
		}
		if adapter != nil {
			offers = append(offers, codeclient.Offer{Adapter: codemode.Adapter(*adapter), Models: models})
		}
	}
	return offers, rows.Err()
}
