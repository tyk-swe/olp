package usage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
)

// Gateway epoch lifecycle states. An epoch is one gateway process's lifetime as
// a metadata producer: open while it checkpoints, gracefully closed when it
// drained, unresolved once it stopped without draining, and acknowledged once
// an operator has seen that loss.
const (
	EpochOpen             = "open"
	EpochGracefullyClosed = "gracefully_closed"
	EpochUnresolved       = "unresolved"
	EpochAcknowledged     = "acknowledged"
)

// GatewayEpoch is one process lifetime with the counters it last checkpointed.
type GatewayEpoch struct {
	GatewayInstance string    `json:"gateway_instance"`
	ProcessEpoch    string    `json:"process_epoch"`
	State           string    `json:"state"`
	StartedAt       time.Time `json:"started_at"`
	UpdatedAt       time.Time `json:"updated_at"`
	Accepted        int64     `json:"accepted"`
	Persisted       int64     `json:"persisted"`
	Dropped         int64     `json:"dropped"`
	Abandoned       int64     `json:"abandoned"`
	// UncertainEventLowerBound is what an unclean epoch still held when it was
	// last heard from: a floor on the events it may have lost, never a total.
	UncertainEventLowerBound int64      `json:"uncertain_event_lower_bound"`
	Retrying                 bool       `json:"retrying"`
	WriterClosed             bool       `json:"writer_closed"`
	GracefullyClosedAt       *time.Time `json:"gracefully_closed_at"`
	StaleDetectedAt          *time.Time `json:"stale_detected_at"`
	AcknowledgedAt           *time.Time `json:"acknowledged_at"`
	AcknowledgedBy           *string    `json:"acknowledged_by"`
	// UncertaintyGapID is the ingestion gap opened for the events this epoch
	// could not account for.
	UncertaintyGapID *string `json:"uncertainty_gap_id"`
}

// Acknowledgement records that an operator has seen an unclean epoch. It
// changes no completeness evidence; the gap it opened stays on the record.
type Acknowledgement struct {
	ProcessEpoch    string    `json:"process_epoch"`
	GatewayInstance string    `json:"gateway_instance"`
	AcknowledgedAt  time.Time `json:"acknowledged_at"`
	AcknowledgedBy  *string   `json:"acknowledged_by"`
}

const epochColumns = `SELECT gateway_instance, process_epoch::text, started_at, updated_at,
        accepted, persisted, dropped, abandoned, retrying, writer_closed, gracefully_closed_at,
        stale_detected_at, acknowledged_at, acknowledged_by::text, uncertainty_gap_id::text,
        CASE WHEN stale_detected_at IS NOT NULL
             THEN GREATEST(accepted - persisted - abandoned, 0) ELSE 0 END
    FROM olp_go.request_metadata_gateway_epochs WHERE true`

// ListGatewayEpochs pages process epochs by their last durable checkpoint. An
// unknown state filter is refused rather than ignored: a silently unfiltered
// page would read as "no unresolved epochs".
func ListGatewayEpochs(ctx context.Context, q access.Queryer, state *string, cursor *Cursor, limit int) ([]GatewayEpoch, *string, error) {
	if limit < 1 {
		limit = 1
	}
	if limit > 200 {
		limit = 200
	}
	var query filterQuery
	query.push(epochColumns)
	if state != nil {
		switch *state {
		case EpochOpen:
			query.push(" AND gracefully_closed_at IS NULL AND stale_detected_at IS NULL")
		case EpochGracefullyClosed:
			query.push(" AND gracefully_closed_at IS NOT NULL")
		case EpochUnresolved:
			query.push(" AND stale_detected_at IS NOT NULL AND acknowledged_at IS NULL")
		case EpochAcknowledged:
			query.push(" AND stale_detected_at IS NOT NULL AND acknowledged_at IS NOT NULL")
		default:
			return nil, nil, access.Fail(400, "invalid_state",
				"Use open, gracefully_closed, unresolved, or acknowledged.")
		}
	}
	if cursor != nil {
		query.push(" AND (updated_at, process_epoch) < (" + query.bind(cursor.At) + ", " + query.bind(cursor.ID) + ")")
	}
	query.push(" ORDER BY updated_at DESC, process_epoch DESC LIMIT " + query.bind(int64(limit)+1))
	rows, err := q.Query(ctx, query.sql(), query.args...)
	if err != nil {
		return nil, nil, fmt.Errorf("list gateway epochs: %w", err)
	}
	defer rows.Close()
	items := []GatewayEpoch{}
	for rows.Next() {
		var epoch GatewayEpoch
		if err = rows.Scan(&epoch.GatewayInstance, &epoch.ProcessEpoch, &epoch.StartedAt,
			&epoch.UpdatedAt, &epoch.Accepted, &epoch.Persisted, &epoch.Dropped, &epoch.Abandoned,
			&epoch.Retrying, &epoch.WriterClosed, &epoch.GracefullyClosedAt, &epoch.StaleDetectedAt,
			&epoch.AcknowledgedAt, &epoch.AcknowledgedBy, &epoch.UncertaintyGapID,
			&epoch.UncertainEventLowerBound); err != nil {
			return nil, nil, fmt.Errorf("list gateway epochs: %w", err)
		}
		if epoch.Accepted < 0 || epoch.Persisted < 0 || epoch.Dropped < 0 || epoch.Abandoned < 0 ||
			epoch.UncertainEventLowerBound < 0 {
			return nil, nil, errors.New("stored gateway epoch counters are invalid")
		}
		epoch.State = epochState(epoch)
		epoch.normalize()
		items = append(items, epoch)
	}
	if err = rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("list gateway epochs: %w", err)
	}
	var next *string
	if len(items) > limit {
		items = items[:limit]
		last := items[len(items)-1]
		token := EncodeCursor(last.UpdatedAt, last.ProcessEpoch)
		next = &token
	}
	return items, next, nil
}

// epochState classifies a row. A gracefully closed epoch is closed whatever
// else is recorded against it, and an acknowledgement only resolves an epoch
// that was actually detected as stale.
func epochState(e GatewayEpoch) string {
	switch {
	case e.GracefullyClosedAt != nil:
		return EpochGracefullyClosed
	case e.StaleDetectedAt != nil && e.AcknowledgedAt != nil:
		return EpochAcknowledged
	case e.StaleDetectedAt != nil:
		return EpochUnresolved
	default:
		return EpochOpen
	}
}

func (e *GatewayEpoch) normalize() {
	e.StartedAt, e.UpdatedAt = e.StartedAt.UTC(), e.UpdatedAt.UTC()
	for _, at := range []**time.Time{&e.GracefullyClosedAt, &e.StaleDetectedAt, &e.AcknowledgedAt} {
		if *at != nil {
			utc := (*at).UTC()
			*at = &utc
		}
	}
}

// AcknowledgeGatewayEpoch marks one unclean epoch as seen. Only a stale-detected
// epoch can be acknowledged, and acknowledging twice returns the first
// acknowledgement rather than restamping it. A nil result means there is no such
// unclean epoch, which the HTTP layer renders as a 404.
func AcknowledgeGatewayEpoch(ctx context.Context, tx pgx.Tx, processEpoch, actor string) (*Acknowledgement, error) {
	var instance string
	var acknowledgedAt *time.Time
	var acknowledgedBy *string
	err := tx.QueryRow(ctx, `SELECT gateway_instance, acknowledged_at, acknowledged_by::text
            FROM olp_go.request_metadata_gateway_epochs
            WHERE process_epoch = $1 AND stale_detected_at IS NOT NULL FOR UPDATE`, processEpoch).
		Scan(&instance, &acknowledgedAt, &acknowledgedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read gateway epoch: %w", err)
	}
	if acknowledgedAt != nil {
		return &Acknowledgement{ProcessEpoch: processEpoch, GatewayInstance: instance,
			AcknowledgedAt: acknowledgedAt.UTC(), AcknowledgedBy: acknowledgedBy}, nil
	}
	// The stored acknowledgement never precedes detection, so the row cannot
	// claim an operator saw the loss before it was recorded.
	var stamped time.Time
	if err = tx.QueryRow(ctx, `UPDATE olp_go.request_metadata_gateway_epochs
            SET acknowledged_at = GREATEST($1, stale_detected_at), acknowledged_by = $2
            WHERE process_epoch = $3 AND acknowledged_at IS NULL
            RETURNING acknowledged_at, acknowledged_by::text`,
		time.Now().UTC(), actor, processEpoch).Scan(&stamped, &acknowledgedBy); err != nil {
		return nil, fmt.Errorf("acknowledge gateway epoch: %w", err)
	}
	return &Acknowledgement{ProcessEpoch: processEpoch, GatewayInstance: instance,
		AcknowledgedAt: stamped.UTC(), AcknowledgedBy: acknowledgedBy}, nil
}
