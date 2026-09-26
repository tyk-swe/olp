//go:build integration

package integration_test

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"runtime"
	"runtime/pprof"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/coordination"
	"github.com/tyk-swe/olp/internal/database"
	glide "github.com/valkey-io/valkey-glide/go/v2"
	"github.com/valkey-io/valkey-glide/go/v2/config"
	"github.com/valkey-io/valkey-glide/go/v2/models"
)

func required(t *testing.T, name string) string {
	t.Helper()
	value := os.Getenv(name)
	if value == "" {
		t.Fatalf("%s is required; run make integration", name)
	}
	return value
}

func client(t *testing.T, rawURL string, timeout time.Duration) *coordination.Client {
	t.Helper()
	cfg, err := coordination.Configuration(rawURL, "", timeout)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	c, err := coordination.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func do(t *testing.T, c *coordination.Client, args ...string) any {
	t.Helper()
	value, err := c.Do(t.Context(), args...)
	if err != nil {
		t.Fatalf("%s: %v", args[0], err)
	}
	return value
}

func TestPostgresTransactionsCancellationAuthenticationAndTLS(t *testing.T) {
	cfg, err := database.Configuration(required(t, "OLP_TEST_DATABASE_URL"), 3, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := database.Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), "CREATE TEMP TABLE foundation_test (id integer PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), "INSERT INTO foundation_test VALUES ($1)", 42); err != nil {
		t.Fatal(err)
	}
	var id int
	if err := tx.QueryRow(t.Context(), "SELECT id FROM foundation_test").Scan(&id); err != nil || id != 42 {
		t.Fatalf("query: %d %v", id, err)
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if _, err := pool.Exec(ctx, "SELECT pg_sleep(2)"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("query cancellation: %v", err)
	}
	if err := pool.Ping(t.Context()); err != nil {
		t.Fatalf("pool did not recover: %v", err)
	}
	bad := cfg.Copy()
	bad.ConnConfig.Password = "wrong-password"
	if p, err := database.Open(t.Context(), bad); err == nil {
		p.Close()
		t.Fatal("accepted invalid PostgreSQL credentials")
	}
	tls, err := database.Configuration(required(t, "OLP_TEST_DATABASE_TLS_URL"), 2, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	p, err := database.Open(t.Context(), tls)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	var encrypted bool
	if err := p.QueryRow(t.Context(), "SELECT ssl FROM pg_stat_ssl WHERE pid = pg_backend_pid()").Scan(&encrypted); err != nil || !encrypted {
		t.Fatalf("TLS: %v %v", encrypted, err)
	}
}

func TestValkeyScriptsStreamsRecoveryAndIsolation(t *testing.T) {
	c := client(t, required(t, "OLP_TEST_VALKEY_URL"), time.Second)
	prefix := "olp-test:" + rand.Text()
	key, stream := prefix+":counter", prefix+":events"
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		c.Do(ctx, "DEL", key, stream)
	})
	value := do(t, c, "EVAL", "local n = redis.call('INCR', KEYS[1]); return {n, redis.call('TIME')[1]}", "1", key)
	if !strings.Contains(fmt.Sprint(value), "1") {
		t.Fatalf("Lua/TIME: %v", value)
	}
	if other := do(t, c, "GET", prefix+":other-installation:counter"); other != nil {
		t.Fatal("installation keys collided")
	}
	do(t, c, "XGROUP", "CREATE", stream, "workers", "0", "MKSTREAM")
	id := fmt.Sprint(do(t, c, "XADD", stream, "*", "event", "usage"))
	read := do(t, c, "XREADGROUP", "GROUP", "workers", "dead-worker", "COUNT", "1", "STREAMS", stream, ">")
	if !strings.Contains(fmt.Sprint(read), id) {
		t.Fatalf("stream read: %v", read)
	}
	pending := do(t, c, "XPENDING", stream, "workers")
	if !strings.Contains(fmt.Sprint(pending), id) {
		t.Fatalf("pending lost: %v", pending)
	}
	claimed := do(t, c, "XAUTOCLAIM", stream, "workers", "replacement", "0", "0-0", "COUNT", "10")
	if !strings.Contains(fmt.Sprint(claimed), id) {
		t.Fatalf("pending not recovered: %v", claimed)
	}
	if ack := do(t, c, "XACK", stream, "workers", id); fmt.Sprint(ack) != "1" {
		t.Fatalf("ack: %v", ack)
	}
	if empty := do(t, c, "XREADGROUP", "GROUP", "workers", "replacement", "BLOCK", "20", "COUNT", "1", "STREAMS", stream, ">"); empty != nil {
		t.Fatalf("blocking read: %v", empty)
	}
}

func TestValkeyTLSAuthenticationAndPubSubHints(t *testing.T) {
	raw := required(t, "OLP_TEST_VALKEY_URL")
	tls, err := coordination.Configuration(required(t, "OLP_TEST_VALKEY_TLS_URL"), required(t, "OLP_TEST_CA_FILE"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c, err := coordination.Open(t.Context(), tls)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Ping(t.Context()); err != nil {
		t.Fatal(err)
	}
	bad, _ := coordination.Configuration(strings.Replace(raw, "olp-local", "wrong-password", 1), "", 100*time.Millisecond)
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	if c, err := coordination.Open(ctx, bad); err == nil {
		c.Close()
		t.Fatal("accepted invalid Valkey credentials")
	}
	channel := "olp-test:" + rand.Text() + ":hints"
	ch := make(chan string, 1)
	scfg, err := coordination.Configuration(raw, "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	scfg.WithSubscriptionConfig(config.NewStandaloneSubscriptionConfig().WithCallback(func(m *models.PubSubMessage, _ any) {
		select {
		case ch <- m.Message:
		default:
		}
	}, nil))
	subscriber, err := glide.NewClient(scfg)
	if err != nil {
		t.Fatal(err)
	}
	defer subscriber.Close()
	if err := subscriber.Subscribe(t.Context(), []string{channel}, 1000); err != nil {
		t.Fatal(err)
	}
	if count := do(t, c, "PUBLISH", channel, "refresh"); fmt.Sprint(count) != "1" {
		t.Fatalf("subscription: %v", count)
	}
	select {
	case value := <-ch:
		if value != "refresh" {
			t.Fatal(value)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("missing PubSub hint")
	}
}

func TestValkeyCancellationDisconnectReconnectAndClose(t *testing.T) {
	raw := required(t, "OLP_TEST_VALKEY_URL")
	c := client(t, raw, 100*time.Millisecond)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := c.Do(ctx, "INCR", "must-not-dispatch")
	var outcome *coordination.CommandError
	if !errors.As(err, &outcome) || outcome.Ambiguous {
		t.Fatalf("pre-dispatch cancellation: %v", err)
	}
	// Pausing command processing forces a transport deadline while the queued
	// increment still executes afterward. Retrying would duplicate the write.
	key := "olp-test:" + rand.Text() + ":ambiguous"
	t.Cleanup(func() { c.Do(context.Background(), "DEL", key) })
	do(t, c, "CLIENT", "PAUSE", "300", "ALL")
	_, err = c.Do(t.Context(), "INCR", key)
	var requestTimeout *glide.TimeoutError
	if !errors.As(err, &outcome) || !outcome.Ambiguous || !errors.As(err, &requestTimeout) {
		t.Fatalf("request timeout was not reported as ambiguous: %v", err)
	}
	time.Sleep(250 * time.Millisecond)
	if value := do(t, c, "GET", key); fmt.Sprint(value) != "1" {
		t.Fatalf("timed-out write did not execute exactly once: %v", value)
	}
	// A bounded blocking operation remains ambiguous after dispatch. The native
	// response eventually releases the canceled call's cleanup goroutine.
	before := runtime.NumGoroutine()
	for range 20 {
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Millisecond)
		_, err := c.Do(ctx, "BLPOP", "olp-test:"+rand.Text(), "0.03")
		cancel()
		if !errors.As(err, &outcome) || !outcome.Ambiguous {
			t.Fatalf("in-flight cancellation: %v", err)
		}
	}
	killer := client(t, raw, time.Second)
	do(t, killer, "CLIENT", "KILL", "TYPE", "normal", "SKIPME", "yes")
	deadline := time.Now().Add(5 * time.Second)
	for c.Ping(t.Context()) != nil {
		if time.Now().After(deadline) {
			t.Fatal("GLIDE failed to reconnect")
		}
		time.Sleep(20 * time.Millisecond)
	}
	for range 10 {
		closing := client(t, raw, 100*time.Millisecond)
		done := make(chan error, 1)
		go func() { _, err := closing.Do(t.Context(), "BLPOP", "olp-test:"+rand.Text(), "1"); done <- err }()
		time.Sleep(5 * time.Millisecond)
		closing.Close()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("closed in-flight request succeeded")
			}
		case <-time.After(time.Second):
			t.Fatal("close stranded in-flight request")
		}
	}
	for range 10 {
		closing := client(t, raw, 100*time.Millisecond)
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Millisecond)
		_, err := closing.Do(ctx, "BLPOP", "olp-test:"+rand.Text(), "0.03")
		cancel()
		if !errors.As(err, &outcome) || !outcome.Ambiguous {
			t.Fatalf("cancel before close: %v", err)
		}
		closing.Close()
	}
	deadline = time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > before+4 {
		if time.Now().After(deadline) {
			var dump strings.Builder
			pprof.Lookup("goroutine").WriteTo(&dump, 2)
			t.Fatalf("abandoned goroutines: before %d after %d\n%s", before, runtime.NumGoroutine(), dump.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}
