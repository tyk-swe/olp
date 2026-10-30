package resources

import (
	"context"
	"encoding/json"
	"github.com/tyk-swe/olp/internal/routing"
	"slices"

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
	return CodeOffers(ctx, tx, route.RevisionID, key)
}

func CodeOffers(ctx context.Context, db access.Queryer, revision string, keys ...string) (codeclient.Offers, error) {
	rows, err := db.Query(ctx, `SELECT DISTINCT a.models,`+codeadapter.SQL("(v.connections->(a.provider_id::text))")+`,CASE WHEN v.document->>'paid_participation'='true' THEN jsonb_build_object('route',v.document,'connections',v.paid_connections) ELSE NULL END
 FROM olp.code_route_revisions v LEFT JOIN LATERAL(
 SELECT a.* FROM olp.code_pool_accounts pa JOIN olp.code_accounts a ON a.id=pa.account_id
 JOIN olp.provider_credentials c ON c.id=a.credential_id JOIN olp.provider_grants g ON g.credential_id=c.id JOIN olp.providers p ON p.id=a.provider_id
 WHERE pa.pool_id=(v.document->>'pool_id')::uuid AND v.connections ? a.provider_id::text
 AND a.project_id=(v.document->>'project_id')::uuid AND a.enabled AND olp.code_account_available(a)
 AND p.state<>'disabled' AND p.project_id=a.project_id
 AND c.plugin_digest=v.connections->(a.provider_id::text)->>'profile_revision' AND c.profile_id=v.connections->(a.provider_id::text)->>'profile_id'
 AND c.provider_id=a.provider_id AND c.principal=a.principal AND c.revoked_at IS NULL AND g.lapsed_at IS NULL AND (g.expires_at IS NULL OR g.expires_at>now())
 )a ON true WHERE v.id=$1`, revision)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var publication []byte
	offers := codeclient.Offers{}
	for rows.Next() {
		var models []string
		var adapter *string
		if err := rows.Scan(&models, &adapter, &publication); err != nil {
			return nil, err
		}
		if adapter != nil {
			offers = append(offers, codeclient.Offer{Adapter: codemode.Adapter(*adapter), Models: models})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	if len(publication) == 0 {
		return offers, nil
	}
	var published struct {
		Route       codemode.Route              `json:"route"`
		Connections map[string]routing.Provider `json:"connections"`
	}
	if err := json.Unmarshal(publication, &published); err != nil {
		return nil, err
	}
	route, paid := published.Route, published.Connections
	key := ""
	if len(keys) > 0 {
		key = keys[0]
	}
	for model, target := range route.AllPaidTargets() {
		provider, ok := paid[target.ProviderID]
		slot := slices.IndexFunc(provider.Slots, func(slot routing.Slot) bool { return slot.ID == target.SlotID })
		if !ok || slot < 0 || provider.Slots[slot].CredentialID == nil || provider.Slots[slot].CredentialVersion == nil {
			continue
		}
		selected := provider.Slots[slot]
		if key != "" && !selected.Allows(model, route.Slug, key) {
			continue
		}
		if err := paidCodeTarget(ctx, db, PaidCodeAdmission{CodeAdmission: CodeAdmission{Route: route, APIKeyID: key, Operation: codemode.Operation{Model: model}}, Provider: provider, Slot: selected}, false); err != nil {
			continue
		}
		protocol, ok := routing.CodePaidProtocol(provider, model)
		if ok {
			offers = append(offers, codeclient.Offer{PaidProtocol: protocol, Models: []string{model}})
		}
	}
	return offers, nil
}
