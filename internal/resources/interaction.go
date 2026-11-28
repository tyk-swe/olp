package resources

import (
	"context"
	"encoding/json"
	"errors"
	"time"

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

func validInteractionExpiry(expiresAt *time.Time, now time.Time) bool {
	return expiresAt != nil && expiresAt.After(now) && !expiresAt.After(now.Add(ContinuationLifetime))
}

func validateInteractionContract(encrypted bool, r *Resource, upstreamID string, now time.Time) error {
	if !encrypted || r == nil || r.TargetID == "" || r.Kind != KindInteraction {
		return ErrContract
	}
	if r.ContractVersion == nil || *r.ContractVersion != InteractionContract || r.SubmissionID != nil {
		return ErrContract
	}
	if !interactionState(r.State) || len(upstreamID) == 0 || len(upstreamID) > 512 {
		return ErrContract
	}
	if !validInteractionExpiry(r.ExpiresAt, now) || len(r.Metadata) > metadataLimit {
		return ErrContract
	}
	return nil
}

func (s *Store) PutInteractionContract(ctx context.Context, r *Resource, upstreamID string) (*Resource, error) {
	if err := validateInteractionContract(s.Encrypted(), r, upstreamID, time.Now()); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(interactionPayload{UpstreamID: upstreamID})
	if err != nil {
		return nil, err
	}
	id, err := newUUIDv7()
	if err != nil {
		return nil, err
	}
	prepared := *r
	prepared.UUID = id
	prepared.UpstreamID = id.String()
	if len(prepared.Metadata) == 0 {
		prepared.Metadata = []byte(`{}`)
	}
	return withTxValue(ctx, s.pool, func(tx pgx.Tx) (*Resource, error) {
		if err := authorizeOwner(ctx, tx, prepared.APIKeyID); err != nil {
			return nil, err
		}
		if prepared.ParentID != nil {
			var found bool
			err := tx.QueryRow(ctx, `SELECT true FROM olp.provider_resources WHERE id=$1 AND api_key_id=$2 AND kind=$3 AND state<>$4 AND expires_at>now() FOR SHARE`,
				*prepared.ParentID, prepared.APIKeyID, KindInteraction, StateDeleted).Scan(&found)
			if err != nil || !found {
				return nil, ErrNotFound
			}
		}
		return s.insertSealedContract(ctx, tx, &prepared, payload)
	})
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

func (s *Store) ObserveInteraction(ctx context.Context, owner, localID, status string, evidence usage.AttemptUsage, now time.Time) (ingest.Persisted, error) {
	if !interactionState(status) || status == StateDeleted {
		return ingest.Persisted{}, ErrContract
	}
	parsedKind, id, err := ParseLocalID(localID)
	if err != nil || parsedKind != KindInteraction {
		return ingest.Persisted{}, ErrNotFound
	}
	return withTxValue(ctx, s.pool, func(tx pgx.Tx) (ingest.Persisted, error) {
		var current string
		var pending []byte
		err := tx.QueryRow(ctx, `SELECT state,metadata->'pending_usage' FROM olp.provider_resources
 WHERE id=$1 AND kind=$2 AND api_key_id=$3 AND state<>$4 AND expires_at>now() FOR UPDATE`, id, KindInteraction, owner, StateDeleted).Scan(&current, &pending)
		if errors.Is(err, pgx.ErrNoRows) {
			return ingest.Persisted{}, ErrNotFound
		}
		if err != nil {
			return ingest.Persisted{}, err
		}
		terminal := geminilifecycle.InteractionTurnFinished(status)
		currentTerminal := geminilifecycle.InteractionTurnFinished(current)
		nextState := status
		if currentTerminal {
			nextState = current
		}
		return applyPendingUsageTx(ctx, tx, pendingUsageUpdate{
			id:              id,
			currentState:    current,
			nextState:       nextState,
			raw:             pending,
			terminal:        terminal,
			currentTerminal: currentTerminal,
			evidence:        evidence,
			now:             now,
		})
	})
}

func (s *Store) DeleteInteractionContract(ctx context.Context, owner, localID string) error {
	parsedKind, id, err := ParseLocalID(localID)
	if err != nil || parsedKind != KindInteraction {
		return ErrNotFound
	}
	return s.withTx(ctx, func(tx pgx.Tx) error {
		if err := authorizeOwner(ctx, tx, owner); err != nil {
			return err
		}
		var pending []byte
		err := tx.QueryRow(ctx, `SELECT metadata->'pending_usage' FROM olp.provider_resources
 WHERE id=$1 AND kind=$2 AND api_key_id=$3 AND state<>$4 FOR UPDATE`, id, KindInteraction, owner, StateDeleted).Scan(&pending)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if _, err := persistPendingUsage(ctx, tx, pending, usage.AttemptUsage{Evidence: usage.EvidenceUnobservedUncertain}, time.Now(), false); err != nil {
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
		return secrets.Delete(ctx, tx, secrets.ProviderContinuation, id.String())
	})
}

func interactionState(status string) bool {
	return status == "created" || status == StateDeleted || geminilifecycle.ValidInteractionStatus(status)
}
