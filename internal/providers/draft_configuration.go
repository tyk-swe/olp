package providers

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/grants"
)

// StoreDraftConfiguration owns the provider configuration/evidence transition
// after a caller has acquired the installation lock and current authorization.
// next already has pinned plugin/profile and destination-owned secret references.
// Both ordinary edits and promotion use the stored full configuration as the
// evidence boundary; a portable document cannot stand in for that identity.
func StoreDraftConfiguration(ctx context.Context, tx pgx.Tx, id, name string, next *Configuration, policy *egress.Policy) error {
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
	next.Normalize()
	if err := next.Validate(policy); err != nil {
		return err
	}
	if err := PreserveCredentialBoundary(ctx, tx, id, &previous, next); err != nil {
		return err
	}
	if err := validateNetworkReference(ctx, tx, id, next); err != nil {
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
