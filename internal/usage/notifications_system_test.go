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
	"testing"

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
