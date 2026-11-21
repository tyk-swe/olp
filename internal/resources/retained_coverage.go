package resources

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/usage"
)

func retainedCoverageComplete(ctx context.Context, tx pgx.Tx, id uuid.UUID, templates map[string]*usage.Event, start, end time.Time) (bool, error) {
	var operation string
	if err := tx.QueryRow(ctx, `SELECT operation FROM olp.retained_settlements WHERE resource_id=$1`, id).Scan(&operation); err != nil {
		return false, err
	}
	var nativeMeta struct {
		CreatedAt  string   `json:"created_at"`
		CreateTime string   `json:"create_time"`
		FinishedAt string   `json:"finished_at"`
		ExpiresAt  string   `json:"expires_at"`
		Status     string   `json:"native_status"`
		MemoryTier string   `json:"memory_tier"`
		Models     []string `json:"authorized_models"`
	}
	var metadata []byte
	if err := tx.QueryRow(ctx, `SELECT metadata FROM olp.provider_resources WHERE id=$1`, id).Scan(&metadata); err != nil {
		return false, err
	}
	if json.Unmarshal(metadata, &nativeMeta) != nil {
		return false, ErrContract
	}
	if len(templates) == 0 {
		return operation == "eval" && nativeMeta.Models != nil && len(nativeMeta.Models) == 0, nil
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
	if operation == "vector_store" && nativeMeta.Status == "expired" && nativeMeta.ExpiresAt != "" {
		expiry, err := time.Parse(time.RFC3339Nano, nativeMeta.ExpiresAt)
		if err != nil || !expiry.After(start) {
			return false, ErrContract
		}
		end = expiry.UTC().Truncate(time.Microsecond)
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
	} else if operation == "fine_tuning" || operation == "eval" {
		// A final evidence statement can arrive before the remote termination
		// check. Its authoritative interval ends independently of that later
		// check; the whole interval must still be covered for every model.
		var finalEnd *time.Time
		if err := tx.QueryRow(ctx, `SELECT max((line->>'end')::timestamptz) FROM olp.retained_usage_evidence e
CROSS JOIN LATERAL jsonb_array_elements(CASE WHEN jsonb_typeof(e.document->'lines')='array' THEN e.document->'lines' ELSE '[]'::jsonb END) line
WHERE e.resource_id=$1 AND e.document @> '{"coverage_complete":true}'::jsonb`, id).Scan(&finalEnd); err != nil {
			return false, err
		}
		if finalEnd != nil && finalEnd.After(start) && !finalEnd.After(end) {
			end = *finalEnd
		}
	}
	component := retainedComponent(operation)
	if component == "" {
		return false, nil
	}
	for model := range templates {
		covered, err := retainedModelCovered(ctx, tx, id, component, model, nativeMeta.MemoryTier, start, end)
		if err != nil {
			return false, err
		}
		if !covered {
			return false, nil
		}
	}
	return len(templates) > 0, nil
}

func retainedModelCovered(ctx context.Context, tx pgx.Tx, id uuid.UUID, component, model, tier string, start, end time.Time) (bool, error) {
	rows, err := tx.Query(ctx, `SELECT starts_at,ends_at FROM olp.retained_usage_coverage WHERE resource_id=$1 AND component=$2 AND model=$3 AND memory_tier=$4 ORDER BY starts_at,ends_at`, id, component, model, tier)
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
	return !cursor.Before(end), nil
}
