package providers

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
)

// BindDraftCredential stages an already stored destination credential. A new
// identity clears slot evidence and advances both mutation preconditions; a
// repeated binding is a no-op. The caller owns authorization and transaction.
func BindDraftCredential(ctx context.Context, tx pgx.Tx, providerID, slotID, credentialID string) error {
	tag, err := tx.Exec(ctx, `UPDATE olp.provider_slots SET credential_id=$3,validated_at=NULL,validated_fingerprint=NULL
  WHERE provider_id=$1 AND id=$2 AND credential_id IS DISTINCT FROM $3::uuid`, providerID, slotID, credentialID)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	_, err = tx.Exec(ctx, "UPDATE olp.providers SET etag=$2,slots_etag=$3,draft_dirty=true,updated_at=now() WHERE id=$1", providerID, access.NewID(), access.NewID())
	return err
}
