package resources

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/usage"
	"github.com/tyk-swe/olp/internal/usage/ingest"
)

var ErrEvidenceConflict = errors.New("retained usage evidence conflicts with existing coverage")

type RetainedAccounting struct {
	Operation      string
	Templates      map[string]*usage.Event
	TokenConsuming bool
}

type RetainedEvidenceLine struct {
	Component  string             `json:"component"`
	Model      string             `json:"model"`
	MemoryTier string             `json:"memory_tier,omitempty"`
	Start      time.Time          `json:"start"`
	End        time.Time          `json:"end"`
	Usage      usage.AttemptUsage `json:"usage"`
}

type RetainedObservation struct {
	Lines            []RetainedEvidenceLine `json:"lines"`
	RemoteTerminated bool                   `json:"remote_terminated"`
	CoverageComplete bool                   `json:"coverage_complete"`
	ObservedAt       time.Time              `json:"observed_at"`
}

type EvidenceSubmission struct {
	ResourceID  uuid.UUID
	Key         string
	Digest      string
	Provenance  string
	Actor       string
	Observation RetainedObservation
	Claim       *uuid.UUID
}

type lockedSettlement struct {
	templates     map[string]*usage.Event
	operation     string
	previousState string
	memoryTier    string
	terminated    bool
	terminalAt    *time.Time
	started       time.Time
	storedClaim   *uuid.UUID
	leaseActive   bool
}

func (s *Store) prepareRetainedTx(ctx context.Context, tx pgx.Tx, r *Resource) error {
	a := r.RetainedAccounting
	if a == nil {
		return nil
	}
	if err := validateRetainedAccounting(r, a); err != nil {
		return err
	}
	data, err := json.Marshal(a.Templates)
	if err != nil {
		return err
	}
	owner := r.UUID
	if r.ParentID != nil && (a.Operation == "vector_store" || a.Operation == "container") {
		owner = *r.ParentID
	}
	_, err = tx.Exec(ctx, `INSERT INTO olp.retained_settlements(resource_id,operation,templates,token_consuming) VALUES($1,$2,$3,$4) ON CONFLICT(resource_id) DO NOTHING`, owner, a.Operation, data, a.TokenConsuming)
	return err
}

func validateRetainedAccounting(r *Resource, a *RetainedAccounting) error {
	if a.Operation == "" || a.Templates == nil {
		return ErrContract
	}
	if len(a.Templates) == 0 {
		var metadata struct {
			Models []string `json:"authorized_models"`
		}
		if a.Operation != "eval" || a.TokenConsuming || json.Unmarshal(r.Metadata, &metadata) != nil || metadata.Models == nil || len(metadata.Models) != 0 {
			return ErrContract
		}
		return nil
	}
	for model, event := range a.Templates {
		if err := validateAccountingTemplate(r, model, event); err != nil {
			return err
		}
	}
	return nil
}

func validateAccountingTemplate(r *Resource, model string, event *usage.Event) error {
	if event == nil || len(event.Attempts) != 1 || event.APIKeyID != r.APIKeyID || event.Attempts[0].ProviderID != r.ProviderID || event.Attempts[0].UpstreamModel != model {
		return ErrContract
	}
	routing := event.Attempts[0].Routing
	if routing == nil || routing.ProviderRevisionID != r.ProviderRevisionID || routing.CredentialSlotID == nil || *routing.CredentialSlotID != r.SlotID || !sameOptional(routing.CredentialVersionID, r.CredentialID) || event.RouteSlug != r.RouteSlug {
		return ErrContract
	}
	_, err := usage.Validate(event)
	return err
}

func (s *SettlementStore) applyEvidence(ctx context.Context, sub EvidenceSubmission) (ingest.Persisted, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ingest.Persisted{}, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	result, err := s.ApplyEvidenceTx(ctx, tx, sub)
	if err != nil && !(errors.Is(err, ErrEvidenceConflict) && sub.Provenance == "provider_verified" && sub.Observation.RemoteTerminated) {
		return result, err
	}
	if commitErr := tx.Commit(ctx); commitErr != nil {
		return ingest.Persisted{}, commitErr
	}
	return result, err
}

// Provider termination remains valid even when its usage conflicts with earlier
// financial coverage. On that conflict the caller commits the termination proof
// while reporting ErrEvidenceConflict so no conflicting charges survive.
func (s *SettlementStore) ApplyEvidenceTx(ctx context.Context, tx pgx.Tx, sub EvidenceSubmission) (ingest.Persisted, error) {
	if sub.Provenance != "provider_verified" || !sub.Observation.RemoteTerminated {
		return s.applyEvidenceTx(ctx, tx, sub)
	}
	financial, err := tx.Begin(ctx)
	if err != nil {
		return ingest.Persisted{}, err
	}
	defer financial.Rollback(context.WithoutCancel(ctx))
	result, err := s.applyEvidenceTx(ctx, financial, sub)
	if err == nil {
		return result, financial.Commit(ctx)
	}
	if rollbackErr := financial.Rollback(context.WithoutCancel(ctx)); rollbackErr != nil {
		return ingest.Persisted{}, rollbackErr
	}
	if !errors.Is(err, ErrEvidenceConflict) {
		return ingest.Persisted{}, err
	}
	termination := RetainedObservation{RemoteTerminated: true, ObservedAt: sub.Observation.ObservedAt}
	terminationDigest, digestErr := retainedObservationDigest(termination)
	if digestErr != nil {
		return ingest.Persisted{}, digestErr
	}
	proof := EvidenceSubmission{
		ResourceID:  sub.ResourceID,
		Key:         "termination:" + terminationDigest,
		Digest:      terminationDigest,
		Provenance:  sub.Provenance,
		Actor:       sub.Actor,
		Observation: termination,
		Claim:       sub.Claim,
	}
	if _, proofErr := s.applyEvidenceTx(ctx, tx, proof); proofErr != nil {
		return ingest.Persisted{}, proofErr
	}
	return ingest.Persisted{}, err
}

func (s *SettlementStore) applyEvidenceTx(ctx context.Context, tx pgx.Tx, sub EvidenceSubmission) (ingest.Persisted, error) {
	result := ingest.Persisted{Outcome: ingest.PersistOutcomeDuplicate}
	if err := validateEvidenceSubmission(sub); err != nil {
		return result, err
	}
	row, err := lockSettlementRow(ctx, tx, sub.ResourceID)
	if err != nil {
		return result, err
	}
	if sub.Claim != nil && (row.storedClaim == nil || *row.storedClaim != *sub.Claim || !row.leaseActive) {
		return result, ErrSubmissionConflict
	}
	pendingMutation := false
	if row.operation == "context_cache" {
		pendingMutation, err = hasPendingContextCacheMutation(ctx, tx, sub.ResourceID)
		if err != nil {
			return result, err
		}
		if pendingMutation && sub.Provenance == "provider_verified" && len(sub.Observation.Lines) > 0 {
			return result, ErrEvidenceConflict
		}
	}
	normalizedLines, err := normalizeRetainedLines(sub.Observation.Lines, retainedComponent(row.operation), row.memoryTier, row.templates)
	if err != nil {
		return result, err
	}
	normalizedSub := sub
	normalizedSub.Observation.Lines = normalizedLines
	result, err = persistRetainedEvidence(ctx, tx, normalizedSub, row.previousState, row.templates)
	if err != nil {
		return result, err
	}
	terminated, terminalAt, state, err := resolveSettlementState(ctx, tx, sub.ResourceID, row, pendingMutation, normalizedSub)
	if err != nil {
		return result, err
	}
	_, err = tx.Exec(ctx, `UPDATE olp.retained_settlements SET remote_terminated=$2,terminal_at=$3,state=$4,updated_at=now(),last_error=NULL WHERE resource_id=$1`, sub.ResourceID, terminated, terminalAt, state)
	return result, err
}

func validateEvidenceSubmission(sub EvidenceSubmission) error {
	if len(sub.Key) < 1 || len(sub.Key) > 128 || len(sub.Digest) != 64 || sub.Digest != strings.ToLower(sub.Digest) {
		return ErrContract
	}
	if sub.Provenance != "provider_verified" && sub.Provenance != "operator_verified" {
		return ErrContract
	}
	decoded, err := hex.DecodeString(sub.Digest)
	if err != nil || len(decoded) != 32 {
		return ErrContract
	}
	return nil
}

func lockSettlementRow(ctx context.Context, tx pgx.Tx, id uuid.UUID) (lockedSettlement, error) {
	var raw, pinnedMetadata []byte
	var row lockedSettlement
	err := tx.QueryRow(ctx, `SELECT s.templates,s.remote_terminated,s.terminal_at,s.started_at,s.claim_id,s.operation,r.metadata,s.state,COALESCE(s.lease_until>clock_timestamp(),false) FROM olp.retained_settlements s JOIN olp.provider_resources r ON r.id=s.resource_id WHERE s.resource_id=$1 FOR UPDATE OF s`, id).Scan(
		&raw, &row.terminated, &row.terminalAt, &row.started, &row.storedClaim, &row.operation, &pinnedMetadata, &row.previousState, &row.leaseActive,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return lockedSettlement{}, ErrNotFound
	}
	if err != nil {
		return lockedSettlement{}, err
	}
	if err := json.Unmarshal(raw, &row.templates); err != nil {
		return lockedSettlement{}, err
	}
	var pins struct {
		MemoryTier string `json:"memory_tier"`
	}
	if json.Unmarshal(pinnedMetadata, &pins) != nil {
		return lockedSettlement{}, ErrContract
	}
	row.memoryTier = pins.MemoryTier
	return row, nil
}

func hasPendingContextCacheMutation(ctx context.Context, tx pgx.Tx, id uuid.UUID) (bool, error) {
	var pending bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.retained_mutations WHERE resource_id=$1 AND state IN ('dispatched','submission_unknown'))`, id).Scan(&pending)
	return pending, err
}

func resolveSettlementState(ctx context.Context, tx pgx.Tx, id uuid.UUID, row lockedSettlement, pendingMutation bool, sub EvidenceSubmission) (bool, *time.Time, string, error) {
	terminated := row.terminated
	terminalAt := row.terminalAt
	if sub.Observation.RemoteTerminated {
		terminated = true
		if terminalAt == nil {
			terminalAt = verifiedTerminalTime(sub.Observation)
		}
	}
	closed := false
	if terminated && terminalAt != nil && !(pendingMutation && sub.Provenance == "provider_verified") {
		var complete bool
		err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.retained_usage_evidence WHERE resource_id=$1 AND document @> '{"coverage_complete":true}'::jsonb)`, id).Scan(&complete)
		if err != nil {
			return false, nil, "", err
		}
		if complete {
			closed, err = retainedCoverageComplete(ctx, tx, id, row.templates, row.started, *terminalAt)
			if err != nil {
				return false, nil, "", err
			}
		}
	}
	state := "evidence_missing"
	if closed || row.previousState == "closed" {
		state = "closed"
	}
	return terminated, terminalAt, state, nil
}

func (s *Store) RecordRetainedObservation(ctx context.Context, r *Resource, o RetainedObservation, claim *uuid.UUID) error {
	if len(o.Lines) == 0 && !o.RemoteTerminated {
		return nil
	}
	digest, err := retainedObservationDigest(o)
	if err != nil {
		return err
	}
	sub := EvidenceSubmission{
		ResourceID:  r.UUID,
		Key:         "provider:" + digest,
		Digest:      digest,
		Provenance:  "provider_verified",
		Observation: o,
		Claim:       claim,
	}
	_, err = s.Settlements().applyEvidence(ctx, sub)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

func (s *Store) CheckPendingTokenOwner(ctx context.Context, owner string) error {
	var pending bool
	err := s.pool.QueryRow(ctx, `SELECT olp.has_pending_retained_tokens($1::uuid)`, owner).Scan(&pending)
	if err != nil {
		return err
	}
	if pending {
		return ErrPendingUsage
	}
	return nil
}

func sameOptional(a, b *string) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

func retainedComponent(operation string) string {
	switch operation {
	case "fine_tuning":
		return "training"
	case "eval":
		return "generation"
	case "context_cache":
		return "cache_storage"
	case "vector_store":
		return "storage"
	case "container":
		return "container"
	default:
		return ""
	}
}

func verifiedTerminalTime(o RetainedObservation) *time.Time {
	if !o.RemoteTerminated {
		return nil
	}
	at := o.ObservedAt.UTC().Truncate(time.Microsecond)
	if at.IsZero() {
		at = time.Now().UTC().Truncate(time.Microsecond)
	}
	return &at
}
