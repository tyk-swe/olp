package session

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/code"
	"github.com/tyk-swe/olp/internal/code/client"
	"github.com/tyk-swe/olp/internal/code/vendor"
	"github.com/tyk-swe/olp/internal/routing"
)

func (s *Store) Models(ctx context.Context, route code.Route, key string) (client.Offers, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return client.Offers{}, err
	}
	defer tx.Rollback(ctx)
	var authority keyauthority.Authority
	if _, err := loadAuthority(ctx, tx, Admission{Route: route, APIKeyID: key}, &authority, scopeModels); err != nil {
		return client.Offers{}, err
	}
	return Offers(ctx, tx, route.RevisionID, key)
}

func Offers(ctx context.Context, db access.Queryer, revisionID, apiKeyID string) (client.Offers, error) {
	rows, err := db.Query(ctx, `SELECT (SELECT coalesce(jsonb_agg(m.model ORDER BY m.ordinal),'[]'::jsonb) FROM jsonb_array_elements_text(a.models) WITH ORDINALITY AS m(model,ordinal)
		WHERE NOT EXISTS(SELECT 1 FROM olp.code_account_cooldowns cd WHERE cd.account_id=a.id AND cd.retry_at>now() AND (cd.model IS NULL OR cd.model=m.model))),
		v.connections->(a.provider_id::text),
		CASE WHEN v.document->>'paid_participation'='true' THEN jsonb_build_object('route',v.document,'connections',v.paid_connections) ELSE NULL END
		FROM olp.code_route_revisions v LEFT JOIN LATERAL(
			SELECT a.* FROM olp.code_pool_accounts pa JOIN olp.code_accounts a ON a.id=pa.account_id
			JOIN olp.provider_credentials c ON c.id=a.credential_id JOIN olp.provider_grants g ON g.credential_id=c.id JOIN olp.providers p ON p.id=a.provider_id
			WHERE pa.pool_id=(v.document->>'pool_id')::uuid AND v.connections ? a.provider_id::text
			AND a.project_id=(v.document->>'project_id')::uuid AND a.enabled AND olp.code_account_available(a)
			AND p.state<>'disabled' AND p.project_id=a.project_id
			AND c.plugin_digest=v.connections->(a.provider_id::text)->>'profile_revision' AND c.profile_id=v.connections->(a.provider_id::text)->>'profile_id'
			AND c.provider_id=a.provider_id AND c.principal=a.principal AND c.revoked_at IS NULL AND g.lapsed_at IS NULL AND (g.expires_at IS NULL OR g.expires_at>now())
		) a ON true WHERE v.id=$1`, revisionID)
	if err != nil {
		return client.Offers{}, err
	}
	defer rows.Close()
	var publication []byte
	var out client.Offers
	for rows.Next() {
		var models []string
		var connJSON []byte
		if err := rows.Scan(&models, &connJSON, &publication); err != nil {
			return client.Offers{}, err
		}
		if len(connJSON) == 0 {
			continue
		}
		var cfg vendor.ConnectionIdentity
		if err := json.Unmarshal(connJSON, &cfg); err != nil {
			return client.Offers{}, err
		}
		v, ok := vendor.ForConnection(cfg.Kind, cfg.AuthMode, cfg.ProfileID)
		if !ok {
			continue
		}
		if !slices.ContainsFunc(out.Subscriptions, func(existing client.SubscriptionOffer) bool {
			return existing.Adapter == v.Adapter && slices.Equal(existing.Models, models)
		}) {
			out.Subscriptions = append(out.Subscriptions, client.SubscriptionOffer{Adapter: v.Adapter, Models: models})
		}
	}
	if err := rows.Err(); err != nil {
		return client.Offers{}, err
	}
	rows.Close()
	if len(publication) == 0 {
		return out, nil
	}
	var published struct {
		Route       code.Route                  `json:"route"`
		Connections map[string]routing.Provider `json:"connections"`
	}
	if err := json.Unmarshal(publication, &published); err != nil {
		return client.Offers{}, err
	}
	route, paid := published.Route, published.Connections
	for model, target := range route.AllPaidTargets() {
		provider, ok := paid[target.ProviderID]
		slot := slices.IndexFunc(provider.Slots, func(slot routing.Slot) bool { return slot.ID == target.SlotID })
		if !ok || slot < 0 || provider.Slots[slot].CredentialID == nil || provider.Slots[slot].CredentialVersion == nil {
			continue
		}
		selected := provider.Slots[slot]
		if apiKeyID != "" && !selected.Allows(model, route.Slug, apiKeyID) {
			continue
		}
		if err := checkPaidTarget(ctx, db, PaidAdmission{Admission: Admission{Route: route, APIKeyID: apiKeyID, Operation: code.Operation{Model: model}}, Provider: provider, Slot: selected}, false); err != nil {
			continue
		}
		protocol, streaming, ok := routing.CodePaidProtocol(provider, model)
		if ok {
			out.Paid = append(out.Paid, client.PaidOffer{Model: model, Protocol: protocol, Streaming: streaming})
		}
	}
	return out, nil
}
