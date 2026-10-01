//go:build integration

package usage

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
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

// Disabling a rule or destination after pending read must preserve both fresh
// deliveries and their last retry. Re-enabling resumes the same attempt.
func TestIntegrationDisabledNotificationDoesNotConsumeAttempt(t *testing.T) {
	pool := notificationPool(t)
	for _, disabled := range []string{"rule", "destination"} {
		for _, attempts := range []int{0, maxDeliveryAttempts - 1} {
			t.Run(fmt.Sprintf("%s/attempts_%d", disabled, attempts), func(t *testing.T) {
				fixture := newNotificationDeliveryFixture(t, pool)
				if attempts > 0 {
					fixture.exec(t, `UPDATE olp.notification_deliveries SET status='failed',attempts=$2,
						last_attempt_at=now()-interval '1 day',last_error_code='http_5xx' WHERE id=$1`, fixture.deliveryID, attempts)
				}
				d := fixture.pending(t)
				before := fixture.deliveryState(t)
				fixture.setEnabled(t, disabled, false)
				for range maxDeliveryAttempts + 1 {
					if fixture.worker.deliver(t.Context(), d) {
						t.Fatal("disabled delivery was attempted")
					}
				}
				if after := fixture.deliveryState(t); after != before {
					t.Fatalf("disabled delivery changed:\nbefore: %s\nafter:  %s", before, after)
				}
				if got := fixture.requests.Load(); got != 0 {
					t.Fatalf("disabled webhook requests = %d, want 0", got)
				}
				fixture.setEnabled(t, disabled, true)
				if !fixture.worker.deliver(t.Context(), fixture.pending(t)) {
					t.Fatal("re-enabled delivery was not attempted")
				}
				fixture.assertDelivered(t, attempts+1)
			})
		}
	}
}

// Rolling back a disabled claim must not undo another replica's claim or let
// replicas send the same attempt twice when the destination is enabled again.
func TestIntegrationConcurrentNotificationClaimsAfterDisable(t *testing.T) {
	pool := notificationPool(t)
	fixture := newNotificationDeliveryFixture(t, pool)
	d := fixture.pending(t)
	before := fixture.deliveryState(t)
	claimConcurrently := func() int32 {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		var attempts atomic.Int32
		start := make(chan struct{})
		var workers sync.WaitGroup
		for range 8 {
			workers.Go(func() {
				<-start
				if fixture.worker.deliver(ctx, d) {
					attempts.Add(1)
				}
			})
		}
		close(start)
		workers.Wait()
		if err := ctx.Err(); err != nil {
			t.Fatal(err)
		}
		return attempts.Load()
	}
	fixture.setEnabled(t, "destination", false)
	if got := claimConcurrently(); got != 0 {
		t.Fatalf("disabled attempts = %d, want 0", got)
	}
	if after := fixture.deliveryState(t); after != before {
		t.Fatalf("disabled delivery changed:\nbefore: %s\nafter:  %s", before, after)
	}
	fixture.setEnabled(t, "destination", true)
	if got := claimConcurrently(); got != 1 {
		t.Fatalf("re-enabled attempts = %d, want 1", got)
	}
	fixture.assertDelivered(t, 1)
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
