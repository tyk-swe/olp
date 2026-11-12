package providers

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/grants"
	"github.com/tyk-swe/olp/internal/providers/connection"
)

// StoreDraftConfiguration owns the provider configuration/evidence transition
// after a caller has acquired the installation lock and current authorization.
// Both ordinary edits and promotion use the stored full configuration as the
// evidence boundary; a portable document cannot stand in for that identity.
func (s *Server) StoreDraftConfiguration(ctx context.Context, tx pgx.Tx, id, name string, prepared connection.Prepared) error {
	next := prepared.Document()
	var encoded []byte
	var previousName string
	if err := tx.QueryRow(ctx, "SELECT name,configuration FROM olp.providers WHERE id=$1 FOR UPDATE", id).Scan(&previousName, &encoded); err != nil {
		return err
	}
	previous, err := connection.Decode(encoded)
	if err != nil {
		return err
	}
	if err := preserveCredentialBoundary(ctx, tx, id, previous, next); err != nil {
		return err
	}
	if err := validateNetworkReference(ctx, tx, id, next.Options.Network); err != nil {
		return err
	}
	change, err := connection.Diff(previous, next)
	if err != nil {
		return err
	}
	if previousName == name && !change.Changed {
		return nil
	}
	if change.Enrollment {
		if err := grants.CancelProviderPending(ctx, tx, id); err != nil {
			return err
		}
	}
	if change.Transport {
		if err := invalidateEvidence(ctx, tx, id); err != nil {
			return err
		}
	}
	after, err := json.Marshal(prepared)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, "UPDATE olp.providers SET name=$2,kind=$3,configuration=$4,etag=$5,draft_dirty=true,updated_at=now() WHERE id=$1", id, name, next.Kind, after, access.NewID())
	return err
}

func preserveCredentialBoundary(ctx context.Context, q access.Queryer, providerID string, current, next connection.Configuration) error {
	move := connection.MoveCredentials(current, next)
	if !move.Destination {
		return nil
	}
	grantMigration := move.GrantIdentity
	if grantMigration {
		// The revision is the destination: a grant build binds its upstream
		// and authority origins into the manifest, so a migration may only
		// land on a build declaring the same origins. A missing build or
		// different origins is a destination change, not a migration.
		if err := q.QueryRow(ctx, `SELECT COALESCE((SELECT a.manifest::jsonb->'origins' FROM olp.plugins a WHERE a.digest=$1)
			= (SELECT b.manifest::jsonb->'origins' FROM olp.plugins b WHERE b.digest=$2), false)`,
			current.ProfileRevision, next.ProfileRevision).Scan(&grantMigration); err != nil {
			return err
		}
	}
	var stored bool
	if err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.provider_credentials
		WHERE provider_id=$1 AND revoked_at IS NULL AND (NOT $2 OR plugin_digest IS NULL))
		OR ($3 AND EXISTS(SELECT 1 FROM olp.provider_network_credentials WHERE provider_id=$1 AND revoked_at IS NULL))`,
		providerID, grantMigration, move.Network).Scan(&stored); err != nil {
		return err
	}
	if stored {
		return access.Fail(422, "credential_destination_changed", "This provider owns unrevoked credential versions bound to its authentication, endpoint, network and profile. Create a new provider or revoke every old credential before supplying credentials for a different destination.")
	}
	return nil
}

type Draft struct {
	Name          string
	ProjectID     *string
	Configuration connection.Prepared
	CreatedBy     string
}

type CreatedDraft struct {
	ID   string
	ETag string
}

func (s *Server) CreateDraft(ctx context.Context, tx pgx.Tx, draft Draft) (CreatedDraft, error) {
	created := CreatedDraft{ID: access.NewID(), ETag: access.NewID()}
	configuration := draft.Configuration.Document()
	if err := validateNetworkReference(ctx, tx, created.ID, configuration.Options.Network); err != nil {
		return CreatedDraft{}, err
	}
	encoded, err := json.Marshal(draft.Configuration)
	if err != nil {
		return CreatedDraft{}, err
	}
	if _, err := tx.Exec(ctx, "INSERT INTO olp.providers(id,name,kind,state,configuration,etag,slots_etag,created_by,project_id) VALUES($1,$2,$3,'draft',$4,$5,$6,$7,$8)", created.ID, draft.Name, configuration.Kind, encoded, created.ETag, access.NewID(), draft.CreatedBy, draft.ProjectID); err != nil {
		return CreatedDraft{}, err
	}
	return created, nil
}
