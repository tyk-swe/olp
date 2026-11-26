package resources

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/platform/secrets"
)

const (
	DurableContractVersion = "native-durable-v1"
	DurableLifetime        = 7 * 24 * time.Hour
)

type BatchRecoveryEvidence struct {
	State           string
	Metadata        json.RawMessage
	Model           string
	CreatedAt       time.Time
	SubmittedFileID string
	ObservedFileID  string
	NativeStatus    string
	Result          []byte
}

type BatchRecovery struct {
	store    *Store
	resource Resource
	payload  []byte
}

func (r BatchRecovery) UpstreamID() string { return r.resource.UpstreamID }

func (r BatchRecovery) Persist(ctx context.Context, tx pgx.Tx) error {
	if r.resource.Kind == KindStrictBatch {
		_, err := r.store.putRecoveredDurableContractTx(ctx, tx, &r.resource, r.payload)
		return err
	}
	_, err := r.store.PutTx(ctx, tx, &r.resource)
	return err
}

func (s *Store) PrepareBatchRecovery(ctx context.Context, candidate Resource, evidence BatchRecoveryEvidence) (BatchRecovery, error) {
	resource := candidate
	resource.State, resource.Metadata = evidence.State, evidence.Metadata
	if candidate.Kind != KindStrictBatch {
		return BatchRecovery{store: s, resource: resource}, nil
	}
	if evidence.SubmittedFileID == "" || candidate.UpstreamID == "" || evidence.ObservedFileID != evidence.SubmittedFileID || wire.BatchStageOf(evidence.NativeStatus) == wire.BatchUnknown {
		return BatchRecovery{}, ErrContract
	}
	staged, err := s.readStagedDurableContract(ctx, candidate.UUID)
	if err != nil {
		return BatchRecovery{}, err
	}
	payload, err := recoverBatchContract(staged, evidence.SubmittedFileID, evidence.Result)
	if err != nil {
		return BatchRecovery{}, err
	}
	metadata, err := json.Marshal(map[string]string{"upstream_model": evidence.Model})
	if err != nil {
		return BatchRecovery{}, err
	}
	version, expires := DurableContractVersion, evidence.CreatedAt.Add(DurableLifetime-time.Second)
	resource.Metadata, resource.ContractVersion, resource.ExpiresAt = metadata, &version, &expires
	return BatchRecovery{store: s, resource: resource, payload: payload}, nil
}

func recoverBatchContract(staged []byte, inputFileID string, body []byte) ([]byte, error) {
	var document map[string]json.RawMessage
	if err := json.Unmarshal(staged, &document); err != nil {
		return nil, err
	}
	var effective []byte
	if err := json.Unmarshal(document["effective"], &effective); err != nil {
		return nil, err
	}
	var request struct {
		InputFileID string `json:"input_file_id"`
	}
	if err := json.Unmarshal(effective, &request); err != nil || request.InputFileID != inputFileID {
		return nil, ErrContract
	}
	result, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	merged := maps.Clone(document)
	merged["result"] = result
	return json.Marshal(merged)
}

func durableKind(kind string) bool { return kind == KindStrictFile || kind == KindStrictBatch }

func (s *Store) validateDurable(r *Resource, payload []byte) error {
	return validateDurableAt(s.Encrypted(), r, payload, time.Now())
}

func validateDurableAt(encrypted bool, r *Resource, payload []byte, now time.Time) error {
	if !encrypted || r == nil || !durableKind(r.Kind) {
		return ErrContract
	}
	if r.ContractVersion == nil || *r.ContractVersion != DurableContractVersion || r.SubmissionID != nil {
		return ErrContract
	}
	if r.ExpiresAt == nil || !r.ExpiresAt.After(now) || r.ExpiresAt.After(now.Add(DurableLifetime)) {
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

func (s *Store) PutDurableContract(ctx context.Context, r *Resource, payload []byte) (*Resource, error) {
	if err := s.validateDurable(r, payload); err != nil {
		return nil, err
	}
	return withTxValue(ctx, s.pool, func(tx pgx.Tx) (*Resource, error) {
		return s.PutDurableContractTx(ctx, tx, r, payload)
	})
}

func (s *Store) PutDurableContractTx(ctx context.Context, tx pgx.Tx, r *Resource, payload []byte) (*Resource, error) {
	return s.putDurableContractTx(ctx, tx, r, payload, true)
}

func (s *Store) putRecoveredDurableContractTx(ctx context.Context, tx pgx.Tx, r *Resource, payload []byte) (*Resource, error) {
	if r == nil || r.Kind != KindStrictBatch {
		return nil, ErrContract
	}
	return s.putDurableContractTx(ctx, tx, r, payload, false)
}

func (s *Store) putDurableContractTx(ctx context.Context, tx pgx.Tx, r *Resource, payload []byte, authorize bool) (*Resource, error) {
	if err := s.validateDurable(r, payload); err != nil {
		return nil, err
	}
	if r.TargetID == "" || (r.Kind == KindStrictBatch && wire.BatchStageOf(r.State) == wire.BatchUnknown) {
		return nil, ErrContract
	}
	prepared := *r
	if prepared.UUID == uuid.Nil {
		id, err := newUUIDv7()
		if err != nil {
			return nil, err
		}
		prepared.UUID = id
	}
	if authorize {
		if err := authorizeOwner(ctx, tx, prepared.APIKeyID); err != nil {
			return nil, err
		}
	}
	if len(prepared.Metadata) == 0 {
		prepared.Metadata = []byte(`{}`)
	}
	return s.insertSealedContract(ctx, tx, &prepared, payload)
}

func (s *Store) StageDurableContract(ctx context.Context, id uuid.UUID, payload []byte, expiresAt *time.Time) error {
	now := time.Now()
	if !s.Encrypted() || id == uuid.Nil || expiresAt == nil || !expiresAt.After(now) ||
		expiresAt.After(now.Add(DurableLifetime)) || len(payload) == 0 || len(payload) > MaxContinuationBytes {
		return ErrContract
	}
	return s.withTx(ctx, func(tx pgx.Tx) error {
		return s.keys.Store(ctx, tx, s.installation, id.String(), secrets.ProviderContinuation, payload, expiresAt)
	})
}

func (s *Store) readStagedDurableContract(ctx context.Context, id uuid.UUID) ([]byte, error) {
	if !s.Encrypted() || id == uuid.Nil {
		return nil, ErrContract
	}
	payload, err := s.keys.Read(ctx, s.pool, s.installation, id.String(), secrets.ProviderContinuation)
	if err != nil || len(payload) == 0 || len(payload) > MaxContinuationBytes {
		return nil, ErrContract
	}
	return payload, nil
}

func (s *Store) ReadDurableContract(ctx context.Context, kind, owner, localID string) (*Resource, []byte, error) {
	if !durableKind(kind) {
		return nil, nil, ErrContract
	}
	return s.readSealedContract(ctx, kind, owner, localID, MaxContinuationBytes, func(r *Resource) bool {
		return r.ContractVersion != nil && *r.ContractVersion == DurableContractVersion
	})
}

func batchUpdateRegresses(kind, current, next string) bool {
	if kind != KindStrictBatch {
		return false
	}
	return wire.BatchStageOf(current) == wire.BatchTerminal || wire.BatchStageOf(next) < wire.BatchStageOf(current)
}

func validateDurableUpdate(encrypted bool, kind, state string, payload []byte) error {
	if !encrypted || !durableKind(kind) || state == "" {
		return ErrContract
	}
	if kind == KindStrictBatch && wire.BatchStageOf(state) == wire.BatchUnknown {
		return ErrContract
	}
	if len(payload) == 0 || len(payload) > MaxContinuationBytes {
		return ErrContract
	}
	return nil
}

func (s *Store) readDurablePayload(ctx context.Context, tx pgx.Tx, id uuid.UUID) ([]byte, error) {
	current, err := s.keys.Read(ctx, tx, s.installation, id.String(), secrets.ProviderContinuation)
	if err != nil || len(current) == 0 || len(current) > MaxContinuationBytes {
		return nil, ErrContract
	}
	return current, nil
}

func (s *Store) updateDurableContractTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, kind, owner, state string, payload []byte) (sealedContract, error) {
	if err := authorizeOwner(ctx, tx, owner); err != nil {
		return sealedContract{}, err
	}
	r, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM olp.provider_resources
 WHERE id=$1 AND kind=$2 AND api_key_id=$3 AND state<>$4 AND expires_at>now() FOR UPDATE`, id, kind, owner, StateDeleted))
	if errors.Is(err, pgx.ErrNoRows) {
		return sealedContract{}, ErrNotFound
	}
	if err != nil {
		return sealedContract{}, err
	}
	if r.ContractVersion == nil || *r.ContractVersion != DurableContractVersion {
		return sealedContract{}, ErrContract
	}
	if batchUpdateRegresses(kind, r.State, state) {
		current, err := s.readDurablePayload(ctx, tx, id)
		if err != nil {
			return sealedContract{}, err
		}
		return sealedContract{resource: r, payload: current}, nil
	}
	if _, err := tx.Exec(ctx, `UPDATE olp.provider_resources SET state=$2,updated_at=now() WHERE id=$1`, id, state); err != nil {
		return sealedContract{}, err
	}
	if err := s.keys.Store(ctx, tx, s.installation, id.String(), secrets.ProviderContinuation, payload, r.ExpiresAt); err != nil {
		return sealedContract{}, err
	}
	r.State = state
	return sealedContract{resource: r, payload: payload}, nil
}

func (s *Store) UpdateDurableContract(ctx context.Context, kind, owner, localID, state string, payload []byte) (*Resource, []byte, error) {
	if err := validateDurableUpdate(s.Encrypted(), kind, state, payload); err != nil {
		return nil, nil, err
	}
	parsedKind, id, err := ParseLocalID(localID)
	if err != nil || parsedKind != kind {
		return nil, nil, ErrNotFound
	}
	updated, err := withTxValue(ctx, s.pool, func(tx pgx.Tx) (sealedContract, error) {
		return s.updateDurableContractTx(ctx, tx, id, kind, owner, state, payload)
	})
	if err != nil {
		return nil, nil, err
	}
	return updated.resource, updated.payload, nil
}
