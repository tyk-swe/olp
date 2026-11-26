package resources

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/usage"
)

type coverageMetadata struct {
	CreatedAt  string   `json:"created_at"`
	CreateTime string   `json:"create_time"`
	FinishedAt string   `json:"finished_at"`
	ExpiresAt  string   `json:"expires_at"`
	Status     string   `json:"native_status"`
	MemoryTier string   `json:"memory_tier"`
	Models     []string `json:"authorized_models"`
}

func retainedCoverageComplete(ctx context.Context, tx pgx.Tx, id uuid.UUID, templates map[string]*usage.Event, start, end time.Time) (bool, error) {
	operation, meta, err := loadCoverageMeta(ctx, tx, id)
	if err != nil {
		return false, err
	}
	if len(templates) == 0 {
		return operation == "eval" && meta.Models != nil && len(meta.Models) == 0, nil
	}
	windowStart, err := resolveCoverageStart(meta, start)
	if err != nil {
		return false, err
	}
	windowEnd, err := resolveCoverageEnd(ctx, tx, id, operation, meta, windowStart, end)
	if err != nil {
		return false, err
	}
	component := retainedComponent(operation)
	if component == "" {
		return false, nil
	}
	for model := range templates {
		covered, err := retainedModelCovered(ctx, tx, id, component, model, meta.MemoryTier, windowStart, windowEnd)
		if err != nil || !covered {
			return false, err
		}
	}
	return true, nil
}

func loadCoverageMeta(ctx context.Context, tx pgx.Tx, id uuid.UUID) (string, coverageMetadata, error) {
	var operation string
	var metadata []byte
	err := tx.QueryRow(ctx, `SELECT s.operation,r.metadata FROM olp.retained_settlements s JOIN olp.provider_resources r ON r.id=s.resource_id WHERE s.resource_id=$1`, id).Scan(&operation, &metadata)
	if err != nil {
		return "", coverageMetadata{}, err
	}
	var meta coverageMetadata
	if json.Unmarshal(metadata, &meta) != nil {
		return "", coverageMetadata{}, ErrContract
	}
	return operation, meta, nil
}

func resolveCoverageStart(meta coverageMetadata, fallback time.Time) (time.Time, error) {
	start := fallback
	for _, raw := range []string{meta.CreatedAt, meta.CreateTime} {
		if raw == "" {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return time.Time{}, ErrContract
		}
		start = at.UTC().Truncate(time.Microsecond)
	}
	return start, nil
}

func resolveCoverageEnd(ctx context.Context, tx pgx.Tx, id uuid.UUID, operation string, meta coverageMetadata, start, fallback time.Time) (time.Time, error) {
	switch operation {
	case "vector_store":
		if meta.Status != "expired" || meta.ExpiresAt == "" {
			return fallback, nil
		}
		expiry, err := time.Parse(time.RFC3339Nano, meta.ExpiresAt)
		if err != nil || !expiry.After(start) {
			return time.Time{}, ErrContract
		}
		return expiry.UTC().Truncate(time.Microsecond), nil
	case "fine_tuning", "eval":
		if meta.FinishedAt != "" {
			finished, err := time.Parse(time.RFC3339Nano, meta.FinishedAt)
			if err != nil {
				return time.Time{}, ErrContract
			}
			if finished.After(start) {
				return finished.UTC().Truncate(time.Microsecond), nil
			}
			return fallback, nil
		}
		return latestCompleteEvidenceEnd(ctx, tx, id, start, fallback)
	default:
		return fallback, nil
	}
}

func latestCompleteEvidenceEnd(ctx context.Context, tx pgx.Tx, id uuid.UUID, start, fallback time.Time) (time.Time, error) {
	var finalEnd *time.Time
	err := tx.QueryRow(ctx, `SELECT max((line->>'end')::timestamptz) FROM olp.retained_usage_evidence e
CROSS JOIN LATERAL jsonb_array_elements(CASE WHEN jsonb_typeof(e.document->'lines')='array' THEN e.document->'lines' ELSE '[]'::jsonb END) line
WHERE e.resource_id=$1 AND e.document @> '{"coverage_complete":true}'::jsonb`, id).Scan(&finalEnd)
	if err != nil {
		return time.Time{}, err
	}
	if finalEnd != nil && finalEnd.After(start) && !finalEnd.After(fallback) {
		return *finalEnd, nil
	}
	return fallback, nil
}

func retainedModelCovered(ctx context.Context, tx pgx.Tx, id uuid.UUID, component, model, tier string, start, end time.Time) (bool, error) {
	rows, err := tx.Query(ctx, `SELECT starts_at,ends_at FROM olp.retained_usage_coverage WHERE resource_id=$1 AND component=$2 AND model=$3 AND memory_tier=$4 ORDER BY starts_at,ends_at`, id, component, model, tier)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	cursor := start
	for rows.Next() {
		var a, b time.Time
		if err = rows.Scan(&a, &b); err != nil {
			return false, err
		}
		if a.After(cursor) {
			return false, nil
		}
		if b.After(cursor) {
			cursor = b
		}
	}
	if err = rows.Err(); err != nil {
		return false, err
	}
	return !cursor.Before(end), nil
}
