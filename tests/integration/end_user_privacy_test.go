//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/gateway"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/telemetry"
	"github.com/tyk-swe/olp/internal/usage"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type endUserAuditLog struct {
	mu   sync.Mutex
	data []byte
}

func (l *endUserAuditLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.data = append(l.data, p...)
	return len(p), nil
}

func TestEndUserConcurrentCostBudgetsKeepTelemetryPrivate(t *testing.T) {
	for _, dimension := range []string{"daily_cost_limit", "monthly_cost_limit"} {
		t.Run(dimension, func(t *testing.T) { testEndUserConcurrentCostBudget(t, dimension) })
	}
}

func testEndUserConcurrentCostBudget(t *testing.T, dimension string) {
	f := glSeedIn(t, "private-end-user", glPrice{input: "1000000", output: "1000000"})
	id, secret := f.key("private", map[string]any{
		"end_user_source": "native",
		"end_user_policy": map[string]any{"defaults": map[string]any{dimension: "100"}},
	})
	const identifier = "private-customer-sentinel"
	digest := access.DigestEndUser(f.h.Server.Auth, nil, identifier)
	owner := limits.EndUserKeyID(id, digest)
	var logBuffer endUserAuditLog
	log := slog.New(slog.NewJSONHandler(&logBuffer, &slog.HandlerOptions{Level: slog.LevelDebug}))
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter), sdktrace.WithSampler(sdktrace.AlwaysSample()))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	var gateways []*accessHarness
	for range 2 {
		h := newAccessHarnessOn(t, f.h.Pool, f.h.DBURL, log)
		h.Gateway.Admission = gateway.NewAdmission(limLimiter(t, limClient(t), f.namespace), nil, log)
		h.refresh()
		// Use the production tracing middleware without replacing a live server's handler.
		view := *h
		view.HTTP = httptest.NewServer(telemetry.AdmittedRequest(telemetry.RequestConfig{}, provider.Tracer("end-user-test"), "openai", h.HTTP.Config.Handler))
		t.Cleanup(view.HTTP.Close)
		gateways = append(gateways, &view)
	}
	valkey, prefix, stream := acctKeyspace(t)
	emitter := usage.NewEmitter(64)
	sink := &gateway.AccountingSink{Emitter: emitter, Log: log}
	for _, h := range gateways {
		h.Gateway.Sink = sink
	}
	ctx, cancel := context.WithCancel(t.Context())
	written := make(chan struct{})
	go func() { defer close(written); emitter.RunWriter(ctx, valkey, stream, log) }()
	t.Cleanup(func() { emitter.Close(); <-written; cancel() })
	stop := acctConsumer(t, f.h.Pool, acctValkey(t), stream, "private-end-users", f.limiter, 30*time.Second)
	if status, err := endUserChat(t.Context(), gateways[0], secret, routeSlug, identifier); err != nil || status != 503 {
		t.Fatalf("unknown balance: %d %v", status, err)
	}
	day, month := limCostKeys(f.namespace, owner)
	glEventually(t, "authoritative end-user balance", func() bool {
		value, err := valkey.Do(t.Context(), "HGET", day, "accrued")
		return err == nil && value != nil
	})
	f.vendor.delay.Store(int64(20 * time.Millisecond))
	const requests = 20
	results := make(chan int, requests)
	var wg sync.WaitGroup
	for i := range requests {
		wg.Go(func() {
			status, err := endUserChatMode(t.Context(), gateways[i%2], secret, routeSlug, identifier, true)
			if err != nil {
				t.Error(err)
			}
			results <- status
		})
	}
	wg.Wait()
	close(results)
	admitted := 0
	for status := range results {
		switch status {
		case 200:
			admitted++
		case 429:
		default:
			t.Fatalf("unexpected admission status %d", status)
		}
	}
	// Every successful fixture completion bills exactly ten tokens at $1/token.
	if admitted == 0 || admitted > 10 {
		t.Fatalf("budget admitted %d ten-dollar requests", admitted)
	}
	glEventually(t, "all request metadata", func() bool {
		var count int
		err := f.h.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.requests WHERE api_key_id=$1", id).Scan(&count)
		return err == nil && count == requests+1
	})
	wantCost := big.NewRat(int64(admitted*10), 1)
	glEventually(t, "settled daily and monthly balances", func() bool {
		for _, key := range []string{day, month} {
			value, err := valkey.Do(t.Context(), "HGET", key, "accrued")
			if err != nil {
				return false
			}
			actual, ok := new(big.Rat).SetString(fmt.Sprint(value))
			if !ok || actual.Cmp(wantCost) != 0 {
				return false
			}
		}
		return true
	})
	glEventually(t, "settled cost reservations", func() bool {
		reserved, err := f.limiter.Reserved(t.Context(), owner)
		return err == nil && reserved == "0"
	})
	stop()
	for _, table := range []string{"requests", "attempt_usage_facts", "end_user_accounts", "end_user_cost_windows", "audit"} {
		var leaked bool
		if err := f.h.Pool.QueryRow(t.Context(), "SELECT EXISTS(SELECT 1 FROM olp."+table+" r WHERE to_jsonb(r)::text LIKE $1)", "%"+identifier+"%").Scan(&leaked); err != nil || leaked {
			t.Fatalf("privacy check on %s: leak=%v error=%v", table, leaked, err)
		}
	}
	for _, pattern := range []string{prefix + "*", f.namespace + "*"} {
		keys := do(t, valkey, "KEYS", pattern).([]any)
		if len(keys) == 0 {
			t.Fatal("privacy check found no Valkey state")
		}
		for _, item := range keys {
			key := limText(t, item)
			var value any
			switch limText(t, do(t, valkey, "TYPE", key)) {
			case "hash":
				value = do(t, valkey, "HGETALL", key)
			case "stream":
				value = do(t, valkey, "XRANGE", key, "-", "+")
			case "string":
				value = do(t, valkey, "GET", key)
			case "zset":
				value = do(t, valkey, "ZRANGE", key, "0", "-1", "WITHSCORES")
			case "set":
				value = do(t, valkey, "SMEMBERS", key)
			default:
				t.Fatalf("uninspected Valkey type for %s", key)
			}
			raw, err := json.Marshal([]any{key, value})
			if err != nil || strings.Contains(string(raw), identifier) {
				t.Fatalf("Valkey retained raw identity: %v", err)
			}
		}
	}
	glEventually(t, "completed request traces", func() bool {
		count := 0
		for _, span := range exporter.GetSpans() {
			if span.Name == "request" {
				count++
			}
		}
		return count == requests+1
	})
	traces, err := json.Marshal(exporter.GetSpans())
	if err != nil || strings.Contains(string(traces), identifier) {
		t.Fatalf("trace retained raw identity: %v", err)
	}
	logBuffer.mu.Lock()
	defer logBuffer.mu.Unlock()
	if len(logBuffer.data) == 0 || strings.Contains(string(logBuffer.data), identifier) {
		t.Fatal("logs missing or retained raw identity")
	}
}
