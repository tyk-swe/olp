package notifications

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/secrets"
)

const (
	cancelSupersededSQL = `UPDATE olp.notification_deliveries SET status='cancelled',last_error_code='superseded' WHERE id=$1`
	claimAttemptSQL     = `UPDATE olp.notification_deliveries SET attempts=attempts+1,last_attempt_at=now()
 WHERE id=$1 AND attempts=$2 AND attempts<$3 AND status IN ('pending','failed')`
	recordAttemptSQL = `UPDATE olp.notification_deliveries
 SET status=$2,last_error_code=$3,delivered_at=CASE WHEN $4 THEN now() ELSE delivered_at END WHERE id=$1`
	connectionCloseTimeout = 5 * time.Second
)

type delivery struct {
	id, ruleID, ruleName string
	event                contract.NotificationEvent
	attempts             int
	dedupKey             *string
	resolved             bool
	evidence             evidence
}

type evidence interface{ sealedEvidence() }

type budgetEvidence struct {
	subjectKind, subjectID, windowKind *string
	windowID                           *int64
	threshold                          *int
	accrued, limit                     *string
	currency                           string
}

type payloadEvidence struct {
	raw    []byte
	stored storedPayload
}

type storedPayload struct {
	incidentRef
	DedupKey string `json:"dedup_key"`
	Resolved bool   `json:"resolved"`
}

func (budgetEvidence) sealedEvidence()  {}
func (payloadEvidence) sealedEvidence() {}

func newPayloadEvidence(raw []byte) payloadEvidence {
	var stored storedPayload
	if json.Unmarshal(raw, &stored) != nil {
		return payloadEvidence{raw: raw}
	}
	return payloadEvidence{raw: raw, stored: stored}
}

func (d delivery) storedDedupKey() string {
	if d.dedupKey != nil {
		return *d.dedupKey
	}
	return ""
}

func (d delivery) lockKey() string {
	if p, ok := d.evidence.(payloadEvidence); ok {
		return cmp.Or(p.stored.Key, d.storedDedupKey())
	}
	return d.storedDedupKey()
}

func (d delivery) openIncident() (incidentRef, bool) {
	p, ok := d.evidence.(payloadEvidence)
	if !ok || d.resolved || p.stored.Subject == "" || p.stored.Number == nil {
		return incidentRef{}, false
	}
	return p.stored.incidentRef, true
}

func (d delivery) message() (message, error) {
	switch e := d.evidence.(type) {
	case budgetEvidence:
		body, err := json.Marshal(map[string]any{
			"event": d.event, "rule_id": d.ruleID, "rule_name": d.ruleName,
			"subject_kind": e.subjectKind, "subject_id": e.subjectID, "window_kind": e.windowKind,
			"window_id": e.windowID, "threshold_percent": e.threshold, "accrued": e.accrued,
			"limit": e.limit, "currency": e.currency, "delivery_id": d.id,
		})
		if err != nil {
			return message{}, err
		}
		dedup := d.id
		if e.windowID != nil {
			dedup = d.ruleID + ":" + string(d.event) + ":" + strconv.FormatInt(*e.windowID, 10)
		}
		return newMessage(body, d.event, d.ruleName, dedup, false)
	case payloadEvidence:
		var body map[string]any
		if err := json.Unmarshal(e.raw, &body); err != nil {
			return message{}, err
		}
		body["event"], body["rule_id"], body["rule_name"], body["delivery_id"] = d.event, d.ruleID, d.ruleName, d.id
		if d.resolved {
			body["resolved"] = true
		}
		if d.dedupKey != nil {
			body["dedup_key"] = *d.dedupKey
		}
		raw, err := json.Marshal(body)
		if err != nil {
			return message{}, err
		}
		dedup := cmp.Or(e.stored.Key, cmp.Or(d.storedDedupKey(), e.stored.DedupKey), d.id)
		return newMessage(raw, d.event, d.ruleName, dedup, d.resolved || e.stored.Resolved)
	}
	return message{}, errors.New("delivery has no evidence")
}

func (w *deliveryWorker) pending(ctx context.Context) ([]delivery, error) {
	rows, err := w.Pool.Query(ctx, pendingChannelDeliverySQL, maxDeliveryAttempts, deliveriesPerPass, contract.NotificationEventKeyExpiring)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (delivery, error) {
		var d delivery
		var budget budgetEvidence
		var payload []byte
		if err := row.Scan(&d.id, &d.ruleID, &d.event, &d.attempts, &d.ruleName,
			&budget.subjectKind, &budget.subjectID, &budget.windowKind, &budget.windowID, &budget.threshold,
			&budget.accrued, &budget.limit, &budget.currency, &payload, &d.dedupKey, &d.resolved); err != nil {
			return d, err
		}
		if d.event == contract.NotificationEventBudgetThreshold {
			d.evidence = budget
			return d, nil
		}
		d.evidence = newPayloadEvidence(payload)
		return d, nil
	})
}

type database interface {
	Begin(context.Context) (pgx.Tx, error)
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

func (w *deliveryWorker) deliver(ctx context.Context, d delivery) bool {
	key := d.lockKey()
	if key == "" {
		return w.attempt(ctx, w.Pool, d)
	}
	conn, locked := w.lockIncident(ctx, d.id, key)
	if !locked {
		return false
	}
	defer w.unlockIncident(ctx, conn, d.id, key)
	return w.attempt(ctx, conn, d)
}

func (w *deliveryWorker) lockIncident(ctx context.Context, id, key string) (*pgxpool.Conn, bool) {
	conn, err := w.Pool.Acquire(ctx)
	if err != nil {
		w.Log.Warn("notification incident connection failed", "delivery", id, "error", err)
		return nil, false
	}
	var locked bool
	if err := conn.QueryRow(ctx, lockNotificationIncidentSQL, key).Scan(&locked); err != nil {
		w.Log.Warn("notification incident lock failed", "delivery", id, "error", err)
		discard(ctx, conn)
		return nil, false
	}
	if !locked {
		conn.Release()
		return nil, false
	}
	return conn, true
}

func (w *deliveryWorker) unlockIncident(ctx context.Context, conn *pgxpool.Conn, id, key string) {
	unlock, done := context.WithTimeout(context.WithoutCancel(ctx), connectionCloseTimeout)
	defer done()
	if _, err := conn.Exec(unlock, unlockNotificationIncidentSQL, key); err != nil {
		w.Log.Warn("notification incident unlock failed", "delivery", id, "error", err)
		discard(ctx, conn)
		return
	}
	conn.Release()
}

func discard(ctx context.Context, conn *pgxpool.Conn) {
	closing, done := context.WithTimeout(context.WithoutCancel(ctx), connectionCloseTimeout)
	defer done()
	_ = conn.Hijack().Close(closing)
}

func (d delivery) current(ctx context.Context, tx pgx.Tx) (bool, error) {
	if d.event == contract.NotificationEventKeyExpiring {
		return keyReminderCurrent(ctx, tx, d.id)
	}
	incident, open := d.openIncident()
	if !open {
		return true, nil
	}
	var current bool
	err := tx.QueryRow(ctx, signalDeliveryCurrentSQL, d.ruleID, incident.Subject, *incident.Number).Scan(&current)
	return current, err
}

// Claim the enabled destination and attempt atomically, then commit before
// sending. A later disable cannot cancel an already claimed attempt.
func (w *deliveryWorker) attempt(ctx context.Context, db database, d delivery) bool {
	tx, err := db.Begin(ctx)
	if err != nil {
		w.Log.Warn("notification delivery claim failed", "delivery", d.id, "error", err)
		return false
	}
	defer tx.Rollback(ctx)
	current, err := d.current(ctx, tx)
	if err != nil {
		return false
	}
	if !current {
		w.cancelSuperseded(ctx, tx, d.id)
		return false
	}
	claimed, err := tx.Exec(ctx, claimAttemptSQL, d.id, d.attempts, maxDeliveryAttempts)
	if err != nil {
		w.Log.Warn("notification delivery claim failed", "delivery", d.id, "error", err)
		return false
	}
	if claimed.RowsAffected() == 0 {
		return false
	}
	target, secret, err := w.notificationDestination(ctx, tx, d.ruleID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false
	}
	if err != nil {
		w.Log.Warn("notification destination unavailable", "delivery", d.id, "error", err)
		return false
	}
	if err := tx.Commit(ctx); err != nil {
		w.Log.Warn("notification delivery claim failed", "delivery", d.id, "error", err)
		return false
	}
	code := w.send(ctx, d, target, secret)
	status, lastError := contract.NotificationDeliveryStatusDelivered, (*string)(nil)
	if code != "" {
		status, lastError = contract.NotificationDeliveryStatusFailed, &code
	}
	if _, err := db.Exec(ctx, recordAttemptSQL, d.id, status, lastError, code == ""); err != nil {
		w.Log.Warn("notification delivery update failed", "delivery", d.id, "error", err)
	}
	return true
}

func (w *deliveryWorker) send(ctx context.Context, d delivery, target destination, secret []byte) string {
	m, err := d.message()
	if err != nil {
		return "invalid_destination"
	}
	return sendErrorCode(w.Sender.send(ctx, target, secret, m))
}

func (w *deliveryWorker) cancelSuperseded(ctx context.Context, tx pgx.Tx, id string) {
	if _, err := tx.Exec(ctx, cancelSupersededSQL, id); err != nil {
		return
	}
	if err := tx.Commit(ctx); err != nil {
		w.Log.Warn("notification cancellation failed", "delivery", id, "error", err)
	}
}

// The rule and destination stay locked through the claim so rotations cannot
// delete the selected secret or redirect the claim. Disabled pairs return
// pgx.ErrNoRows.
func (w *deliveryWorker) notificationDestination(ctx context.Context, tx pgx.Tx, ruleID string) (destination, []byte, error) {
	var d destination
	var secretID *string
	if err := tx.QueryRow(ctx, readChannelDestinationSQL, ruleID).Scan(&d.URL, &secretID, &d.Type, &d.Configuration); err != nil {
		return d, nil, err
	}
	if secretID == nil {
		return d, nil, nil
	}
	secret, err := w.Keys.Read(ctx, tx, w.Installation, *secretID, secrets.NotificationSecret)
	return d, secret, err
}
