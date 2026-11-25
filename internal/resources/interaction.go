package resources

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/domain/geminilifecycle"
	"github.com/tyk-swe/olp/internal/platform/secrets"
	"github.com/tyk-swe/olp/internal/usage"
	"github.com/tyk-swe/olp/internal/usage/ingest"
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
	out, err := s.insertSealedContract(ctx, tx, &copy, payload)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) ReadInteractionContract(ctx context.Context, owner, localID string) (*Resource, string, error) {
	r, payload, err := s.readSealedContract(ctx, KindInteraction, owner, localID, 1024, func(r *Resource) bool {
		return r.ContractVersion != nil && *r.ContractVersion == InteractionContract &&
			r.UpstreamID == r.UUID.String() && interactionState(r.State)
	})
	if err != nil {
		return nil, "", err
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
func (s *Store) ObserveInteraction(ctx context.Context, owner, localID, status string, evidence usage.AttemptUsage, now time.Time) (ingest.Persisted, error) {
	if !interactionState(status) || status == StateDeleted {
		return ingest.Persisted{}, ErrContract
	}
	id, err := parseLocal(localID)
	if err != nil || LocalID(KindInteraction, id) != localID {
		return ingest.Persisted{}, ErrNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ingest.Persisted{}, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	var current string
	var pending []byte
	err = tx.QueryRow(ctx, `SELECT state,metadata->'pending_usage' FROM olp.provider_resources
 WHERE id=$1 AND kind=$2 AND api_key_id=$3 AND state<>$4 AND expires_at>now() FOR UPDATE`, id, KindInteraction, owner, StateDeleted).Scan(&current, &pending)
	if errors.Is(err, pgx.ErrNoRows) {
		return ingest.Persisted{}, ErrNotFound
	}
	if err != nil {
		return ingest.Persisted{}, err
	}
	terminal := geminilifecycle.InteractionTurnFinished(status)
	result := ingest.Persisted{Outcome: ingest.PersistOutcomeDuplicate}
	if terminal {
		result, err = persistPendingUsage(ctx, tx, pending, evidence, now, true)
		if err != nil {
			return ingest.Persisted{}, err
		}
	} else if geminilifecycle.InteractionTurnFinished(current) {
		status = current
	} else if evidence.Evidence.Observed() && len(pending) != 0 && string(pending) != "null" {
		generation, decodeErr := usage.DecodePendingGeneration(pending)
		if decodeErr != nil {
			return ingest.Persisted{}, decodeErr
		}
		observed, observeErr := generation.Observe(evidence, usage.GenerationPending)
		if observeErr != nil {
			return ingest.Persisted{}, observeErr
		}
		encoded, encodeErr := observed.Encode()
		if encodeErr != nil {
			return ingest.Persisted{}, encodeErr
		}
		if _, err = tx.Exec(ctx, `UPDATE olp.provider_resources SET metadata=jsonb_set(metadata,'{pending_usage}',$2::jsonb) WHERE id=$1`, id, encoded); err != nil {
			return ingest.Persisted{}, err
		}
	}
	_, err = tx.Exec(ctx, `UPDATE olp.provider_resources SET state=$2,
 metadata=CASE WHEN $3 THEN metadata-'pending_usage' ELSE metadata END, updated_at=now() WHERE id=$1`, id, status, terminal)
	if err != nil {
		return ingest.Persisted{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return ingest.Persisted{}, err
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
	if _, err = persistPendingUsage(ctx, tx, pending, usage.AttemptUsage{Evidence: usage.EvidenceUnobservedUncertain}, time.Now(), false); err != nil {
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
	return status == "created" || status == StateDeleted || geminilifecycle.ValidInteractionStatus(status)
}
