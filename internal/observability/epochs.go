package observability

import (
	"context"
	"errors"
	"fmt"

	"github.com/tyk-swe/olp/internal/access"
)

// EpochHealth is the gateway-epoch completeness picture readiness reports.
type EpochHealth struct {
	OpenEpochs                int64
	UnresolvedEpochs          int64
	HistoricalUncertainGaps   int64
	UnresolvedEventLowerBound int64
}

// ReadEpochHealth summarizes gateway epochs and retained uncertainty gaps.
func ReadEpochHealth(ctx context.Context, q access.Queryer) (EpochHealth, error) {
	var health EpochHealth
	err := q.QueryRow(ctx, `SELECT COUNT(*) FILTER (WHERE gracefully_closed_at IS NULL
				AND stale_detected_at IS NULL)::bigint,
			COUNT(*) FILTER (WHERE stale_detected_at IS NOT NULL
				AND acknowledged_at IS NULL)::bigint,
			COALESCE(sum(GREATEST(accepted - persisted - abandoned, 0))
				FILTER (WHERE stale_detected_at IS NOT NULL
					AND acknowledged_at IS NULL), 0)::bigint,
			((SELECT COUNT(*) FROM olp.request_metadata_ingestion_gaps
				WHERE certainty = 'lower_bound')
				+ (SELECT COALESCE(sum(uncertain_gap_count), 0) FROM olp.request_metadata_gap_hourly))::bigint
		FROM olp.request_metadata_gateway_epochs`).Scan(
		&health.OpenEpochs, &health.UnresolvedEpochs, &health.UnresolvedEventLowerBound, &health.HistoricalUncertainGaps)
	if err != nil {
		return EpochHealth{}, fmt.Errorf("read gateway epoch health: %w", err)
	}
	if health.UnresolvedEventLowerBound < 0 || health.HistoricalUncertainGaps < 0 {
		return EpochHealth{}, errors.New("stored request metadata gap count is invalid")
	}
	return health, nil
}
