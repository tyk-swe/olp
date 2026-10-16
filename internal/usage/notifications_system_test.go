//go:build integration

package usage

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tyk-swe/olp/internal/database"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/secrets"
)

// notificationTestEnv names a PostgreSQL admin connection the test creates
// and drops a scratch database on. The integration suite provisions it.
const notificationTestEnv = "OLP_TEST_DATABASE_ADMIN_URL"

func notificationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	admin := os.Getenv(notificationTestEnv)
	if admin == "" {
		t.Fatalf("%s is required; run make integration", notificationTestEnv)
	}
	cfg, err := pgxpool.ParseConfig(admin)
	if err != nil {
		t.Fatal(err)
	}
	dbName := fmt.Sprintf("olp_usage_notify_%s", strings.ReplaceAll(uuid.NewString()[:13], "-", ""))
	adminPool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = adminPool.Exec(t.Context(), "CREATE DATABASE "+dbName); err != nil {
		adminPool.Close()
		t.Fatal(err)
	}
	cfg.ConnConfig.Database = dbName
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		adminPool.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		if _, err := adminPool.Exec(context.Background(), "DROP DATABASE "+dbName+" WITH (FORCE)"); err != nil {
			t.Logf("drop scratch database: %v", err)
		}
		adminPool.Close()
	})
	if err := database.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

// A rule moved to another destination after its delivery was read is sent to
// the new destination, signed with that destination's secret: the URL and the
// secret never come from different destinations.
func TestIntegrationMovedRuleSendsToItsCurrentDestination(t *testing.T) {
	ctx := t.Context()
	pool := notificationPool(t)
	ring, err := secrets.ParseRing([]byte(`{"active_version":1,"keys":[{"version":1,"key":"` +
		strings.Repeat("ab", 32) + `"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var installation string
	if err = pool.QueryRow(ctx, "UPDATE olp.installation SET active_key_version=1 RETURNING id::text").Scan(&installation); err != nil {
		t.Fatal(err)
	}

	type received struct {
		path, signature string
		body            []byte
	}
	var mu sync.Mutex
	var got []received
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, received{r.URL.Path, r.Header.Get("X-OLP-Signature"), body})
		mu.Unlock()
	}))
	t.Cleanup(hook.Close)

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	owner := uuid.NewString()
	exec("INSERT INTO olp.users(id,email,display_name,role,etag) VALUES($1,'owner@example.test','Owner','owner',$2)",
		owner, uuid.NewString())
	destination := func(name, secret string) string {
		t.Helper()
		id, secretID := uuid.NewString(), uuid.NewString()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if err = ring.Store(ctx, tx, installation, secretID, secrets.NotificationSecret, []byte(secret), nil); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO olp.notification_destinations(id,name,url,secret_id,etag,created_by)
			VALUES($1,$2,$3,$4,$5,$6)`, id, name, hook.URL+"/"+name, secretID, uuid.NewString(), owner); err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		return id
	}
	first, second := destination("first", "first-secret"), destination("second", "second-secret")
	rule := uuid.NewString()
	exec(`INSERT INTO olp.notification_rules(id,name,event,subject_kind,subject_id,window_kind,threshold_percent,destination_id,etag,created_by)
		VALUES($1,'moved','budget.threshold','api_key',$2,'day',50,$3,$4,$5)`,
		rule, uuid.NewString(), first, uuid.NewString(), owner)
	exec(`INSERT INTO olp.notification_deliveries(id,rule_id,window_id,threshold_percent,accrued,limit_amount,status)
		VALUES($1,$2,1,50,'6','10','pending')`, uuid.NewString(), rule)

	policy := &egress.Policy{
		AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
		PlainHTTPHosts:  []string{"127.0.0.1"},
	}
	w := &notificationWorker{
		pool: pool, keys: ring, installation: installation, policy: policy,
		client: webhookClient(policy), log: slog.New(slog.DiscardHandler),
		newID: uuid7,
	}
	deliveries, err := w.pending(ctx)
	if err != nil || len(deliveries) != 1 {
		t.Fatalf("pending deliveries = %d, %v", len(deliveries), err)
	}
	// An operator moves the rule after the worker read its delivery.
	exec("UPDATE olp.notification_rules SET destination_id=$2 WHERE id=$1", rule, second)
	if !w.deliver(ctx, deliveries[0]) {
		t.Fatal("the delivery was not attempted")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("webhook requests = %d, want 1", len(got))
	}
	sum := hmac.New(sha256.New, []byte("second-secret"))
	sum.Write(got[0].body)
	if want := "sha256=" + hex.EncodeToString(sum.Sum(nil)); got[0].path != "/second" || got[0].signature != want {
		t.Fatalf("webhook sent to %s with signature %s; want /second signed with its own secret", got[0].path, got[0].signature)
	}
	var status string
	if err = pool.QueryRow(ctx, "SELECT status FROM olp.notification_deliveries WHERE rule_id=$1", rule).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "delivered" {
		t.Fatalf("delivery status = %q, want delivered", status)
	}
}

// A disable that already holds the rule or destination row lock wins over
// a claim in progress, even though the enabled read initially sees the old row.
func TestIntegrationNotificationWaitsForConcurrentDisable(t *testing.T) {
	pool := notificationPool(t)
	for _, disabled := range []string{"rule", "destination"} {
		t.Run(disabled, func(t *testing.T) {
			fixture := newNotificationDeliveryFixture(t, pool)
			d := fixture.pending(t)
			before := fixture.deliveryState(t)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			sql, id := "UPDATE olp.notification_rules SET enabled=false WHERE id=$1", fixture.ruleID
			if disabled == "destination" {
				sql, id = "UPDATE olp.notification_destinations SET enabled=false WHERE id=$1", fixture.destinationID
			}
			if _, err := tx.Exec(ctx, sql, id); err != nil {
				t.Fatal(err)
			}
			finished := make(chan bool, 1)
			go func() { finished <- fixture.worker.deliver(ctx, d) }()
			ticker := time.NewTicker(5 * time.Millisecond)
			defer ticker.Stop()
			for {
				var waiting bool
				if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity
					WHERE datname=current_database() AND $1=ANY(pg_blocking_pids(pid)))`, tx.Conn().PgConn().PID()).Scan(&waiting); err != nil {
					t.Fatal(err)
				}
				if waiting {
					break
				}
				select {
				case attempted := <-finished:
					t.Fatalf("delivery finished before concurrent disable committed: attempted=%v", attempted)
				case <-ticker.C:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case attempted := <-finished:
				if attempted {
					t.Fatal("concurrently disabled delivery was attempted")
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if after := fixture.deliveryState(t); after != before {
				t.Fatalf("disabled delivery changed:\nbefore: %s\nafter:  %s", before, after)
			}
			fixture.setEnabled(t, disabled, true)
			if !fixture.worker.deliver(ctx, fixture.pending(t)) {
				t.Fatal("re-enabled delivery was not attempted")
			}
			fixture.assertDelivered(t, 1)
		})
	}
}

type notificationDeliveryFixture struct {
	worker                            *notificationWorker
	ruleID, destinationID, deliveryID string
	requests                          atomic.Int32
}

func newNotificationDeliveryFixture(t *testing.T, pool *pgxpool.Pool) *notificationDeliveryFixture {
	t.Helper()
	fixture := &notificationDeliveryFixture{
		ruleID: uuid.NewString(), destinationID: uuid.NewString(), deliveryID: uuid.NewString(),
	}
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture.requests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(hook.Close)
	policy := &egress.Policy{
		AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
		PlainHTTPHosts:  []string{"127.0.0.1"},
	}
	fixture.worker = &notificationWorker{
		pool: pool, policy: policy, client: webhookClient(policy), log: slog.New(slog.DiscardHandler),
	}
	owner := uuid.NewString()
	fixture.exec(t, "INSERT INTO olp.users(id,email,display_name,role,etag) VALUES($1,$2,'Owner','owner',$3)",
		owner, owner+"@example.test", uuid.NewString())
	fixture.exec(t, `INSERT INTO olp.notification_destinations(id,name,url,etag,created_by) VALUES($1,$2,$3,$4,$5)`,
		fixture.destinationID, fixture.destinationID, hook.URL, uuid.NewString(), owner)
	fixture.exec(t, `INSERT INTO olp.notification_rules(id,name,event,subject_kind,subject_id,window_kind,threshold_percent,destination_id,etag,created_by)
		VALUES($1,'disable test','budget.threshold','api_key',$2,'day',50,$3,$4,$5)`,
		fixture.ruleID, uuid.NewString(), fixture.destinationID, uuid.NewString(), owner)
	fixture.exec(t, `INSERT INTO olp.notification_deliveries(id,rule_id,window_id,threshold_percent,accrued,limit_amount,status)
		VALUES($1,$2,1,50,'6','10','pending')`, fixture.deliveryID, fixture.ruleID)
	return fixture
}

func (f *notificationDeliveryFixture) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := f.worker.pool.Exec(t.Context(), sql, args...); err != nil {
		t.Fatal(err)
	}
}

func (f *notificationDeliveryFixture) setEnabled(t *testing.T, target string, enabled bool) {
	t.Helper()
	if target == "rule" {
		f.exec(t, "UPDATE olp.notification_rules SET enabled=$2 WHERE id=$1", f.ruleID, enabled)
	} else {
		f.exec(t, "UPDATE olp.notification_destinations SET enabled=$2 WHERE id=$1", f.destinationID, enabled)
	}
}

func (f *notificationDeliveryFixture) pending(t *testing.T) delivery {
	t.Helper()
	deliveries, err := f.worker.pending(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range deliveries {
		if d.id == f.deliveryID {
			return d
		}
	}
	t.Fatal("delivery is not pending")
	return delivery{}
}

func (f *notificationDeliveryFixture) deliveryState(t *testing.T) string {
	t.Helper()
	var state string
	if err := f.worker.pool.QueryRow(t.Context(), "SELECT to_jsonb(v)::text FROM olp.notification_deliveries v WHERE id=$1", f.deliveryID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

func (f *notificationDeliveryFixture) assertDelivered(t *testing.T, wantAttempts int) {
	t.Helper()
	var status string
	var attempts int
	var lastError *string
	if err := f.worker.pool.QueryRow(t.Context(), "SELECT status,attempts,last_error_code FROM olp.notification_deliveries WHERE id=$1", f.deliveryID).
		Scan(&status, &attempts, &lastError); err != nil {
		t.Fatal(err)
	}
	if status != "delivered" || attempts != wantAttempts || lastError != nil {
		t.Fatalf("delivery = (%s, %d, %v), want (delivered, %d, nil)", status, attempts, lastError, wantAttempts)
	}
	if got := f.requests.Load(); got != 1 {
		t.Fatalf("webhook requests = %d, want 1", got)
	}
}

func TestIntegrationIncidentLockSingleConnectionPool(t *testing.T) {
	ctx := t.Context()
	pool := notificationPool(t)
	var dbName string
	if err := pool.QueryRow(ctx, "SELECT current_database()").Scan(&dbName); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(os.Getenv(notificationTestEnv))
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Database = dbName
	cfg.MaxConns = 1
	single, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(single.Close)
	policy := &egress.Policy{
		AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
		PlainHTTPHosts:  []string{"127.0.0.1"},
	}
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(hook.Close)
	owner := uuid.NewString()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec("INSERT INTO olp.users(id,email,display_name,role,etag) VALUES($1,$2,'Owner','owner',$3)",
		owner, owner+"@example.test", uuid.NewString())
	destinationID, ruleID := uuid.NewString(), uuid.NewString()
	exec("INSERT INTO olp.notification_destinations(id,name,url,etag,created_by) VALUES($1,$2,$3,$4,$5)",
		destinationID, "single", hook.URL, uuid.NewString(), owner)
	exec("INSERT INTO olp.notification_rules(id,name,event,destination_id,etag,created_by) VALUES($1,'inc','worker.stale',$2,$3,$4)",
		ruleID, destinationID, uuid.NewString(), owner)
	exec("INSERT INTO olp.notification_signal_states(rule_id,subject,active,incident) VALUES($1,'task',true,1)", ruleID)
	payload := func(resolved bool) []byte {
		body, _ := json.Marshal(map[string]any{
			"subject": "task", "incident": 1, "incident_key": "olp:incident:single", "resolved": resolved,
		})
		return body
	}
	exec(`INSERT INTO olp.notification_deliveries(id,rule_id,event,dedup_key,resolved,payload,status)
		VALUES($1,$2,'worker.stale','olp:incident:single',false,$3,'pending')`, uuid.NewString(), ruleID, payload(false))
	exec(`INSERT INTO olp.notification_deliveries(id,rule_id,event,dedup_key,resolved,payload,status)
		VALUES($1,$2,'worker.stale','olp:incident:single',true,$3,'pending')`, uuid.NewString(), ruleID, payload(true))

	worker := &notificationWorker{
		pool: single, policy: policy, client: webhookClient(policy), log: slog.New(slog.DiscardHandler),
		newID: uuid7,
	}
	deliveries, err := worker.pending(ctx)
	if err != nil || len(deliveries) != 1 {
		t.Fatalf("single-pool pending = %d, %v", len(deliveries), err)
	}
	if !worker.deliver(ctx, deliveries[0]) {
		t.Fatal("trigger delivery not attempted on a single-connection pool")
	}
	deliveries, err = worker.pending(ctx)
	if err != nil || len(deliveries) != 1 {
		t.Fatalf("single-pool second pending = %d, %v", len(deliveries), err)
	}
	if !worker.deliver(ctx, deliveries[0]) {
		t.Fatal("resolve delivery not attempted on a single-connection pool")
	}
	locked, err := single.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	backendPID := locked.Conn().PgConn().PID()
	locked.Release()
	var held bool
	if err = single.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_locks WHERE locktype='advisory' AND granted AND pid=$1)`,
		backendPID).Scan(&held); err != nil {
		t.Fatal(err)
	}
	if held {
		t.Fatal("backend still holds the advisory lock")
	}
	var statuses []string
	rows, err := single.Query(ctx, "SELECT status FROM olp.notification_deliveries WHERE rule_id=$1 ORDER BY created_at", ruleID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var status string
		if err = rows.Scan(&status); err != nil {
			t.Fatal(err)
		}
		statuses = append(statuses, status)
	}
	rows.Close()
	if len(statuses) != 2 || statuses[0] != "delivered" || statuses[1] != "delivered" {
		t.Fatalf("delivery statuses %v", statuses)
	}
}

func TestIntegrationIncidentLockRetiresBrokenConn(t *testing.T) {
	ctx := t.Context()
	pool := notificationPool(t)
	var dbName string
	if err := pool.QueryRow(ctx, "SELECT current_database()").Scan(&dbName); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(os.Getenv(notificationTestEnv))
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Database = dbName
	cfg.MaxConns = 1
	single, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	policy := &egress.Policy{
		AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
		PlainHTTPHosts:  []string{"127.0.0.1"},
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		hook.Close()
	})
	owner := uuid.NewString()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec("INSERT INTO olp.users(id,email,display_name,role,etag) VALUES($1,$2,'Owner','owner',$3)",
		owner, owner+"@example.test", uuid.NewString())
	destinationID, ruleID := uuid.NewString(), uuid.NewString()
	exec("INSERT INTO olp.notification_destinations(id,name,url,etag,created_by) VALUES($1,$2,$3,$4,$5)",
		destinationID, "broken", hook.URL, uuid.NewString(), owner)
	exec("INSERT INTO olp.notification_rules(id,name,event,destination_id,etag,created_by) VALUES($1,'inc','worker.stale',$2,$3,$4)",
		ruleID, destinationID, uuid.NewString(), owner)
	exec("INSERT INTO olp.notification_signal_states(rule_id,subject,active,incident) VALUES($1,'task',true,1)", ruleID)
	payload, _ := json.Marshal(map[string]any{
		"subject": "task", "incident": 1, "incident_key": "olp:incident:broken", "resolved": false,
	})
	exec(`INSERT INTO olp.notification_deliveries(id,rule_id,event,dedup_key,resolved,payload,status)
		VALUES($1,$2,'worker.stale','olp:incident:broken',false,$3,'pending')`, uuid.NewString(), ruleID, payload)

	worker := &notificationWorker{
		pool: single, policy: policy, client: webhookClient(policy), log: slog.New(slog.DiscardHandler),
		newID: uuid7,
	}
	probe, err := single.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var backendPID uint32
	if err := probe.Conn().QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&backendPID); err != nil {
		probe.Release()
		t.Fatal(err)
	}
	probe.Release()
	deliveries, err := worker.pending(ctx)
	if err != nil || len(deliveries) != 1 {
		t.Fatalf("pending = %d, %v", len(deliveries), err)
	}
	attempted := make(chan bool, 1)
	go func() { attempted <- worker.deliver(ctx, deliveries[0]) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		releaseOnce.Do(func() { close(release) })
		t.Fatal("delivery never reached the hook")
	}
	var terminated bool
	if err := pool.QueryRow(ctx, "SELECT pg_terminate_backend($1)", backendPID).Scan(&terminated); err != nil || !terminated {
		releaseOnce.Do(func() { close(release) })
		t.Fatalf("backend termination = %v, %v", terminated, err)
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case <-attempted:
	case <-time.After(5 * time.Second):
		t.Fatal("delivery never completed after backend termination")
	}
	var held bool
	if err = pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_locks WHERE locktype='advisory' AND granted AND pid=$1)`, backendPID).Scan(&held); err != nil {
		t.Fatal(err)
	}
	if held {
		t.Fatal("terminated backend still holds the advisory lock")
	}
	acquireCtx, acquireCancel := context.WithTimeout(ctx, 5*time.Second)
	replacement, err := single.Acquire(acquireCtx)
	acquireCancel()
	if err != nil {
		t.Fatalf("pool cannot replace the terminated connection: %v", err)
	}
	var newPID uint32
	if err := replacement.Conn().QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&newPID); err != nil {
		replacement.Release()
		t.Fatal(err)
	}
	replacement.Release()
	if newPID == backendPID {
		t.Fatal("pool returned the terminated backend")
	}
	single.Close()
}
