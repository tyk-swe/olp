package resources

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/secrets"
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
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	copy := *r
	out, err := s.PutDurableContractTx(ctx, tx, &copy, payload)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) PutDurableContractTx(ctx context.Context, tx pgx.Tx, r *Resource, payload []byte) (*Resource, error) {
	return s.putDurableContractTx(ctx, tx, r, payload, true)
}

// PutRecoveredBatchContractTx adopts already-accepted work under its fenced
// submission intent. Current caller authorization still applies to API access.
func (s *Store) PutRecoveredBatchContractTx(ctx context.Context, tx pgx.Tx, r *Resource, payload []byte, claimID uuid.UUID) (*Resource, error) {
	if r == nil || r.Kind != KindStrictBatch || claimID == uuid.Nil {
		return nil, ErrContract
	}
	var claimed bool
	err := tx.QueryRow(ctx, `SELECT true FROM olp.batch_settlements
		WHERE resource_id=$1 AND api_key_id=$2 AND provider_id=$3 AND provider_revision_id=$4
		AND route_revision_id=$5 AND target_id=$6 AND slot_id=$7
		AND credential_id IS NOT DISTINCT FROM $8::uuid AND claim_id=$9
		AND kind='strict_batch' AND state='submission_unknown' FOR UPDATE`,
		r.UUID, r.APIKeyID, r.ProviderID, r.ProviderRevisionID, r.RouteRevisionID, r.TargetID, r.SlotID, r.CredentialID, claimID).Scan(&claimed)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrContract
	}
	if err != nil {
		return nil, err
	}
	if !claimed {
		return nil, ErrContract
	}
	return s.putDurableContractTx(ctx, tx, r, payload, false)
}

func (s *Store) putDurableContractTx(ctx context.Context, tx pgx.Tx, r *Resource, payload []byte, authorize bool) (*Resource, error) {
	if err := s.validateDurable(r, payload); err != nil {
		return nil, err
	}
	if r.TargetID == "" || r.Kind == KindStrictBatch && batchStateRank(r.State) == 0 {
		return nil, ErrContract
	}
	if r.UUID == uuid.Nil {
		r.UUID = uuid.Must(uuid.NewV7())
	}
	if authorize {
		if err := s.authorizeOwner(ctx, tx, r.APIKeyID); err != nil {
			return nil, err
		}
	}
	if len(r.Metadata) == 0 {
		r.Metadata = []byte(`{}`)
	}
	out, err := scan(tx.QueryRow(ctx, `INSERT INTO olp.provider_resources
 (id,kind,api_key_id,route_slug,provider_id,provider_revision_id,route_revision_id,slot_id,credential_id,upstream_id,state,metadata,expires_at,contract_version,parent_id,target_id)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16) RETURNING `+columns,
		r.UUID, r.Kind, r.APIKeyID, r.RouteSlug, r.ProviderID, r.ProviderRevisionID, r.RouteRevisionID, r.SlotID,
		r.CredentialID, r.UpstreamID, r.State, r.Metadata, r.ExpiresAt, r.ContractVersion, r.ParentID, r.TargetID))
	if err != nil {
		return nil, err
	}
	if err = s.keys.Store(ctx, tx, s.installation, out.UUID.String(), secrets.ProviderContinuation, payload, out.ExpiresAt); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) StageDurableContract(ctx context.Context, id uuid.UUID, payload []byte, expiresAt *time.Time) error {
	if !s.Encrypted() || id == uuid.Nil || expiresAt == nil || !expiresAt.After(time.Now()) ||
		expiresAt.After(time.Now().Add(DurableLifetime)) || len(payload) == 0 || len(payload) > MaxContinuationBytes {
		return ErrContract
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = s.keys.Store(ctx, tx, s.installation, id.String(), secrets.ProviderContinuation, payload, expiresAt); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) ReadStagedDurableContract(ctx context.Context, id uuid.UUID) ([]byte, error) {
	if !s.Encrypted() || id == uuid.Nil {
		return nil, ErrContract
	}
	payload, err := s.keys.Read(ctx, s.pool, s.installation, id.String(), secrets.ProviderContinuation)
	if err != nil || len(payload) == 0 || len(payload) > MaxContinuationBytes {
		return nil, ErrContract
	}
	return payload, nil
}

func (s *Store) DeleteStagedDurableContract(ctx context.Context, id uuid.UUID) error {
	if !s.Encrypted() || id == uuid.Nil {
		return ErrContract
	}
	_, err := s.pool.Exec(ctx, "DELETE FROM olp.secrets WHERE id=$1 AND purpose=$2", id, secrets.ProviderContinuation)
	return err
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
	r, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM olp.provider_resources
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
	payload, err := s.keys.Read(ctx, tx, s.installation, id.String(), secrets.ProviderContinuation)
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
	r, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM olp.provider_resources
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
		current, err := s.keys.Read(ctx, tx, s.installation, id.String(), secrets.ProviderContinuation)
		if err != nil || len(current) == 0 || len(current) > MaxContinuationBytes {
			return nil, nil, ErrContract
		}
		return r, current, nil
	}
	if _, err = tx.Exec(ctx, `UPDATE olp.provider_resources SET state=$2,updated_at=now() WHERE id=$1`, id, state); err != nil {
		return nil, nil, err
	}
	if err = s.keys.Store(ctx, tx, s.installation, id.String(), secrets.ProviderContinuation, payload, r.ExpiresAt); err != nil {
		return nil, nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, nil, err
	}
	r.State = state
	return r, payload, nil
}
