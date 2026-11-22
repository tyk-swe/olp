package resources

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"
	"github.com/tyk-swe/olp/internal/usage"
	"github.com/tyk-swe/olp/internal/usage/ingest"
)

var ErrEvidenceConflict = errors.New("retained usage evidence conflicts with existing coverage")

// RetainedAccounting contains only authority and price metadata captured before
// dispatch. The templates are keyed by every authorized model, including graders.
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

func (s *Store) prepareRetainedTx(ctx context.Context, tx pgx.Tx, r *Resource) error {
	a := r.RetainedAccounting
	if a == nil {
		return nil
	}
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
	}
	for model, event := range a.Templates {
		if event == nil || len(event.Attempts) != 1 || event.APIKeyID != r.APIKeyID || event.Attempts[0].ProviderID != r.ProviderID || event.Attempts[0].UpstreamModel != model {
			return ErrContract
		}
		routing := event.Attempts[0].Routing
		if routing == nil || routing.ProviderRevisionID != r.ProviderRevisionID || routing.CredentialSlotID == nil || *routing.CredentialSlotID != r.SlotID || !sameOptional(routing.CredentialVersionID, r.CredentialID) || event.RouteSlug != r.RouteSlug {
			return ErrContract
		}
		if _, err := usage.Validate(event); err != nil {
			return err
		}
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

// NormalizeRetainedEvidence rejects missing dimensions, incompatible dimensions,
// non-UTC intervals, and partial token tuples. Omission never supplies a zero.
func NormalizeRetainedEvidence(line RetainedEvidenceLine) (RetainedEvidenceLine, error) {
	if line.Model == "" || len(line.Model) > 512 || !line.End.After(line.Start) || line.Start.IsZero() || line.End.IsZero() {
		return line, ErrContract
	}
	_, a := line.Start.Zone()
	_, b := line.End.Zone()
	if a != 0 || b != 0 {
		return line, ErrContract
	}
	u := line.Usage
	if u.MediaUnits != nil || u.Batch || line.Component != "generation" && u.InvocationCount != nil {
		return line, ErrContract
	}
	hasGeneration := u.InputTokens != nil || u.OutputTokens != nil || len(u.ToolCalls) > 0
	switch line.Component {
	case "training":
		if u.TrainingTokens == nil || u.StorageByteSeconds != nil || u.CacheTokenSeconds != nil || len(u.ContainerSessions) > 0 || hasGeneration {
			return line, ErrContract
		}
	case "storage":
		if u.StorageByteSeconds == nil || u.TrainingTokens != nil || u.CacheTokenSeconds != nil || len(u.ContainerSessions) > 0 || hasGeneration {
			return line, ErrContract
		}
	case "cache_storage":
		if u.CacheTokenSeconds == nil || u.TrainingTokens != nil || u.StorageByteSeconds != nil || len(u.ContainerSessions) > 0 || hasGeneration {
			return line, ErrContract
		}
	case "container":
		if line.MemoryTier == "" || len(u.ContainerSessions) != 1 || u.ContainerSessions[line.MemoryTier] < 0 || u.TrainingTokens != nil || u.StorageByteSeconds != nil || u.CacheTokenSeconds != nil || hasGeneration {
			return line, ErrContract
		}
		if _, ok := u.ContainerSessions[line.MemoryTier]; !ok {
			return line, ErrContract
		}
	case "generation":
		if u.InputTokens == nil || u.OutputTokens == nil || u.CachedInputTokens == nil || u.InvocationCount == nil || u.TrainingTokens != nil || u.StorageByteSeconds != nil || u.CacheTokenSeconds != nil || len(u.ContainerSessions) > 0 {
			return line, ErrContract
		}
	default:
		return line, ErrContract
	}
	if line.Component != "container" && line.MemoryTier != "" {
		return line, ErrContract
	}
	if u.TrainingTokens != nil && *u.TrainingTokens < 0 {
		return line, ErrContract
	}
	for _, v := range []*string{u.StorageByteSeconds, u.CacheTokenSeconds} {
		if v != nil {
			normalized, err := normalizeExactQuantity(*v)
			if err != nil {
				return line, err
			}
			*v = normalized
		}
	}
	u.Evidence, u.Deferred = usage.EvidenceObservedComplete, true
	if !usage.ConsistentUsage(&u) {
		return line, ErrContract
	}
	line.Usage = u
	line.Start = line.Start.UTC().Truncate(time.Microsecond)
	line.End = line.End.UTC().Truncate(time.Microsecond)
	if !line.End.After(line.Start) {
		return line, ErrContract
	}
	return line, nil
}

func evidenceDigest(v any) string {
	data, _ := json.Marshal(v)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ApplyRetainedEvidence atomically records cross-source interval coverage and
// ordinary usage facts. Every resource is serialized before overlap checks, so
// two different evidence sources cannot charge an overlapping interval twice.
// The caller verifies remote termination independently of operator assertions.
func (s *Store) ApplyRetainedEvidence(ctx context.Context, id uuid.UUID, key, digest, provenance, actor string, observation RetainedObservation, claim *uuid.UUID) (ingest.Persisted, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ingest.Persisted{}, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	result, err := s.ApplyRetainedEvidenceTx(ctx, tx, id, key, digest, provenance, actor, observation, claim)
	if err != nil && !(errors.Is(err, ErrEvidenceConflict) && provenance == "provider_verified" && observation.RemoteTerminated) {
		return result, err
	}
	if commitErr := tx.Commit(ctx); commitErr != nil {
		return ingest.Persisted{}, commitErr
	}
	return result, err
}

// Provider termination remains valid even when its usage conflicts with earlier
// financial coverage. On that conflict the caller must commit the termination
// proof while reporting ErrEvidenceConflict; no conflicting charges survive.
func (s *Store) ApplyRetainedEvidenceTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, key, digest, provenance, actor string, observation RetainedObservation, claim *uuid.UUID) (ingest.Persisted, error) {
	if provenance != "provider_verified" || !observation.RemoteTerminated {
		return s.applyRetainedEvidenceTx(ctx, tx, id, key, digest, provenance, actor, observation, claim)
	}
	financial, err := tx.Begin(ctx)
	if err != nil {
		return ingest.Persisted{}, err
	}
	defer financial.Rollback(context.WithoutCancel(ctx))
	result, err := s.applyRetainedEvidenceTx(ctx, financial, id, key, digest, provenance, actor, observation, claim)
	if err == nil {
		return result, financial.Commit(ctx)
	}
	if rollbackErr := financial.Rollback(ctx); rollbackErr != nil {
		return ingest.Persisted{}, rollbackErr
	}
	if !errors.Is(err, ErrEvidenceConflict) {
		return ingest.Persisted{}, err
	}
	termination := RetainedObservation{RemoteTerminated: true, ObservedAt: observation.ObservedAt}
	terminationDigest := retainedObservationDigest(termination)
	if _, proofErr := s.applyRetainedEvidenceTx(ctx, tx, id, "termination:"+terminationDigest, terminationDigest, provenance, actor, termination, claim); proofErr != nil {
		return ingest.Persisted{}, proofErr
	}
	return ingest.Persisted{}, err
}

func (s *Store) applyRetainedEvidenceTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, key, digest, provenance, actor string, observation RetainedObservation, claim *uuid.UUID) (ingest.Persisted, error) {
	result := ingest.Persisted{Outcome: ingest.PersistOutcomeDuplicate}
	if len(key) < 1 || len(key) > 128 || len(digest) != 64 || digest != strings.ToLower(digest) || (provenance != "provider_verified" && provenance != "operator_verified") {
		return result, ErrContract
	}
	if d, err := hex.DecodeString(digest); err != nil || len(d) != 32 {
		return result, ErrContract
	}
	var raw []byte
	var operation, previousState string
	var pinnedMetadata []byte
	var terminated bool
	var terminalAt *time.Time
	var started time.Time
	var storedClaim *uuid.UUID
	var leaseActive bool
	err := tx.QueryRow(ctx, `SELECT s.templates,s.remote_terminated,s.terminal_at,s.started_at,s.claim_id,s.operation,r.metadata,s.state,COALESCE(s.lease_until>clock_timestamp(),false) FROM olp.retained_settlements s JOIN olp.provider_resources r ON r.id=s.resource_id WHERE s.resource_id=$1 FOR UPDATE OF s`, id).Scan(&raw, &terminated, &terminalAt, &started, &storedClaim, &operation, &pinnedMetadata, &previousState, &leaseActive)
	if errors.Is(err, pgx.ErrNoRows) {
		return result, ErrNotFound
	}
	if err != nil {
		return result, err
	}
	if claim != nil && (storedClaim == nil || *storedClaim != *claim || !leaseActive) {
		return result, ErrSubmissionConflict
	}
	var pendingMutation bool
	if operation == "context_cache" {
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.retained_mutations WHERE resource_id=$1 AND state IN ('dispatched','submission_unknown'))`, id).Scan(&pendingMutation); err != nil {
			return result, err
		}
		if pendingMutation && provenance == "provider_verified" && len(observation.Lines) > 0 {
			return result, ErrEvidenceConflict
		}
	}
	var templates map[string]*usage.Event
	if err = json.Unmarshal(raw, &templates); err != nil {
		return result, err
	}
	var pins struct {
		MemoryTier string `json:"memory_tier"`
	}
	if json.Unmarshal(pinnedMetadata, &pins) != nil {
		return result, ErrContract
	}
	expectedComponent := retainedComponent(operation)
	observation.Lines, err = normalizeRetainedLines(observation.Lines, expectedComponent, pins.MemoryTier, templates)
	if err != nil {
		return result, err
	}
	result, err = persistRetainedEvidence(ctx, tx, id, key, digest, provenance, actor, previousState, templates, observation)
	if err != nil {
		return result, err
	}
	if observation.RemoteTerminated {
		terminated = true
		if terminalAt == nil {
			v := observation.ObservedAt.UTC().Truncate(time.Microsecond)
			if v.IsZero() {
				v = time.Now().UTC()
			}
			terminalAt = &v
		}
	}
	// Coverage is checked server-side. A terminal indicator alone is insufficient.
	closed := false
	if terminated && terminalAt != nil && !(pendingMutation && provenance == "provider_verified") {
		var complete bool
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.retained_usage_evidence WHERE resource_id=$1 AND document @> '{"coverage_complete":true}'::jsonb)`, id).Scan(&complete)
		if err != nil {
			return result, err
		}
		if complete {
			closed, err = retainedCoverageComplete(ctx, tx, id, templates, started, *terminalAt)
			if err != nil {
				return result, err
			}
		}
	}
	state := "evidence_missing"
	if closed || previousState == "closed" {
		state = "closed"
	}
	_, err = tx.Exec(ctx, `UPDATE olp.retained_settlements SET remote_terminated=$2,terminal_at=$3,state=$4,updated_at=now(),last_error=NULL WHERE resource_id=$1`, id, terminated, terminalAt, state)
	return result, err
}

func sameJSON(a, b []byte) bool {
	var x, y any
	if decodeExactJSON(a, &x) != nil || decodeExactJSON(b, &y) != nil {
		return false
	}
	xx, _ := json.Marshal(x)
	yy, _ := json.Marshal(y)
	return bytes.Equal(xx, yy)
}

func retainedEvidenceKey(observation RetainedObservation) string {
	return "provider:" + retainedObservationDigest(observation)
}

func (s *Store) RecordRetainedObservation(ctx context.Context, r *Resource, o RetainedObservation, claim *uuid.UUID) error {
	if len(o.Lines) == 0 && !o.RemoteTerminated {
		return nil
	}
	_, err := s.ApplyRetainedEvidence(ctx, r.UUID, retainedEvidenceKey(o), retainedObservationDigest(o), "provider_verified", "", o, claim)
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

func normalizeExactQuantity(value string) (string, error) {
	if len(value) > 128 || value == "" {
		return "", ErrContract
	}
	for _, r := range value {
		if r != '.' && (r < '0' || r > '9') {
			return "", ErrContract
		}
	}
	d, err := decimal.NewFromString(value)
	if err != nil || d.IsNegative() {
		return "", ErrContract
	}
	return d.String(), nil
}

func sameOptional(a, b *string) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }
func decodeExactJSON(data []byte, out any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	return decoder.Decode(out)
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
	}
	return ""
}

func retainedObservationDigest(o RetainedObservation) string {
	o.ObservedAt = time.Time{}
	o.Lines = slices.Clone(o.Lines)
	for i, line := range o.Lines {
		if normalized, err := NormalizeRetainedEvidence(line); err == nil {
			o.Lines[i] = normalized
		}
	}
	slices.SortFunc(o.Lines, func(a, b RetainedEvidenceLine) int {
		return strings.Compare(a.Component+":"+a.Model+":"+a.MemoryTier+":"+a.Start.Format(time.RFC3339Nano), b.Component+":"+b.Model+":"+b.MemoryTier+":"+b.Start.Format(time.RFC3339Nano))
	})
	return evidenceDigest(o)
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

func normalizeRetainedLines(lines []RetainedEvidenceLine, component, tier string, templates map[string]*usage.Event) ([]RetainedEvidenceLine, error) {
	lines = slices.Clone(lines)
	for i, line := range lines {
		if line.Component != component || line.Component == "container" && line.MemoryTier != tier {
			return nil, ErrContract
		}
		normalized, err := NormalizeRetainedEvidence(line)
		if err != nil {
			return nil, err
		}
		lines[i] = normalized
		if templates[line.Model] == nil {
			return nil, ErrContract
		}
	}
	// Stable ordering also makes cross-source normalization deterministic.
	slices.SortFunc(lines, func(a, b RetainedEvidenceLine) int {
		return strings.Compare(a.Component+":"+a.Model+":"+a.MemoryTier+":"+a.Start.Format(time.RFC3339Nano), b.Component+":"+b.Model+":"+b.MemoryTier+":"+b.Start.Format(time.RFC3339Nano))
	})
	return lines, nil
}
