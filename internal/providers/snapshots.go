package providers

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
)

type ProviderSnapshot struct {
	ID                    string
	Name                  string
	Project               *string
	Kind                  string
	Configuration         []byte
	Network               *string
	Imported              bool
	RevisionConfiguration []byte
	RevisionModels        []byte
	RevisionSlots         []byte
}

type ModelSnapshot struct {
	ProviderID    string
	UpstreamModel string
	DisplayName   string
	Enabled       bool
	Capabilities  []byte
}

type SlotSnapshot struct {
	ProviderID   string
	Name         string
	ID           string
	Default      bool
	Position     int
	Enabled      bool
	Priority     int
	Weight       int64
	CredentialID *string
	Restrictions []byte
	Limits       []byte
	Usable       *string
	PluginDigest string
	ProfileID    string
}

func ReadProviderSnapshots(ctx context.Context, q access.Queryer) ([]ProviderSnapshot, error) {
	rows, err := q.Query(ctx, `SELECT p.id::text,p.name,pr.name,p.kind,p.configuration,
	(SELECT c.id::text FROM olp.provider_network_credentials c WHERE c.id::text=p.configuration#>>'{options,network,credential_id}' AND c.provider_id=p.id AND c.revoked_at IS NULL),
	EXISTS (SELECT 1 FROM olp.provider_billing_imports bi WHERE bi.provider_id=p.id),
	r.configuration,r.models,r.slots
	FROM olp.providers p LEFT JOIN olp.projects pr ON pr.id=p.project_id
	LEFT JOIN olp.provider_revisions r ON r.id=p.active_revision_id AND p.state IN ('active','disabled')`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[ProviderSnapshot])
}

func ReadModelSnapshots(ctx context.Context, q access.Queryer) ([]ModelSnapshot, error) {
	rows, err := q.Query(ctx, `SELECT provider_id::text,upstream_model,display_name,enabled,capabilities FROM olp.provider_models ORDER BY provider_id,upstream_model`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[ModelSnapshot])
}

func ReadSlotSnapshots(ctx context.Context, q access.Queryer) ([]SlotSnapshot, error) {
	rows, err := q.Query(ctx, `SELECT s.provider_id::text,s.name,s.id::text,s.is_default,s.position,s.enabled,s.priority,s.weight,s.credential_id::text,s.restrictions,s.limits,
	c.id::text,coalesce(c.plugin_digest,''),coalesce(c.profile_id,'')
	FROM olp.provider_slots s LEFT JOIN olp.provider_credentials c ON c.id=s.credential_id AND c.revoked_at IS NULL
	AND NOT EXISTS (SELECT 1 FROM olp.provider_grants g WHERE g.credential_id=c.id AND g.lapsed_at IS NOT NULL)
	ORDER BY s.provider_id,s.position`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[SlotSnapshot])
}
