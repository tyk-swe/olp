package resources

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/tyk-swe/olp/internal/usage"
	"github.com/tyk-swe/olp/internal/usage/ingest"
)

func normalizeRetainedEvidence(line RetainedEvidenceLine) (RetainedEvidenceLine, error) {
	if line.Model == "" || len(line.Model) > 512 {
		return RetainedEvidenceLine{}, ErrContract
	}
	start, end, err := validateEvidenceInterval(line.Start, line.End)
	if err != nil {
		return RetainedEvidenceLine{}, err
	}
	if err := validateEvidenceComponent(line.Component, line.MemoryTier, line.Usage); err != nil {
		return RetainedEvidenceLine{}, err
	}
	cloned := usage.AttemptUsage{
		UsageQuantities: cloneUsageQuantities(line.Usage.UsageQuantities),
		Evidence:        usage.EvidenceObservedComplete,
		Deferred:        true,
	}
	if cloned.StorageByteSeconds != nil {
		normalized, err := normalizeExactQuantity(*cloned.StorageByteSeconds)
		if err != nil {
			return RetainedEvidenceLine{}, err
		}
		cloned.StorageByteSeconds = &normalized
	}
	if cloned.CacheTokenSeconds != nil {
		normalized, err := normalizeExactQuantity(*cloned.CacheTokenSeconds)
		if err != nil {
			return RetainedEvidenceLine{}, err
		}
		cloned.CacheTokenSeconds = &normalized
	}
	if !usage.ConsistentUsage(&cloned) {
		return RetainedEvidenceLine{}, ErrContract
	}
	return RetainedEvidenceLine{
		Component:  line.Component,
		Model:      line.Model,
		MemoryTier: line.MemoryTier,
		Start:      start,
		End:        end,
		Usage:      cloned,
	}, nil
}

func validateEvidenceInterval(start, end time.Time) (time.Time, time.Time, error) {
	if start.IsZero() || end.IsZero() || !end.After(start) {
		return time.Time{}, time.Time{}, ErrContract
	}
	_, startZone := start.Zone()
	_, endZone := end.Zone()
	if startZone != 0 || endZone != 0 {
		return time.Time{}, time.Time{}, ErrContract
	}
	s := start.UTC().Truncate(time.Microsecond)
	e := end.UTC().Truncate(time.Microsecond)
	if !e.After(s) {
		return time.Time{}, time.Time{}, ErrContract
	}
	return s, e, nil
}

func validateEvidenceComponent(component, tier string, u usage.AttemptUsage) error {
	if u.MediaUnits != nil || u.Batch {
		return ErrContract
	}
	if component != "generation" && u.InvocationCount != nil {
		return ErrContract
	}
	if component != "container" && tier != "" {
		return ErrContract
	}
	active, ok := activeEvidenceComponent(u)
	if !ok || active != component {
		return ErrContract
	}
	return validateComponentQuantities(component, tier, u)
}

func validateComponentQuantities(component, tier string, u usage.AttemptUsage) error {
	switch component {
	case "training":
		if *u.TrainingTokens < 0 {
			return ErrContract
		}
		return nil
	case "storage", "cache_storage":
		return nil
	case "container":
		return validateContainerUsage(tier, u)
	case "generation":
		return validateGenerationUsage(u)
	default:
		return ErrContract
	}
}

func activeEvidenceComponent(u usage.AttemptUsage) (string, bool) {
	dims := []struct {
		name    string
		present bool
	}{
		{"training", u.TrainingTokens != nil},
		{"storage", u.StorageByteSeconds != nil},
		{"cache_storage", u.CacheTokenSeconds != nil},
		{"container", len(u.ContainerSessions) > 0},
		{"generation", u.InputTokens != nil || u.OutputTokens != nil || len(u.ToolCalls) > 0},
	}
	active := ""
	for _, dim := range dims {
		if !dim.present {
			continue
		}
		if active != "" {
			return "", false
		}
		active = dim.name
	}
	return active, active != ""
}

func validateContainerUsage(tier string, u usage.AttemptUsage) error {
	count, ok := u.ContainerSessions[tier]
	if tier == "" || len(u.ContainerSessions) != 1 || !ok || count < 0 {
		return ErrContract
	}
	return nil
}

func validateGenerationUsage(u usage.AttemptUsage) error {
	if u.InputTokens == nil || u.OutputTokens == nil || u.CachedInputTokens == nil || u.InvocationCount == nil {
		return ErrContract
	}
	return nil
}

func clonePtr[T any](v *T) *T {
	if v == nil {
		return nil
	}
	copyVal := *v
	return &copyVal
}

func cloneUsageQuantities(q usage.UsageQuantities) usage.UsageQuantities {
	return usage.UsageQuantities{
		InvocationCount:         clonePtr(q.InvocationCount),
		TrainingTokens:          clonePtr(q.TrainingTokens),
		StorageByteSeconds:      clonePtr(q.StorageByteSeconds),
		CacheTokenSeconds:       clonePtr(q.CacheTokenSeconds),
		ContainerSessions:       maps.Clone(q.ContainerSessions),
		InputTokens:             clonePtr(q.InputTokens),
		OutputTokens:            clonePtr(q.OutputTokens),
		CachedInputTokens:       clonePtr(q.CachedInputTokens),
		CacheWriteInputTokens:   clonePtr(q.CacheWriteInputTokens),
		CacheWrite5MInputTokens: clonePtr(q.CacheWrite5MInputTokens),
		CacheWrite1HInputTokens: clonePtr(q.CacheWrite1HInputTokens),
		AudioInputTokens:        clonePtr(q.AudioInputTokens),
		CachedAudioInputTokens:  clonePtr(q.CachedAudioInputTokens),
		AudioOutputTokens:       clonePtr(q.AudioOutputTokens),
		ImageInputTokens:        clonePtr(q.ImageInputTokens),
		CachedImageInputTokens:  clonePtr(q.CachedImageInputTokens),
		ImageOutputTokens:       clonePtr(q.ImageOutputTokens),
		MediaUnits:              clonePtr(q.MediaUnits),
		ToolCalls:               maps.Clone(q.ToolCalls),
		Batch:                   q.Batch,
	}
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

func compareEvidenceLines(a, b RetainedEvidenceLine) int {
	return cmp.Or(
		cmp.Compare(a.Component, b.Component),
		cmp.Compare(a.Model, b.Model),
		cmp.Compare(a.MemoryTier, b.MemoryTier),
		a.Start.Compare(b.Start),
		a.End.Compare(b.End),
	)
}

func normalizeRetainedLines(lines []RetainedEvidenceLine, component, tier string, templates map[string]*usage.Event) ([]RetainedEvidenceLine, error) {
	normalized := make([]RetainedEvidenceLine, len(lines))
	for i, line := range lines {
		if line.Component != component || (line.Component == "container" && line.MemoryTier != tier) {
			return nil, ErrContract
		}
		item, err := normalizeRetainedEvidence(line)
		if err != nil {
			return nil, err
		}
		if templates[item.Model] == nil {
			return nil, ErrContract
		}
		normalized[i] = item
	}
	slices.SortFunc(normalized, compareEvidenceLines)
	return normalized, nil
}

func evidenceDigest(v any) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func retainedObservationDigest(o RetainedObservation) (string, error) {
	canonical := RetainedObservation{
		Lines:            slices.Clone(o.Lines),
		RemoteTerminated: o.RemoteTerminated,
		CoverageComplete: o.CoverageComplete,
	}
	for i, line := range canonical.Lines {
		if normalized, err := normalizeRetainedEvidence(line); err == nil {
			canonical.Lines[i] = normalized
		}
	}
	slices.SortFunc(canonical.Lines, compareEvidenceLines)
	return evidenceDigest(canonical)
}

func sameJSON(a, b []byte) bool {
	var x, y any
	if decodeExactJSON(a, &x) != nil || decodeExactJSON(b, &y) != nil {
		return false
	}
	xx, errX := json.Marshal(x)
	yy, errY := json.Marshal(y)
	if errX != nil || errY != nil {
		return false
	}
	return bytes.Equal(xx, yy)
}

func decodeExactJSON(data []byte, out any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	return decoder.Decode(out)
}

func persistRetainedEvidence(ctx context.Context, tx pgx.Tx, sub EvidenceSubmission, previousState string, templates map[string]*usage.Event) (ingest.Persisted, error) {
	result := ingest.Persisted{Outcome: ingest.PersistOutcomeDuplicate}
	documentObservation := sub.Observation
	documentObservation.ObservedAt = time.Time{}
	documentObservation.RemoteTerminated = false
	document, err := json.Marshal(documentObservation)
	if err != nil {
		return result, err
	}
	exists, err := checkExistingEvidence(ctx, tx, sub.ResourceID, sub.Key, sub.Digest, document)
	if err != nil || exists {
		return result, err
	}
	evidenceID, err := newUUIDv7()
	if err != nil {
		return result, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO olp.retained_usage_evidence(id,resource_id,idempotency_key,digest,provenance,actor,document,remote_termination_verified,verified_terminal_at) VALUES($1,$2,$3,$4,$5,NULLIF($6,''),$7,$8,$9)`, evidenceID, sub.ResourceID, sub.Key, sub.Digest, sub.Provenance, sub.Actor, document, sub.Observation.RemoteTerminated, verifiedTerminalTime(sub.Observation)); err != nil {
		return result, err
	}
	for _, line := range sub.Observation.Lines {
		persisted, inserted, err := persistEvidenceLine(ctx, tx, sub.ResourceID, evidenceID, previousState, templates[line.Model], line)
		if err != nil {
			return result, err
		}
		if inserted {
			result = persisted
		}
	}
	return result, nil
}

func checkExistingEvidence(ctx context.Context, tx pgx.Tx, id uuid.UUID, key, digest string, document []byte) (bool, error) {
	var priorDigest string
	var prior []byte
	err := tx.QueryRow(ctx, `SELECT digest,document FROM olp.retained_usage_evidence WHERE resource_id=$1 AND idempotency_key=$2`, id, key).Scan(&priorDigest, &prior)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if priorDigest != digest || !sameJSON(prior, document) {
		return false, ErrEvidenceConflict
	}
	return true, nil
}

func persistEvidenceLine(ctx context.Context, tx pgx.Tx, resourceID, evidenceID uuid.UUID, previousState string, template *usage.Event, line RetainedEvidenceLine) (ingest.Persisted, bool, error) {
	encoded, err := json.Marshal(line.Usage)
	if err != nil {
		return ingest.Persisted{}, false, err
	}
	duplicate, err := checkCoverageOverlap(ctx, tx, resourceID, line, encoded)
	if err != nil || duplicate {
		return ingest.Persisted{}, false, err
	}
	if previousState == "closed" {
		return ingest.Persisted{}, false, ErrEvidenceConflict
	}
	event, err := buildRetainedUsageEvent(resourceID, template, line)
	if err != nil {
		return ingest.Persisted{}, false, err
	}
	accepted, err := usage.NewAccountingEvent(event.WithFinalUsage(line.Usage))
	if err != nil {
		return ingest.Persisted{}, false, err
	}
	persisted, err := ingest.PersistRetainedEventTx(ctx, tx, accepted)
	if err != nil {
		return ingest.Persisted{}, false, err
	}
	if persisted.Outcome == ingest.PersistOutcomeRejectedOutsideReplayWindow {
		return ingest.Persisted{}, false, ErrContract
	}
	if _, err = tx.Exec(ctx, `INSERT INTO olp.retained_usage_coverage(resource_id,evidence_id,component,model,memory_tier,starts_at,ends_at,quantities,event_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, resourceID, evidenceID, line.Component, line.Model, line.MemoryTier, line.Start, line.End, encoded, event.EventID); err != nil {
		return ingest.Persisted{}, false, err
	}
	return persisted, true, nil
}

func checkCoverageOverlap(ctx context.Context, tx pgx.Tx, resourceID uuid.UUID, line RetainedEvidenceLine, encoded []byte) (bool, error) {
	rows, err := tx.Query(ctx, `SELECT starts_at,ends_at,quantities FROM olp.retained_usage_coverage WHERE resource_id=$1 AND component=$2 AND model=$3 AND memory_tier=$4 AND starts_at<$6 AND ends_at>$5`, resourceID, line.Component, line.Model, line.MemoryTier, line.Start, line.End)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	duplicate := false
	for rows.Next() {
		var start, end time.Time
		var quantities []byte
		if err := rows.Scan(&start, &end, &quantities); err != nil {
			return false, err
		}
		if !start.Equal(line.Start) || !end.Equal(line.End) || !sameJSON(quantities, encoded) {
			return false, ErrEvidenceConflict
		}
		duplicate = true
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return duplicate, nil
}

func buildRetainedUsageEvent(resourceID uuid.UUID, template *usage.Event, line RetainedEvidenceLine) (*usage.Event, error) {
	raw, err := json.Marshal(template)
	if err != nil {
		return nil, err
	}
	event, err := usage.Decode(raw)
	if err != nil {
		return nil, err
	}
	identity := resourceID.String() + ":" + line.Component + ":" + line.Model + ":" + line.MemoryTier + ":" + line.Start.Format(time.RFC3339Nano) + ":" + line.End.Format(time.RFC3339Nano)
	event.RequestID = uuid.NewSHA1(uuid.NameSpaceOID, []byte(identity)).String()
	event.EventID = event.RequestID
	now := time.Now().UTC()
	event.ObservedAt = now
	event.RequestStartedAt = now
	event.RequestCompletedAt = now
	event.HTTPOutcomePending = false
	event.Attempts[0].ID = uuid.NewSHA1(uuid.NameSpaceOID, []byte(identity+":attempt")).String()
	// Keep the accepted pricing timestamp, including time-window tariffs.
	event.Attempts[0].CompletedAt = event.Attempts[0].StartedAt
	return event, nil
}
