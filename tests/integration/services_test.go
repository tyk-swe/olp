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
	glide "github.com/valkey-io/valkey-glide/go/v2"
)

func required(t testing.TB, name string) string {
	t.Helper()
	value := os.Getenv(name)
	if value == "" {
		t.Fatalf("%s is required; run make integration", name)
	}
	return value
}

// eventually waits for a condition something outside the request path decides:
// a worker settles accounting, a detector marks a replica stale, a refreshed
// grant lands. It polls until the timeout and fails the test if the condition
// never holds.
func eventually(t *testing.T, what string, timeout time.Duration, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if check() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		select {
		case <-t.Context().Done():
			t.Fatalf("waiting for %s: %v", what, t.Context().Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
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
