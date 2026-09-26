//go:build integration

package integration_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/coordination"
	"github.com/tyk-swe/olp/internal/database"
	"github.com/tyk-swe/olp/internal/limits"
)

// limNilUUID is the API key identity used by tests that exercise rate limits
// only, where the cost keys are never touched.
const limNilUUID = "00000000-0000-0000-0000-000000000000"

// limMaximum is the largest integer Lua represents exactly, which is the
// ceiling every counter in the scripts saturates at.
const limMaximum = int64(9007199254740991)

func limClient(t *testing.T) *coordination.Client {
	t.Helper()
	return client(t, required(t, "OLP_TEST_VALKEY_URL"), 5*time.Second)
}

// limNamespace gives one test its own Valkey namespace and removes every key it
// created afterwards. The cleanup is registered after the client's own, so it
// still runs while the connection is open.
func limNamespace(t *testing.T, c *coordination.Client, label string) string {
	t.Helper()
	namespace := "olp-test:limits:" + label + ":" + rand.Text()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		value, err := c.Do(ctx, "KEYS", namespace+"*")
		if err != nil {
			t.Errorf("cleanup KEYS: %v", err)
			return
		}
		items, ok := value.([]any)
		if !ok {
			t.Errorf("cleanup KEYS = %#v", value)
			return
		}
		for _, item := range items {
			key, ok := item.(string)
			if !ok {
				t.Errorf("cleanup KEYS entry = %#v", item)
				continue
			}
			if _, err := c.Do(ctx, "DEL", key); err != nil {
				t.Errorf("cleanup DEL %s: %v", key, err)
			}
		}
	})
	return namespace
}

func limLimiter(t *testing.T, c *coordination.Client, namespace string) *limits.Limiter {
	t.Helper()
	limiter, err := limits.New(c, namespace)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return limiter
}

// limLookup mints a lookup identifier in the shape the gateway derives from an
// API key: short, opaque, and unique per test.
func limLookup() string { return "lk" + strings.ToLower(rand.Text()[:12]) }

func limPointer[T any](value T) *T { return &value }

// limRequest is the reservation every scenario starts from; each test narrows
// the dimension it is about.
func limRequest(lookup string) limits.Request {
	return limits.Request{
		CostOwnerID:       limNilUUID,
		LookupID:          lookup,
		RequestsPerMinute: limPointer(int64(10)),
		TokensPerMinute:   limPointer(int64(1000)),
		MaxConcurrency:    limPointer(int64(10)),
		RequestedTokens:   5,
		LeaseTTL:          5 * time.Second,
	}
}

func limRateKeys(namespace, lookup string) (string, string) {
	prefix := namespace + ":{" + lookup + "}"
	return prefix + ":rate", prefix + ":concurrency"
}

func limCostKeys(namespace, apiKeyID string) (string, string) {
	prefix := namespace + ":{" + strings.ReplaceAll(apiKeyID, "-", "") + "}:cost"
	return prefix + ":day", prefix + ":month"
}

func limText(t *testing.T, value any) string {
	t.Helper()
	text, ok := value.(string)
	if !ok {
		t.Fatalf("reply %#v is not a string", value)
	}
	return text
}

func limNumber(t *testing.T, text string) int64 {
	t.Helper()
	number, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		t.Fatalf("parse %q: %v", text, err)
	}
	return number
}

// limServerTimeMS reads Valkey's own clock, which is the only clock the scripts
// and these assertions may use.
func limServerTimeMS(t *testing.T, c *coordination.Client) int64 {
	t.Helper()
	value := do(t, c, "TIME")
	items, ok := value.([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("TIME = %#v", value)
	}
	return limNumber(t, limText(t, items[0]))*1000 + limNumber(t, limText(t, items[1]))/1000
}

// limSettleInMinute waits out an imminent minute boundary so a scenario that
// spans several calls measures one fixed window.
func limSettleInMinute(t *testing.T, c *coordination.Client, minimum time.Duration) int64 {
	t.Helper()
	now := limServerTimeMS(t, c)
	if remaining := 60_000 - now%60_000; remaining < minimum.Milliseconds() {
		time.Sleep(time.Duration(remaining+20) * time.Millisecond)
		now = limServerTimeMS(t, c)
	}
	return now
}

func limHash(t *testing.T, c *coordination.Client, key string) map[string]string {
	t.Helper()
	value := do(t, c, "HGETALL", key)
	fields, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("HGETALL %s = %#v", key, value)
	}
	hash := make(map[string]string, len(fields))
	for name, raw := range fields {
		hash[name] = limText(t, raw)
	}
	return hash
}

// limField reads a counter out of a rate or cost hash; an absent field counts
// as zero exactly as the scripts treat it.
func limField(t *testing.T, hash map[string]string, name string) int64 {
	t.Helper()
	text, ok := hash[name]
	if !ok {
		return 0
	}
	return limNumber(t, text)
}

func limInt(t *testing.T, c *coordination.Client, args ...string) int64 {
	t.Helper()
	value := do(t, c, args...)
	number, ok := value.(int64)
	if !ok {
		t.Fatalf("%v = %#v", args, value)
	}
	return number
}

func limExceeded(t *testing.T, err error, dimension limits.Dimension) time.Duration {
	t.Helper()
	var exceeded *limits.ExceededError
	if !errors.As(err, &exceeded) {
		t.Fatalf("error = %v, want an ExceededError", err)
	}
	if exceeded.Dimension != dimension {
		t.Fatalf("dimension = %q, want %q", exceeded.Dimension, dimension)
	}
	return exceeded.RetryAfter
}

func limReserve(t *testing.T, limiter *limits.Limiter, request limits.Request) *limits.Lease {
	t.Helper()
	lease, err := limiter.Reserve(t.Context(), request)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	return lease
}

// limReleaseTwice proves a release is safe to repeat, which is what lets the
// gateway retry cleanup after an ambiguous failure.
func limReleaseTwice(t *testing.T, lease *limits.Lease) {
	t.Helper()
	for range 2 {
		if err := lease.Release(t.Context()); err != nil {
			t.Fatalf("Release: %v", err)
		}
	}
}

func limWantCounters(t *testing.T, c *coordination.Client, key string, requests, tokens int64) {
	t.Helper()
	hash := limHash(t, c, key)
	if got := limField(t, hash, "rpm"); got != requests {
		t.Fatalf("rpm = %d, want %d (hash %v)", got, requests, hash)
	}
	if got := limField(t, hash, "tpm"); got != tokens {
		t.Fatalf("tpm = %d, want %d (hash %v)", got, tokens, hash)
	}
}

func TestLimitsRefundsAreIdempotentAndCannotChangeASuccessorWindow(t *testing.T) {
	c := limClient(t)
	namespace := limNamespace(t, c, "refund")
	limiter := limLimiter(t, c, namespace)
	lookup := limLookup()
	rateKey, concurrencyKey := limRateKeys(namespace, lookup)
	limSettleInMinute(t, c, 3*time.Second)

	request := limRequest(lookup)
	first := limReserve(t, limiter, request)
	second := limReserve(t, limiter, request)
	for range 2 {
		if err := first.Refund(t.Context()); err != nil {
			t.Fatalf("Refund: %v", err)
		}
	}
	// A refunded lease can never reconcile tokens afterwards; the script
	// reports that as "nothing to do" rather than moving the counter.
	if err := first.Reconcile(t.Context(), 100); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	limWantCounters(t, c, rateKey, 1, 5)
	if got := limInt(t, c, "ZCARD", concurrencyKey); got != 1 {
		t.Fatalf("ZCARD = %d, want 1", got)
	}

	// A refund that arrives after the minute rolled must not spend the
	// successor window's capacity, but must still free the concurrency slot.
	window := limField(t, limHash(t, c, rateKey), "window")
	do(t, c, "HSET", rateKey, "window", strconv.FormatInt(window+1, 10), "rpm", "7", "tpm", "35")
	if err := second.Refund(t.Context()); err != nil {
		t.Fatalf("Refund: %v", err)
	}
	limWantCounters(t, c, rateKey, 7, 35)
	if got := limInt(t, c, "ZCARD", concurrencyKey); got != 0 {
		t.Fatalf("ZCARD = %d, want 0", got)
	}
}

func TestLimitsServerTimeUnifiesCallers(t *testing.T) {
	first := limClient(t)
	second := limClient(t)
	namespace := limNamespace(t, first, "server-time")
	a := limLimiter(t, first, namespace)
	b := limLimiter(t, second, namespace)
	lookup := limLookup()
	rateKey, _ := limRateKeys(namespace, lookup)

	request := limRequest(lookup)
	request.RequestsPerMinute = limPointer(int64(2))
	request.TokensPerMinute = nil
	request.MaxConcurrency = limPointer(int64(2))

	limSettleInMinute(t, first, 3*time.Second)
	before := limServerTimeMS(t, first)
	leaseA := limReserve(t, a, request)
	leaseB := limReserve(t, b, request)
	if _, err := a.Reserve(t.Context(), request); limExceeded(t, err, limits.DimensionRequests) <= 0 {
		t.Fatal("a rejection must carry a positive retry hint")
	}
	after := limServerTimeMS(t, first)

	window := limField(t, limHash(t, first, rateKey), "window")
	if window < before/60_000 || window > after/60_000 {
		t.Fatalf("window %d is outside the server window [%d,%d]", window, before/60_000, after/60_000)
	}
	// Two replicas spent one shared budget, and no token was charged because
	// the dimension is unlimited.
	limWantCounters(t, first, rateKey, 2, 0)

	other := limRequest(limLookup())
	other.RequestsPerMinute = limPointer(int64(1))
	other.TokensPerMinute = nil
	other.MaxConcurrency = limPointer(int64(2))
	otherRateKey, _ := limRateKeys(namespace, other.LookupID)
	leaseC := limReserve(t, b, other)
	limWantCounters(t, second, otherRateKey, 1, 0)

	limReleaseTwice(t, leaseA)
	limReleaseTwice(t, leaseB)
	limReleaseTwice(t, leaseC)
}

func TestLimitsMinuteRolloverResetsOnceAndRetryMatchesServerWindow(t *testing.T) {
	c := limClient(t)
	namespace := limNamespace(t, c, "rollover")
	limiter := limLimiter(t, c, namespace)
	lookup := limLookup()
	rateKey, _ := limRateKeys(namespace, lookup)

	now := limSettleInMinute(t, c, 4*time.Second)
	window := now / 60_000
	// Seed the previous window, including a reconciliation marker, and give it
	// a TTL that outlives the boundary.
	do(t, c, "HSET", rateKey, "window", strconv.FormatInt(window-1, 10),
		"rpm", "9", "tpm", "900", "reconciled:stale-lease", "1")
	do(t, c, "PEXPIRE", rateKey, "120000")

	request := limRequest(lookup)
	request.RequestsPerMinute = limPointer(int64(2))
	request.TokensPerMinute = limPointer(int64(20))
	request.MaxConcurrency = nil
	request.RequestedTokens = 7

	remainingBefore := 60_000 - limServerTimeMS(t, c)%60_000
	limReserve(t, limiter, request)
	limReserve(t, limiter, request)

	hash := limHash(t, c, rateKey)
	if len(hash) != 3 {
		t.Fatalf("hash = %v, want the stale markers deleted", hash)
	}
	if got := limField(t, hash, "window"); got != window {
		t.Fatalf("window = %d, want %d", got, window)
	}
	limWantCounters(t, c, rateKey, 2, 14)
	ttl := limInt(t, c, "PTTL", rateKey)
	if ttl < 1 || ttl > 60_000 {
		t.Fatalf("PTTL = %d, want a positive TTL no longer than the window", ttl)
	}

	_, err := limiter.Reserve(t.Context(), request)
	retry := limExceeded(t, err, limits.DimensionRequests)
	remainingAfter := 60_000 - limServerTimeMS(t, c)%60_000
	if retry <= 0 || retry > time.Minute {
		t.Fatalf("retry = %v, want a positive hint within the window", retry)
	}
	if retry.Milliseconds() > remainingBefore || retry.Milliseconds() < remainingAfter {
		t.Fatalf("retry = %v, want it inside [%dms,%dms]", retry, remainingAfter, remainingBefore)
	}
}

func TestLimitsRetryStaysPositiveNearTheMinuteEnd(t *testing.T) {
	c := limClient(t)
	namespace := limNamespace(t, c, "minute-end")
	limiter := limLimiter(t, c, namespace)

	for attempt := range 3 {
		// Land inside the last part of a window, where a naive implementation
		// would hand out a zero or negative retry hint.
		for {
			remaining := 60_000 - limServerTimeMS(t, c)%60_000
			if remaining <= 1_500 && remaining >= 500 {
				break
			}
			if remaining > 1_500 {
				time.Sleep(time.Duration(remaining-1_200) * time.Millisecond)
				continue
			}
			time.Sleep(600 * time.Millisecond)
		}
		request := limRequest(limLookup())
		request.RequestsPerMinute = limPointer(int64(1))
		request.TokensPerMinute = nil
		request.MaxConcurrency = nil
		if _, err := limiter.Reserve(t.Context(), request); err != nil {
			if attempt == 2 {
				t.Fatalf("Reserve: %v", err)
			}
			continue
		}
		_, err := limiter.Reserve(t.Context(), request)
		var exceeded *limits.ExceededError
		if !errors.As(err, &exceeded) {
			// The window rolled between the two calls; try again.
			if attempt == 2 {
				t.Fatalf("Reserve error = %v, want an ExceededError", err)
			}
			continue
		}
		if exceeded.Dimension != limits.DimensionRequests {
			t.Fatalf("dimension = %q, want %q", exceeded.Dimension, limits.DimensionRequests)
		}
		if exceeded.RetryAfter <= 0 || exceeded.RetryAfter > 1_500*time.Millisecond {
			t.Fatalf("retry = %v, want a positive hint no longer than the remainder", exceeded.RetryAfter)
		}
		return
	}
}

func TestLimitsRejectionsConsumeNothing(t *testing.T) {
	c := limClient(t)
	namespace := limNamespace(t, c, "rejections")
	limiter := limLimiter(t, c, namespace)

	for _, test := range []struct {
		name      string
		shape     func(*limits.Request)
		second    func(*limits.Request)
		dimension limits.Dimension
	}{
		{
			name: "requests",
			shape: func(r *limits.Request) {
				r.RequestsPerMinute = limPointer(int64(1))
				r.TokensPerMinute = limPointer(int64(100))
				r.MaxConcurrency = limPointer(int64(1))
			},
			dimension: limits.DimensionRequests,
		},
		{
			name: "tokens",
			shape: func(r *limits.Request) {
				r.RequestsPerMinute = limPointer(int64(3))
				r.TokensPerMinute = limPointer(int64(5))
				r.MaxConcurrency = limPointer(int64(1))
			},
			second:    func(r *limits.Request) { r.RequestedTokens = 1 },
			dimension: limits.DimensionTokens,
		},
		{
			name: "concurrency",
			shape: func(r *limits.Request) {
				r.RequestsPerMinute = limPointer(int64(3))
				r.TokensPerMinute = limPointer(int64(100))
				r.MaxConcurrency = limPointer(int64(1))
			},
			dimension: limits.DimensionConcurrency,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			lookup := limLookup()
			rateKey, concurrencyKey := limRateKeys(namespace, lookup)
			limSettleInMinute(t, c, 3*time.Second)

			first := limRequest(lookup)
			test.shape(&first)
			lease := limReserve(t, limiter, first)

			second := first
			if test.second != nil {
				test.second(&second)
			}
			if _, err := limiter.Reserve(t.Context(), second); limExceeded(t, err, test.dimension) <= 0 {
				t.Fatal("a rejection must carry a positive retry hint")
			}
			// The rejected call must not have spent capacity in any other
			// dimension, which the released lease then makes observable.
			limReleaseTwice(t, lease)
			limWantCounters(t, c, rateKey, 1, 5)
			if got := limInt(t, c, "ZCARD", concurrencyKey); got != 0 {
				t.Fatalf("ZCARD = %d, want 0", got)
			}
		})
	}
}

func TestLimitsTokenReconciliationMatchesActualUsage(t *testing.T) {
	c := limClient(t)
	namespace := limNamespace(t, c, "reconcile")
	limiter := limLimiter(t, c, namespace)
	lookup := limLookup()
	rateKey, _ := limRateKeys(namespace, lookup)
	limSettleInMinute(t, c, 3*time.Second)

	request := limRequest(lookup)
	request.MaxConcurrency = nil
	request.RequestedTokens = 8
	lease := limReserve(t, limiter, request)
	for range 2 {
		if err := lease.Reconcile(t.Context(), 3); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
	}
	limWantCounters(t, c, rateKey, 1, 3)

	request.RequestedTokens = 4
	second := limReserve(t, limiter, request)
	for range 2 {
		if err := second.Reconcile(t.Context(), 7); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
	}
	limWantCounters(t, c, rateKey, 2, 10)
}

func TestLimitsTokenReconciliationDoesNotTouchANewWindow(t *testing.T) {
	c := limClient(t)
	namespace := limNamespace(t, c, "reconcile-window")
	limiter := limLimiter(t, c, namespace)
	lookup := limLookup()
	rateKey, _ := limRateKeys(namespace, lookup)
	limSettleInMinute(t, c, 3*time.Second)

	request := limRequest(lookup)
	request.MaxConcurrency = nil
	lease := limReserve(t, limiter, request)

	window := limField(t, limHash(t, c, rateKey), "window")
	do(t, c, "HSET", rateKey, "window", strconv.FormatInt(window+1, 10), "rpm", "1", "tpm", "11")
	if err := lease.Reconcile(t.Context(), 0); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	limWantCounters(t, c, rateKey, 1, 11)
}

func TestLimitsConcurrencyExpiryAndReleaseUseServerState(t *testing.T) {
	c := limClient(t)
	namespace := limNamespace(t, c, "concurrency")
	limiter := limLimiter(t, c, namespace)
	lookup := limLookup()
	rateKey, concurrencyKey := limRateKeys(namespace, lookup)

	before := limServerTimeMS(t, c)
	// An entry whose lease already expired must be swept, not counted.
	do(t, c, "ZADD", concurrencyKey, strconv.FormatInt(before-1, 10), "expired-fixture")

	request := limRequest(lookup)
	request.RequestsPerMinute = nil
	request.TokensPerMinute = nil
	request.MaxConcurrency = limPointer(int64(1))
	request.RequestedTokens = 0
	lease := limReserve(t, limiter, request)
	after := limServerTimeMS(t, c)

	if got := limInt(t, c, "ZCARD", concurrencyKey); got != 1 {
		t.Fatalf("ZCARD = %d, want 1", got)
	}
	if got := limInt(t, c, "ZCOUNT", concurrencyKey,
		strconv.FormatInt(before+5_000, 10), strconv.FormatInt(after+5_000, 10)); got != 1 {
		t.Fatalf("the lease expiry is not server-derived: %d entries in range", got)
	}
	if value := do(t, c, "ZSCORE", concurrencyKey, "expired-fixture"); value != nil {
		t.Fatalf("ZSCORE expired-fixture = %#v, want it swept", value)
	}
	if ttl := limInt(t, c, "PTTL", concurrencyKey); ttl < 1 || ttl > 5_000 {
		t.Fatalf("PTTL = %d, want it bounded by the lease", ttl)
	}
	if got := limInt(t, c, "EXISTS", rateKey); got != 0 {
		t.Fatalf("EXISTS rate = %d, want no rate state", got)
	}

	limReleaseTwice(t, lease)
	if got := limInt(t, c, "ZCARD", concurrencyKey); got != 0 {
		t.Fatalf("ZCARD = %d, want 0", got)
	}
}

func TestLimitsUnlimitedDimensionsCreateNoState(t *testing.T) {
	c := limClient(t)
	namespace := limNamespace(t, c, "unlimited")
	limiter := limLimiter(t, c, namespace)
	lookup := limLookup()
	rateKey, concurrencyKey := limRateKeys(namespace, lookup)

	request := limRequest(lookup)
	request.RequestsPerMinute = nil
	request.TokensPerMinute = nil
	request.MaxConcurrency = nil
	request.RequestedTokens = 0
	lease := limReserve(t, limiter, request)
	limReleaseTwice(t, lease)
	if err := lease.Reconcile(t.Context(), 12); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if err := lease.Refund(t.Context()); err != nil {
		t.Fatalf("Refund: %v", err)
	}
	for _, key := range []string{rateKey, concurrencyKey} {
		if got := limInt(t, c, "EXISTS", key); got != 0 {
			t.Fatalf("EXISTS %s = %d, want no state", key, got)
		}
	}
}

func TestLimitsMalformedRateStateFailsClosedBeforeMutation(t *testing.T) {
	c := limClient(t)
	namespace := limNamespace(t, c, "malformed")
	limiter := limLimiter(t, c, namespace)
	lookup := limLookup()
	rateKey, concurrencyKey := limRateKeys(namespace, lookup)

	do(t, c, "HSET", rateKey, "window", "not-an-integer")
	_, err := limiter.Reserve(t.Context(), limRequest(lookup))
	if !errors.Is(err, limits.ErrMalformedState) {
		t.Fatalf("Reserve error = %v, want ErrMalformedState", err)
	}
	if got := limInt(t, c, "HLEN", rateKey); got != 1 {
		t.Fatalf("HLEN = %d, want the malformed state untouched", got)
	}
	if got := limInt(t, c, "EXISTS", concurrencyKey); got != 0 {
		t.Fatalf("EXISTS concurrency = %d, want no lease", got)
	}

	// An argument outside the Lua integer range is refused by the script
	// itself, before it reads or writes anything.
	script, err := os.ReadFile("../../internal/limits/scripts/reserve_limits.lua")
	if err != nil {
		t.Fatal(err)
	}
	value := do(t, c, "EVAL", string(script), "2", rateKey, concurrencyKey,
		"9007199254740992", "0", "0", "0", "lease", "1000")
	items, ok := value.([]any)
	if !ok || len(items) != 6 {
		t.Fatalf("EVAL = %#v", value)
	}
	if items[0] != int64(1) || items[1] != int64(-1) || items[2] != "invalid_arguments" {
		t.Fatalf("EVAL = %#v, want an invalid_arguments failure", items)
	}
}

func TestLimitsCountersRetainExactBehaviorAtTheLuaSafeMaximum(t *testing.T) {
	c := limClient(t)
	namespace := limNamespace(t, c, "maximum")
	limiter := limLimiter(t, c, namespace)
	lookup := limLookup()
	rateKey, _ := limRateKeys(namespace, lookup)

	now := limSettleInMinute(t, c, 4*time.Second)
	seeded := strconv.FormatInt(limMaximum-1, 10)
	do(t, c, "HSET", rateKey, "window", strconv.FormatInt(now/60_000, 10), "rpm", seeded, "tpm", seeded)
	do(t, c, "PEXPIRE", rateKey, "60000")

	request := limRequest(lookup)
	request.RequestsPerMinute = limPointer(limMaximum)
	request.TokensPerMinute = limPointer(limMaximum)
	request.MaxConcurrency = nil
	request.RequestedTokens = 1
	limReserve(t, limiter, request)
	limWantCounters(t, c, rateKey, limMaximum, limMaximum)

	_, err := limiter.Reserve(t.Context(), request)
	limExceeded(t, err, limits.DimensionRequests)
	limWantCounters(t, c, rateKey, limMaximum, limMaximum)
}

func TestLimitsTokenReconciliationSaturatesAtTheLuaSafeMaximum(t *testing.T) {
	c := limClient(t)
	namespace := limNamespace(t, c, "saturate")
	limiter := limLimiter(t, c, namespace)
	lookup := limLookup()
	rateKey, _ := limRateKeys(namespace, lookup)

	now := limSettleInMinute(t, c, 4*time.Second)
	do(t, c, "HSET", rateKey, "window", strconv.FormatInt(now/60_000, 10),
		"rpm", "0", "tpm", strconv.FormatInt(limMaximum-1, 10))
	do(t, c, "PEXPIRE", rateKey, "60000")

	request := limRequest(lookup)
	request.RequestsPerMinute = nil
	request.TokensPerMinute = limPointer(limMaximum)
	request.MaxConcurrency = nil
	request.RequestedTokens = 1
	lease := limReserve(t, limiter, request)
	limWantCounters(t, c, rateKey, 0, limMaximum)

	if err := lease.Reconcile(t.Context(), 2); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	limWantCounters(t, c, rateKey, 0, limMaximum)
	_, err := limiter.Reserve(t.Context(), request)
	limExceeded(t, err, limits.DimensionTokens)
}

func TestLimitsConcurrentReplicasEnforceOneAtomicLimit(t *testing.T) {
	first := limClient(t)
	second := limClient(t)
	namespace := limNamespace(t, first, "atomic")
	a := limLimiter(t, first, namespace)
	b := limLimiter(t, second, namespace)

	const callers = 32
	const allowed = 10
	for range 3 {
		request := limRequest(limLookup())
		request.RequestsPerMinute = limPointer(int64(allowed))
		request.TokensPerMinute = nil
		request.MaxConcurrency = nil
		request.RequestedTokens = 0

		limSettleInMinute(t, first, 3*time.Second)
		before := limServerTimeMS(t, first) / 60_000
		results := make(chan error, callers)
		var wg sync.WaitGroup
		for caller := range callers {
			limiter := a
			if caller%2 == 1 {
				limiter = b
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := limiter.Reserve(t.Context(), request)
				results <- err
			}()
		}
		wg.Wait()
		close(results)
		after := limServerTimeMS(t, first) / 60_000

		var granted, rejected int
		for err := range results {
			if err == nil {
				granted++
				continue
			}
			var exceeded *limits.ExceededError
			if !errors.As(err, &exceeded) || exceeded.Dimension != limits.DimensionRequests {
				t.Fatalf("Reserve: %v", err)
			}
			rejected++
		}
		if before != after {
			// The window rolled mid-run, so the budget was not one window's.
			continue
		}
		if granted != allowed || rejected != callers-allowed {
			t.Fatalf("granted %d and rejected %d, want %d and %d", granted, rejected, allowed, callers-allowed)
		}
		return
	}
	t.Fatal("every attempt crossed a minute boundary")
}

func TestLimitsCooldownsKeepTheLongestExpiry(t *testing.T) {
	first := limClient(t)
	second := limClient(t)
	namespace := limNamespace(t, first, "cooldown")
	a := limLimiter(t, first, namespace)
	b := limLimiter(t, second, namespace)

	scope := limits.SlotScope(uuid.NewString())
	key := namespace + ":provider-cooldown:" + scope
	durations := []time.Duration{time.Second, 120 * time.Second, 4 * time.Second, 60 * time.Second}
	errs := make(chan error, len(durations))
	var wg sync.WaitGroup
	for index, duration := range durations {
		limiter := a
		if index%2 == 1 {
			limiter = b
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- limiter.Cooldown(t.Context(), scope, duration)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Cooldown: %v", err)
		}
	}
	if ttl := limInt(t, first, "PTTL", key); ttl <= 110_000 {
		t.Fatalf("PTTL = %d, want the longest cooldown to win", ttl)
	}
	// A shorter cooldown never shortens one that is already running.
	if err := b.Cooldown(t.Context(), scope, time.Second); err != nil {
		t.Fatalf("Cooldown: %v", err)
	}
	if ttl := limInt(t, first, "PTTL", key); ttl <= 110_000 {
		t.Fatalf("PTTL = %d, want the longest cooldown retained", ttl)
	}
	cooling, err := a.Cooling(t.Context(), scope)
	if err != nil {
		t.Fatalf("Cooling: %v", err)
	}
	if !cooling {
		t.Fatal("the scope must report as cooling")
	}

	// Only an explicit clear ends it.
	if err := a.Cooldown(t.Context(), scope, 0); err != nil {
		t.Fatalf("Cooldown: %v", err)
	}
	cooling, err = b.Cooling(t.Context(), scope)
	if err != nil {
		t.Fatalf("Cooling: %v", err)
	}
	if cooling {
		t.Fatal("a cleared scope must not report as cooling")
	}
}

func TestLimitsProviderSlotsShareQuotaAndCooldownsAcrossGateways(t *testing.T) {
	first := limClient(t)
	second := limClient(t)
	namespace := limNamespace(t, first, "slots")
	a := limLimiter(t, first, namespace)
	b := limLimiter(t, second, namespace)

	slot := uuid.NewString()
	lookup := limits.SlotLookup(slot)
	request := limRequest(lookup)
	request.RequestsPerMinute = limPointer(int64(10))
	request.TokensPerMinute = nil
	request.MaxConcurrency = limPointer(int64(1))
	request.RequestedTokens = 0

	limSettleInMinute(t, first, 4*time.Second)
	lease := limReserve(t, a, request)
	if _, err := b.Reserve(t.Context(), request); limExceeded(t, err, limits.DimensionConcurrency) <= 0 {
		t.Fatal("a concurrency rejection must carry a positive retry hint")
	}

	usage, err := b.ProviderUsage(t.Context(), lookup)
	if err != nil {
		t.Fatalf("ProviderUsage: %v", err)
	}
	if usage != (limits.Usage{RequestsThisMinute: 1, ConcurrentRequests: 1}) {
		t.Fatalf("usage = %+v, want one request and one in flight", usage)
	}

	limReleaseTwice(t, lease)
	limReserve(t, b, request)
	usage, err = a.ProviderUsage(t.Context(), lookup)
	if err != nil {
		t.Fatalf("ProviderUsage: %v", err)
	}
	if usage != (limits.Usage{RequestsThisMinute: 2, ConcurrentRequests: 1}) {
		t.Fatalf("usage = %+v, want two requests and one in flight", usage)
	}

	credential := uuid.NewString()
	scope := limits.CredentialScope(uuid.NewString(), &credential)
	if err := a.Cooldown(t.Context(), scope, 2*time.Second); err != nil {
		t.Fatalf("Cooldown: %v", err)
	}
	cooling, err := b.Cooling(t.Context(), limits.SlotScope(slot), scope)
	if err != nil {
		t.Fatalf("Cooling: %v", err)
	}
	if !cooling {
		t.Fatal("a cooling credential must be visible to every gateway")
	}
	cooling, err = b.Cooling(t.Context(), limits.SlotScope(slot))
	if err != nil {
		t.Fatalf("Cooling: %v", err)
	}
	if cooling {
		t.Fatal("an unrelated scope must not report as cooling")
	}
}

func TestLimitsCostBudgetsFailClosedUntilReconciled(t *testing.T) {
	c := limClient(t)
	namespace := limNamespace(t, c, "cost")
	limiter := limLimiter(t, c, namespace)
	windows := limits.BudgetWindows(time.Now())

	apiKey := uuid.NewString()
	dayKey, monthKey := limCostKeys(namespace, apiKey)
	lookup := limLookup()
	rateKey, _ := limRateKeys(namespace, lookup)
	request := limRequest(lookup)
	request.CostOwnerID = apiKey
	request.DailyCostLimit = limPointer("1.000000000000")
	request.MonthlyCostLimit = limPointer("10")

	// Spending is never admitted against a balance nobody has reconstructed.
	_, err := limiter.Reserve(t.Context(), request)
	if !errors.Is(err, limits.ErrUninitializedCost) {
		t.Fatalf("Reserve error = %v, want ErrUninitializedCost", err)
	}
	for _, key := range []string{dayKey, monthKey, rateKey} {
		if got := limInt(t, c, "EXISTS", key); got != 0 {
			t.Fatalf("EXISTS %s = %d, want nothing written", key, got)
		}
	}

	// A snapshot for a window other than the current one cannot initialise it.
	stale := limits.CostSnapshot{
		CostOwnerID:     apiKey,
		DailyWindowID:   windows.DailyID - 1,
		DailyAccrued:    "0",
		MonthlyWindowID: windows.MonthlyID + 1,
		MonthlyAccrued:  "0",
	}
	daily, monthly, err := limiter.ApplyCostSnapshot(t.Context(), stale)
	if err != nil {
		t.Fatalf("ApplyCostSnapshot: %v", err)
	}
	if daily || monthly {
		t.Fatalf("ApplyCostSnapshot reconciled %v/%v, want neither window", daily, monthly)
	}
	for _, key := range []string{dayKey, monthKey} {
		if got := limInt(t, c, "EXISTS", key); got != 0 {
			t.Fatalf("EXISTS %s = %d, want nothing written", key, got)
		}
	}

	current := limits.CostSnapshot{
		CostOwnerID:      apiKey,
		DailyWindowID:    windows.DailyID,
		DailyAccrued:     "0.250000000000",
		MonthlyWindowID:  windows.MonthlyID,
		MonthlyAccrued:   "2.500000000000",
		UnpricedAttempts: 5,
	}
	if daily, monthly, err = limiter.ApplyCostSnapshot(t.Context(), current); err != nil {
		t.Fatalf("ApplyCostSnapshot: %v", err)
	}
	if !daily || !monthly {
		t.Fatalf("ApplyCostSnapshot reconciled %v/%v, want both windows", daily, monthly)
	}
	limReleaseTwice(t, limReserve(t, limiter, request))

	// Reconciliation never lowers a balance, and never lowers the count of
	// attempts still awaiting a price.
	lower := current
	lower.DailyAccrued = "0"
	lower.MonthlyAccrued = "1"
	lower.UnpricedAttempts = 2
	if _, _, err = limiter.ApplyCostSnapshot(t.Context(), lower); err != nil {
		t.Fatalf("ApplyCostSnapshot: %v", err)
	}
	month := limHash(t, c, monthKey)
	if month["accrued"] != "2.5" || limField(t, month, "unpriced") != 5 {
		t.Fatalf("month = %v, want the higher balance retained", month)
	}
	if day := limHash(t, c, dayKey); day["accrued"] != "0.25" {
		t.Fatalf("day = %v, want the higher balance retained", day)
	}

	// An exhausted budget rejects with a hint bounded by the window.
	exhausted := current
	exhausted.DailyAccrued = "1.000000000000"
	if _, _, err = limiter.ApplyCostSnapshot(t.Context(), exhausted); err != nil {
		t.Fatalf("ApplyCostSnapshot: %v", err)
	}
	_, err = limiter.Reserve(t.Context(), request)
	retry := limExceeded(t, err, limits.DimensionDailyCost)
	if retry <= 0 || retry > 24*time.Hour {
		t.Fatalf("retry = %v, want it bounded by the day", retry)
	}
	// The rejection spent nothing: the request never reached the rate script.
	limWantCounters(t, c, rateKey, 1, 5)

	// Corrupt state fails closed and is then repaired without disturbing the
	// window that is still valid.
	do(t, c, "SET", dayKey, "not-a-hash")
	if _, err = limiter.Reserve(t.Context(), request); !errors.Is(err, limits.ErrMalformedState) {
		t.Fatalf("Reserve error = %v, want ErrMalformedState", err)
	}
	repaired := current
	repaired.DailyAccrued = "0.100000000000"
	repaired.MonthlyAccrued = "0"
	repaired.UnpricedAttempts = 0
	if daily, monthly, err = limiter.ApplyCostSnapshot(t.Context(), repaired); err != nil {
		t.Fatalf("ApplyCostSnapshot: %v", err)
	}
	if !daily || !monthly {
		t.Fatalf("ApplyCostSnapshot reconciled %v/%v, want both windows", daily, monthly)
	}
	if day := limHash(t, c, dayKey); day["accrued"] != "0.1" {
		t.Fatalf("day = %v, want the repaired balance", day)
	}
	month = limHash(t, c, monthKey)
	if month["accrued"] != "2.5" || limField(t, month, "unpriced") != 5 {
		t.Fatalf("month = %v, want the valid window untouched", month)
	}
	limReleaseTwice(t, limReserve(t, limiter, request))
}

func TestLimitsMonthlyBudgetExhaustionRejectsWithItsOwnWindow(t *testing.T) {
	c := limClient(t)
	namespace := limNamespace(t, c, "monthly")
	limiter := limLimiter(t, c, namespace)
	windows := limits.BudgetWindows(time.Now())

	apiKey := uuid.NewString()
	request := limRequest(limLookup())
	request.CostOwnerID = apiKey
	request.DailyCostLimit = limPointer("1000")
	request.MonthlyCostLimit = limPointer("5")

	if _, _, err := limiter.ApplyCostSnapshot(t.Context(), limits.CostSnapshot{
		CostOwnerID:     apiKey,
		DailyWindowID:   windows.DailyID,
		DailyAccrued:    "0",
		MonthlyWindowID: windows.MonthlyID,
		MonthlyAccrued:  "5.000000000000",
	}); err != nil {
		t.Fatalf("ApplyCostSnapshot: %v", err)
	}
	_, err := limiter.Reserve(t.Context(), request)
	retry := limExceeded(t, err, limits.DimensionMonthlyCost)
	if retry <= 0 || retry > 31*24*time.Hour {
		t.Fatalf("retry = %v, want it bounded by the month", retry)
	}
}

// limSeedAuthority creates the owner and provider every accounting fixture row
// references.
func limSeedAuthority(t *testing.T, pool *pgxpool.Pool) (string, string) {
	t.Helper()
	owner := uuid.NewString()
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO olp.users (id,email,display_name,role,etag)
		 VALUES ($1,$2,'Owner','owner',gen_random_uuid())`,
		owner, "owner-"+strings.ToLower(rand.Text())+"@limits.test"); err != nil {
		t.Fatal(err)
	}
	provider := uuid.NewString()
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO olp.providers (id,name,kind,state,configuration,etag,slots_etag,created_by)
		 VALUES ($1,$2,'openai','active','{}'::jsonb,gen_random_uuid(),gen_random_uuid(),$3)`,
		provider, "provider-"+strings.ToLower(rand.Text()), owner); err != nil {
		t.Fatal(err)
	}
	return owner, provider
}

func limSeedKey(t *testing.T, pool *pgxpool.Pool, owner string, revoked bool) string {
	t.Helper()
	id := uuid.NewString()
	var revokedAt *time.Time
	if revoked {
		now := time.Now().UTC()
		revokedAt = &now
	}
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO olp.api_keys (id,lookup_id,digest,name,created_by,policy,etag,revoked_at)
		 VALUES ($1,$2,$3,'key',$4,'{}'::jsonb,gen_random_uuid(),$5)`,
		id, limLookup(), []byte{1, 2, 3}, owner, revokedAt); err != nil {
		t.Fatal(err)
	}
	return id
}

// limSeedFact writes one attempt's billable evidence. A nil cost records an
// attempt that is billable but still awaiting a price.
func limSeedFact(t *testing.T, pool *pgxpool.Pool, provider, apiKey string, observedAt time.Time, cost *string) {
	t.Helper()
	request := uuid.NewString()
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO olp.usage_request_anchors (request_id,request_started_at) VALUES ($1,$2)`,
		request, observedAt); err != nil {
		t.Fatal(err)
	}
	unpriced := cost == nil
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO olp.attempt_usage_facts (
			attempt_id,event_id,request_id,request_started_at,attempt_ordinal,api_key_id,provider_id,
			route_slug,upstream_model,operation,surface,observed_at,charge_status,usage_observed,
			usage_complete,input_tokens,output_tokens,cached_input_tokens,estimated_cost,unpriced,currency,
			request_counted,provider_request_counted,model_request_counted,target_request_counted,
			request_unpriced_counted,provider_unpriced_counted,model_unpriced_counted,target_unpriced_counted,
			request_incomplete_counted,provider_incomplete_counted,model_incomplete_counted,
			target_incomplete_counted)
		 VALUES ($1,gen_random_uuid(),$2,$3,1,$4,$5,'route','model','generation','openai',$3,'billable',
			true,true,1,1,0,$6::text::numeric,$7,'USD',true,true,true,true,$7,$7,$7,$7,false,false,false,false)`,
		uuid.NewString(), request, observedAt, apiKey, provider, cost, unpriced); err != nil {
		t.Fatal(err)
	}
}

// limSeedHourly writes the rolled up form of attempts retention has already
// folded away; budgets must keep counting them.
func limSeedHourly(t *testing.T, pool *pgxpool.Pool, provider, apiKey string, bucket time.Time, cost string, unpriced int64) {
	t.Helper()
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO olp.attempt_usage_hourly (
			bucket,route_slug,provider_id,upstream_model,operation,surface,api_key_id,
			request_count,provider_request_count,model_request_count,target_request_count,
			input_tokens,output_tokens,cached_input_tokens,media_units,estimated_cost,
			request_unpriced_count,provider_unpriced_count,model_unpriced_count,target_unpriced_count,
			request_incomplete_count,provider_incomplete_count,model_incomplete_count,
			target_incomplete_count,currency,unpriced_attempt_count)
		 VALUES ($1,'route',$2,'model','generation','openai',$3,1,1,1,1,0,0,0,0,$4::text::numeric,
			0,0,0,0,0,0,0,0,'USD',$5)`,
		bucket, provider, apiKey, cost, unpriced); err != nil {
		t.Fatal(err)
	}
}

func limAccounting(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	pool, dbURL := accessDatabase(t)
	if err := database.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	return pool, dbURL
}

func limWindowRows(t *testing.T, pool *pgxpool.Pool, apiKey string) map[string]string {
	t.Helper()
	rows, err := pool.Query(t.Context(),
		`SELECT window_kind||':'||window_id, accrued::text||'/'||unpriced_attempts
		 FROM olp.api_key_cost_windows WHERE api_key_id=$1`, apiKey)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	windows := map[string]string{}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			t.Fatal(err)
		}
		windows[key] = value
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return windows
}

func TestLimitsDurableSpendReconcilesIntoValkey(t *testing.T) {
	pool, _ := limAccounting(t)
	c := limClient(t)
	namespace := limNamespace(t, c, "accounting")
	limiter := limLimiter(t, c, namespace)

	var now time.Time
	if err := pool.QueryRow(t.Context(), "SELECT now()").Scan(&now); err != nil {
		t.Fatal(err)
	}
	windows := limits.BudgetWindows(now)
	owner, provider := limSeedAuthority(t, pool)
	spender := limSeedKey(t, pool, owner, false)
	idle := limSeedKey(t, pool, owner, false)
	revoked := limSeedKey(t, pool, owner, true)

	// Two attempts inside today, one earlier in the month, and two outside the
	// month entirely, plus the rolled up form of an hour that has aged out.
	limSeedFact(t, pool, provider, spender, now, limPointer("0.010000000000"))
	limSeedFact(t, pool, provider, spender, now, nil)
	limSeedFact(t, pool, provider, spender, windows.MonthlyStart, limPointer("0.030000000000"))
	limSeedFact(t, pool, provider, spender, windows.MonthlyStart.Add(-time.Hour), limPointer("5"))
	limSeedFact(t, pool, provider, spender, windows.MonthlyEnd, limPointer("7"))
	limSeedHourly(t, pool, provider, spender, windows.DailyStart, "0.002000000000", 3)
	limSeedFact(t, pool, provider, revoked, now, limPointer("9"))

	// wantDailyStored is the same total as Valkey keeps it: the script stores the
	// canonical decimal, without the trailing zeros PostgreSQL's scale carries.
	wantDaily, wantDailyStored := "0.012000000000", "0.012"
	if windows.DailyStart.Equal(windows.MonthlyStart) {
		// The month began today, so the month's earlier attempt is also today's.
		wantDaily, wantDailyStored = "0.042000000000", "0.042"
	}
	const wantMonthly = "0.042000000000"

	// A window that has passed is pruned rather than left to accumulate.
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO olp.api_key_cost_windows VALUES ($1,'day',$2,9,0)`,
		spender, windows.DailyID-1); err != nil {
		t.Fatal(err)
	}

	// A live delta lands in the window that contains its observation, and a
	// delta for a later window cannot overwrite the current one.
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := limits.AddCostDelta(t.Context(), tx, idle, now, "0.500000000000", 1)
	if err != nil {
		t.Fatalf("AddCostDelta: %v", err)
	}
	if snapshot.DailyAccrued != "0.500000000000" || snapshot.MonthlyAccrued != "0.500000000000" ||
		snapshot.UnpricedAttempts != 1 || snapshot.DailyWindowID != windows.DailyID ||
		snapshot.MonthlyWindowID != windows.MonthlyID {
		t.Fatalf("AddCostDelta = %+v", snapshot)
	}
	if snapshot, err = limits.AddCostDelta(t.Context(), tx, idle, now, "0.500000000000", 1); err != nil {
		t.Fatalf("AddCostDelta: %v", err)
	}
	if snapshot.MonthlyAccrued != "1.000000000000" || snapshot.UnpricedAttempts != 2 {
		t.Fatalf("AddCostDelta = %+v, want the deltas accumulated", snapshot)
	}
	if _, err = limits.AddCostDelta(t.Context(), tx, idle, windows.MonthlyEnd, "0.100000000000", 0); err != nil {
		t.Fatalf("AddCostDelta: %v", err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	next := limits.BudgetWindows(windows.MonthlyEnd)
	rows := limWindowRows(t, pool, idle)
	if len(rows) != 4 ||
		rows["day:"+strconv.FormatInt(windows.DailyID, 10)] != "1.000000000000/0" ||
		rows["month:"+strconv.FormatInt(windows.MonthlyID, 10)] != "1.000000000000/2" ||
		rows["day:"+strconv.FormatInt(next.DailyID, 10)] != "0.100000000000/0" ||
		rows["month:"+strconv.FormatInt(next.MonthlyID, 10)] != "0.100000000000/0" {
		t.Fatalf("windows = %v", rows)
	}

	conn, err := pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	snapshots, err := limits.ReconciliationSnapshots(t.Context(), conn.Conn(), now)
	if err != nil {
		t.Fatalf("ReconciliationSnapshots: %v", err)
	}
	byKey := map[string]limits.CostSnapshot{}
	for _, snapshot := range snapshots {
		byKey[snapshot.CostOwnerID] = snapshot
	}
	if len(byKey) != 2 {
		t.Fatalf("snapshots = %+v, want only the two live keys", snapshots)
	}
	if _, ok := byKey[revoked]; ok {
		t.Fatal("a revoked key must not be reconciled")
	}
	got := byKey[spender]
	if got.DailyAccrued != wantDaily || got.MonthlyAccrued != wantMonthly ||
		got.UnpricedAttempts != 4 || got.DailyWindowID != windows.DailyID ||
		got.MonthlyWindowID != windows.MonthlyID {
		t.Fatalf("snapshot = %+v, want daily %s monthly %s", got, wantDaily, wantMonthly)
	}
	// The durable total of a key whose attempts are no longer visible is kept,
	// never lowered to what the facts can still see.
	if idled := byKey[idle]; idled.MonthlyAccrued != "1.000000000000" || idled.UnpricedAttempts != 2 {
		t.Fatalf("snapshot = %+v, want the durable total retained", idled)
	}
	if rows = limWindowRows(t, pool, spender); len(rows) != 2 {
		t.Fatalf("windows = %v, want the passed window pruned", rows)
	}

	daily, monthly, err := limiter.ApplyCostSnapshot(t.Context(), got)
	if err != nil {
		t.Fatalf("ApplyCostSnapshot: %v", err)
	}
	if !daily || !monthly {
		t.Fatalf("ApplyCostSnapshot reconciled %v/%v, want both windows", daily, monthly)
	}
	dayKey, monthKey := limCostKeys(namespace, spender)
	if day := limHash(t, c, dayKey); day["accrued"] != wantDailyStored {
		t.Fatalf("day = %v, want %s", day, wantDailyStored)
	}
	month := limHash(t, c, monthKey)
	if month["accrued"] != "0.042" || limField(t, month, "unpriced") != 4 {
		t.Fatalf("month = %v, want 0.042 and four unpriced attempts", month)
	}

	// The status expression the key API renders reports the same balances.
	var budget []byte
	if err := pool.QueryRow(t.Context(),
		"SELECT "+limits.BudgetSQL+" FROM olp.api_keys k WHERE k.id=$1", spender).Scan(&budget); err != nil {
		t.Fatalf("BudgetSQL: %v", err)
	}
	var status struct {
		Daily struct {
			Accrued      string    `json:"accrued"`
			WindowEndsAt time.Time `json:"window_ends_at"`
		} `json:"daily"`
		Monthly struct {
			Accrued      string    `json:"accrued"`
			WindowEndsAt time.Time `json:"window_ends_at"`
		} `json:"monthly"`
		UnpricedAttempts int64 `json:"unpriced_attempts"`
	}
	if err := json.Unmarshal(budget, &status); err != nil {
		t.Fatalf("budget %s: %v", budget, err)
	}
	if status.Daily.Accrued != wantDaily || status.Monthly.Accrued != wantMonthly ||
		status.UnpricedAttempts != 4 {
		t.Fatalf("budget = %+v", status)
	}
	if !status.Daily.WindowEndsAt.Equal(windows.DailyEnd) ||
		!status.Monthly.WindowEndsAt.Equal(windows.MonthlyEnd) {
		t.Fatalf("budget windows = %v/%v, want %v/%v", status.Daily.WindowEndsAt,
			status.Monthly.WindowEndsAt, windows.DailyEnd, windows.MonthlyEnd)
	}
	if err := pool.QueryRow(t.Context(),
		"SELECT "+limits.BudgetSQL+" FROM olp.api_keys k WHERE k.id=$1", revoked).Scan(&budget); err != nil {
		t.Fatalf("BudgetSQL: %v", err)
	}
	if err := json.Unmarshal(budget, &status); err != nil {
		t.Fatalf("budget %s: %v", budget, err)
	}
	if status.Daily.Accrued != "9.000000000000" || status.Monthly.Accrued != "9.000000000000" {
		t.Fatalf("budget = %+v, want a revoked key to still report its spend", status)
	}
}

func TestLimitsOutagePolicyIsReadFromSettings(t *testing.T) {
	pool, _ := limAccounting(t)
	owner, _ := limSeedAuthority(t, pool)

	// A missing setting is an error, never a silently widened admission.
	if _, err := limits.LoadOutagePolicy(t.Context(), pool); err == nil {
		t.Fatal("LoadOutagePolicy must fail when the setting is absent")
	}
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO olp.settings (key,value,etag,updated_by)
		 VALUES ('limits.valkey_unavailable','fail_open',gen_random_uuid(),$1)`, owner); err != nil {
		t.Fatal(err)
	}
	policy, err := limits.LoadOutagePolicy(t.Context(), pool)
	if err != nil {
		t.Fatalf("LoadOutagePolicy: %v", err)
	}
	if policy != limits.FailOpen {
		t.Fatalf("policy = %v, want fail_open", policy)
	}
	if _, err = pool.Exec(t.Context(),
		`UPDATE olp.settings SET value='maybe' WHERE key='limits.valkey_unavailable'`); err != nil {
		t.Fatal(err)
	}
	policy, err = limits.LoadOutagePolicy(t.Context(), pool)
	if err == nil {
		t.Fatal("LoadOutagePolicy must reject an unknown value")
	}
	if policy != limits.FailClosed {
		t.Fatalf("policy = %v, want the closed default on error", policy)
	}
}

// limAwaitLeadership polls until this pool can take the reconciliation lock,
// which is how a replica discovers the previous leader's session ended.
func limAwaitLeadership(t *testing.T, pool *pgxpool.Pool) *limits.Leader {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		leader, err := limits.TryAcquireLeader(t.Context(), pool)
		if err != nil {
			t.Fatalf("TryAcquireLeader: %v", err)
		}
		if leader != nil {
			return leader
		}
		if time.Now().After(deadline) {
			t.Fatal("leadership was never released")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestLimitsCostReconciliationLeadershipIsExclusive(t *testing.T) {
	pool, dbURL := limAccounting(t)
	c := limClient(t)
	namespace := limNamespace(t, c, "leadership")
	limiter := limLimiter(t, c, namespace)
	owner, provider := limSeedAuthority(t, pool)
	spender := limSeedKey(t, pool, owner, false)
	limSeedFact(t, pool, provider, spender, time.Now().UTC(), limPointer("0.020000000000"))

	contender := limPool(t, dbURL)
	leader, err := limits.TryAcquireLeader(t.Context(), pool)
	if err != nil {
		t.Fatalf("TryAcquireLeader: %v", err)
	}
	if leader == nil {
		t.Fatal("the first replica must win the lock")
	}
	// The session that holds the lock left the pool for good: no unrelated
	// caller can ever be handed a connection that still owns it.
	if stat := pool.Stat(); stat.TotalConns() != 0 || stat.AcquiredConns() != 0 {
		t.Fatalf("pool has %d connections (%d acquired), want the leader's hijacked",
			stat.TotalConns(), stat.AcquiredConns())
	}

	// Leadership is retained across passes; contenders skip cheaply instead of
	// blocking, and every pass the leader runs still reports the lock.
	for range 3 {
		other, err := limits.TryAcquireLeader(t.Context(), contender)
		if err != nil {
			t.Fatalf("TryAcquireLeader: %v", err)
		}
		if other != nil {
			other.Close(t.Context())
			t.Fatal("a second replica must not acquire the lock")
		}
		report, err := leader.Reconcile(t.Context(), limiter, time.Now())
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if !report.LockAcquired || report.KeysReconciled != 1 ||
			report.DailyWindowsReconciled != 1 || report.MonthlyWindowsReconciled != 1 {
			t.Fatalf("report = %+v", report)
		}
	}

	// A pass whose context was cancelled still gives the lock back.
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	leader.Close(cancelled)
	next := limAwaitLeadership(t, contender)

	// The pool the leader came from keeps working with fresh connections.
	var alive int
	if err := pool.QueryRow(t.Context(), "SELECT 1").Scan(&alive); err != nil {
		t.Fatal(err)
	}
	if alive != 1 {
		t.Fatalf("SELECT 1 = %d", alive)
	}

	// A pass that is cut short must not leave the lock stranded either.
	abandoned, abandon := context.WithCancel(t.Context())
	abandon()
	if _, err := next.Reconcile(abandoned, limiter, time.Now()); err == nil {
		t.Fatal("a cancelled pass must fail rather than report success")
	}
	next.Close(t.Context())
	final := limAwaitLeadership(t, pool)
	final.Close(t.Context())
}

func limPool(t *testing.T, dbURL string) *pgxpool.Pool {
	t.Helper()
	cfg, err := database.Configuration(dbURL, 2, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := database.Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// limBudgetClock swaps the clock of a budget expression for a bind parameter.
// The boundary defect this pins only shows on particular calendar days, and a
// regression test must not have to wait for one of them to come round.
func limBudgetClock(t *testing.T, sql, placeholder string) string {
	t.Helper()
	const clock = "now()"
	if strings.Count(sql, clock) != 2 {
		t.Fatalf("budget expression no longer reads the clock twice: %s", sql)
	}
	return strings.ReplaceAll(sql, clock, placeholder+"::timestamptz")
}

// limBudgetWindowSQL lifts the window subquery out of BudgetSQL verbatim, so
// the test observes the four boundaries the console query really gates its sums
// with rather than a copy of them that could drift.
func limBudgetWindowSQL(t *testing.T) string {
	t.Helper()
	const opening, closing = "FROM (SELECT b.day", ") w,"
	start := strings.Index(limits.BudgetSQL, opening)
	stop := strings.Index(limits.BudgetSQL, closing)
	if start < 0 || stop <= start {
		t.Fatalf("BudgetSQL no longer exposes its window subquery: %s", limits.BudgetSQL)
	}
	window := limits.BudgetSQL[start : stop+len(closing)-1]
	return "SELECT w.daily_start,w.daily_end,w.monthly_start,w.monthly_end " +
		limBudgetClock(t, window, "$1")
}

// limBudgetFixture is one piece of spend the budget expression must attribute
// to a window, or refuse to. An empty cost is an attempt that is billable but
// still unpriced; rolled marks the hourly form retention folds facts into.
type limBudgetFixture struct {
	label  string
	at     time.Time
	cost   string
	rolled bool
}

// limTrimDecimal drops the trailing zeros PostgreSQL renders a numeric sum
// with, so a difference in scale is not mistaken for a difference in money.
func limTrimDecimal(value string) string {
	if !strings.Contains(value, ".") {
		return value
	}
	return strings.TrimSuffix(strings.TrimRight(value, "0"), ".")
}

func limSameDecimal(got, want string) bool { return limTrimDecimal(got) == limTrimDecimal(want) }

// TestLimitsBudgetWindowsIgnoreTheSessionTimeZone pins the console budget
// expression to the same UTC day and month the enforcement path charges
// against. A calendar day or month added to a timestamptz is added in the
// session's timezone, so a connection with any TimeZone but UTC used to report
// a month that ends days early — zeroing accrued spend and unpriced attempts
// while the gateway kept refusing the key with 429.
func TestLimitsBudgetWindowsIgnoreTheSessionTimeZone(t *testing.T) {
	pool, _ := limAccounting(t)
	owner, provider := limSeedAuthority(t, pool)
	windowSQL := limBudgetWindowSQL(t)
	budgetSQL := "SELECT " + limBudgetClock(t, limits.BudgetSQL, "$2") +
		" FROM olp.api_keys k WHERE k.id=$1"

	cases := []struct {
		name string
		zone string
		// clock is the instant the expression reads; the zero value reads the
		// database's own clock on the same connection instead.
		clock time.Time
	}{
		{"new_york_month_end", "America/New_York", time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC)},
		{"new_york_daylight_saving", "America/New_York", time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)},
		{"berlin_month_end", "Europe/Berlin", time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC)},
		{"berlin_daylight_saving", "Europe/Berlin", time.Date(2026, 3, 29, 12, 0, 0, 0, time.UTC)},
		{"new_york_now", "America/New_York", time.Time{}},
		{"berlin_now", "Europe/Berlin", time.Time{}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			conn, err := pool.Acquire(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Release()
			var zone string
			if err := conn.QueryRow(t.Context(),
				"SELECT set_config('TimeZone',$1,false)", testCase.zone).Scan(&zone); err != nil {
				t.Fatal(err)
			}
			if zone != testCase.zone {
				t.Fatalf("session TimeZone = %q, want %q", zone, testCase.zone)
			}
			defer func() {
				// The pool is shared with the other cases, so this connection
				// must not carry the timezone back into them.
				if _, err := conn.Exec(t.Context(), "RESET TimeZone"); err != nil {
					t.Errorf("RESET TimeZone: %v", err)
				}
			}()

			clock := testCase.clock
			if clock.IsZero() {
				if err := conn.QueryRow(t.Context(), "SELECT now()").Scan(&clock); err != nil {
					t.Fatal(err)
				}
			}
			windows := limits.BudgetWindows(clock)

			var dailyStart, dailyEnd, monthlyStart, monthlyEnd time.Time
			if err := conn.QueryRow(t.Context(), windowSQL, clock).
				Scan(&dailyStart, &dailyEnd, &monthlyStart, &monthlyEnd); err != nil {
				t.Fatal(err)
			}
			for _, boundary := range []struct {
				name      string
				got, want time.Time
			}{
				{"daily_start", dailyStart, windows.DailyStart},
				{"daily_end", dailyEnd, windows.DailyEnd},
				{"monthly_start", monthlyStart, windows.MonthlyStart},
				{"monthly_end", monthlyEnd, windows.MonthlyEnd},
			} {
				if !boundary.got.Equal(boundary.want) {
					t.Errorf("%s = %s, want %s (clock %s)", boundary.name,
						boundary.got.UTC().Format(time.RFC3339Nano),
						boundary.want.Format(time.RFC3339Nano),
						clock.UTC().Format(time.RFC3339Nano))
				}
			}

			key := limSeedKey(t, pool, owner, false)
			fixtures := []limBudgetFixture{
				{"last half hour of the UTC month", windows.MonthlyEnd.Add(-30 * time.Minute), "1", false},
				{"first half hour after the previous UTC month ended", windows.MonthlyStart.Add(30 * time.Minute), "2", false},
				{"last half hour of the previous UTC month", windows.MonthlyStart.Add(-30 * time.Minute), "4", false},
				{"first half hour of the next UTC month", windows.MonthlyEnd.Add(30 * time.Minute), "8", false},
				{"first half hour of the UTC day", windows.DailyStart.Add(30 * time.Minute), "16", false},
				{"last half hour of the UTC day", windows.DailyEnd.Add(-30 * time.Minute), "32", false},
				{"last half hour of the previous UTC day", windows.DailyStart.Add(-30 * time.Minute), "64", false},
				{"rolled up inside the UTC month", windows.MonthlyEnd.Add(-90 * time.Minute), "128", true},
				{"rolled up in the next UTC month", windows.MonthlyEnd.Add(90 * time.Minute), "256", true},
				{"unpriced inside the UTC month", windows.MonthlyEnd.Add(-45 * time.Minute), "", false},
				{"unpriced in the next UTC month", windows.MonthlyEnd.Add(45 * time.Minute), "", false},
			}
			var daily, monthly, unpriced int64
			for _, fixture := range fixtures {
				inMonth := !fixture.at.Before(windows.MonthlyStart) && fixture.at.Before(windows.MonthlyEnd)
				inDay := !fixture.at.Before(windows.DailyStart) && fixture.at.Before(windows.DailyEnd)
				switch {
				case fixture.cost == "":
					limSeedFact(t, pool, provider, key, fixture.at, nil)
					if inMonth {
						unpriced++
					}
					continue
				case fixture.rolled:
					limSeedHourly(t, pool, provider, key, fixture.at, fixture.cost, 0)
				default:
					limSeedFact(t, pool, provider, key, fixture.at, &fixture.cost)
				}
				amount, err := strconv.ParseInt(fixture.cost, 10, 64)
				if err != nil {
					t.Fatalf("fixture %q: %v", fixture.label, err)
				}
				if inMonth {
					monthly += amount
				}
				if inDay {
					daily += amount
				}
			}

			var payload []byte
			if err := conn.QueryRow(t.Context(), budgetSQL, key, clock).Scan(&payload); err != nil {
				t.Fatal(err)
			}
			var reported struct {
				Daily struct {
					Accrued      string    `json:"accrued"`
					WindowEndsAt time.Time `json:"window_ends_at"`
				} `json:"daily"`
				Monthly struct {
					Accrued      string    `json:"accrued"`
					WindowEndsAt time.Time `json:"window_ends_at"`
				} `json:"monthly"`
				UnpricedAttempts int64 `json:"unpriced_attempts"`
			}
			if err := json.Unmarshal(payload, &reported); err != nil {
				t.Fatalf("budget payload %s: %v", payload, err)
			}
			if !reported.Daily.WindowEndsAt.Equal(windows.DailyEnd) {
				t.Errorf("reported daily window ends at %s, want %s",
					reported.Daily.WindowEndsAt.UTC().Format(time.RFC3339Nano),
					windows.DailyEnd.Format(time.RFC3339Nano))
			}
			if !reported.Monthly.WindowEndsAt.Equal(windows.MonthlyEnd) {
				t.Errorf("reported monthly window ends at %s, want %s",
					reported.Monthly.WindowEndsAt.UTC().Format(time.RFC3339Nano),
					windows.MonthlyEnd.Format(time.RFC3339Nano))
			}
			if want := strconv.FormatInt(daily, 10); !limSameDecimal(reported.Daily.Accrued, want) {
				t.Errorf("daily accrued = %s, want %s", reported.Daily.Accrued, want)
			}
			if want := strconv.FormatInt(monthly, 10); !limSameDecimal(reported.Monthly.Accrued, want) {
				t.Errorf("monthly accrued = %s, want %s", reported.Monthly.Accrued, want)
			}
			if reported.UnpricedAttempts != unpriced {
				t.Errorf("unpriced attempts = %d, want %d", reported.UnpricedAttempts, unpriced)
			}
		})
	}
}
