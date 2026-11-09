package providers

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/grants"
)

// StoreDraftConfiguration owns the provider configuration/evidence transition
// after a caller has acquired the installation lock and current authorization.
// Both ordinary edits and promotion use the stored full configuration as the
// evidence boundary; a portable document cannot stand in for that identity.
func StoreDraftConfiguration(ctx context.Context, tx pgx.Tx, id, name string, prepared PreparedConfiguration) error {
	next := prepared.configuration
	if next.Endpoint == nil {
		return access.Invalid("configuration", "Prepare a valid provider configuration before saving it.")
	}
	var previous Configuration
	var encoded []byte
	var previousName string
	if err := tx.QueryRow(ctx, "SELECT name,configuration FROM olp.providers WHERE id=$1 FOR UPDATE", id).Scan(&previousName, &encoded); err != nil {
		return err
	}
	if err := json.Unmarshal(encoded, &previous); err != nil {
		return err
	}
	previous.Normalize()
	if err := PreserveCredentialBoundary(ctx, tx, id, &previous, &next); err != nil {
		return err
	}
	if err := validateNetworkReference(ctx, tx, id, &next); err != nil {
		return err
	}
	before, err := json.Marshal(previous)
	if err != nil {
		return err
	}
	after, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if previousName == name && string(before) == string(after) {
		return nil
	}
	if previous.enrollmentBoundary() != next.enrollmentBoundary() {
		if err := grants.CancelProviderPending(ctx, tx, id); err != nil {
			return err
		}
	}
	if next.transportFingerprint() != previous.transportFingerprint() {
		if err := invalidateEvidence(ctx, tx, id); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, "UPDATE olp.providers SET name=$2,kind=$3,configuration=$4,etag=$5,draft_dirty=true,updated_at=now() WHERE id=$1", id, name, next.Kind, after, access.NewID())
	return err
}

type Draft struct {
	Name          string
	ProjectID     *string
	Configuration PreparedConfiguration
	CreatedBy     string
}

type CreatedDraft struct {
	ID   string
	ETag string
}

func CreateDraft(ctx context.Context, tx pgx.Tx, draft Draft) (CreatedDraft, error) {
	created := CreatedDraft{ID: access.NewID(), ETag: access.NewID()}
	configuration := draft.Configuration.configuration
	if err := validateNetworkReference(ctx, tx, created.ID, &configuration); err != nil {
		return CreatedDraft{}, err
	}
	encoded, err := json.Marshal(configuration)
	if err != nil {
		return CreatedDraft{}, err
	}
	if _, err := tx.Exec(ctx, "INSERT INTO olp.providers(id,name,kind,state,configuration,etag,slots_etag,created_by,project_id) VALUES($1,$2,$3,'draft',$4,$5,$6,$7,$8)", created.ID, draft.Name, configuration.Kind, encoded, created.ETag, access.NewID(), draft.CreatedBy, draft.ProjectID); err != nil {
		return CreatedDraft{}, err
	}
	return created, nil
}
