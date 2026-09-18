package observability

import (
	"context"
	"errors"
	"fmt"
	"math/big"

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
// Negative or non-integral counters are corrupt storage and fail closed.
func ReadEpochHealth(ctx context.Context, q access.Queryer) (EpochHealth, error) {
	var health EpochHealth
	var unresolvedLower, historical any
	err := q.QueryRow(ctx, `SELECT COUNT(*) FILTER (WHERE gracefully_closed_at IS NULL
				AND stale_detected_at IS NULL)::bigint,
			COUNT(*) FILTER (WHERE stale_detected_at IS NOT NULL
				AND acknowledged_at IS NULL)::bigint,
			COALESCE(sum(GREATEST(accepted - persisted - abandoned, 0))
				FILTER (WHERE stale_detected_at IS NOT NULL
					AND acknowledged_at IS NULL), 0)::bigint,
			((SELECT COUNT(*) FROM olp_go.request_metadata_ingestion_gaps
				WHERE certainty = 'lower_bound')
				+ (SELECT COALESCE(sum(uncertain_gap_count), 0) FROM olp_go.request_metadata_gap_hourly))::bigint
		FROM olp_go.request_metadata_gateway_epochs`).Scan(
		&health.OpenEpochs, &health.UnresolvedEpochs, &unresolvedLower, &historical)
	if err != nil {
		return health, fmt.Errorf("read gateway epoch health: %w", err)
	}
	lower, err := gapCount(unresolvedLower)
	if err != nil {
		return health, err
	}
	gaps, err := gapCount(historical)
	if err != nil {
		return health, err
	}
	health.UnresolvedEventLowerBound = lower
	health.HistoricalUncertainGaps = gaps
	return health, nil
}

// gapCount converts a SQL numeric sum into a non-negative int64, rejecting
// anything else as corrupt stored state.
func gapCount(value any) (int64, error) {
	switch v := value.(type) {
	case int64:
		if v < 0 {
			return 0, errors.New("stored request metadata gap count is invalid")
		}
		return v, nil
	case []byte:
		i, ok := new(big.Int).SetString(string(v), 10)
		if !ok || !i.IsInt64() || i.Sign() < 0 {
			return 0, errors.New("stored request metadata gap count is invalid")
		}
		return i.Int64(), nil
	case string:
		i, ok := new(big.Int).SetString(v, 10)
		if !ok || !i.IsInt64() || i.Sign() < 0 {
			return 0, errors.New("stored request metadata gap count is invalid")
		}
		return i.Int64(), nil
	}
	return 0, errors.New("stored request metadata gap count is invalid")
}
