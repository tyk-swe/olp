package resources

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type ListCursor struct {
	ID        string
	CreatedAt *time.Time
}

func (c ListCursor) localUUID(kinds []string) (uuid.UUID, error) {
	if c.ID == "" {
		return uuid.Nil, nil
	}
	kind, id, err := ParseLocalID(c.ID)
	if err != nil || !slices.Contains(kinds, kind) {
		return uuid.Nil, ErrNotFound
	}
	return id, nil
}

func (s *Store) ListKinds(ctx context.Context, kinds []string, apiKeyID string, limit int, afterID string) ([]*Resource, error) {
	return s.ListKindsPage(ctx, kinds, apiKeyID, limit, ListCursor{ID: afterID}, false)
}

func (s *Store) ListKindsPage(ctx context.Context, kinds []string, apiKeyID string, limit int, cursor ListCursor, ascending bool) ([]*Resource, error) {
	return s.listKindsOrdered(ctx, kinds, apiKeyID, limit, cursor, false, ascending)
}

func (s *Store) ListLifecycle(ctx context.Context, kind, apiKeyID string, limit int, afterID string) ([]*Resource, error) {
	return s.ListLifecycleOrdered(ctx, kind, apiKeyID, limit, afterID, false)
}

func (s *Store) ListLifecycleOrdered(ctx context.Context, kind, owner string, limit int, after string, ascending bool, route ...string) ([]*Resource, error) {
	return s.listKindsOrdered(ctx, []string{kind}, owner, limit, ListCursor{ID: after}, true, ascending, route...)
}

func (s *Store) listKindsOrdered(ctx context.Context, kinds []string, apiKeyID string, limit int, cursor ListCursor, deletedCursor, ascending bool, route ...string) ([]*Resource, error) {
	var routeSlug string
	if len(route) > 0 {
		routeSlug = route[0]
	}
	anchor, err := cursor.localUUID(kinds)
	if err != nil {
		return nil, err
	}
	comparison, direction := "<", "DESC"
	if ascending {
		comparison, direction = ">", "ASC"
	}
	rows, err := s.pool.Query(ctx, `SELECT `+columns+` FROM olp.provider_resources r
		WHERE kind=ANY($1) AND api_key_id=$2 AND state<>$3 AND (expires_at IS NULL OR expires_at>now())
		AND ($8='' OR route_slug=$8)
		AND NOT(kind=$7 AND state IN ('submitting','submission_unknown','not_accepted')
		 AND EXISTS(SELECT 1 FROM olp.retained_submissions s WHERE s.resource_id=r.id))
		AND ($4::uuid IS NULL OR (created_at,id)`+comparison+`(COALESCE($9::timestamptz, (SELECT created_at FROM olp.provider_resources
		 anchor WHERE id=$4 AND api_key_id=$2 AND kind=ANY($1) AND ($6 OR state<>$3) AND (expires_at IS NULL OR expires_at>now())
		 AND ($8='' OR route_slug=$8)
		 AND NOT(kind=$7 AND state IN ('submitting','submission_unknown','not_accepted')
		  AND EXISTS(SELECT 1 FROM olp.retained_submissions s WHERE s.resource_id=anchor.id)))), $4))
		ORDER BY created_at `+direction+`, id `+direction+` LIMIT $5`,
		kinds, apiKeyID, StateDeleted, nilUUID(anchor), limit, deletedCursor, KindFile, routeSlug, cursor.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("provider resource list: %w", err)
	}
	return scanResourceRows(rows, "provider resource list")
}

func (s *Store) ListCloudBatches(ctx context.Context, apiKeyID, dialect string, limit int, cursor ListCursor) ([]*Resource, error) {
	anchor, err := cursor.localUUID([]string{KindCloudBatch})
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT `+columns+` FROM olp.provider_resources
		WHERE kind=$1 AND api_key_id=$2 AND metadata->>'dialect'=$3 AND state<>$4
		AND (expires_at IS NULL OR expires_at>now())
		AND ($5::uuid IS NULL OR (created_at,id)<(COALESCE($7::timestamptz, (SELECT created_at FROM olp.provider_resources
			WHERE id=$5 AND api_key_id=$2 AND kind=$1 AND metadata->>'dialect'=$3 AND state<>$4
			AND (expires_at IS NULL OR expires_at>now()))), $5))
		ORDER BY created_at DESC,id DESC LIMIT $6`,
		KindCloudBatch, apiKeyID, dialect, StateDeleted, nilUUID(anchor), limit, cursor.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("cloud batch list: %w", err)
	}
	return scanResourceRows(rows, "cloud batch list")
}

func scanResourceRows(rows pgx.Rows, label string) ([]*Resource, error) {
	defer rows.Close()
	out := []*Resource{}
	for rows.Next() {
		r, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("%s scan: %w", label, err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func nilUUID(id uuid.UUID) any {
	if id == uuid.Nil {
		return nil
	}
	return id
}
