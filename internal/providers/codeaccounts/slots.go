package codeaccounts

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/routing"
)

type SlotName struct {
	ProviderID string
	Provider   string
	SlotID     string
	Slot       string
}

type SlotLimits struct {
	Configuration routing.Configuration
	Slot          routing.Slot
}

func ReadSlotNames(ctx context.Context, q access.Queryer) ([]SlotName, error) {
	rows, err := q.Query(ctx, `SELECT p.id::text,p.name,s.id::text,s.name FROM olp.providers p JOIN olp.provider_slots s ON s.provider_id=p.id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[SlotName])
}

func ReadPublishedSlot(ctx context.Context, q access.Queryer, providerName, projectID, slot, credentialID string) (codemode.PaidTarget, bool, error) {
	var target codemode.PaidTarget
	err := q.QueryRow(ctx, `SELECT p.id::text,s.id::text FROM olp.providers p JOIN olp.provider_revisions v ON v.id=p.active_revision_id JOIN olp.provider_slots s ON s.provider_id=p.id JOIN olp.provider_credentials c ON c.id=s.credential_id WHERE lower(p.name)=lower($1) AND p.project_id=$2 AND s.name=$3 AND s.credential_id=$4 AND c.revoked_at IS NULL AND s.enabled AND p.state='active' AND EXISTS(SELECT 1 FROM jsonb_array_elements(v.slots) x WHERE x->>'id'=s.id::text AND x->>'credential_id'=c.id::text AND (x->>'enabled')::boolean)`, providerName, projectID, slot, credentialID).Scan(&target.ProviderID, &target.SlotID)
	if errors.Is(err, pgx.ErrNoRows) {
		return codemode.PaidTarget{}, false, nil
	}
	return target, err == nil, err
}

func ReadSlotLimits(ctx context.Context, q access.Queryer, providerID, slotID, projectID string) (SlotLimits, error) {
	var configurationJSON, slotJSON []byte
	if err := q.QueryRow(ctx, `SELECT p.configuration,s.limits FROM olp.providers p JOIN olp.provider_slots s ON s.provider_id=p.id WHERE p.id=$1 AND s.id=$2 AND p.project_id=$3`, providerID, slotID, projectID).Scan(&configurationJSON, &slotJSON); err != nil {
		return SlotLimits{}, err
	}
	var configuration routing.Configuration
	if err := json.Unmarshal(configurationJSON, &configuration); err != nil {
		return SlotLimits{}, err
	}
	var slot routing.Slot
	if err := json.Unmarshal(slotJSON, &slot); err != nil {
		return SlotLimits{}, err
	}
	return SlotLimits{Configuration: configuration, Slot: slot}, nil
}

func SlotCredentialCurrent(ctx context.Context, q access.Queryer, slotID, providerID, credentialID string, requireEnabled bool) (bool, error) {
	var current bool
	err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.provider_slots s JOIN olp.provider_credentials c ON c.id=s.credential_id WHERE s.id=$1 AND s.provider_id=$2 AND c.id=$3 AND c.revoked_at IS NULL AND ($4=false OR s.enabled))`, slotID, providerID, credentialID, requireEnabled).Scan(&current)
	return current, err
}

func CredentialEnrolledFor(ctx context.Context, q access.Queryer, credentialID, pluginDigest, profileID string) (bool, error) {
	var enrolled bool
	err := q.QueryRow(ctx, `SELECT coalesce(plugin_digest=$2 AND profile_id=$3,false) FROM olp.provider_credentials WHERE id=$1`, credentialID, pluginDigest, profileID).Scan(&enrolled)
	return enrolled, err
}
