package notifications

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
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
	kind                 deliveryKind
}

type deliveryKind interface{ sealedDeliveryKind() }

type budgetAlert struct {
	subjectKind, subjectID, windowKind string
	windowID                           int64
	threshold                          int32
	accrued, limit, currency           string
}

type keyReminder struct{ payload []byte }

type signalIncident struct {
	payload      []byte
	key, subject string
	number       int64
	resolved     bool
}

type storedPayload struct {
	payload  []byte
	key      string
	resolved bool
}

func (budgetAlert) sealedDeliveryKind()    {}
func (keyReminder) sealedDeliveryKind()    {}
func (signalIncident) sealedDeliveryKind() {}
func (storedPayload) sealedDeliveryKind()  {}

type pendingRow struct {
	id, ruleID, ruleName               string
	event                              contract.NotificationEvent
	attempts                           int
	subjectKind, subjectID, windowKind pgtype.Text
	windowID                           pgtype.Int8
	threshold                          pgtype.Int4
	accrued, limit                     pgtype.Text
	currency                           string
	payload                            []byte
	reminder                           bool
	incidentKey, subject               pgtype.Text
	incident                           pgtype.Int8
	resolved                           bool
}

func (r pendingRow) kind() deliveryKind {
	switch {
	case r.windowID.Valid:
		return budgetAlert{
			subjectKind: r.subjectKind.String, subjectID: r.subjectID.String, windowKind: r.windowKind.String,
			windowID: r.windowID.Int64, threshold: r.threshold.Int32,
			accrued: r.accrued.String, limit: r.limit.String, currency: r.currency,
		}
	case r.reminder:
		return keyReminder{payload: r.payload}
	case r.incident.Valid && r.subject.String != "":
		return signalIncident{payload: r.payload, key: r.incidentKey.String, subject: r.subject.String, number: r.incident.Int64, resolved: r.resolved}
	}
	return storedPayload{payload: r.payload, key: r.incidentKey.String, resolved: r.resolved}
}

func (d delivery) lockKey() string {
	switch k := d.kind.(type) {
	case signalIncident:
		return k.key
	case storedPayload:
		return k.key
	}
	return ""
}

func (d delivery) message() (message, error) {
	switch k := d.kind.(type) {
	case budgetAlert:
		body, err := json.Marshal(map[string]any{
			"event": d.event, "rule_id": d.ruleID, "rule_name": d.ruleName,
			"subject_kind": k.subjectKind, "subject_id": k.subjectID, "window_kind": k.windowKind,
			"window_id": k.windowID, "threshold_percent": k.threshold, "accrued": k.accrued,
			"limit": k.limit, "currency": k.currency, "delivery_id": d.id,
		})
		if err != nil {
			return message{}, err
		}
		return newMessage(body, d.event, d.ruleName, d.ruleID+":"+string(d.event)+":"+strconv.FormatInt(k.windowID, 10), false)
	case keyReminder:
		return d.payloadMessage(k.payload, d.id, false)
	case signalIncident:
		return d.payloadMessage(k.payload, k.key, k.resolved)
	case storedPayload:
		return d.payloadMessage(k.payload, cmp.Or(k.key, d.id), k.resolved)
	}
	return message{}, errors.New("delivery has no kind")
}

func (d delivery) payloadMessage(payload []byte, dedupKey string, resolved bool) (message, error) {
	var fields map[string]any
	if err := json.Unmarshal(payload, &fields); err != nil {
		return message{}, err
	}
	overlay := map[string]any{"event": d.event, "rule_id": d.ruleID, "rule_name": d.ruleName, "delivery_id": d.id}
	body, err := json.Marshal(maps.Collect(func(yield func(string, any) bool) {
		for _, source := range []map[string]any{fields, overlay} {
			for key, value := range source {
				if !yield(key, value) {
					return
				}
			}
		}
	}))
	if err != nil {
		return message{}, err
	}
	return newMessage(body, d.event, d.ruleName, dedupKey, resolved)
}

func (w *deliveryWorker) pending(ctx context.Context) ([]delivery, error) {
	rows, err := w.Pool.Query(ctx, pendingChannelDeliverySQL, maxDeliveryAttempts, deliveriesPerPass)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (delivery, error) {
		var r pendingRow
		err := row.Scan(&r.id, &r.ruleID, &r.event, &r.attempts, &r.ruleName,
			&r.subjectKind, &r.subjectID, &r.windowKind, &r.windowID, &r.threshold, &r.accrued, &r.limit, &r.currency,
			&r.payload, &r.reminder, &r.incidentKey, &r.subject, &r.incident, &r.resolved)
		return delivery{id: r.id, ruleID: r.ruleID, ruleName: r.ruleName, event: r.event, attempts: r.attempts, kind: r.kind()}, err
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
	switch k := d.kind.(type) {
	case keyReminder:
		return keyReminderCurrent(ctx, tx, d.id)
	case signalIncident:
		if k.resolved {
			return true, nil
		}
		var current bool
		err := tx.QueryRow(ctx, signalDeliveryCurrentSQL, d.ruleID, k.subject, k.number).Scan(&current)
		return current, err
	}
	return true, nil
}

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
