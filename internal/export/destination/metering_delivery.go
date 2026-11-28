package destination

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/domain/ids"
	"github.com/tyk-swe/olp/internal/domain/problem"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/platform/database"
)

type MeteringDeliveryOutcome = contract.MeteringDeliveryLookupOutcome

const (
	MeteringPending              = contract.MeteringDeliveryLookupOutcomePending
	MeteringRetrying             = contract.MeteringDeliveryLookupOutcomeRetrying
	MeteringAccepted             = contract.MeteringDeliveryLookupOutcomeAccepted
	MeteringDeliveryUnconfirmed  = contract.MeteringDeliveryLookupOutcomeDeliveryUnconfirmed
	MeteringExpiredBeforeDeliver = contract.MeteringDeliveryLookupOutcomeExpiredBeforeDelivery
	MeteringNoRetainedEvidence   = contract.MeteringDeliveryLookupOutcomeNoRetainedEvidence
)

type MeteringDelivery struct {
	SinkID        string                  `json:"export_sink_id"`
	AttemptID     string                  `json:"attempt_id"`
	Outcome       MeteringDeliveryOutcome `json:"outcome"`
	EventID       *string                 `json:"event_id"`
	LastErrorCode *string                 `json:"last_error_code"`
	Attempts      *int                    `json:"attempts"`
	OccurredAt    *time.Time              `json:"occurred_at"`
	QueuedAt      *time.Time              `json:"queued_at"`
	LastAttemptAt *time.Time              `json:"last_attempt_at"`
	SettledAt     *time.Time              `json:"settled_at"`
}

const (
	meteringSinkSQL = `SELECT project_id::text,format FROM olp.export_sinks WHERE id=$1`
	liveDeliverySQL = `SELECT r.id::text,r.project_id::text,r.occurred_at,r.queued_at,p.attempts,p.last_attempt_at,p.last_error_code,p.delivered_at,p.rejected_at,p.send_may_have_occurred
 FROM olp.export_records r JOIN olp.export_pending p ON p.record_id=r.id AND p.sink_id=$1
 WHERE r.stream='metering' AND r.source_id=$2`
	receiptDeliverySQL = `SELECT record_id::text,project_id::text,outcome,last_error_code,attempts,occurred_at,queued_at,last_attempt_at,settled_at
 FROM olp.metering_delivery_receipts WHERE sink_id=$1 AND attempt_id=$2`
)

func PendingOutcome(attempts int, deliveredAt, rejectedAt *time.Time, sendMayHaveOccurred bool) MeteringDeliveryOutcome {
	switch {
	case deliveredAt != nil:
		return MeteringAccepted
	case rejectedAt != nil && sendMayHaveOccurred:
		return MeteringDeliveryUnconfirmed
	case rejectedAt != nil:
		return MeteringExpiredBeforeDeliver
	case attempts == 0:
		return MeteringPending
	default:
		return MeteringRetrying
	}
}

func LookupMeteringDelivery(ctx context.Context, q database.Queryer, p authz.Principal, sinkID, attemptID string) (MeteringDelivery, error) {
	none := MeteringDelivery{SinkID: sinkID, AttemptID: attemptID, Outcome: MeteringNoRetainedEvidence}
	var sinkProject *string
	var format Format
	if err := q.QueryRow(ctx, meteringSinkSQL, sinkID).Scan(&sinkProject, &format); err != nil {
		return MeteringDelivery{}, database.NotFoundOr(err)
	}
	if err := p.Project(sinkProject, authz.View); err != nil {
		return MeteringDelivery{}, err
	}
	if format != contract.CreateExportSinkRequestFormatCloudevents {
		return MeteringDelivery{}, problem.NotFound
	}
	live, found, err := liveDelivery(ctx, q, sinkID, attemptID)
	if err != nil {
		return MeteringDelivery{}, err
	}
	if found {
		if !consumerVisible(p, live.projectID) {
			return none, nil
		}
		return live.delivery(sinkID, attemptID), nil
	}
	receipt, found, err := receiptDelivery(ctx, q, sinkID, attemptID)
	if err != nil {
		return MeteringDelivery{}, err
	}
	if !found || !consumerVisible(p, receipt.projectID) {
		return none, nil
	}
	return receipt.delivery(sinkID, attemptID), nil
}

func consumerVisible(p authz.Principal, projectID *string) bool {
	return projectID == nil || p.Project(projectID, authz.View) == nil
}

type liveRow struct {
	eventID       string
	projectID     *string
	occurredAt    time.Time
	queuedAt      time.Time
	attempts      int
	lastAttemptAt *time.Time
	lastErrorCode *string
	deliveredAt   *time.Time
	rejectedAt    *time.Time
	sendMay       bool
}

func liveDelivery(ctx context.Context, q database.Queryer, sinkID, attemptID string) (liveRow, bool, error) {
	var row liveRow
	err := q.QueryRow(ctx, liveDeliverySQL, sinkID, attemptID).Scan(&row.eventID, &row.projectID, &row.occurredAt, &row.queuedAt, &row.attempts, &row.lastAttemptAt, &row.lastErrorCode, &row.deliveredAt, &row.rejectedAt, &row.sendMay)
	if errors.Is(err, pgx.ErrNoRows) {
		return liveRow{}, false, nil
	}
	return row, err == nil, err
}

func (r liveRow) delivery(sinkID, attemptID string) MeteringDelivery {
	outcome := PendingOutcome(r.attempts, r.deliveredAt, r.rejectedAt, r.sendMay)
	return MeteringDelivery{
		SinkID: sinkID, AttemptID: attemptID, Outcome: outcome, EventID: &r.eventID,
		LastErrorCode: unlessAccepted(outcome, r.lastErrorCode), Attempts: &r.attempts, OccurredAt: &r.occurredAt,
		QueuedAt: &r.queuedAt, LastAttemptAt: r.lastAttemptAt, SettledAt: cmp.Or(r.deliveredAt, r.rejectedAt),
	}
}

func unlessAccepted(outcome MeteringDeliveryOutcome, code *string) *string {
	if outcome == MeteringAccepted {
		return nil
	}
	return code
}

type receiptRow struct {
	eventID       string
	projectID     *string
	outcome       MeteringDeliveryOutcome
	lastErrorCode *string
	attempts      int
	occurredAt    time.Time
	queuedAt      time.Time
	lastAttemptAt *time.Time
	settledAt     time.Time
}

func receiptDelivery(ctx context.Context, q database.Queryer, sinkID, attemptID string) (receiptRow, bool, error) {
	var row receiptRow
	err := q.QueryRow(ctx, receiptDeliverySQL, sinkID, attemptID).Scan(&row.eventID, &row.projectID, &row.outcome, &row.lastErrorCode, &row.attempts, &row.occurredAt, &row.queuedAt, &row.lastAttemptAt, &row.settledAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return receiptRow{}, false, nil
	}
	return row, err == nil, err
}

func (r receiptRow) delivery(sinkID, attemptID string) MeteringDelivery {
	return MeteringDelivery{
		SinkID: sinkID, AttemptID: attemptID, Outcome: r.outcome, EventID: &r.eventID,
		LastErrorCode: r.lastErrorCode, Attempts: &r.attempts, OccurredAt: &r.occurredAt,
		QueuedAt: &r.queuedAt, LastAttemptAt: r.lastAttemptAt, SettledAt: &r.settledAt,
	}
}

func (a *API) meteringDelivery(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	sinkID, err := httpkit.IDParam(r, "export_sink_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	attemptID, err := ids.Parse(r.PathValue("attempt_id"))
	if err != nil {
		return httpkit.Reply{}, problem.Invalid("attempt_id", "attempt_id must be a valid UUID")
	}
	delivery, err := LookupMeteringDelivery(r.Context(), a.server.Pool, p, sinkID, attemptID)
	if err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.OK(delivery), nil
}
