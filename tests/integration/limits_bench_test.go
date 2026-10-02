//go:build integration

package integration_test

import (
	"context"
	"crypto/rand"
	"os"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/coordination"
	"github.com/tyk-swe/olp/internal/limits"
)

var limScriptTime = regexp.MustCompile(`cmdstat_eval(?:sha)?:calls=(\d+),usec=(\d+),`)

// limScriptMicroseconds is how long the Valkey has spent inside scripts and how
// many it has run, by digest and by source together.
func limScriptMicroseconds(b *testing.B, c *coordination.Client) (calls, microseconds int64) {
	b.Helper()
	text, err := c.Do(context.Background(), "INFO", "commandstats")
	if err != nil {
		b.Fatalf("INFO commandstats: %v", err)
	}
	for _, match := range limScriptTime.FindAllStringSubmatch(text.(string), -1) {
		n, _ := strconv.ParseInt(match[1], 10, 64)
		us, _ := strconv.ParseInt(match[2], 10, 64)
		calls, microseconds = calls+n, microseconds+us
	}
	return calls, microseconds
}

// limBenchLimiter opens the Valkey the integration run provides and a limiter over
// a namespace of its own, which is emptied when the benchmark ends.
func limBenchLimiter(b *testing.B) (*coordination.Client, *limits.Limiter) {
	b.Helper()
	raw := os.Getenv("OLP_TEST_VALKEY_URL")
	if raw == "" {
		b.Fatal("OLP_TEST_VALKEY_URL is required; run make integration")
	}
	cfg, err := coordination.Configuration(raw, "", 5*time.Second)
	if err != nil {
		b.Fatal(err)
	}
	c, err := coordination.Open(context.Background(), cfg)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(c.Close)
	namespace := "olp-test:limits:bench:" + rand.Text()
	limiter, err := limits.New(c, namespace)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		ctx := context.Background()
		keys, err := c.Do(ctx, "KEYS", namespace+"*")
		if items, ok := keys.([]any); err == nil && ok {
			for _, key := range items {
				c.Do(ctx, "DEL", key.(string))
			}
		}
	})
	return c, limiter
}

// BenchmarkLimitsPricedRequest runs what one priced request on a key with a cost
// budget asks of Valkey: admission, the settlement as it ends, and the accrual
// that records it. Its own time is not the point, since a Valkey on the same
// machine answers in microseconds. valkey-us/op is: the time the server spent
// inside the scripts, on the one thread that every replica shares, which bounds
// how many such requests a second a single Valkey can admit.
//
//	OLP_TEST_VALKEY_URL=... go test -tags=integration -run '^$' -bench LimitsPricedRequest ./tests/integration
func BenchmarkLimitsPricedRequest(b *testing.B) {
	c, limiter := limBenchLimiter(b)
	owner, windows := uuid.NewString(), limits.BudgetWindows(time.Now())
	snapshot := limits.CostSnapshot{
		CostOwnerID: owner, DailyWindowID: windows.DailyID, DailyAccrued: "0.5",
		MonthlyWindowID: windows.MonthlyID, MonthlyAccrued: "0.5",
	}
	ctx := context.Background()
	if _, _, err := limiter.ApplyCostSnapshot(ctx, snapshot); err != nil {
		b.Fatal(err)
	}
	lookup, daily, monthly := limLookup(), "1000", "10000"
	cycle := func() {
		request := limits.Request{
			CostOwnerID: owner, LookupID: lookup, DailyCostLimit: &daily, MonthlyCostLimit: &monthly,
			LeaseTTL: 5 * time.Minute, CostEstimate: "0.4", RequestID: uuid.NewString(),
		}
		lease, err := limiter.Reserve(ctx, request)
		if err != nil {
			b.Fatalf("Reserve: %v", err)
		}
		lease.SetActualCost("0.1")
		if err := lease.SettleCost(ctx); err != nil {
			b.Fatalf("SettleCost: %v", err)
		}
		snapshot.RequestID = request.RequestID
		if _, _, err := limiter.ApplyCostSnapshot(ctx, snapshot); err != nil {
			b.Fatalf("ApplyCostSnapshot: %v", err)
		}
	}
	// The scripts are sent once and cached, which is not what is measured.
	cycle()
	callsBefore, usBefore := limScriptMicroseconds(b, c)
	for b.Loop() {
		cycle()
	}
	b.StopTimer()
	calls, us := limScriptMicroseconds(b, c)
	b.ReportMetric(float64(us-usBefore)/float64(b.N), "valkey-us/op")
	b.ReportMetric(float64(calls-callsBefore)/float64(b.N), "scripts/op")
}

// BenchmarkLimitsRateReservation reserves one request against the rate script as
// each kind of reservation that reaches it does, with the allowance asked for and
// without. The reply is decoded for the caller before it is parsed, field by
// field, so what allocs/op and B/op say is what the size of a reply costs the
// process that reserves: a provider's quota is reserved on every attempt and
// reports nothing, and a key bound by concurrency alone has no allowance to
// report, so neither should be made to receive one. The limits are far above what
// the loop uses, so every reservation is granted.
//
//	OLP_TEST_VALKEY_URL=... go test -tags=integration -run '^$' -bench LimitsRateReservation -benchmem ./tests/integration
func BenchmarkLimitsRateReservation(b *testing.B) {
	_, limiter := limBenchLimiter(b)
	const huge = int64(1) << 40
	for _, shape := range []struct {
		name  string
		shape func(*limits.Request)
	}{
		{"requests and tokens", func(*limits.Request) {}},
		{"requests only", func(r *limits.Request) { r.TokensPerMinute, r.RequestedTokens = nil, 0 }},
		{"concurrency only", func(r *limits.Request) { r.RequestsPerMinute, r.TokensPerMinute, r.RequestedTokens = nil, nil, 0 }},
	} {
		for _, ask := range []bool{false, true} {
			name := shape.name + " unasked"
			if ask {
				name = shape.name + " asked"
			}
			b.Run(name, func(b *testing.B) {
				request := limRequest(limLookup())
				request.RequestsPerMinute, request.TokensPerMinute, request.MaxConcurrency = limPointer(huge), limPointer(huge), limPointer(huge)
				request.ReportRate = ask
				shape.shape(&request)
				ctx := context.Background()
				// The script is sent once and cached, which is not what is measured.
				if _, err := limiter.Reserve(ctx, request); err != nil {
					b.Fatalf("Reserve: %v", err)
				}
				b.ReportAllocs()
				for b.Loop() {
					if _, err := limiter.Reserve(ctx, request); err != nil {
						b.Fatalf("Reserve: %v", err)
					}
				}
			})
		}
	}
}
