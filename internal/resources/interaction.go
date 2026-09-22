package resources

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const InteractionContract = "gemini-interaction/v1beta"

type interactionPayload struct {
	UpstreamID string `json:"upstream_id"`
}

// PutInteractionContract commits the provider's opaque ID under the existing
// secret authority before a local ID can be observed. Ordinary metadata
// contains only model/serving information needed by the resolver.
func (s *Store) PutInteractionContract(ctx context.Context, r *Resource, upstreamID string) (*Resource, error) {
	if !s.Encrypted() || r == nil || r.Kind != KindInteraction || r.ContractVersion == nil || *r.ContractVersion != InteractionContract || r.SubmissionID != nil || !interactionState(r.State) || len(upstreamID) == 0 || len(upstreamID) > 512 || r.ExpiresAt == nil || !r.ExpiresAt.After(time.Now()) || r.ExpiresAt.After(time.Now().Add(ContinuationLifetime)) || len(r.Metadata) > metadataLimit {
		return nil, ErrContract
	}
	payload, err := json.Marshal(interactionPayload{UpstreamID: upstreamID})
	if err != nil {
		return nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if err := s.authorizeOwner(ctx, tx, r.APIKeyID); err != nil {
		return nil, err
	}
	if r.ParentID != nil {
		var found bool
		if err := tx.QueryRow(ctx, `SELECT true FROM olp_go.provider_resources WHERE id=$1 AND api_key_id=$2 AND kind=$3 AND state<>$4 AND expires_at>now() FOR SHARE`, *r.ParentID, r.APIKeyID, KindInteraction, StateDeleted).Scan(&found); err != nil || !found {
			return nil, ErrNotFound
		}
	}
	copy := *r
	copy.UUID = uuid.Must(uuid.NewV7())
	copy.UpstreamID = copy.UUID.String()
	if len(copy.Metadata) == 0 {
		copy.Metadata = []byte(`{}`)
	}
	out, err := scan(tx.QueryRow(ctx, `INSERT INTO olp_go.provider_resources
 (id,kind,api_key_id,route_slug,provider_id,provider_revision_id,route_revision_id,slot_id,credential_id,upstream_id,state,metadata,expires_at,contract_version,parent_id)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15) RETURNING `+columns,
		copy.UUID, copy.Kind, copy.APIKeyID, copy.RouteSlug, copy.ProviderID, copy.ProviderRevisionID, copy.RouteRevisionID, copy.SlotID, copy.CredentialID, copy.UpstreamID, copy.State, copy.Metadata, copy.ExpiresAt, copy.ContractVersion, copy.ParentID))
	if err != nil {
		return nil, err
	}
	if err = s.keys.Store(ctx, tx, s.installation, out.UUID.String(), continuationPurpose, payload, out.ExpiresAt); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) ReadInteractionContract(ctx context.Context, owner, localID string) (*Resource, string, error) {
	if !s.Encrypted() {
		return nil, "", ErrContract
	}
	id, err := parseLocal(localID)
	if err != nil || LocalID(KindInteraction, id) != localID {
		return nil, "", ErrNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback(ctx)
	if err := s.authorizeOwner(ctx, tx, owner); err != nil {
		return nil, "", err
	}
	r, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM olp_go.provider_resources
 WHERE id=$1 AND kind=$2 AND api_key_id=$3 AND state<>$4 AND expires_at>now() FOR SHARE`, id, KindInteraction, owner, StateDeleted))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", ErrNotFound
	}
	if err != nil {
		return nil, "", err
	}
	if r.ContractVersion == nil || *r.ContractVersion != InteractionContract || r.UpstreamID != id.String() || !interactionState(r.State) {
		return nil, "", ErrContract
	}
	payload, err := s.keys.Read(ctx, tx, s.installation, id.String(), continuationPurpose)
	if err != nil || len(payload) == 0 || len(payload) > 1024 {
		return nil, "", ErrContract
	}
	var native interactionPayload
	if json.Unmarshal(payload, &native) != nil || native.UpstreamID == "" || len(native.UpstreamID) > 512 {
		return nil, "", ErrContract
	}
	return r, native.UpstreamID, nil
}

func (s *Store) MarkInteractionStatus(ctx context.Context, owner, localID, status string) error {
	if !interactionState(status) || status == StateDeleted {
		return ErrContract
	}
	id, err := parseLocal(localID)
	if err != nil || LocalID(KindInteraction, id) != localID {
		return ErrNotFound
	}
	tag, err := s.pool.Exec(ctx, `UPDATE olp_go.provider_resources SET state=$4,updated_at=now()
 WHERE id=$1 AND kind=$2 AND api_key_id=$3 AND state<>$5 AND expires_at>now()`, id, KindInteraction, owner, status, StateDeleted)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrNotFound
	}
	return nil
}

// DeleteInteractionContract removes the encrypted provider ID in the same
// transaction that tombstones the local mapping. The upstream DELETE is a
// separate provider effect and must have succeeded before callers invoke it.
func (s *Store) DeleteInteractionContract(ctx context.Context, owner, localID string) error {
	id, err := parseLocal(localID)
	if err != nil || LocalID(KindInteraction, id) != localID {
		return ErrNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := s.authorizeOwner(ctx, tx, owner); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE olp_go.provider_resources SET state=$4,updated_at=now()
 WHERE id=$1 AND kind=$2 AND api_key_id=$3 AND state<>$4`, id, KindInteraction, owner, StateDeleted)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrNotFound
	}
	if _, err = tx.Exec(ctx, `DELETE FROM olp_go.secrets WHERE id=$1 AND purpose=$2`, id, continuationPurpose); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func interactionState(status string) bool {
	switch status {
	case "created", "pending", "queued", "in_progress", "requires_action", "completed", "failed", "cancelled", "incomplete", StateDeleted:
		return true
	}
	return false
}
