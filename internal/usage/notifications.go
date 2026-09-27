package usage

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/secrets"
)

const (
	budgetAlertLockID   = int64(0x4f4c505f414c5254)
	notificationEvery   = time.Minute
	notificationTimeout = 2 * time.Minute
	webhookTimeout      = 5 * time.Second

	webhookDrainBytes   = 64 << 10
	maxDeliveryAttempts = 5
	maxWebhookRedirects = 5

	deliveriesPerPass = 200
)

const dueAlertSQL = `SELECT r.id::text,r.threshold_percent,r.window_kind,
 CASE r.subject_kind WHEN 'api_key' THEN k.id ELSE g.id END::text AS subject_id,
 CASE WHEN r.window_kind='day' THEN COALESCE(kwd.window_id,gwd.window_id)
      ELSE COALESCE(kwm.window_id,gwm.window_id) END AS window_id,
 CASE WHEN r.window_kind='day' THEN COALESCE(kwd.accrued,gwd.accrued,0)
      ELSE COALESCE(kwm.accrued,gwm.accrued,0) END::text AS accrued,
 CASE WHEN r.subject_kind='api_key' AND r.window_kind='day' THEN k.policy->>'daily_cost_limit'
      WHEN r.subject_kind='api_key' THEN k.policy->>'monthly_cost_limit'
      WHEN r.window_kind='day' THEN g.daily_cost_limit::text ELSE g.monthly_cost_limit::text END AS limit,
 d.id::text,d.url,d.secret_id::text,r.name
FROM olp.notification_rules r
LEFT JOIN olp.api_keys k ON r.subject_kind='api_key' AND k.id=r.subject_id
LEFT JOIN olp.budget_groups g ON r.subject_kind='budget_group' AND g.id=r.subject_id
LEFT JOIN olp.api_key_cost_windows kwd ON kwd.api_key_id=k.id AND kwd.window_kind='day'
LEFT JOIN olp.api_key_cost_windows kwm ON kwm.api_key_id=k.id AND kwm.window_kind='month'
LEFT JOIN olp.budget_group_cost_windows gwd ON gwd.budget_group_id=g.id AND gwd.window_kind='day'
LEFT JOIN olp.budget_group_cost_windows gwm ON gwm.budget_group_id=g.id AND gwm.window_kind='month'
JOIN olp.notification_destinations d ON d.id=r.destination_id
WHERE r.event=$1 AND r.enabled AND d.enabled`

const pendingDeliverySQL = `SELECT v.id::text,v.rule_id::text,r.event,v.attempts,v.last_attempt_at,
 r.name,r.subject_kind,r.subject_id::text,r.window_kind,v.window_id,v.threshold_percent,
 v.accrued::text,v.limit_amount::text,COALESCE(v.currency::text,''),v.payload,
 d.url,d.secret_id::text
FROM olp.notification_deliveries v
JOIN olp.notification_rules r ON r.id=v.rule_id
JOIN olp.notification_destinations d ON d.id=r.destination_id
WHERE v.status IN ('pending','failed') AND v.attempts<$1
ORDER BY v.created_at
LIMIT $2`

type dueAlert struct {
	ruleID        string
	threshold     int
	windowKind    string
	subjectID     *string
	windowID      *int64
	accrued       string
	limit         *string
	destinationID string
	url           string
	secretID      *string
	ruleName      string
}

// delivery is an event notification awaiting delivery to its rule's
// destination, with the event's evidence: a budget threshold's spend as it was
// claimed, or the payload of a provider event as it was when it happened.
type delivery struct {
	id            string
	ruleID        string
	event         string
	attempts      int
	lastAttemptAt *time.Time
	ruleName      string
	subjectKind   *string
	subjectID     *string
	windowKind    *string
	windowID      *int64
	threshold     *int
	accrued       *string
	limit         *string
	currency      string
	payload       []byte
	url           string
	secretID      *string
}

func thresholdEvidence(accrued string, limit *string, threshold int) (decimal.Decimal, decimal.Decimal, bool) {
	if limit == nil {
		return decimal.Decimal{}, decimal.Decimal{}, false
	}
	limitValue, err := decimal.NewFromString(*limit)
	if err != nil || !limitValue.IsPositive() {
		return decimal.Decimal{}, decimal.Decimal{}, false
	}
	accruedValue, err := decimal.NewFromString(accrued)
	if err != nil || accruedValue.IsNegative() {
		return decimal.Decimal{}, decimal.Decimal{}, false
	}
	return accruedValue, limitValue, accruedValue.Mul(decimal.NewFromInt(100)).
		Cmp(limitValue.Mul(decimal.NewFromInt(int64(threshold)))) >= 0
}

func thresholdMet(accrued string, limit *string, threshold int) bool {
	_, _, met := thresholdEvidence(accrued, limit, threshold)
	return met
}

func retryDue(lastAttemptAt *time.Time, attempts int, now time.Time) bool {
	if lastAttemptAt == nil {
		return true
	}
	if attempts < 1 {
		return true
	}
	backoff := time.Duration(int64(1)<<(attempts-1)) * time.Minute
	return !now.Before(lastAttemptAt.Add(backoff))
}

type notificationWorker struct {
	pool         *pgxpool.Pool
	keys         *secrets.KeyRing
	installation string
	policy       *egress.Policy
	client       *http.Client
	log          *slog.Logger
	newID        func() string
	now          func() time.Time
}

// RunNotificationDelivery evaluates budget threshold rules and delivers event
// notifications to their rules' destinations every minute until ctx ends,
// checkpointing each pass as the notification_delivery worker task. A
// delivery that fails is retried with backoff, up to maxDeliveryAttempts.
func RunNotificationDelivery(ctx context.Context, pool *pgxpool.Pool, keys *secrets.KeyRing, installation string, policy *egress.Policy, log *slog.Logger) {
	w := &notificationWorker{
		pool:         pool,
		keys:         keys,
		installation: installation,
		policy:       policy,
		client:       webhookClient(policy),
		log:          log,
		newID:        uuid7,
		now:          time.Now,
	}
	ticker := time.NewTicker(notificationEvery)
	defer ticker.Stop()
	for ctx.Err() == nil {
		outcome, progress := w.pass(ctx)
		if ctx.Err() != nil {
			return
		}
		if err := CheckpointTask(ctx, pool, TaskNotificationDelivery, outcome, progress); err != nil {
			log.Warn("notification delivery checkpoint failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func webhookClient(policy *egress.Policy) *http.Client {
	client := policy.Client(webhookTimeout)
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxWebhookRedirects {
			return errors.New("too many redirects")
		}
		if _, err := policy.ValidateEndpoint(req.URL.String()); err != nil {
			return fmt.Errorf("redirect target refused: %w", err)
		}
		return nil
	}
	return client
}

var errLockNotHeld = errors.New("another replica holds the budget alert lock")

func (w *notificationWorker) pass(ctx context.Context) (Outcome, bool) {
	passCtx, cancel := context.WithTimeout(ctx, notificationTimeout)
	defer cancel()
	claimed, err := w.claimDue(passCtx)
	if errors.Is(err, errLockNotHeld) {
		return OutcomeSkipped, false
	}
	if err != nil {
		w.log.Warn("budget alert evaluation failed", "error", err)
		return OutcomeFailure, false
	}
	deliveries, err := w.pending(passCtx)
	if err != nil {
		w.log.Warn("pending notification query failed", "error", err)
		return OutcomeFailure, claimed > 0
	}
	now := w.now()
	sent := 0
	for _, d := range deliveries {
		if passCtx.Err() != nil {
			break
		}
		if !retryDue(d.lastAttemptAt, d.attempts, now) {
			continue
		}
		if w.deliver(passCtx, d) {
			sent++
		}
	}
	return OutcomeSuccess, claimed > 0 || sent > 0
}

func (w *notificationWorker) claimDue(ctx context.Context) (int, error) {
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	var held bool
	if err = tx.QueryRow(ctx, "SELECT pg_try_advisory_xact_lock($1)", budgetAlertLockID).Scan(&held); err != nil {
		return 0, err
	}
	if !held {
		return 0, errLockNotHeld
	}
	rows, err := tx.Query(ctx, dueAlertSQL, access.BudgetThresholdEvent)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var alerts []dueAlert
	for rows.Next() {
		var a dueAlert
		if err = rows.Scan(&a.ruleID, &a.threshold, &a.windowKind, &a.subjectID,
			&a.windowID, &a.accrued, &a.limit, &a.destinationID, &a.url, &a.secretID, &a.ruleName); err != nil {
			return 0, err
		}
		alerts = append(alerts, a)
	}
	if err = rows.Err(); err != nil {
		return 0, err
	}
	windows := limits.BudgetWindows(w.now())
	currency := w.currency(ctx)
	claimed := 0
	for _, a := range alerts {
		if a.windowID == nil {
			continue
		}
		current := a.windowKind == "day" && *a.windowID == windows.DailyID ||
			a.windowKind == "month" && *a.windowID == windows.MonthlyID
		if !current {
			continue
		}
		accrued, limit, met := thresholdEvidence(a.accrued, a.limit, a.threshold)
		if !met {
			continue
		}
		var storedCurrency *string
		if currency != "" {
			storedCurrency = &currency
		}
		tag, err := tx.Exec(ctx,
			`INSERT INTO olp.notification_deliveries (id, rule_id, window_id, threshold_percent, accrued, limit_amount, currency, status)
			 VALUES ($1, $2, $3, $4, $5::numeric, $6::numeric, $7, 'pending') ON CONFLICT DO NOTHING`,
			w.newID(), a.ruleID, *a.windowID, a.threshold, accrued.String(), limit.String(), storedCurrency)
		if err != nil {
			return 0, err
		}
		if tag.RowsAffected() > 0 {
			claimed++
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, err
	}
	return claimed, nil
}

// grantLapseSQL returns the payload that reports the lapse of a credential
// version's grant: its provider, the credential slots bound to it in the
// provider's draft or active revision, by their draft names, and the
// principal its grant enrollment observed. It carries no secret material.
const grantLapseSQL = `SELECT jsonb_build_object('provider_id',p.id,'provider_name',p.name,
 'credential_version_id',c.id,'credential_version',c.version,'observed_principal',c.principal,'lapsed_at',g.lapsed_at,
 'credential_slots',(SELECT coalesce(jsonb_agg(jsonb_build_object('id',s.id,'name',s.name) ORDER BY s.name,s.id),'[]')
   FROM (SELECT DISTINCT ON (id) id,name FROM (
     SELECT id,name,false AS published FROM olp.provider_slots WHERE credential_id=c.id
     UNION ALL
     SELECT (e->>'id')::uuid,e->>'name',true FROM olp.provider_revisions r CROSS JOIN jsonb_array_elements(r.slots) e
     WHERE r.id=p.active_revision_id AND e->>'credential_id'=c.id::text) bound
   ORDER BY id,published) s))
FROM olp.provider_credentials c
JOIN olp.providers p ON p.id=c.provider_id
JOIN olp.provider_grants g ON g.credential_id=c.id
WHERE c.id=$1`

// NotifyGrantLapsed enqueues, in the transaction that records the lapse of a
// credential version's grant, one delivery of the provider.grant.lapsed event
// to each enabled rule subscribed to it whose destination is enabled. The
// notification delivery task delivers it like a budget alert.
func NotifyGrantLapsed(ctx context.Context, tx pgx.Tx, credentialID string) error {
	rows, err := tx.Query(ctx, `SELECT r.id::text FROM olp.notification_rules r
		JOIN olp.notification_destinations d ON d.id=r.destination_id WHERE r.event=$1 AND r.enabled AND d.enabled`,
		access.GrantLapsedEvent)
	if err != nil {
		return err
	}
	rules, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(rules) == 0 {
		return err
	}
	var payload []byte
	if err = tx.QueryRow(ctx, grantLapseSQL, credentialID).Scan(&payload); err != nil {
		return err
	}
	for _, rule := range rules {
		if _, err = tx.Exec(ctx, `INSERT INTO olp.notification_deliveries (id, rule_id, credential_id, payload, status)
			VALUES ($1, $2, $3, $4, 'pending') ON CONFLICT DO NOTHING`, uuid7(), rule, credentialID, payload); err != nil {
			return err
		}
	}
	return nil
}

func (w *notificationWorker) pending(ctx context.Context) ([]delivery, error) {
	rows, err := w.pool.Query(ctx, pendingDeliverySQL, maxDeliveryAttempts, deliveriesPerPass)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var deliveries []delivery
	for rows.Next() {
		var d delivery
		if err = rows.Scan(&d.id, &d.ruleID, &d.event, &d.attempts, &d.lastAttemptAt,
			&d.ruleName, &d.subjectKind, &d.subjectID, &d.windowKind, &d.windowID, &d.threshold,
			&d.accrued, &d.limit, &d.currency, &d.payload, &d.url, &d.secretID); err != nil {
			return nil, err
		}
		deliveries = append(deliveries, d)
	}
	return deliveries, rows.Err()
}

func (w *notificationWorker) currency(ctx context.Context) string {
	var currency string
	err := w.pool.QueryRow(ctx, "SELECT currency FROM olp.pricing_currency WHERE singleton").Scan(&currency)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		w.log.Warn("budget alert currency lookup failed", "error", err)
	}
	return currency
}

// webhookBody is the body that reports a delivery's event to its rule's
// destination: metadata only, never secret material.
func webhookBody(d delivery) ([]byte, error) {
	if d.event == access.BudgetThresholdEvent {
		return json.Marshal(map[string]any{
			"event":             d.event,
			"rule_id":           d.ruleID,
			"rule_name":         d.ruleName,
			"subject_kind":      d.subjectKind,
			"subject_id":        d.subjectID,
			"window_kind":       d.windowKind,
			"window_id":         d.windowID,
			"threshold_percent": d.threshold,
			"accrued":           d.accrued,
			"limit":             d.limit,
			"currency":          d.currency,
		})
	}
	var body map[string]any
	if err := json.Unmarshal(d.payload, &body); err != nil {
		return nil, err
	}
	body["event"], body["rule_id"], body["rule_name"] = d.event, d.ruleID, d.ruleName
	return json.Marshal(body)
}

func (w *notificationWorker) notificationSecret(ctx context.Context, secretID *string) ([]byte, error) {
	if secretID == nil {
		return nil, nil
	}
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	return w.keys.Read(ctx, tx, w.installation, *secretID, "notification_secret")
}

func deliveryErrorCode(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Timeout() || errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "network"
}

// deliver makes one attempt to deliver d and records how it ended, reporting
// whether it made one. The attempt is recorded before it is made, unless
// another replica recorded one since d was read, so replicas never make the
// same attempt twice.
func (w *notificationWorker) deliver(ctx context.Context, d delivery) bool {
	claimed, err := w.pool.Exec(ctx,
		"UPDATE olp.notification_deliveries SET attempts=attempts+1,last_attempt_at=now() WHERE id=$1 AND attempts=$2",
		d.id, d.attempts)
	if err != nil {
		w.log.Warn("notification delivery claim failed", "delivery", d.id, "error", err)
		return false
	}
	if claimed.RowsAffected() == 0 {
		return false
	}
	code := w.send(ctx, d)
	delivered := code == ""
	var lastError *string
	if !delivered {
		lastError = &code
	}
	if _, err := w.pool.Exec(ctx,
		`UPDATE olp.notification_deliveries
		 SET status=$2,last_error_code=$3,delivered_at=CASE WHEN $2='delivered' THEN now() ELSE delivered_at END
		 WHERE id=$1`,
		d.id, map[bool]string{true: "delivered", false: "failed"}[delivered], lastError); err != nil {
		w.log.Warn("notification delivery update failed", "delivery", d.id, "error", err)
	}
	return true
}

func (w *notificationWorker) send(ctx context.Context, d delivery) string {
	if w.policy == nil {
		return "invalid_destination"
	}
	target, err := w.policy.ValidateEndpoint(d.url)
	if err != nil {
		return "invalid_destination"
	}
	body, err := webhookBody(d)
	if err != nil {
		return "invalid_destination"
	}
	secret, err := w.notificationSecret(ctx, d.secretID)
	if err != nil {
		w.log.Warn("notification secret unavailable", "delivery", d.id, "error", err)
		return "network"
	}
	sendCtx, cancel := context.WithTimeout(ctx, webhookTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(sendCtx, http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		return "invalid_destination"
	}
	req.Header.Set("Content-Type", "application/json")
	if secret != nil {
		sum := hmac.New(sha256.New, secret)
		sum.Write(body)
		req.Header.Set("X-OLP-Signature", "sha256="+hex.EncodeToString(sum.Sum(nil)))
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return deliveryErrorCode(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, webhookDrainBytes))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return ""
	}
	if resp.StatusCode < 500 {
		return "http_4xx"
	}
	return "http_5xx"
}
