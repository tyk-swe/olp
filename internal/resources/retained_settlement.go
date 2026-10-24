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
	if a.Operation == "" || len(a.Templates) == 0 {
		return ErrContract
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
	u.Observed, u.Complete, u.BillingUncertain, u.Deferred = true, true, false, true
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
func (s *Store) ApplyRetainedEvidence(ctx context.Context, id uuid.UUID, key, digest, provenance, actor string, observation RetainedObservation, claim *uuid.UUID) (usage.Persisted, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return usage.Persisted{}, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	result, err := s.ApplyRetainedEvidenceTx(ctx, tx, id, key, digest, provenance, actor, observation, claim)
	if err != nil && !(errors.Is(err, ErrEvidenceConflict) && provenance == "provider_verified" && observation.RemoteTerminated) {
		return result, err
	}
	if commitErr := tx.Commit(ctx); commitErr != nil {
		return usage.Persisted{}, commitErr
	}
	return result, err
}

// Provider termination remains valid even when its usage conflicts with earlier
// financial coverage. On that conflict the caller must commit the termination
// proof while reporting ErrEvidenceConflict; no conflicting charges survive.
func (s *Store) ApplyRetainedEvidenceTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, key, digest, provenance, actor string, observation RetainedObservation, claim *uuid.UUID) (usage.Persisted, error) {
	if provenance != "provider_verified" || !observation.RemoteTerminated {
		return s.applyRetainedEvidenceTx(ctx, tx, id, key, digest, provenance, actor, observation, claim)
	}
	financial, err := tx.Begin(ctx)
	if err != nil {
		return usage.Persisted{}, err
	}
	defer financial.Rollback(context.WithoutCancel(ctx))
	result, err := s.applyRetainedEvidenceTx(ctx, financial, id, key, digest, provenance, actor, observation, claim)
	if err == nil {
		return result, financial.Commit(ctx)
	}
	if rollbackErr := financial.Rollback(ctx); rollbackErr != nil {
		return usage.Persisted{}, rollbackErr
	}
	if !errors.Is(err, ErrEvidenceConflict) {
		return usage.Persisted{}, err
	}
	termination := RetainedObservation{RemoteTerminated: true, ObservedAt: observation.ObservedAt}
	terminationDigest := retainedObservationDigest(termination)
	if _, proofErr := s.applyRetainedEvidenceTx(ctx, tx, id, "termination:"+terminationDigest, terminationDigest, provenance, actor, termination, claim); proofErr != nil {
		return usage.Persisted{}, proofErr
	}
	return usage.Persisted{}, err
}

func (s *Store) applyRetainedEvidenceTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, key, digest, provenance, actor string, observation RetainedObservation, claim *uuid.UUID) (usage.Persisted, error) {
	result := usage.Persisted{Outcome: usage.PersistOutcomeDuplicate}
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
	observation.Lines = slices.Clone(observation.Lines)
	for i, line := range observation.Lines {
		if line.Component != expectedComponent || line.Component == "container" && line.MemoryTier != pins.MemoryTier {
			return result, ErrContract
		}
		normalized, e := NormalizeRetainedEvidence(line)
		if e != nil {
			return result, e
		}
		observation.Lines[i] = normalized
		if templates[line.Model] == nil {
			return result, ErrContract
		}
	}
	// Stable ordering also makes cross-source normalization deterministic.
	slices.SortFunc(observation.Lines, func(a, b RetainedEvidenceLine) int {
		return strings.Compare(a.Component+":"+a.Model+":"+a.MemoryTier+":"+a.Start.Format(time.RFC3339Nano), b.Component+":"+b.Model+":"+b.MemoryTier+":"+b.Start.Format(time.RFC3339Nano))
	})
	documentObservation := observation
	documentObservation.ObservedAt = time.Time{}
	documentObservation.RemoteTerminated = false
	document, err := json.Marshal(documentObservation)
	if err != nil {
		return result, err
	}
	var priorDigest string
	var prior []byte
	err = tx.QueryRow(ctx, `SELECT digest,document FROM olp.retained_usage_evidence WHERE resource_id=$1 AND idempotency_key=$2`, id, key).Scan(&priorDigest, &prior)
	if err == nil {
		if priorDigest != digest || !sameJSON(prior, document) {
			return result, ErrEvidenceConflict
		}
		return result, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return result, err
	}
	evidenceID := uuid.Must(uuid.NewV7())
	if _, err = tx.Exec(ctx, `INSERT INTO olp.retained_usage_evidence(id,resource_id,idempotency_key,digest,provenance,actor,document,remote_termination_verified,verified_terminal_at) VALUES($1,$2,$3,$4,$5,NULLIF($6,''),$7,$8,$9)`, evidenceID, id, key, digest, provenance, actor, document, observation.RemoteTerminated, verifiedTerminalTime(observation)); err != nil {
		return result, err
	}
	for _, line := range observation.Lines {
		encoded, _ := json.Marshal(line.Usage)
		rows, e := tx.Query(ctx, `SELECT starts_at,ends_at,quantities FROM olp.retained_usage_coverage WHERE resource_id=$1 AND component=$2 AND model=$3 AND memory_tier=$4 AND starts_at<$6 AND ends_at>$5`, id, line.Component, line.Model, line.MemoryTier, line.Start, line.End)
		if e != nil {
			return result, e
		}
		duplicate := false
		for rows.Next() {
			var start, end time.Time
			var quantities []byte
			if e = rows.Scan(&start, &end, &quantities); e != nil {
				rows.Close()
				return result, e
			}
			if !start.Equal(line.Start) || !end.Equal(line.End) || !sameJSON(quantities, encoded) {
				rows.Close()
				return result, ErrEvidenceConflict
			}
			duplicate = true
		}
		rows.Close()
		if e = rows.Err(); e != nil {
			return result, e
		}
		if duplicate {
			continue
		}
		if previousState == "closed" {
			return result, ErrEvidenceConflict
		}
		template, _ := json.Marshal(templates[line.Model])
		event, e := usage.Decode(template)
		if e != nil {
			return result, e
		}
		identity := id.String() + ":" + line.Component + ":" + line.Model + ":" + line.MemoryTier + ":" + line.Start.Format(time.RFC3339Nano) + ":" + line.End.Format(time.RFC3339Nano)
		event.RequestID = uuid.NewSHA1(uuid.NameSpaceOID, []byte(identity)).String()
		event.EventID = event.RequestID
		event.ObservedAt = time.Now().UTC()
		event.RequestStartedAt = event.ObservedAt
		event.RequestCompletedAt = event.ObservedAt
		event.HTTPOutcomePending = false
		event.Attempts[0].ID = uuid.NewSHA1(uuid.NameSpaceOID, []byte(identity+":attempt")).String()
		// Keep the accepted pricing timestamp, including time-window tariffs.
		event.Attempts[0].CompletedAt = event.Attempts[0].StartedAt
		setPendingUsage(event, line.Usage)
		payload, e := usage.Encode(event)
		if e != nil {
			return result, e
		}
		persisted, e := usage.PersistRetainedEventTx(ctx, tx, event, payload)
		if e != nil {
			return result, e
		}
		if persisted.Outcome == usage.PersistOutcomeRejectedOutsideReplayWindow {
			return result, ErrContract
		}
		result = persisted
		if _, e = tx.Exec(ctx, `INSERT INTO olp.retained_usage_coverage(resource_id,evidence_id,component,model,memory_tier,starts_at,ends_at,quantities,event_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, id, evidenceID, line.Component, line.Model, line.MemoryTier, line.Start, line.End, encoded, event.EventID); e != nil {
			return result, e
		}
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
	if terminated && observation.CoverageComplete && terminalAt != nil {
		closed, err = retainedCoverageComplete(ctx, tx, id, templates, started, *terminalAt)
		if err != nil {
			return result, err
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

func retainedCoverageComplete(ctx context.Context, tx pgx.Tx, id uuid.UUID, templates map[string]*usage.Event, start, end time.Time) (bool, error) {
	var operation string
	if err := tx.QueryRow(ctx, `SELECT operation FROM olp.retained_settlements WHERE resource_id=$1`, id).Scan(&operation); err != nil {
		return false, err
	}
	var nativeMeta struct {
		CreatedAt  string `json:"created_at"`
		CreateTime string `json:"create_time"`
		FinishedAt string `json:"finished_at"`
		MemoryTier string `json:"memory_tier"`
	}
	var metadata []byte
	if err := tx.QueryRow(ctx, `SELECT metadata FROM olp.provider_resources WHERE id=$1`, id).Scan(&metadata); err != nil {
		return false, err
	}
	if json.Unmarshal(metadata, &nativeMeta) != nil {
		return false, ErrContract
	}
	for _, raw := range []string{nativeMeta.CreatedAt, nativeMeta.CreateTime} {
		if raw != "" {
			at, err := time.Parse(time.RFC3339Nano, raw)
			if err != nil {
				return false, ErrContract
			}
			start = at.UTC().Truncate(time.Microsecond)
		}
	}
	// terminal_at records when termination was first verified. A native finish
	// time supplied later defines the billing endpoint independently of that
	// observation, which may have happened well after the job actually ended.
	if (operation == "fine_tuning" || operation == "eval") && nativeMeta.FinishedAt != "" {
		finished, err := time.Parse(time.RFC3339Nano, nativeMeta.FinishedAt)
		if err != nil {
			return false, ErrContract
		}
		if finished.After(start) {
			end = finished.UTC().Truncate(time.Microsecond)
		}
	}
	component := ""
	switch operation {
	case "fine_tuning":
		component = "training"
	case "eval":
		component = "generation"
	case "context_cache":
		component = "cache_storage"
	case "vector_store":
		component = "storage"
	case "container":
		component = "container"
	default:
		return false, nil
	}
	for model := range templates {
		rows, err := tx.Query(ctx, `SELECT starts_at,ends_at FROM olp.retained_usage_coverage WHERE resource_id=$1 AND component=$2 AND model=$3 AND memory_tier=$4 ORDER BY starts_at,ends_at`, id, component, model, nativeMeta.MemoryTier)
		if err != nil {
			return false, err
		}
		cursor := start
		for rows.Next() {
			var a, b time.Time
			if err = rows.Scan(&a, &b); err != nil {
				rows.Close()
				return false, err
			}
			if a.After(cursor) {
				rows.Close()
				return false, nil
			}
			if b.After(cursor) {
				cursor = b
			}
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return false, err
		}
		if cursor.Before(end) {
			return false, nil
		}
	}
	return len(templates) > 0, nil
}

func (s *Store) RetainedStatus(ctx context.Context, id uuid.UUID) (map[string]any, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT to_jsonb(s)-'templates'-'claim_id'-'lease_until' FROM olp.retained_settlements s WHERE resource_id=$1`, id).Scan(&raw)
	if err != nil {
		return nil, err
	}
	var result map[string]any
	err = json.Unmarshal(raw, &result)
	return result, err
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
