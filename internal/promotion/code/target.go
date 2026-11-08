package code

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/promotion/provider"
	"github.com/tyk-swe/olp/internal/routing"
)

type PaidTarget struct {
	Provider      string
	Slot          string
	CredentialRef string
}

type slotName struct {
	ProviderID string
	Provider   string
	SlotID     string
	Slot       string
}

func targetOf(e contract.ConfigurationCodePaidTarget) PaidTarget {
	return PaidTarget{Provider: e.Provider, Slot: e.Slot, CredentialRef: e.CredentialRef}
}

func (t PaidTarget) render() contract.ConfigurationCodePaidTarget {
	return contract.ConfigurationCodePaidTarget{Provider: t.Provider, Slot: t.Slot, CredentialRef: t.CredentialRef}
}

func resolveTargets(ctx context.Context, q access.Queryer, targets map[string]PaidTarget, project string, credentials map[string]string) (map[string]codemode.PaidTarget, error) {
	models := sortedKeys(targets)
	destinations, err := promotion.TryMapIndex(models, func(_ int, model string) (codemode.PaidTarget, error) {
		return resolveTarget(ctx, q, targets[model], project, credentials)
	})
	if err != nil {
		return nil, err
	}
	return keyed(models, destinations), nil
}

func resolveTarget(ctx context.Context, q access.Queryer, t PaidTarget, project string, credentials map[string]string) (codemode.PaidTarget, error) {
	credential, err := bound(t.CredentialRef, credentials)
	if err != nil {
		return codemode.PaidTarget{}, err
	}
	var target codemode.PaidTarget
	if err := q.QueryRow(ctx, `SELECT p.id::text,s.id::text FROM olp.providers p JOIN olp.provider_revisions v ON v.id=p.active_revision_id JOIN olp.provider_slots s ON s.provider_id=p.id JOIN olp.provider_credentials c ON c.id=s.credential_id WHERE lower(p.name)=lower($1) AND p.project_id=$2 AND s.name=$3 AND s.credential_id=$4 AND c.revoked_at IS NULL AND s.enabled AND p.state='active' AND EXISTS(SELECT 1 FROM jsonb_array_elements(v.slots) x WHERE x->>'id'=s.id::text AND x->>'credential_id'=c.id::text AND (x->>'enabled')::boolean)`, t.Provider, project, t.Slot, credential).Scan(&target.ProviderID, &target.SlotID); errors.Is(err, pgx.ErrNoRows) {
		return codemode.PaidTarget{}, access.Invalid("code.paid_targets", "Bind the current credential of an enabled published destination provider slot.")
	} else if err != nil {
		return codemode.PaidTarget{}, err
	}
	return target, nil
}

func validLimits(ctx context.Context, q access.Queryer, frozen routing.Provider, slot routing.Slot, project string) error {
	var configurationJSON, slotJSON []byte
	if err := q.QueryRow(ctx, `SELECT p.configuration,s.limits FROM olp.providers p JOIN olp.provider_slots s ON s.provider_id=p.id WHERE p.id=$1 AND s.id=$2 AND p.project_id=$3`, frozen.ID, slot.ID, project).Scan(&configurationJSON, &slotJSON); err != nil {
		return err
	}
	var configuration routing.Configuration
	if err := json.Unmarshal(configurationJSON, &configuration); err != nil {
		return err
	}
	var live routing.Slot
	if err := json.Unmarshal(slotJSON, &live); err != nil {
		return err
	}
	if !routing.CodePaidLimitsMatch(configuration.Options.Limits, frozen.Limits, live, slot) {
		return access.Invalid("code.paid_connections.limits", "Frozen paid provider and slot limits must match the destination's live serving policy.")
	}
	return nil
}

func readSlotNames(ctx context.Context, q access.Queryer) (map[string]PaidTarget, error) {
	rows, err := q.Query(ctx, `SELECT p.id::text,p.name,s.id::text,s.name FROM olp.providers p JOIN olp.provider_slots s ON s.provider_id=p.id`)
	if err != nil {
		return nil, err
	}
	names, err := pgx.CollectRows(rows, pgx.RowToStructByPos[slotName])
	if err != nil {
		return nil, err
	}
	keys := promotion.Map(names, func(n slotName) string { return n.ProviderID + "/" + n.SlotID })
	return keyed(keys, promotion.Map(names, func(n slotName) PaidTarget {
		return PaidTarget{Provider: n.Provider, Slot: n.Slot, CredentialRef: string(provider.SlotRef(n.Provider, n.Slot))}
	})), nil
}

func portableTargets(targets map[string]codemode.PaidTarget, names map[string]PaidTarget) (map[string]PaidTarget, error) {
	models := sortedKeys(targets)
	portable, err := promotion.TryMapIndex(models, func(_ int, model string) (PaidTarget, error) {
		target, ok := names[targets[model].ProviderID+"/"+targets[model].SlotID]
		if !ok {
			return PaidTarget{}, access.Invalid("code.paid_targets", "A paid target no longer has a portable provider and slot reference.")
		}
		return target, nil
	})
	if err != nil {
		return nil, err
	}
	return keyed(models, portable), nil
}
