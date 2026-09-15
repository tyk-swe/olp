//go:build integration

package integration_test

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/limits"
)

func TestWrongTypeLimitStateIsNotAnOutage(t *testing.T) {
	c := limClient(t)
	ns := limNamespace(t, c, "wrong-type")
	l := limLimiter(t, c, ns)
	lookup := limLookup()
	rate, concurrency := limRateKeys(ns, lookup)
	for _, malformed := range []string{rate, concurrency} {
		do(t, c, "DEL", rate, concurrency)
		do(t, c, "SET", malformed, "corrupt")
		if _, err := l.Reserve(t.Context(), limRequest(lookup)); !errors.Is(err, limits.ErrMalformedState) {
			t.Fatalf("wrong-type state: %v, want a semantic error, not ServiceError", err)
		}
	}
}

func TestProviderUsageRejectsMalformedCounters(t *testing.T) {
	c := limClient(t)
	ns := limNamespace(t, c, "invalid-usage")
	l := limLimiter(t, c, ns)
	lookup := limLookup()
	rate, _ := limRateKeys(ns, lookup)
	for _, value := range []string{"garbage", "1.5", "-1", "9007199254740992"} {
		window := limSettleInMinute(t, c, time.Second) / 60000
		do(t, c, "HSET", rate, "window", strconv.FormatInt(window, 10), "rpm", value, "tpm", "0")
		if _, err := l.ProviderUsage(t.Context(), lookup); err == nil {
			t.Fatalf("malformed counter %q reported as idle usage", value)
		}
	}
}

func TestProviderConcurrencyCoversTheWholeStream(t *testing.T) {
	c := limClient(t)
	f := glSeed(t, limLimiter(t, c, limNamespace(t, c, "stream-lease")), limits.FailClosed, 1000)
	listPath := f.path + "/credential-slots"
	slots := f.h.want(f.owner, "GET", listPath, nil, nil, 200)
	slotID := slots["items"].([]any)[0].(map[string]any)["id"].(string)
	f.h.want(f.owner, "PUT", listPath+"/"+slotID, map[string]any{
		"slot": map[string]any{"name": "default", "enabled": true, "max_concurrency": 1},
	}, withMatch(slots, map[string]string{"Idempotency-Key": "stream-slot"}), 200)
	f.activate("activate-stream-slot")
	_, secret := f.key("stream-lease", nil)
	f.vendor.delay.Store(int64(500 * time.Millisecond))
	before := f.vendor.chats.Load()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	type result struct {
		status int
		err    error
	}
	done := make(chan result, 1)
	go func() {
		status, err := glStream(ctx, f.h, secret)
		done <- result{status, err}
	}()
	glInFlight(t, f.vendor, before)
	// Frames arrive within the idle deadline, but the stream outlives the
	// target's first-byte timeout. Its concurrency lease must still exist.
	time.Sleep(1200 * time.Millisecond)
	select {
	case r := <-done:
		t.Fatalf("stream ended before the concurrency check: %+v", r)
	default:
	}
	if status, code, _ := f.chat(secret); status != http.StatusTooManyRequests || code != "rate_limit_exceeded" {
		t.Fatalf("overlapping request admitted: %d %s", status, code)
	}
	if r := <-done; r.status != http.StatusOK || r.err != nil {
		t.Fatalf("stream failed: %+v", r)
	}
}
