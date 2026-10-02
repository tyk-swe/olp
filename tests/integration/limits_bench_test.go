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

// BenchmarkLimitsPricedRequest runs what one priced request on a key with a cost
// budget asks of Valkey: admission, the settlement as it ends, and the accrual
// that records it. Its own time is not the point, since a Valkey on the same
// machine answers in microseconds. valkey-us/op is: the time the server spent
// inside the scripts, on the one thread that every replica shares, which bounds
// how many such requests a second a single Valkey can admit.
//
//	OLP_TEST_VALKEY_URL=... go test -tags=integration -run '^$' -bench LimitsPricedRequest ./tests/integration
func BenchmarkLimitsPricedRequest(b *testing.B) {
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
	owner, windows := uuid.NewString(), limits.BudgetWindows(time.Now())
	snapshot := limits.CostSnapshot{
		CostOwnerID: owner, DailyWindowID: windows.DailyID, DailyAccrued: "0.5",
		MonthlyWindowID: windows.MonthlyID, MonthlyAccrued: "0.5",
	}
	ctx := context.Background()
	if _, _, err := limiter.ApplyCostSnapshot(ctx, snapshot); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		keys, err := c.Do(ctx, "KEYS", namespace+"*")
		if items, ok := keys.([]any); err == nil && ok {
			for _, key := range items {
				c.Do(ctx, "DEL", key.(string))
			}
		}
	})
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
