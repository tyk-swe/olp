package resources

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/geminilifecycle"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/usage"
)

const InteractionContract = "gemini-interaction/v1beta"

type interactionPayload struct {
	UpstreamID string `json:"upstream_id"`
}

// PutInteractionContract commits the provider's opaque ID under the existing
// secret authority before a local ID can be observed. Ordinary metadata
// contains only model/serving information needed by the resolver.
func (s *Store) PutInteractionContract(ctx context.Context, r *Resource, upstreamID string) (*Resource, error) {
	if !s.Encrypted() || r == nil || r.TargetID == "" || r.Kind != KindInteraction || r.ContractVersion == nil || *r.ContractVersion != InteractionContract || r.SubmissionID != nil || !interactionState(r.State) || len(upstreamID) == 0 || len(upstreamID) > 512 || r.ExpiresAt == nil || !r.ExpiresAt.After(time.Now()) || r.ExpiresAt.After(time.Now().Add(ContinuationLifetime)) || len(r.Metadata) > metadataLimit {
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
		if err := tx.QueryRow(ctx, `SELECT true FROM olp.provider_resources WHERE id=$1 AND api_key_id=$2 AND kind=$3 AND state<>$4 AND expires_at>now() FOR SHARE`, *r.ParentID, r.APIKeyID, KindInteraction, StateDeleted).Scan(&found); err != nil || !found {
			return nil, ErrNotFound
		}
	}
	copy := *r
	copy.UUID = uuid.Must(uuid.NewV7())
	copy.UpstreamID = copy.UUID.String()
	if len(copy.Metadata) == 0 {
		copy.Metadata = []byte(`{}`)
	}
	out, err := scan(tx.QueryRow(ctx, `INSERT INTO olp.provider_resources
 (id,kind,api_key_id,route_slug,provider_id,provider_revision_id,route_revision_id,slot_id,credential_id,upstream_id,state,metadata,expires_at,contract_version,parent_id,target_id)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16) RETURNING `+columns,
		copy.UUID, copy.Kind, copy.APIKeyID, copy.RouteSlug, copy.ProviderID, copy.ProviderRevisionID, copy.RouteRevisionID, copy.SlotID, copy.CredentialID, copy.UpstreamID, copy.State, copy.Metadata, copy.ExpiresAt, copy.ContractVersion, copy.ParentID, copy.TargetID))
	if err != nil {
		return nil, err
	}
	if err = s.keys.Store(ctx, tx, s.installation, out.UUID.String(), secrets.ProviderContinuation, payload, out.ExpiresAt); err != nil {
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
	r, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM olp.provider_resources
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
	payload, err := s.keys.Read(ctx, tx, s.installation, id.String(), secrets.ProviderContinuation)
	if err != nil || len(payload) == 0 || len(payload) > 1024 {
		return nil, "", ErrContract
	}
	var native interactionPayload
	if json.Unmarshal(payload, &native) != nil || native.UpstreamID == "" || len(native.UpstreamID) > 512 {
		return nil, "", ErrContract
	}
	return r, native.UpstreamID, nil
}

// ObserveInteraction commits native state and any terminal accounting together.
// A resumed stream may repeat its earlier created/status events; those cannot
// move an already terminal retained interaction back into pending work.
func (s *Store) ObserveInteraction(ctx context.Context, owner, localID, status string, evidence usage.AttemptUsage, now time.Time) (usage.Persisted, error) {
	if !interactionState(status) || status == StateDeleted {
		return usage.Persisted{}, ErrContract
	}
	id, err := parseLocal(localID)
	if err != nil || LocalID(KindInteraction, id) != localID {
		return usage.Persisted{}, ErrNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return usage.Persisted{}, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	var current string
	var pending []byte
	err = tx.QueryRow(ctx, `SELECT state,metadata->'pending_usage' FROM olp.provider_resources
 WHERE id=$1 AND kind=$2 AND api_key_id=$3 AND state<>$4 AND expires_at>now() FOR UPDATE`, id, KindInteraction, owner, StateDeleted).Scan(&current, &pending)
	if errors.Is(err, pgx.ErrNoRows) {
		return usage.Persisted{}, ErrNotFound
	}
	if err != nil {
		return usage.Persisted{}, err
	}
	terminal := geminilifecycle.InteractionTurnFinished(status)
	result := usage.Persisted{Outcome: usage.PersistOutcomeDuplicate}
	if terminal {
		result, err = persistPendingUsage(ctx, tx, pending, evidence, now, true)
		if err != nil {
			return usage.Persisted{}, err
		}
	} else if geminilifecycle.InteractionTurnFinished(current) {
		status = current
	} else if evidence.Observed && len(pending) != 0 && string(pending) != "null" {
		event, decodeErr := decodePendingUsage(pending)
		if decodeErr != nil {
			return usage.Persisted{}, decodeErr
		}
		if len(event.Attempts) == 0 {
			return usage.Persisted{}, ErrContract
		}
		if err := observePendingUsage(event, evidence, false); err != nil {
			return usage.Persisted{}, err
		}
		encoded, encodeErr := usage.Encode(event)
		if encodeErr != nil {
			return usage.Persisted{}, encodeErr
		}
		if _, err = tx.Exec(ctx, `UPDATE olp.provider_resources SET metadata=jsonb_set(metadata,'{pending_usage}',$2::jsonb) WHERE id=$1`, id, encoded); err != nil {
			return usage.Persisted{}, err
		}
	}
	_, err = tx.Exec(ctx, `UPDATE olp.provider_resources SET state=$2,
 metadata=CASE WHEN $3 THEN metadata-'pending_usage' ELSE metadata END, updated_at=now() WHERE id=$1`, id, status, terminal)
	if err != nil {
		return usage.Persisted{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return usage.Persisted{}, err
	}
	return result, nil
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
	var pending []byte
	if err = tx.QueryRow(ctx, `SELECT metadata->'pending_usage' FROM olp.provider_resources
 WHERE id=$1 AND kind=$2 AND api_key_id=$3 AND state<>$4 FOR UPDATE`, id, KindInteraction, owner, StateDeleted).Scan(&pending); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if _, err = persistPendingUsage(ctx, tx, pending, usage.AttemptUsage{BillingUncertain: true}, time.Now(), false); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE olp.provider_resources SET state=$4,metadata=metadata-'pending_usage',updated_at=now()
 WHERE id=$1 AND kind=$2 AND api_key_id=$3 AND state<>$4`, id, KindInteraction, owner, StateDeleted)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrNotFound
	}
	if _, err = tx.Exec(ctx, `DELETE FROM olp.secrets WHERE id=$1 AND purpose=$2`, id, secrets.ProviderContinuation); err != nil {
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
