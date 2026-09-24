package resources

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	DurableContractVersion = "native-durable-v1"
	DurableLifetime        = 7 * 24 * time.Hour
)

func durableKind(kind string) bool { return kind == KindStrictFile || kind == KindStrictBatch }

func (s *Store) validateDurable(r *Resource, payload []byte) error {
	if !s.Encrypted() || r == nil || !durableKind(r.Kind) || r.ContractVersion == nil || *r.ContractVersion != DurableContractVersion ||
		r.SubmissionID != nil || r.ExpiresAt == nil || !r.ExpiresAt.After(time.Now()) || r.ExpiresAt.After(time.Now().Add(DurableLifetime)) {
		return ErrContract
	}
	if len(payload) == 0 || len(payload) > MaxContinuationBytes {
		return ErrPayloadTooLarge
	}
	if len(r.Metadata) > metadataLimit {
		return ErrMetadataTooLarge
	}
	return nil
}

// PutDurableContract atomically publishes a provider-accepted file or batch
// identity and its encrypted native source/result. No local ID can escape
// before both records commit. The caller owns the provider Attempt.
func (s *Store) PutDurableContract(ctx context.Context, r *Resource, payload []byte) (*Resource, error) {
	if err := s.validateDurable(r, payload); err != nil {
		return nil, err
	}
	if r.Kind == KindStrictBatch && batchStateRank(r.State) == 0 {
		return nil, ErrContract
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if err = s.authorizeOwner(ctx, tx, r.APIKeyID); err != nil {
		return nil, err
	}
	copy := *r
	copy.UUID = uuid.Must(uuid.NewV7())
	if len(copy.Metadata) == 0 {
		copy.Metadata = []byte(`{}`)
	}
	out, err := scan(tx.QueryRow(ctx, `INSERT INTO olp_go.provider_resources
 (id,kind,api_key_id,route_slug,provider_id,provider_revision_id,route_revision_id,slot_id,credential_id,upstream_id,state,metadata,expires_at,contract_version,parent_id)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15) RETURNING `+columns,
		copy.UUID, copy.Kind, copy.APIKeyID, copy.RouteSlug, copy.ProviderID, copy.ProviderRevisionID, copy.RouteRevisionID, copy.SlotID,
		copy.CredentialID, copy.UpstreamID, copy.State, copy.Metadata, copy.ExpiresAt, copy.ContractVersion, copy.ParentID))
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

// ReadDurableContract checks the current owner before decrypting. The gateway
// additionally resolves the historical serving pin and current revocation.
func (s *Store) ReadDurableContract(ctx context.Context, kind, owner, localID string) (*Resource, []byte, error) {
	if !s.Encrypted() || !durableKind(kind) {
		return nil, nil, ErrContract
	}
	id, err := parseLocal(localID)
	if err != nil || LocalID(kind, id) != localID {
		return nil, nil, ErrNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback(ctx)
	if err = s.authorizeOwner(ctx, tx, owner); err != nil {
		return nil, nil, err
	}
	r, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM olp_go.provider_resources
 WHERE id=$1 AND kind=$2 AND api_key_id=$3 AND state<>$4 AND expires_at>now() FOR SHARE`, id, kind, owner, StateDeleted))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	if r.ContractVersion == nil || *r.ContractVersion != DurableContractVersion {
		return nil, nil, ErrContract
	}
	payload, err := s.keys.Read(ctx, tx, s.installation, id.String(), continuationPurpose)
	if err != nil || len(payload) == 0 || len(payload) > MaxContinuationBytes {
		return nil, nil, ErrContract
	}
	return r, payload, nil
}

func terminalBatch(state string) bool {
	switch state {
	case "completed", "failed", "expired", "cancelled":
		return true
	}
	return false
}

func batchStateRank(state string) int {
	switch state {
	case "validating":
		return 1
	case "in_progress":
		return 2
	case "finalizing":
		return 3
	case "cancelling":
		return 4
	case "completed", "failed", "expired", "cancelled":
		return 5
	}
	return 0
}

// UpdateDurableContract commits the latest native result with the status in
// one transaction. Once a batch reaches a terminal result, a delayed poll or
// cancellation response cannot replace it with an earlier state.
func (s *Store) UpdateDurableContract(ctx context.Context, kind, owner, localID, state string, payload []byte) (*Resource, []byte, error) {
	if !s.Encrypted() || !durableKind(kind) || state == "" || kind == KindStrictBatch && batchStateRank(state) == 0 || len(payload) == 0 || len(payload) > MaxContinuationBytes {
		return nil, nil, ErrContract
	}
	id, err := parseLocal(localID)
	if err != nil || LocalID(kind, id) != localID {
		return nil, nil, ErrNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback(ctx)
	if err = s.authorizeOwner(ctx, tx, owner); err != nil {
		return nil, nil, err
	}
	r, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM olp_go.provider_resources
 WHERE id=$1 AND kind=$2 AND api_key_id=$3 AND state<>$4 AND expires_at>now() FOR UPDATE`, id, kind, owner, StateDeleted))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	if r.ContractVersion == nil || *r.ContractVersion != DurableContractVersion {
		return nil, nil, ErrContract
	}
	if kind == KindStrictBatch && (terminalBatch(r.State) || batchStateRank(state) < batchStateRank(r.State)) {
		current, err := s.keys.Read(ctx, tx, s.installation, id.String(), continuationPurpose)
		if err != nil || len(current) == 0 || len(current) > MaxContinuationBytes {
			return nil, nil, ErrContract
		}
		return r, current, nil
	}
	if _, err = tx.Exec(ctx, `UPDATE olp_go.provider_resources SET state=$2,updated_at=now() WHERE id=$1`, id, state); err != nil {
		return nil, nil, err
	}
	if err = s.keys.Store(ctx, tx, s.installation, id.String(), continuationPurpose, payload, r.ExpiresAt); err != nil {
		return nil, nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, nil, err
	}
	r.State = state
	return r, payload, nil
}
