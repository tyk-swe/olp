//go:build integration

package integration_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/usage"
)

func alertPolicy() *egress.Policy {
	return &egress.Policy{
		AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
		PlainHTTPHosts:  []string{"127.0.0.1"},
	}
}

type webhookHit struct {
	body      []byte
	path      string
	signature string
}

type webhookFixture struct {
	*httptest.Server
	mu     sync.Mutex
	hits   []webhookHit
	status int
}

func newWebhookFixture(t *testing.T) *webhookFixture {
	f := &webhookFixture{status: http.StatusOK}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.hits = append(f.hits, webhookHit{body: body, path: r.URL.Path, signature: r.Header.Get("X-OLP-Signature")})
		f.mu.Unlock()
		w.WriteHeader(f.status)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *webhookFixture) setStatus(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = status
}

func (f *webhookFixture) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.hits)
}

func (f *webhookFixture) hit(i int) webhookHit {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[i]
}

func alertInstallation(t *testing.T, h *accessHarness) string {
	t.Helper()
	var installation string
	if err := h.Pool.QueryRow(context.Background(), "SELECT id::text FROM olp_go.installation WHERE singleton").Scan(&installation); err != nil {
		t.Fatalf("installation: %v", err)
	}
	return installation
}

func alertPass(t *testing.T, h *accessHarness, policy *egress.Policy) {
	t.Helper()
	ring, err := secrets.ParseRing([]byte(h.Ring))
	if err != nil {
		t.Fatalf("key ring: %v", err)
	}
	var baseline int64
	_ = h.Pool.QueryRow(context.Background(),
		"SELECT successes_total+failures_total+skipped_total FROM olp_go.worker_task_health WHERE task='budget_alert_delivery'").Scan(&baseline)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		usage.RunBudgetAlertDelivery(ctx, h.Pool, ring, alertInstallation(t, h), policy,
			slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	deadline := time.Now().Add(20 * time.Second)
	for {
		var checked int64
		err := h.Pool.QueryRow(context.Background(),
			"SELECT successes_total+failures_total+skipped_total FROM olp_go.worker_task_health WHERE task='budget_alert_delivery'").Scan(&checked)
		if err == nil && checked > baseline {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatalf("budget alert pass never checkpointed: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	<-done
}

func alertDeliveries(t *testing.T, h *accessHarness, ruleID string) []map[string]any {
	t.Helper()
	rows, err := h.Pool.Query(context.Background(),
		`SELECT jsonb_build_object('id',id,'status',status,'attempts',attempts,'last_error_code',last_error_code,
		 'window_id',window_id,'threshold_percent',threshold_percent,'delivered_at',delivered_at,
		 'accrued',accrued::text,'limit',limit_amount::text,'currency',currency)
		 FROM olp_go.budget_alert_deliveries WHERE rule_id=$1 ORDER BY created_at`, ruleID)
	if err != nil {
		t.Fatalf("deliveries: %v", err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			t.Fatalf("delivery row: %v", err)
		}
		var item map[string]any
		if err := json.Unmarshal(data, &item); err != nil {
			t.Fatalf("delivery json: %v", err)
		}
		out = append(out, item)
	}
	return out
}

func TestBudgetAlertDelivery(t *testing.T) {
	h := newAccessHarness(t)
	policy := alertPolicy()
	h.Server.Egress = policy
	owner := h.owner()
	hook := newWebhookFixture(t)

	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := h.Pool.Exec(context.Background(), query, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	exec("INSERT INTO olp_go.pricing_currency (singleton, currency) VALUES (true, 'USD')")

	keyHit := h.want(owner, "POST", "/api/v3/api-keys",
		map[string]any{"name": "alerted key", "scopes": []string{"inference"}, "daily_cost_limit": "10.00"},
		map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	keyMiss := h.want(owner, "POST", "/api/v3/api-keys",
		map[string]any{"name": "quiet key", "scopes": []string{"inference"}, "daily_cost_limit": "10.00"},
		map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	keyStale := h.want(owner, "POST", "/api/v3/api-keys",
		map[string]any{"name": "stale key", "scopes": []string{"inference"}, "daily_cost_limit": "10.00"},
		map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	group := h.want(owner, "POST", "/api/v3/budget-groups",
		map[string]any{"name": "shared spend", "monthly_cost_limit": "5.00"},
		map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	windows := limits.BudgetWindows(time.Now())
	exec(`INSERT INTO olp_go.api_key_cost_windows (api_key_id, window_kind, window_id, accrued, unpriced_attempts)
	      VALUES ($1, 'day', $2, '8.000000000000', 0)`, keyHit["id"], windows.DailyID)
	exec(`INSERT INTO olp_go.api_key_cost_windows (api_key_id, window_kind, window_id, accrued, unpriced_attempts)
	      VALUES ($1, 'day', $2, '7.900000000000', 0)`, keyMiss["id"], windows.DailyID)

	exec(`INSERT INTO olp_go.api_key_cost_windows (api_key_id, window_kind, window_id, accrued, unpriced_attempts)
	      VALUES ($1, 'day', $2, '9.500000000000', 0)`, keyStale["id"], windows.DailyID-1)
	exec(`INSERT INTO olp_go.api_key_cost_windows (api_key_id, window_kind, window_id, accrued, unpriced_attempts)
	      VALUES ($1, 'day', $2, '1.000000000000', 0)`, keyStale["id"], windows.DailyID)
	exec(`INSERT INTO olp_go.budget_group_cost_windows (budget_group_id, window_kind, window_id, accrued, unpriced_attempts)
	      VALUES ($1, 'month', $2, '5.000000000000', 0)`, group["id"], windows.MonthlyID)

	signed := h.want(owner, "POST", "/api/v3/notifications/destinations",
		map[string]any{"name": "signed hook", "url": hook.URL + "/signed", "secret": "signing-key"},
		map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	unsigned := h.want(owner, "POST", "/api/v3/notifications/destinations",
		map[string]any{"name": "plain hook", "url": hook.URL + "/plain"},
		map[string]string{"Idempotency-Key": uuid.NewString()}, 201)

	for _, path := range []string{"/api/v3/notifications/destinations/" + signed["id"].(string), "/api/v3/notifications/destinations"} {
		status, body, _ := h.request(owner, "GET", path, nil, nil)
		if status != 200 {
			t.Fatalf("destination read: %d", status)
		}
		raw, _ := json.Marshal(body)
		if strings.Contains(string(raw), "signing-key") || strings.Contains(string(raw), "secret_id") {
			t.Fatalf("destination read leaked the signing secret: %s", raw)
		}
	}

	ruleKey := h.want(owner, "POST", "/api/v3/notifications/rules",
		map[string]any{"name": "key over 80", "subject_kind": "api_key", "subject_id": keyHit["id"],
			"window_kind": "day", "threshold_percent": 80, "destination_id": signed["id"]},
		map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	ruleGroup := h.want(owner, "POST", "/api/v3/notifications/rules",
		map[string]any{"name": "group at limit", "subject_kind": "budget_group", "subject_id": group["id"],
			"window_kind": "month", "threshold_percent": 100, "destination_id": unsigned["id"]},
		map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	ruleQuiet := h.want(owner, "POST", "/api/v3/notifications/rules",
		map[string]any{"name": "quiet rule", "subject_kind": "api_key", "subject_id": keyMiss["id"],
			"window_kind": "day", "threshold_percent": 80, "destination_id": signed["id"]},
		map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	ruleStale := h.want(owner, "POST", "/api/v3/notifications/rules",
		map[string]any{"name": "stale rule", "subject_kind": "api_key", "subject_id": keyStale["id"],
			"window_kind": "day", "threshold_percent": 80, "destination_id": signed["id"]},
		map[string]string{"Idempotency-Key": uuid.NewString()}, 201)

	alertPass(t, h, policy)

	if hook.count() != 2 {
		t.Fatalf("deliveries = %d, want 2 (below-threshold rule must not fire)", hook.count())
	}
	seen := map[string]map[string]any{}
	for i := 0; i < hook.count(); i++ {
		hit := hook.hit(i)
		var body map[string]any
		if err := json.Unmarshal(hit.body, &body); err != nil {
			t.Fatalf("webhook body: %v", err)
		}
		if body["event"] != "budget.threshold" {
			t.Fatalf("event = %v", body["event"])
		}
		for _, forbidden := range []string{"prompt", "messages", "content", "attribution", "secret"} {
			if _, ok := body[forbidden]; ok {
				t.Fatalf("payload leaked %s", forbidden)
			}
		}
		if body["currency"] != "USD" {
			t.Fatalf("currency = %v", body["currency"])
		}
		seen[body["rule_id"].(string)] = body
		switch hit.path {
		case "/signed":
			mac := hmac.New(sha256.New, []byte("signing-key"))
			mac.Write(hit.body)
			want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
			if hit.signature != want {
				t.Fatalf("signature = %q, want %q", hit.signature, want)
			}
		case "/plain":
			if hit.signature != "" {
				t.Fatalf("unsigned destination carried a signature: %q", hit.signature)
			}
		}
	}
	keyBody, ok := seen[ruleKey["id"].(string)]
	if !ok {
		t.Fatalf("key rule delivered bodies: %v", seen)
	}
	if keyBody["subject_kind"] != "api_key" || keyBody["subject_id"] != keyHit["id"] ||
		keyBody["window_kind"] != "day" || keyBody["accrued"] != "8.000000000000" ||
		keyBody["limit"] != "10.000000000000" || keyBody["threshold_percent"].(float64) != 80 {
		t.Fatalf("key alert body: %v", keyBody)
	}
	if _, ok := seen[ruleGroup["id"].(string)]; !ok {
		t.Fatalf("group rule did not deliver at exactly 100%%")
	}
	if _, ok := seen[ruleQuiet["id"].(string)]; ok {
		t.Fatalf("below-threshold rule delivered")
	}
	if _, ok := seen[ruleStale["id"].(string)]; ok {
		t.Fatalf("stale-window rule delivered")
	}
	if rows := alertDeliveries(t, h, ruleStale["id"].(string)); len(rows) != 0 {
		t.Fatalf("stale window claimed a delivery: %v", rows)
	}

	for id, want := range map[string]string{ruleKey["id"].(string): "delivered", ruleGroup["id"].(string): "delivered"} {
		rows := alertDeliveries(t, h, id)
		if len(rows) != 1 || rows[0]["status"] != want || rows[0]["attempts"].(float64) != 1 || rows[0]["last_error_code"] != nil {
			t.Fatalf("deliveries for %s: %v", id, rows)
		}
	}
	keyRow := alertDeliveries(t, h, ruleKey["id"].(string))[0]
	if keyRow["accrued"] != "8.000000000000" || keyRow["limit"] != "10.000000000000" || keyRow["currency"] != "USD" {
		t.Fatalf("stored evidence = %v", keyRow)
	}
	if rows := alertDeliveries(t, h, ruleQuiet["id"].(string)); len(rows) != 0 {
		t.Fatalf("quiet rule recorded a delivery: %v", rows)
	}

	alertPass(t, h, policy)
	if hook.count() != 2 {
		t.Fatalf("second pass delivered again: %d", hook.count())
	}

	listed := h.want(owner, "GET", "/api/v3/notifications/deliveries?rule_id="+ruleKey["id"].(string), nil, nil, 200)
	items := listed["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("delivery listing = %v", listed)
	}
	item := items[0].(map[string]any)
	for _, field := range []string{"id", "rule_id", "rule_name", "status", "attempts", "window_id", "threshold_percent", "accrued", "limit", "currency"} {
		if _, ok := item[field]; !ok {
			t.Fatalf("delivery metadata missing %s: %v", field, item)
		}
	}
	for _, forbidden := range []string{"body", "response", "secret", "url"} {
		if _, ok := item[forbidden]; ok {
			t.Fatalf("delivery listing leaked %s", forbidden)
		}
	}
}

func TestBudgetAlertRetryAndFailure(t *testing.T) {
	h := newAccessHarness(t)
	policy := alertPolicy()
	h.Server.Egress = policy
	owner := h.owner()
	hook := newWebhookFixture(t)
	hook.setStatus(http.StatusBadGateway)

	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := h.Pool.Exec(context.Background(), query, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	exec("INSERT INTO olp_go.pricing_currency (singleton, currency) VALUES (true, 'USD')")
	key := h.want(owner, "POST", "/api/v3/api-keys",
		map[string]any{"name": "retry key", "scopes": []string{"inference"}, "daily_cost_limit": "10.00"},
		map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	windows := limits.BudgetWindows(time.Now())
	exec(`INSERT INTO olp_go.api_key_cost_windows (api_key_id, window_kind, window_id, accrued, unpriced_attempts)
	      VALUES ($1, 'day', $2, '9.000000000000', 0)`, key["id"], windows.DailyID)
	destination := h.want(owner, "POST", "/api/v3/notifications/destinations",
		map[string]any{"name": "flaky hook", "url": hook.URL + "/flaky"},
		map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	rule := h.want(owner, "POST", "/api/v3/notifications/rules",
		map[string]any{"name": "retry rule", "subject_kind": "api_key", "subject_id": key["id"],
			"window_kind": "day", "threshold_percent": 50, "destination_id": destination["id"]},
		map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	ruleID := rule["id"].(string)

	alertPass(t, h, policy)
	rows := alertDeliveries(t, h, ruleID)
	if len(rows) != 1 || rows[0]["status"] != "failed" || rows[0]["last_error_code"] != "http_5xx" || rows[0]["attempts"].(float64) != 1 {
		t.Fatalf("failed delivery = %v", rows)
	}

	alertPass(t, h, policy)
	if hook.count() != 1 {
		t.Fatalf("retry fired inside backoff: %d", hook.count())
	}

	exec("DELETE FROM olp_go.api_key_cost_windows WHERE api_key_id=$1 AND window_kind='day'", key["id"])
	exec(`UPDATE olp_go.api_keys SET policy = jsonb_set(policy, '{daily_cost_limit}', '"999.00"') WHERE id=$1`, key["id"])

	exec("UPDATE olp_go.budget_alert_deliveries SET last_attempt_at=now()-interval '2 minutes' WHERE rule_id=$1", ruleID)
	hook.setStatus(http.StatusBadRequest)
	alertPass(t, h, policy)
	rows = alertDeliveries(t, h, ruleID)
	if rows[0]["last_error_code"] != "http_4xx" || rows[0]["attempts"].(float64) != 2 {
		t.Fatalf("retried delivery = %v", rows)
	}
	var retryBody map[string]any
	if err := json.Unmarshal(hook.hit(1).body, &retryBody); err != nil {
		t.Fatalf("retry body: %v", err)
	}
	if retryBody["accrued"] != "9.000000000000" || retryBody["limit"] != "10.000000000000" || retryBody["currency"] != "USD" {
		t.Fatalf("retry did not report claim-time evidence: %v", retryBody)
	}

	exec("UPDATE olp_go.budget_alert_deliveries SET last_attempt_at=now()-interval '3 minutes' WHERE rule_id=$1", ruleID)
	hook.setStatus(http.StatusNoContent)
	alertPass(t, h, policy)
	rows = alertDeliveries(t, h, ruleID)
	if rows[0]["status"] != "delivered" || rows[0]["last_error_code"] != nil || rows[0]["attempts"].(float64) != 3 {
		t.Fatalf("delivered retry = %v", rows)
	}
	if hook.count() != 3 {
		t.Fatalf("attempts = %d, want 3", hook.count())
	}
}

// Every worker replica runs the delivery loop, and the advisory lock only
// covers claiming. A replica whose pass starts while another replica is still
// posting a delivery must not post it again.
func TestBudgetAlertDeliveryIsPostedOnceAcrossReplicas(t *testing.T) {
	h := newAccessHarness(t)
	policy := alertPolicy()
	h.Server.Egress = policy
	owner := h.owner()
	started, release := make(chan struct{}), make(chan struct{})
	var posts atomic.Int32
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Hold only the first post open; a duplicate returns immediately.
		if posts.Add(1) == 1 {
			close(started)
			<-release
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(hook.Close)

	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := h.Pool.Exec(context.Background(), query, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	exec("INSERT INTO olp_go.pricing_currency (singleton, currency) VALUES (true, 'USD')")
	key := h.want(owner, "POST", "/api/v3/api-keys",
		map[string]any{"name": "replicated key", "scopes": []string{"inference"}, "daily_cost_limit": "10.00"},
		map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	windows := limits.BudgetWindows(time.Now())
	exec(`INSERT INTO olp_go.api_key_cost_windows (api_key_id, window_kind, window_id, accrued, unpriced_attempts)
	      VALUES ($1, 'day', $2, '9.000000000000', 0)`, key["id"], windows.DailyID)
	destination := h.want(owner, "POST", "/api/v3/notifications/destinations",
		map[string]any{"name": "slow hook", "url": hook.URL + "/slow"},
		map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	rule := h.want(owner, "POST", "/api/v3/notifications/rules",
		map[string]any{"name": "replicated rule", "subject_kind": "api_key", "subject_id": key["id"],
			"window_kind": "day", "threshold_percent": 50, "destination_id": destination["id"]},
		map[string]string{"Idempotency-Key": uuid.NewString()}, 201)

	ring, err := secrets.ParseRing([]byte(h.Ring))
	if err != nil {
		t.Fatalf("key ring: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		usage.RunBudgetAlertDelivery(ctx, h.Pool, ring, alertInstallation(t, h), policy,
			slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	stop := func() {
		cancel()
		<-done
	}
	select {
	case <-started:
	case <-time.After(20 * time.Second):
		stop()
		t.Fatal("the first replica never posted the delivery")
	}

	// A second replica completes a whole pass while the first is still posting.
	alertPass(t, h, policy)
	close(release)
	var rows []map[string]any
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		rows = alertDeliveries(t, h, rule["id"].(string))
		if len(rows) == 1 && rows[0]["status"] == "delivered" || time.Now().After(deadline) {
			break
		}
	}
	stop()
	if n := posts.Load(); n != 1 {
		t.Fatalf("delivery posted %d times across replicas, want 1", n)
	}
	if len(rows) != 1 || rows[0]["status"] != "delivered" || rows[0]["attempts"].(float64) != 1 {
		t.Fatalf("delivery = %v", rows)
	}
}

func TestBudgetAlertValidationAndScope(t *testing.T) {
	h := newAccessHarness(t)
	policy := alertPolicy()
	h.Server.Egress = policy
	owner := h.owner()
	hook := newWebhookFixture(t)

	status, problem, _ := h.request(owner, "POST", "/api/v3/notifications/destinations",
		map[string]any{"name": "internal", "url": "http://10.0.0.9/hook"},
		map[string]string{"Idempotency-Key": uuid.NewString()})
	if status != 422 {
		t.Fatalf("internal destination = %d %v", status, problem)
	}
	status, problem, _ = h.request(owner, "POST", "/api/v3/notifications/destinations",
		map[string]any{"name": "credentialed", "url": "https://user:pass@127.0.0.1/hook"},
		map[string]string{"Idempotency-Key": uuid.NewString()})
	if status != 422 {
		t.Fatalf("credentialed destination = %d %v", status, problem)
	}

	project := h.want(owner, "POST", "/api/v3/projects",
		map[string]any{"name": "Scoped"}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	projectID := project["id"].(string)
	scopedDestination := h.want(owner, "POST", "/api/v3/notifications/destinations",
		map[string]any{"name": "scoped hook", "url": hook.URL + "/scoped", "project_id": projectID},
		map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	globalKey := h.want(owner, "POST", "/api/v3/api-keys",
		map[string]any{"name": "global key", "scopes": []string{"inference"}},
		map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	projectKey := h.want(owner, "POST", "/api/v3/api-keys",
		map[string]any{"name": "project key", "scopes": []string{"inference"}, "project_id": projectID},
		map[string]string{"Idempotency-Key": uuid.NewString()}, 201)

	status, _, _ = h.request(owner, "POST", "/api/v3/notifications/rules",
		map[string]any{"name": "mismatch subject", "project_id": projectID, "subject_kind": "api_key",
			"subject_id": globalKey["id"], "window_kind": "day", "threshold_percent": 50,
			"destination_id": scopedDestination["id"]},
		map[string]string{"Idempotency-Key": uuid.NewString()})
	if status != 422 {
		t.Fatalf("global subject under project rule = %d", status)
	}
	globalDestination := h.want(owner, "POST", "/api/v3/notifications/destinations",
		map[string]any{"name": "global hook", "url": hook.URL + "/global"},
		map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	status, _, _ = h.request(owner, "POST", "/api/v3/notifications/rules",
		map[string]any{"name": "mismatch destination", "project_id": projectID, "subject_kind": "api_key",
			"subject_id": projectKey["id"], "window_kind": "day", "threshold_percent": 50,
			"destination_id": globalDestination["id"]},
		map[string]string{"Idempotency-Key": uuid.NewString()})
	if status != 422 {
		t.Fatalf("global destination under project rule = %d", status)
	}

	status, _, _ = h.request(owner, "POST", "/api/v3/notifications/rules",
		map[string]any{"name": "global rule project dest", "subject_kind": "api_key",
			"subject_id": globalKey["id"], "window_kind": "day", "threshold_percent": 50,
			"destination_id": scopedDestination["id"]},
		map[string]string{"Idempotency-Key": uuid.NewString()})
	if status != 422 {
		t.Fatalf("project destination under global rule = %d", status)
	}

	h.want(owner, "POST", "/api/v3/notifications/rules",
		map[string]any{"name": "scoped rule", "project_id": projectID, "subject_kind": "api_key",
			"subject_id": projectKey["id"], "window_kind": "month", "threshold_percent": 90,
			"destination_id": scopedDestination["id"]},
		map[string]string{"Idempotency-Key": uuid.NewString()}, 201)

	destination := h.want(owner, "POST", "/api/v3/notifications/destinations",
		map[string]any{"name": "idempotent", "url": hook.URL + "/idem"},
		map[string]string{"Idempotency-Key": "alert-idem-1"}, 201)
	replay := h.want(owner, "POST", "/api/v3/notifications/destinations",
		map[string]any{"name": "idempotent", "url": hook.URL + "/idem"},
		map[string]string{"Idempotency-Key": "alert-idem-1"}, 201)
	if replay["id"] != destination["id"] {
		t.Fatalf("idempotent replay created a second destination: %v vs %v", replay["id"], destination["id"])
	}

	detail := h.want(owner, "GET", "/api/v3/notifications/destinations/"+destination["id"].(string), nil, nil, 200)
	updated := h.want(owner, "PATCH", "/api/v3/notifications/destinations/"+destination["id"].(string),
		map[string]any{"enabled": false}, withMatch(detail, nil), 200)
	status, problem, _ = h.request(owner, "PATCH", "/api/v3/notifications/destinations/"+destination["id"].(string),
		map[string]any{"enabled": true}, withMatch(detail, nil))
	if status != 412 {
		t.Fatalf("stale etag patch = %d %v", status, problem)
	}
	_ = updated
}

// A destination owns its signing secret: replacing or clearing it removes the
// previous ciphertext rather than retaining an unreachable record.
func TestNotificationSecretReplacementRemovesPreviousSecret(t *testing.T) {
	h := newAccessHarness(t)
	h.Server.Egress = alertPolicy()
	owner := h.owner()
	hook := newWebhookFixture(t)
	destination := h.want(owner, "POST", "/api/v3/notifications/destinations",
		map[string]any{"name": "signed", "url": hook.URL + "/signed", "secret": "first-signing-key"},
		map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	path := "/api/v3/notifications/destinations/" + destination["id"].(string)
	assertSecrets := func(step string, want int) {
		t.Helper()
		var stored, owned int
		if err := h.Pool.QueryRow(t.Context(), `SELECT count(*),count(*) FILTER (WHERE id=(SELECT secret_id FROM olp_go.notification_destinations WHERE id=$1))
			FROM olp_go.secrets WHERE purpose='notification_secret'`, destination["id"]).Scan(&stored, &owned); err != nil {
			t.Fatal(err)
		}
		if stored != want || owned != want {
			t.Fatalf("%s: %d notification secrets stored, %d owned; want %d", step, stored, owned, want)
		}
	}
	assertSecrets("create", 1)
	for _, step := range []struct {
		name   string
		secret any
		want   int
	}{{"replace", "second-signing-key", 1}, {"clear", nil, 0}, {"set", "third-signing-key", 1}} {
		detail := h.want(owner, "GET", path, nil, nil, 200)
		h.want(owner, "PATCH", path, map[string]any{"secret": step.secret}, withMatch(detail, nil), 200)
		assertSecrets(step.name, step.want)
	}
}
