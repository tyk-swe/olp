package sinks

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/usage"
)

const deliveryTimeout = 5 * time.Second

// Worker claims durable deliveries with expiring leases. Network calls never
// hold a database transaction. The recipient's event-id idempotency handles an
// uncertain HTTP response or a crash between acceptance and acknowledgement.
type Worker struct {
	Pool         *pgxpool.Pool
	Keys         *secrets.KeyRing
	Installation string
	Egress       *egress.Policy
	clientOnce   sync.Once
	client       *http.Client
}
type delivery struct {
	id, sink, event, stream, destination, lease string
	credential                                  *string
	payload                                     json.RawMessage
	created                                     time.Time
	attempts                                    int
}

func (w *Worker) Run(ctx context.Context, log *slog.Logger) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		pass, cancel := context.WithTimeout(ctx, time.Minute)
		progress, err := w.Deliver(pass)
		cancel()
		if ctx.Err() != nil {
			return
		}
		outcome := usage.OutcomeSuccess
		if err != nil {
			outcome = usage.OutcomeFailure
			log.Warn("export delivery pass failed", "error_type", "export_delivery")
		}
		if err = usage.CheckpointTask(ctx, w.Pool, usage.TaskExportDelivery, outcome, progress); err != nil {
			log.Warn("export delivery checkpoint failed", "error_type", "checkpoint")
		}
		// Drain healthy backlog through additional bounded passes; the ticker
		// governs idle polling, rather than limiting a busy worker's throughput.
		if outcome == usage.OutcomeSuccess && progress {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Deliver performs a bounded pass; failures retain the same event and schedule
// another attempt. Expired payloads are counted and removed independently of
// source fact retention.
func (w *Worker) Deliver(ctx context.Context) (bool, error) {
	if err := w.expire(ctx); err != nil {
		return false, err
	}
	progress := false
	for range 16 {
		d, err := w.claim(ctx)
		if errors.Is(err, pgx.ErrNoRows) {
			return progress, nil
		}
		if err != nil {
			return progress, err
		}
		code := w.send(ctx, d)
		if ctx.Err() != nil {
			return progress, ctx.Err()
		}
		if err = w.settle(ctx, d, code); err != nil {
			return progress, err
		}
		progress = true
	}
	return progress, nil
}

func (w *Worker) expire(ctx context.Context) error {
	tx, err := w.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// Mutations and settlement lock the parent before delivery rows. Expiry
	// follows the same order, so retirement cannot form a lock inversion.
	rows, err := tx.Query(ctx, "SELECT id FROM olp.export_sinks ORDER BY id FOR NO KEY UPDATE")
	if err != nil {
		return err
	}
	for rows.Next() {
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `WITH expired AS (
	 DELETE FROM olp.export_deliveries WHERE id IN (
	 SELECT id FROM olp.export_deliveries WHERE expires_at<=now() ORDER BY expires_at LIMIT 1000 FOR UPDATE SKIP LOCKED)
	 RETURNING sink_id,status)
	 UPDATE olp.export_sinks s SET expired_total=expired_total+x.n FROM
	 (SELECT sink_id,count(*) n FROM expired WHERE status='pending' GROUP BY sink_id) x WHERE s.id=x.sink_id`); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (w *Worker) settle(ctx context.Context, d *delivery, code string) error {
	tx, err := w.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT id FROM olp.export_sinks WHERE id=$1 FOR NO KEY UPDATE", d.sink); err != nil {
		return err
	}
	status := "pending"
	if code == "" {
		status = "delivered"
	}
	delay := time.Second * time.Duration(1<<min(d.attempts, 10))
	result, err := tx.Exec(ctx, "UPDATE olp.export_deliveries SET status=$3,lease=NULL,last_error_code=NULLIF($4,''),next_attempt_at=now()+$5::interval WHERE id=$1 AND lease=$2 AND status='pending'", d.id, d.lease, status, code, delay.String())
	if err == nil && result.RowsAffected() == 1 {
		if code == "" {
			_, err = tx.Exec(ctx, "UPDATE olp.export_sinks SET delivered_total=delivered_total+1,last_delivered_at=now() WHERE id=$1", d.sink)
		} else {
			_, err = tx.Exec(ctx, "UPDATE olp.export_sinks SET failed_total=failed_total+1 WHERE id=$1", d.sink)
		}
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		return err
	}
	return nil
}
func (w *Worker) claim(ctx context.Context) (*delivery, error) {
	tx, err := w.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	d := &delivery{lease: access.NewID()}
	err = tx.QueryRow(ctx, `SELECT d.id::text,d.sink_id::text,d.event_id::text,d.stream,d.payload,d.created_at,d.attempts,
	 s.destination,s.credential_id::text FROM olp.export_deliveries d JOIN olp.export_sinks s ON s.id=d.sink_id
	 WHERE d.status='pending' AND d.next_attempt_at<=now() AND d.expires_at>now() AND s.enabled AND s.retired_at IS NULL
	 AND d.stream=ANY(s.streams) ORDER BY d.next_attempt_at,d.id LIMIT 1 FOR UPDATE OF d SKIP LOCKED`).Scan(&d.id, &d.sink, &d.event, &d.stream, &d.payload, &d.created, &d.attempts, &d.destination, &d.credential)
	if err != nil {
		return nil, err
	}
	d.attempts++
	if _, err = tx.Exec(ctx, "UPDATE olp.export_deliveries SET lease=$2,attempts=attempts+1,next_attempt_at=now()+interval '1 minute' WHERE id=$1", d.id, d.lease); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return d, nil
}
func (w *Worker) send(ctx context.Context, d *delivery) string {
	if w.Egress == nil {
		return "egress_unavailable"
	}
	target, err := w.Egress.ValidateEndpoint(d.destination)
	if err != nil {
		return "destination_refused"
	}
	body, err := json.Marshal(struct {
		Version int             `json:"version"`
		ID      string          `json:"id"`
		Stream  string          `json:"stream"`
		At      time.Time       `json:"occurred_at"`
		Data    json.RawMessage `json:"data"`
	}{1, d.event, d.stream, d.created.UTC(), d.payload})
	if err != nil {
		return "invalid_event"
	}
	bounded, cancel := context.WithTimeout(ctx, deliveryTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(bounded, "POST", target.String(), bytes.NewReader(body))
	if err != nil {
		return "destination_refused"
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", d.event)
	if d.credential != nil {
		if w.Keys == nil {
			return "credential_unavailable"
		}
		secret, err := w.Keys.Read(bounded, w.Pool, w.Installation, *d.credential, secrets.SinkCredential)
		if err != nil {
			return "credential_unavailable"
		}
		sum := hmac.New(sha256.New, secret)
		_, _ = sum.Write(body)
		clear(secret)
		req.Header.Set("X-OLP-Signature", "sha256="+hex.EncodeToString(sum.Sum(nil)))
	}
	w.clientOnce.Do(func() { w.client = w.Egress.Client(deliveryTimeout) })
	response, err := w.client.Do(req)
	if err != nil {
		return "delivery_unavailable"
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 2048))
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return ""
	}
	if response.StatusCode >= 500 {
		return "http_5xx"
	}
	return "http_refused"
}
