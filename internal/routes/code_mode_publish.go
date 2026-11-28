package routes

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codemode/codeadapter"
	"github.com/tyk-swe/olp/internal/codesessions"
	"github.com/tyk-swe/olp/internal/domain/problem"
	"github.com/tyk-swe/olp/internal/grants"
	"github.com/tyk-swe/olp/internal/providers/connection"
	"github.com/tyk-swe/olp/internal/routing"
)

func validateCodePublicationRoute(ctx context.Context, tx pgx.Tx, route codemode.Route) error {
	if err := route.ValidateAliases(); err != nil {
		return problem.Invalid("aliases", err.Error())
	}
	if err := route.ValidateModelPools(); err != nil {
		return problem.Invalid("model_pools", err.Error())
	}
	if err := route.Tools.Validate(); err != nil {
		return problem.Invalid("tools", err.Error())
	}
	if route.Tools != nil {
		if _, err := codesessions.ResearchAccount(ctx, tx, route); err != nil {
			return err
		}
	}
	if !route.Enabled {
		return nil
	}
	for _, model := range route.Models {
		if _, paid := route.AllPaidTargets()[model]; paid {
			continue
		}
		var available bool
		err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.code_pool_accounts pa JOIN olp.code_accounts a ON a.id=pa.account_id JOIN olp.provider_credentials c ON c.id=a.credential_id JOIN olp.provider_grants g ON g.credential_id=c.id
			WHERE pa.pool_id=$1 AND a.enabled AND a.project_id=$2 AND a.models ? $3 AND c.revoked_at IS NULL AND `+grants.LiveCondition+` AND c.principal=a.principal)`, route.PoolID, route.ProjectID, model).Scan(&available)
		if err != nil {
			return err
		}
		if !available {
			return problem.Invalid("models", "Every model needs an enrolled pool account.")
		}
	}
	return nil
}

func resolvePublishedPaidConnections(ctx context.Context, tx pgx.Tx, route codemode.Route, frozenPaid map[string]routing.Provider) ([]byte, error) {
	paidProviders, err := CodePaidProviders(ctx, tx, route)
	if err != nil {
		return nil, err
	}
	if frozenPaid != nil {
		if len(frozenPaid) != len(paidProviders) {
			return nil, problem.Invalid("paid_connections", "Frozen metered providers must match the published paid targets.")
		}
		for id := range frozenPaid {
			if _, ok := paidProviders[id]; !ok {
				return nil, problem.Invalid("paid_connections", "Frozen metered providers must match the published paid targets.")
			}
		}
		if err := ValidateCodePaidProviders(route, frozenPaid); err != nil {
			return nil, err
		}
		return json.Marshal(frozenPaid)
	}
	return json.Marshal(paidProviders)
}

func resolvePublishedPoolConnections(ctx context.Context, tx pgx.Tx, poolID string, frozen map[string]connection.Configuration) ([]byte, []codemode.Adapter, error) {
	connections, adapters, err := codePublication(ctx, tx, poolID)
	if err != nil {
		return nil, nil, err
	}
	if frozen == nil {
		return connections, adapters, nil
	}
	var current map[string]json.RawMessage
	if err := json.Unmarshal(connections, &current); err != nil {
		return nil, nil, err
	}
	if len(current) != len(frozen) {
		return nil, nil, problem.Invalid("connections", "Frozen providers must match the publication pool.")
	}
	for provider, cfg := range frozen {
		if _, ok := current[provider]; !ok {
			return nil, nil, problem.Invalid("connections", "Frozen providers must match the publication pool.")
		}
		if _, ok := codeadapter.ForConnection(cfg.Kind, cfg.AuthMode, cfg.ProfileID); !ok {
			return nil, nil, problem.Invalid("connections", "Frozen connections must support a coding adapter.")
		}
	}
	encoded, err := json.Marshal(frozen)
	if err != nil {
		return nil, nil, err
	}
	return encoded, adapters, nil
}
