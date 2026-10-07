//go:build integration

package integration_test

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"math/rand/v2"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/coordination"
	"github.com/tyk-swe/olp/internal/limits"
)

// limCostFixture is one cost owner whose balance PostgreSQL has reconciled, so
// that reservations can be made against it.
type limCostFixture struct {
	c         *coordination.Client
	namespace string
	limiter   *limits.Limiter
	owner     string
	windows   limits.Windows
}

// limCostSeed opens an owner at the given accrued spend in both windows.
func limCostSeed(t *testing.T, label, accrued string) *limCostFixture {
	t.Helper()
	c := limClient(t)
	namespace := limNamespace(t, c, label)
	f := &limCostFixture{
		c: c, namespace: namespace, limiter: limLimiter(t, c, namespace),
		owner: uuid.NewString(), windows: limits.BudgetWindows(time.Now()),
	}
	f.accrue(t, "", accrued)
	return f
}

// accrue installs a snapshot of the owner's spend the way accounting does,
// naming the request it accounts for when there is one.
func (f *limCostFixture) accrue(t *testing.T, request, accrued string) {
	t.Helper()
	if err := f.accrueErr(t, request, accrued); err != nil {
		t.Fatalf("ApplyCostSnapshot: %v", err)
	}
}

// accrueErr is accrue for a goroutine other than the test's own.
func (f *limCostFixture) accrueErr(t *testing.T, request, accrued string) error {
	_, _, err := f.limiter.ApplyCostSnapshot(t.Context(), limits.CostSnapshot{
		CostOwnerID: f.owner, DailyWindowID: f.windows.DailyID, DailyAccrued: accrued,
		MonthlyWindowID: f.windows.MonthlyID, MonthlyAccrued: accrued, RequestID: request,
	})
	return err
}

// day names the owner's accrued daily balance.
func (f *limCostFixture) day() string {
	day, _ := limCostKeys(f.namespace, f.owner)
	return day
}

// request is a cost-only reservation of amount under a fresh request identity.
func (f *limCostFixture) request(amount string) limits.Request {
	return limits.Request{
		CostOwnerID: f.owner, LookupID: limLookup(),
		DailyCostLimit: limPointer("1"), MonthlyCostLimit: limPointer("10"),
		LeaseTTL: 5 * time.Second, CostEstimate: amount, RequestID: uuid.NewString(),
	}
}

// keys names the owner's pending reservations, which share the balances' tag.
func (f *limCostFixture) keys() (pending, expiry string) {
	prefix := f.namespace + ":{" + strings.ReplaceAll(f.owner, "-", "") + "}:cost"
	return prefix + ":pending", prefix + ":expiry"
}

// reserved reads the total the limiter reports as held.
func (f *limCostFixture) reserved(t *testing.T) string {
	t.Helper()
	total, err := f.limiter.Reserved(t.Context(), f.owner)
	if err != nil {
		t.Fatalf("Reserved: %v", err)
	}
	return total
}

func (f *limCostFixture) wantReserved(t *testing.T, want string) {
	t.Helper()
	if got := f.reserved(t); got != want {
		pending, expiry := f.keys()
		hash := limHashOrNone(t, f.c, pending)
		if len(hash) > 20 {
			t.Fatalf("reserved = %q, want %q (%d reservations)", got, want, len(hash)-1)
		}
		t.Fatalf("reserved = %q, want %q (pending %v, expiry %v)", got, want, hash,
			do(t, f.c, "ZRANGE", expiry, "0", "-1", "WITHSCORES"))
	}
}

func limHashOrNone(t *testing.T, c *coordination.Client, key string) map[string]string {
	t.Helper()
	if limInt(t, c, "EXISTS", key) == 0 {
		return nil
	}
	return limHash(t, c, key)
}

// limDecimal writes an amount of at most twelve fractional digits the canonical
// way, without trailing zeros.
func limDecimal(amount *big.Rat) string {
	text := amount.FloatString(12)
	if strings.Contains(text, ".") {
		text = strings.TrimRight(strings.TrimRight(text, "0"), ".")
	}
	return text
}

// limMoney reads a decimal exactly, so sums are compared without rounding.
func limMoney(t *testing.T, text string) *big.Rat {
	t.Helper()
	value, ok := new(big.Rat).SetString(text)
	if !ok {
		t.Fatalf("%q is not a decimal", text)
	}
	return value
}

func limExpiryScore(t *testing.T, c *coordination.Client, key, member string) int64 {
	t.Helper()
	switch score := do(t, c, "ZSCORE", key, member).(type) {
	case float64:
		return int64(score)
	case string:
		parsed, ok := new(big.Float).SetString(score)
		if !ok {
			t.Fatalf("score %q", score)
		}
		milliseconds, _ := parsed.Int64()
		return milliseconds
	default:
		t.Fatalf("ZSCORE %s %s = %#v", key, member, score)
		return 0
	}
}

// limHas reports whether a hash holds a field.
func limHas(t *testing.T, c *coordination.Client, key, field string) bool {
	t.Helper()
	switch found := do(t, c, "HEXISTS", key, field).(type) {
	case bool:
		return found
	case int64:
		return found == 1
	default:
		t.Fatalf("HEXISTS %s %s = %#v", key, field, found)
		return false
	}
}

func TestLimitsCostReservationsCountAgainstTheBalance(t *testing.T) {
	f := limCostSeed(t, "reserve-cost", "0.1")
	pending, expiry := f.keys()

	first, second := f.request("0.4"), f.request("0.4")
	limReserve(t, f.limiter, first)
	limReserve(t, f.limiter, second)
	f.wantReserved(t, "0.8")
	hash := limHash(t, f.c, pending)
	if hash["sum"] != "0.8" || hash[first.RequestID] != "0.4" || hash[second.RequestID] != "0.4" || len(hash) != 3 {
		t.Fatalf("pending = %v, want a field per request and their sum", hash)
	}
	if got := limInt(t, f.c, "ZCARD", expiry); got != 2 {
		t.Fatalf("expiry members = %d, want one per reservation", got)
	}
	// A cost-only request never asks the rate script for anything.
	rate, _ := limRateKeys(f.namespace, first.LookupID)
	if got := limInt(t, f.c, "EXISTS", rate); got != 0 {
		t.Fatal("a cost-only reservation touched the rate counters")
	}
	// Both keys outlive the newest reservation, which lasts its lease and the
	// default grace, and not by more than a second.
	for _, key := range []string{pending, expiry} {
		ttl := limInt(t, f.c, "PTTL", key)
		longest := (5*time.Second + limits.DefaultCostGrace + time.Second).Milliseconds()
		if ttl < 5_000 || ttl > longest {
			t.Fatalf("PTTL %s = %d, want it to cover the lease and its grace", key, ttl)
		}
	}

	// 0.1 accrued and 0.8 in flight leave room for 0.1 and nothing more.
	rejected := f.request("0.4")
	_, err := f.limiter.Reserve(t.Context(), rejected)
	// In-flight reservations are what stands in the way, so waiting helps.
	if retry := limExceeded(t, err, limits.DimensionDailyCost); retry != time.Second {
		t.Fatalf("retry = %v, want the transient hint of one second", retry)
	}
	f.wantReserved(t, "0.8")
	exact := f.request("0.1")
	limReserve(t, f.limiter, exact)
	f.wantReserved(t, "0.9")
	// The balance is spent to the unit, and one more unit is refused.
	_, err = f.limiter.Reserve(t.Context(), f.request("0.000000000001"))
	limExceeded(t, err, limits.DimensionDailyCost)

	// A request that could never fit, however much is released, waits for the
	// window and not for the second.
	_, err = f.limiter.Reserve(t.Context(), f.request("1"))
	retry := limExceeded(t, err, limits.DimensionDailyCost)
	if remaining := time.Until(f.windows.DailyEnd); retry > remaining+time.Second || retry < remaining-5*time.Second {
		t.Fatalf("retry = %v for a request no release can admit, want the %v the day has left", retry, remaining)
	}
}

// TestLimitsRefusalSaysWhetherTheBudgetIsSpentOrTheEstimateDoesNotFit proves a
// refusal tells a budget that has been used up from one that has room and cannot
// hold this request's estimate beside what is spent and in flight, in either
// window: the first is a fact about the budget, the second about the request.
func TestLimitsRefusalSaysWhetherTheBudgetIsSpentOrTheEstimateDoesNotFit(t *testing.T) {
	f := limCostSeed(t, "refusal-kind", "0.5")
	// 0.5 of 1 is spent, so 0.6 does not fit although the budget is not used up.
	_, err := f.limiter.Reserve(t.Context(), f.request("0.6"))
	if refusal := limRefused(t, err, limits.DimensionDailyCost); !refusal.Estimate {
		t.Fatalf("daily refusal = %+v, want the estimate that does not fit", refusal)
	}
	monthly := f.request("0.6")
	monthly.DailyCostLimit, monthly.MonthlyCostLimit = nil, limPointer("1")
	_, err = f.limiter.Reserve(t.Context(), monthly)
	if refusal := limRefused(t, err, limits.DimensionMonthlyCost); !refusal.Estimate {
		t.Fatalf("monthly refusal = %+v, want the estimate that does not fit", refusal)
	}
	// Once the day's spend reaches its limit the budget is used up, whatever the
	// request would cost, and an unpriced request is refused the same way.
	f.accrue(t, "", "1")
	for _, estimate := range []string{"0.000001", ""} {
		request := f.request(estimate)
		if estimate == "" {
			request.RequestID = ""
		}
		_, err = f.limiter.Reserve(t.Context(), request)
		if refusal := limRefused(t, err, limits.DimensionDailyCost); refusal.Estimate {
			t.Fatalf("refusal of %q against a spent budget = %+v, want the budget exhausted", estimate, refusal)
		}
	}
}

func TestLimitsCostReservationsCountAgainstBothWindows(t *testing.T) {
	f := limCostSeed(t, "reserve-windows", "0")
	request := func(amount string, daily, monthly string) limits.Request {
		r := f.request(amount)
		r.DailyCostLimit, r.MonthlyCostLimit = limPointer(daily), limPointer(monthly)
		return r
	}
	limReserve(t, f.limiter, request("3", "100", "5"))
	// The day has room for plenty; the month, holding 3 of 5, has room for 2.
	_, err := f.limiter.Reserve(t.Context(), request("3", "100", "5"))
	if retry := limExceeded(t, err, limits.DimensionMonthlyCost); retry != time.Second {
		t.Fatalf("retry = %v, want the transient hint", retry)
	}
	limReserve(t, f.limiter, request("2", "100", "5"))
	f.wantReserved(t, "5")
	// A key limited by one window ignores the other window's limit.
	onlyDaily := request("2", "100", "5")
	onlyDaily.MonthlyCostLimit = nil
	limReserve(t, f.limiter, onlyDaily)
}

func TestLimitsCostReservationIncreasesOnlyWhenTheNewTotalFits(t *testing.T) {
	f := limCostSeed(t, "grow-cost", "0")
	request := f.request("0.2")
	lease := limReserve(t, f.limiter, request)
	limReserve(t, f.limiter, f.request("0.3"))

	request.CostEstimate, request.RetainCostReservation = "0.6", true
	limReserve(t, f.limiter, request)
	f.wantReserved(t, "0.9")
	// Replays and smaller estimates retain the larger bound without adding it.
	limReserve(t, f.limiter, request)
	request.CostEstimate = "0.4"
	limReserve(t, f.limiter, request)
	f.wantReserved(t, "0.9")

	request.CostEstimate = "0.8"
	_, err := f.limiter.Reserve(t.Context(), request)
	limExceeded(t, err, limits.DimensionDailyCost)
	f.wantReserved(t, "0.9")
	// The original handle settles the grown reservation under the same identity.
	lease.SetActualCost("0.1")
	if err := lease.SettleCost(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.wantReserved(t, "0.4")
}

func TestLimitsCostReservationsAreGivenBackAndSettled(t *testing.T) {
	f := limCostSeed(t, "settle-cost", "0")
	pending, expiry := f.keys()

	released := f.request("0.4")
	lease := limReserve(t, f.limiter, released)
	if !lease.HasCostReservation() {
		t.Fatal("a granted estimate is not reported")
	}
	f.wantReserved(t, "0.4")
	// A request that never dispatched owes nothing.
	for range 2 {
		if err := lease.Refund(t.Context()); err != nil {
			t.Fatalf("Refund: %v", err)
		}
	}
	f.wantReserved(t, "0")
	if limInt(t, f.c, "EXISTS", pending, expiry) != 0 {
		t.Fatal("releasing the last reservation left state behind")
	}

	// Settlement replaces the estimate with what the request cost.
	settled := f.request("0.4")
	lease = limReserve(t, f.limiter, settled)
	lease.SetActualCost("0.15")
	for range 2 {
		if err := lease.SettleCost(t.Context()); err != nil {
			t.Fatalf("SettleCost: %v", err)
		}
	}
	f.wantReserved(t, "0.15")
	if hash := limHash(t, f.c, pending); hash[settled.RequestID] != "0.15" {
		t.Fatalf("pending = %v, want the actual cost", hash)
	}
	// Spending nothing releases the reservation.
	lease.SetActualCost("0")
	if err := lease.SettleCost(t.Context()); err != nil {
		t.Fatalf("SettleCost: %v", err)
	}
	f.wantReserved(t, "0")

	// Settling a reservation that is gone does nothing and creates nothing, which
	// is what keeps a late settlement from counting spend that accounting has.
	lease.SetActualCost("0.3")
	if err := lease.SettleCost(t.Context()); err != nil {
		t.Fatalf("SettleCost on a released reservation: %v", err)
	}
	f.wantReserved(t, "0")
	if limInt(t, f.c, "EXISTS", pending, expiry) != 0 {
		t.Fatal("settling an absent reservation created one")
	}
	// A lease with no actual cost recorded settles nothing.
	kept := f.request("0.2")
	lease = limReserve(t, f.limiter, kept)
	if err := lease.SettleCost(t.Context()); err != nil {
		t.Fatalf("SettleCost: %v", err)
	}
	f.wantReserved(t, "0.2")
}

func TestLimitsSettlementShortensAReservationAndNeverExtendsIt(t *testing.T) {
	f := limCostSeed(t, "settle-expiry", "0")
	_, expiry := f.keys()

	// Granted for the lease and its grace, which together outlast the settlement,
	// whose own grace begins when it is made.
	long := f.request("0.1")
	long.LeaseTTL, long.CostGrace = time.Minute, 30*time.Second
	lease := limReserve(t, f.limiter, long)
	granted := limExpiryScore(t, f.c, expiry, long.RequestID)
	lease.SetActualCost("0.1")
	if err := lease.SettleCost(t.Context()); err != nil {
		t.Fatalf("SettleCost: %v", err)
	}
	settled := limExpiryScore(t, f.c, expiry, long.RequestID)
	if settled > granted-50_000 {
		t.Fatalf("settled expiry %d, granted %d: settlement did not shorten the reservation", settled, granted)
	}

	// Granted for a moment and a long grace: settling later would put the expiry
	// after the one admission set, which is the backstop and must stand.
	short := f.request("0.1")
	short.LeaseTTL, short.CostGrace = time.Millisecond, 30*time.Second
	lease = limReserve(t, f.limiter, short)
	granted = limExpiryScore(t, f.c, expiry, short.RequestID)
	time.Sleep(50 * time.Millisecond)
	lease.SetActualCost("0.1")
	if err := lease.SettleCost(t.Context()); err != nil {
		t.Fatalf("SettleCost: %v", err)
	}
	if after := limExpiryScore(t, f.c, expiry, short.RequestID); after != granted {
		t.Fatalf("expiry %d after settlement, want the granted %d", after, granted)
	}
}

func TestLimitsAccrualRemovesTheReservationItAccountsFor(t *testing.T) {
	f := limCostSeed(t, "accrual", "0")
	pending, expiry := f.keys()
	first, second := f.request("0.4"), f.request("0.3")
	firstLease := limReserve(t, f.limiter, first)
	limReserve(t, f.limiter, second)
	f.wantReserved(t, "0.7")

	// The spend and the removal of what held its place arrive as one step, so
	// the budget never counts the request twice, and the other request stays.
	f.accrue(t, first.RequestID, "0.35")
	f.wantReserved(t, "0.3")
	if day := limHash(t, f.c, f.day()); day["accrued"] != "0.35" {
		t.Fatalf("day = %v, want the accrued spend installed", day)
	}
	// Delivered again, which accounting does, it removes nothing more.
	f.accrue(t, first.RequestID, "0.35")
	f.wantReserved(t, "0.3")
	// A settlement that arrives after accrual cannot bring the request back.
	firstLease.SetActualCost("0.2")
	if err := firstLease.SettleCost(t.Context()); err != nil {
		t.Fatalf("SettleCost after accrual: %v", err)
	}
	f.wantReserved(t, "0.3")
	if limHas(t, f.c, pending, first.RequestID) {
		t.Fatal("a late settlement recreated an accounted reservation")
	}

	// An accrual that names no request removes none.
	f.accrue(t, "", "0.36")
	f.wantReserved(t, "0.3")
	f.accrue(t, second.RequestID, "0.4")
	f.wantReserved(t, "0")
	if limInt(t, f.c, "EXISTS", pending, expiry) != 0 {
		t.Fatal("removing the last reservation left state behind")
	}
	// Accrual never lowers a balance, replayed or not.
	f.accrue(t, second.RequestID, "0.1")
	if day := limHash(t, f.c, f.day()); day["accrued"] != "0.4" {
		t.Fatalf("day = %v, want the higher balance kept", day)
	}
}

func TestLimitsExpiredReservationsStopCounting(t *testing.T) {
	f := limCostSeed(t, "expiry", "0")
	pending, expiry := f.keys()

	lapsing := f.request("0.9")
	lapsing.LeaseTTL, lapsing.CostGrace = time.Millisecond, time.Millisecond
	limReserve(t, f.limiter, lapsing)
	f.wantReserved(t, "0.9")
	time.Sleep(60 * time.Millisecond)
	// Reading the total retires nothing; the next reservation does.
	f.wantReserved(t, "0.9")
	next := f.request("0.9")
	limReserve(t, f.limiter, next)
	f.wantReserved(t, "0.9")
	if hash := limHash(t, f.c, pending); len(hash) != 2 || hash[next.RequestID] != "0.9" {
		t.Fatalf("pending = %v, want only the live reservation", hash)
	}
	if got := limInt(t, f.c, "ZCARD", expiry); got != 1 {
		t.Fatalf("expiry members = %d, want the lapsed one retired", got)
	}

	// Accounting that arrives after a reservation lapsed finds nothing and may
	// not drive the total below what is still reserved.
	f.accrue(t, lapsing.RequestID, "0")
	f.wantReserved(t, "0.9")

	// A lapsed reservation is not revived by a settlement either.
	stale := f.request("0.05")
	stale.LeaseTTL, stale.CostGrace = time.Millisecond, time.Millisecond
	lease := limReserve(t, f.limiter, stale)
	time.Sleep(60 * time.Millisecond)
	lease.SetActualCost("0.05")
	if err := lease.SettleCost(t.Context()); err != nil {
		t.Fatalf("SettleCost: %v", err)
	}
	f.wantReserved(t, "0.9")
	if limHas(t, f.c, pending, stale.RequestID) {
		t.Fatal("a settlement revived a reservation that had lapsed")
	}
}

func TestLimitsDamagedReservationStateHealsAndNeverBlocksAccrual(t *testing.T) {
	f := limCostSeed(t, "pending-damage", "0")
	pending, expiry := f.keys()

	// Reservations are advisory and derived, so damage to them is discarded and
	// the request that found it is judged as if none were held.
	do(t, f.c, "SET", pending, "junk")
	do(t, f.c, "SET", expiry, "junk")
	healed := f.request("0.1")
	limReserve(t, f.limiter, healed)
	f.wantReserved(t, "0.1")
	do(t, f.c, "HSET", pending, "stray", "0.5")
	do(t, f.c, "HDEL", pending, "sum")
	limReserve(t, f.limiter, f.request("0.2"))
	f.wantReserved(t, "0.2")
	// A total that is not a number cannot be trusted to describe the leases beside
	// it, and is not read as zero: they go with it, or what they hold would stop
	// counting while still being kept.
	do(t, f.c, "HSET", pending, "sum", "abc")
	limReserve(t, f.limiter, f.request("0.3"))
	f.wantReserved(t, "0.3")
	if hash := limHash(t, f.c, pending); len(hash) != 2 || limHas(t, f.c, pending, healed.RequestID) {
		t.Fatalf("pending = %v, want only the reservation made after the damage", hash)
	}

	// Whatever the reservations look like, accrued spend is installed: it is the
	// authority, and a damaged advisory key must not stop PostgreSQL's totals.
	do(t, f.c, "DEL", pending, expiry)
	do(t, f.c, "SET", pending, "junk")
	do(t, f.c, "SET", expiry, "junk")
	f.accrue(t, uuid.NewString(), "0.7")
	if hash := limHash(t, f.c, f.day()); hash["accrued"] != "0.7" {
		t.Fatalf("day = %v, want the accrued spend installed", hash)
	}
	if limInt(t, f.c, "EXISTS", pending, expiry) != 0 {
		t.Fatal("damaged reservation state was kept")
	}
	// Accrued state keeps the fail-closed rule: the balances are never guessed.
	do(t, f.c, "SET", f.day(), "not-a-hash")
	if _, err := f.limiter.Reserve(t.Context(), f.request("0.1")); err == nil {
		t.Fatal("a damaged balance admitted a request")
	}
}

// TestLimitsReservationKeysOutliveTheirNewestReservation proves a reservation that
// ends sooner than one already held does not shorten how long the keys stay: the
// newest one sets it, whichever request made it. A key that expired early would
// take the longer request's reservation with it while that request was running.
func TestLimitsReservationKeysOutliveTheirNewestReservation(t *testing.T) {
	f := limCostSeed(t, "outlive", "0")
	pending, expiry := f.keys()
	long := f.request("0.1")
	long.LeaseTTL, long.CostGrace = 10*time.Minute, 5*time.Minute
	limReserve(t, f.limiter, long)
	short := f.request("0.1")
	short.LeaseTTL, short.CostGrace = time.Second, time.Second
	limReserve(t, f.limiter, short)
	fifteenMinutes := (15 * time.Minute).Milliseconds()
	for _, key := range []string{pending, expiry} {
		// The long reservation lasts fifteen minutes, and the keys a second more.
		if ttl := limInt(t, f.c, "PTTL", key); ttl < fifteenMinutes-5_000 || ttl > fifteenMinutes+1_000 {
			t.Fatalf("PTTL %s = %d, want it to outlast the longer reservation", key, ttl)
		}
	}
}

// TestLimitsEveryScriptSurvivesDamagedReservationKeys proves a reservation key of
// the wrong type, alone or beside a sound one, never raises an error in the
// script that meets it: not for a request being admitted, not for one settling or
// being refunded, and above all not for the accrual that installs PostgreSQL's
// spend. Reservations are advisory, so what is damaged is discarded and the next
// request is judged without it.
func TestLimitsEveryScriptSurvivesDamagedReservationKeys(t *testing.T) {
	// A damage is done to the keys of an owner whose request, named by lease, holds
	// a reservation beside another request's, so that whichever key is sound still
	// describes both.
	type damage func(t *testing.T, f *limCostFixture, lease string)
	soon := func(t *testing.T, f *limCostFixture) string {
		return strconv.FormatInt(limServerTimeMS(t, f.c)+60_000, 10)
	}
	damages := map[string]damage{
		"both keys are text": func(t *testing.T, f *limCostFixture, lease string) {
			pending, expiry := f.keys()
			do(t, f.c, "DEL", pending, expiry)
			do(t, f.c, "SET", pending, "junk")
			do(t, f.c, "SET", expiry, "junk")
		},
		"the hash is text beside a sound set": func(t *testing.T, f *limCostFixture, lease string) {
			pending, expiry := f.keys()
			do(t, f.c, "DEL", pending, expiry)
			do(t, f.c, "SET", pending, "junk")
			do(t, f.c, "ZADD", expiry, soon(t, f), lease, soon(t, f), uuid.NewString())
		},
		"the set is text beside a sound hash": func(t *testing.T, f *limCostFixture, lease string) {
			pending, expiry := f.keys()
			do(t, f.c, "DEL", pending, expiry)
			do(t, f.c, "SET", expiry, "junk")
			do(t, f.c, "HSET", pending, lease, "0.2", uuid.NewString(), "0.5", "sum", "0.7")
		},
		"the hash is a list": func(t *testing.T, f *limCostFixture, lease string) {
			pending, expiry := f.keys()
			do(t, f.c, "DEL", pending, expiry)
			do(t, f.c, "RPUSH", pending, "junk")
		},
		"the set is a hash": func(t *testing.T, f *limCostFixture, lease string) {
			pending, expiry := f.keys()
			do(t, f.c, "DEL", pending, expiry)
			do(t, f.c, "HSET", pending, lease, "0.2", uuid.NewString(), "0.5", "sum", "0.7")
			do(t, f.c, "HSET", expiry, "junk", "1")
		},
	}
	operations := map[string]func(t *testing.T, f *limCostFixture, damaged damage){
		"admission": func(t *testing.T, f *limCostFixture, damaged damage) {
			request := f.request("0.2")
			damaged(t, f, request.RequestID)
			limReserve(t, f.limiter, request)
			f.wantReserved(t, "0.2")
		},
		"settlement": func(t *testing.T, f *limCostFixture, damaged damage) {
			request := f.request("0.2")
			held := limReserve(t, f.limiter, request)
			damaged(t, f, request.RequestID)
			held.SetActualCost("0.1")
			if err := held.SettleCost(t.Context()); err != nil {
				t.Fatalf("SettleCost: %v", err)
			}
		},
		"refund": func(t *testing.T, f *limCostFixture, damaged damage) {
			request := f.request("0.2")
			held := limReserve(t, f.limiter, request)
			damaged(t, f, request.RequestID)
			if err := held.Refund(t.Context()); err != nil {
				t.Fatalf("Refund: %v", err)
			}
		},
		"accrual": func(t *testing.T, f *limCostFixture, damaged damage) {
			request := f.request("0.2")
			limReserve(t, f.limiter, request)
			damaged(t, f, request.RequestID)
			f.accrue(t, request.RequestID, "0.7")
			if day := limHash(t, f.c, f.day()); day["accrued"] != "0.7" {
				t.Fatalf("day = %v, want the accrued spend installed", day)
			}
		},
	}
	for operation, run := range operations {
		for name, damaged := range damages {
			t.Run(operation+" when "+name, func(t *testing.T) {
				f := limCostSeed(t, "damage-matrix", "0")
				run(t, f, damaged)
				// Whatever was left, the next request is admitted and finds sound keys.
				limReserve(t, f.limiter, f.request("0.1"))
				f.wantConsistent(t)
			})
		}
	}
}

func TestLimitsUnpricedRequestsReserveNothing(t *testing.T) {
	f := limCostSeed(t, "unpriced", "0")
	pending, expiry := f.keys()
	request := f.request("")
	request.RequestID = ""
	lease := limReserve(t, f.limiter, request)
	if lease.HasCostReservation() {
		t.Fatal("a request without an estimate reports a reservation")
	}
	lease.SetActualCost("0.4")
	if err := errors.Join(lease.SettleCost(t.Context()), lease.Refund(t.Context())); err != nil {
		t.Fatalf("finishing an unpriced lease: %v", err)
	}
	if limInt(t, f.c, "EXISTS", pending, expiry) != 0 {
		t.Fatal("an unpriced request created reservation state")
	}
	// It is still judged on what has accrued.
	f.accrue(t, "", "1")
	_, err := f.limiter.Reserve(t.Context(), request)
	limExceeded(t, err, limits.DimensionDailyCost)
}

func TestLimitsRateRejectionGivesBackTheCostReservation(t *testing.T) {
	f := limCostSeed(t, "compensation", "0")
	lookup := limLookup()
	request := func(amount string) limits.Request {
		r := f.request(amount)
		r.LookupID = lookup
		r.RequestsPerMinute = limPointer(int64(1))
		return r
	}
	first := limReserve(t, f.limiter, request("0.1"))
	f.wantReserved(t, "0.1")
	// The request window is spent, so the rate script refuses the second request
	// after the cost script has already reserved for it.
	_, err := f.limiter.Reserve(t.Context(), request("0.2"))
	limExceeded(t, err, limits.DimensionRequests)
	f.wantReserved(t, "0.1")
	if err := first.Refund(t.Context()); err != nil {
		t.Fatalf("Refund: %v", err)
	}
	f.wantReserved(t, "0")
	// Both halves of the lease are given back together.
	limReserve(t, f.limiter, request("0.3"))
	f.wantReserved(t, "0.3")
}

func TestLimitsConcurrentReservationsCannotOvershootTheBalance(t *testing.T) {
	// Sixty-four replicas admit at once against a balance of 0.25 spent and a
	// limit of 1: the script decides each one against the others, so exactly
	// floor((1 - 0.25) / 0.3) = 2 are admitted and the rest are refused.
	f := limCostSeed(t, "overshoot", "0.25")
	var granted, refused atomic.Int64
	var group sync.WaitGroup
	for range 64 {
		group.Go(func() {
			_, err := f.limiter.Reserve(t.Context(), f.request("0.3"))
			var exceeded *limits.ExceededError
			switch {
			case err == nil:
				granted.Add(1)
			case errors.As(err, &exceeded) && exceeded.Dimension == limits.DimensionDailyCost:
				refused.Add(1)
			default:
				t.Errorf("Reserve: %v", err)
			}
		})
	}
	group.Wait()
	if granted.Load() != 2 || refused.Load() != 62 {
		t.Fatalf("admitted %d and refused %d, want 2 and 62", granted.Load(), refused.Load())
	}
	f.wantReserved(t, "0.6")
}

// TestLimitsReservationsStayConsistentUnderConcurrentChanges runs reservations,
// settlements, releases and accruals against one owner from many goroutines.
// Each goroutine owns its requests, so what must remain is exactly what each
// of them did not finish, and the total must be the sum of the entries.
func TestLimitsReservationsStayConsistentUnderConcurrentChanges(t *testing.T) {
	f := limCostSeed(t, "interleaved", "0")
	pending, expiry := f.keys()
	var mu sync.Mutex
	model := map[string]*big.Rat{}
	var group sync.WaitGroup
	for worker := range 8 {
		group.Go(func() {
			rng := rand.New(rand.NewPCG(uint64(worker), 77))
			type entry struct {
				request limits.Request
				lease   *limits.Lease
				amount  *big.Rat
			}
			live := map[string]*entry{}
			// Amounts are whole millionths below three, never zero.
			money := func() string {
				millionths := 1 + rng.IntN(2_999_999)
				return fmt.Sprintf("%d.%06d", millionths/1_000_000, millionths%1_000_000)
			}
			rat := func(text string) *big.Rat { value, _ := new(big.Rat).SetString(text); return value }
			for range 120 {
				ids := make([]string, 0, len(live))
				for id := range live {
					ids = append(ids, id)
				}
				switch op := rng.IntN(5); {
				case op < 2 || len(ids) == 0:
					r := f.request(money())
					r.DailyCostLimit, r.MonthlyCostLimit = limPointer("999999999999"), limPointer("999999999999")
					lease, err := f.limiter.Reserve(t.Context(), r)
					if err != nil {
						t.Errorf("Reserve: %v", err)
						return
					}
					live[r.RequestID] = &entry{r, lease, rat(r.CostEstimate)}
				case op == 2:
					e := live[ids[rng.IntN(len(ids))]]
					// A request that cost nothing is settled by releasing it.
					actual := "0"
					if rng.IntN(4) > 0 {
						actual = money()
					}
					e.lease.SetActualCost(actual)
					if err := e.lease.SettleCost(t.Context()); err != nil {
						t.Errorf("SettleCost: %v", err)
						return
					}
					if actual == "0" {
						delete(live, e.request.RequestID)
					} else {
						e.amount = rat(actual)
					}
				case op == 3:
					id := ids[rng.IntN(len(ids))]
					if err := live[id].lease.Refund(t.Context()); err != nil {
						t.Errorf("Refund: %v", err)
						return
					}
					delete(live, id)
				default:
					id := ids[rng.IntN(len(ids))]
					if err := f.accrueErr(t, id, "0"); err != nil {
						t.Errorf("ApplyCostSnapshot: %v", err)
						return
					}
					delete(live, id)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			for id, e := range live {
				model[id] = e.amount
			}
		})
	}
	group.Wait()
	if t.Failed() {
		return
	}

	want := new(big.Rat)
	for _, amount := range model {
		want.Add(want, amount)
	}
	var stored map[string]string
	if len(model) > 0 {
		stored = limHash(t, f.c, pending)
		if got := limMoney(t, stored["sum"]); got.Cmp(want) != 0 {
			t.Fatalf("sum = %s, want the %d unfinished reservations' %s", stored["sum"], len(model), want.FloatString(6))
		}
		delete(stored, "sum")
		if len(stored) != len(model) {
			t.Fatalf("%d entries stored, want %d", len(stored), len(model))
		}
		for id, amount := range model {
			if got, ok := stored[id]; !ok || limMoney(t, got).Cmp(amount) != 0 {
				t.Fatalf("entry %s = %q, want %s", id, got, amount.FloatString(6))
			}
		}
		members, _ := do(t, f.c, "ZRANGE", expiry, "0", "-1").([]any)
		if len(members) != len(model) {
			t.Fatalf("%d expiry members, want one per reservation (%d)", len(members), len(model))
		}
	} else if limInt(t, f.c, "EXISTS", pending, expiry) != 0 {
		t.Fatal("no reservations remain but state does")
	}
}

func TestLimitsRetryHintWaitsForEveryRefusingWindow(t *testing.T) {
	f := limCostSeed(t, "retry-hint", "0")
	request := func(amount, daily, monthly string) limits.Request {
		r := f.request(amount)
		r.DailyCostLimit, r.MonthlyCostLimit = limPointer(daily), limPointer(monthly)
		return r
	}
	limReserve(t, f.limiter, request("1", "10", "1.5"))
	// The day refuses nothing; the month is held by the request in flight, so
	// the wait is the second a reservation takes to clear.
	_, err := f.limiter.Reserve(t.Context(), request("1", "10", "1.5"))
	if retry := limExceeded(t, err, limits.DimensionMonthlyCost); retry != time.Second {
		t.Fatalf("retry = %v, want the transient hint", retry)
	}
	// The day is held by in-flight work while the month could never fit the
	// request: waiting a second admits nothing, so the hint is the month's.
	limReserve(t, f.limiter, request("1", "10", "50"))
	_, err = f.limiter.Reserve(t.Context(), request("3", "2.5", "2"))
	if retry := limExceeded(t, err, limits.DimensionMonthlyCost); retry <= time.Minute {
		t.Fatalf("retry = %v, want the window the request can never fit in", retry)
	}
}

// limRefused is limExceeded for a test that also needs to know which kind of
// refusal it was.
func limRefused(t *testing.T, err error, dimension limits.Dimension) *limits.ExceededError {
	t.Helper()
	var exceeded *limits.ExceededError
	if !errors.As(err, &exceeded) {
		t.Fatalf("error = %v, want an ExceededError", err)
	}
	if exceeded.Dimension != dimension {
		t.Fatalf("dimension = %q, want %q", exceeded.Dimension, dimension)
	}
	return exceeded
}

// seed puts one reservation of amount for each id into the owner's pending state
// as if it had been granted, expiring at the given Valkey time, and counts them
// in the total. It is how a state that admission could not have produced, or a
// backlog too large to build one request at a time, is made.
func (f *limCostFixture) seed(t *testing.T, expiresMS int64, amount string, ids ...string) {
	t.Helper()
	pending, expiry := f.keys()
	held := new(big.Rat)
	if limInt(t, f.c, "EXISTS", pending) == 1 {
		if sum, ok := limHashOrNone(t, f.c, pending)["sum"]; ok {
			held = limMoney(t, sum)
		}
	}
	for len(ids) > 0 {
		batch := ids[:min(len(ids), 500)]
		ids = ids[len(batch):]
		leases, scores := []string{"HSET", pending}, []string{"ZADD", expiry}
		for _, id := range batch {
			leases = append(leases, id, amount)
			scores = append(scores, strconv.FormatInt(expiresMS, 10), id)
		}
		do(t, f.c, leases...)
		do(t, f.c, scores...)
		held.Add(held, new(big.Rat).Mul(limMoney(t, amount), new(big.Rat).SetInt64(int64(len(batch)))))
	}
	do(t, f.c, "HSET", pending, "sum", limDecimal(held))
	do(t, f.c, "PEXPIRE", pending, "60000")
	do(t, f.c, "PEXPIRE", expiry, "60000")
}

// lapse seeds count reservations that lapsed long ago and are still counted: a
// backlog of the kind a stalled accounting pipeline leaves.
func (f *limCostFixture) lapse(t *testing.T, count int, amount string) {
	t.Helper()
	ids := make([]string, count)
	for index := range ids {
		ids[index] = uuid.NewString()
	}
	f.seed(t, 1, amount, ids...)
}

// expiries is how many reservations the owner has, lapsed or not.
func (f *limCostFixture) expiries(t *testing.T) int64 {
	t.Helper()
	_, expiry := f.keys()
	return limInt(t, f.c, "ZCARD", expiry)
}

// wantConsistent proves the owner's two keys describe the same reservations and
// that the total is the sum of them, which is what every script keeps true.
func (f *limCostFixture) wantConsistent(t *testing.T) {
	t.Helper()
	pending, _ := f.keys()
	hash := limHashOrNone(t, f.c, pending)
	if hash == nil {
		if got := f.expiries(t); got != 0 {
			t.Fatalf("no reservation is held but %d expiries remain", got)
		}
		return
	}
	held, leases := new(big.Rat), int64(0)
	for field, amount := range hash {
		if field != "sum" {
			held.Add(held, limMoney(t, amount))
			leases++
		}
	}
	if sum := limMoney(t, hash["sum"]); sum.Cmp(held) != 0 {
		t.Fatalf("sum = %s, but the %d reservations hold %s", sum.FloatString(12), leases, held.FloatString(12))
	}
	if got := f.expiries(t); got != leases {
		t.Fatalf("%d expiries for %d reservations", got, leases)
	}
}

// limBacklog is large enough that no sensible bound on one call reaches the end
// of it.
const limBacklog = 3000

// TestLimitsReservationSweepIsBoundedPerCall proves a backlog of lapsed
// reservations is retired a page at a time and not all at once: one call holds
// the one thread that serves every client of the Valkey for as long as it works,
// so what it may retire is bounded and not left to the size of the backlog. What
// is left keeps counting until later calls retire it, which holds a budget back
// for a while and never lets it overspend.
func TestLimitsReservationSweepIsBoundedPerCall(t *testing.T) {
	f := limCostSeed(t, "sweep-bounded", "0")
	live := f.request("0.2")
	limReserve(t, f.limiter, live)
	f.lapse(t, limBacklog, "0.0001")
	f.wantReserved(t, "0.5")

	// Retired, the backlog leaves room for 0.6 beside the 0.2 in flight; counted,
	// it does not. The refusal is the transient kind: this request would fit.
	before := f.expiries(t)
	_, err := f.limiter.Reserve(t.Context(), f.request("0.6"))
	refusal := limRefused(t, err, limits.DimensionDailyCost)
	if !refusal.Estimate || refusal.RetryAfter != time.Second {
		t.Fatalf("refusal = %+v, want the estimate's transient hint of a second", refusal)
	}
	page := before - f.expiries(t)
	if page < 1 || page > 1000 || page >= limBacklog {
		t.Fatalf("one call retired %d of %d lapsed reservations, want a bounded page", page, limBacklog)
	}
	f.wantConsistent(t)
	want := new(big.Rat).Sub(limMoney(t, "0.5"), new(big.Rat).Mul(limMoney(t, "0.0001"), new(big.Rat).SetInt64(page)))
	if got := limMoney(t, f.reserved(t)); got.Cmp(want) != 0 {
		t.Fatalf("reserved = %s after retiring %d, want the rest still counted: %s", got.FloatString(12), page, want.FloatString(12))
	}

	// Later calls retire the same page each, never more, and the balance is never
	// read as less than the live reservation holds.
	admitted := false
	for call := 2; call <= limBacklog/int(page)+2; call++ {
		before = f.expiries(t)
		_, err = f.limiter.Reserve(t.Context(), f.request("0.6"))
		if err == nil {
			admitted = true
			break
		}
		limRefused(t, err, limits.DimensionDailyCost)
		if retired := before - f.expiries(t); retired > page {
			t.Fatalf("call %d retired %d, more than the %d a call may", call, retired, page)
		}
		if limMoney(t, f.reserved(t)).Cmp(limMoney(t, "0.2")) < 0 {
			t.Fatalf("call %d left less than the live reservation counted", call)
		}
		f.wantConsistent(t)
	}
	if !admitted {
		t.Fatal("the request was never admitted although every call retired a page")
	}

	// Calls that follow drain the rest, and the total is then what is live.
	for call := 0; f.expiries(t) > 2; call++ {
		if call > limBacklog/int(page)+2 {
			t.Fatalf("%d lapsed reservations remain after %d more calls", f.expiries(t)-2, call)
		}
		small := f.request("0.000001")
		lease := limReserve(t, f.limiter, small)
		if err := lease.Refund(t.Context()); err != nil {
			t.Fatalf("Refund: %v", err)
		}
		f.wantConsistent(t)
	}
	f.wantReserved(t, "0.8")
}

// TestLimitsSettlementSweepIsBoundedToo proves the settlement a request makes as
// it ends retires no more than a reservation does, since it can be the call that
// finds the backlog.
func TestLimitsSettlementSweepIsBoundedToo(t *testing.T) {
	f := limCostSeed(t, "settle-bounded", "0")
	pending, _ := f.keys()
	live := f.request("0.2")
	lease := limReserve(t, f.limiter, live)
	f.lapse(t, limBacklog, "0.0001")

	before := f.expiries(t)
	lease.SetActualCost("0.1")
	if err := lease.SettleCost(t.Context()); err != nil {
		t.Fatalf("SettleCost: %v", err)
	}
	page := before - f.expiries(t)
	if page < 1 || page > 1000 || page >= limBacklog {
		t.Fatalf("one settlement retired %d of %d lapsed reservations, want a bounded page", page, limBacklog)
	}
	if got := limHash(t, f.c, pending)[live.RequestID]; got != "0.1" {
		t.Fatalf("the settled reservation holds %q, want what the request cost", got)
	}
	f.wantConsistent(t)
}

// TestLimitsSettlementReleasesALapsedReservationTheSweepDidNotReach proves a
// settlement does not give a reservation that has lapsed a new amount to count: the
// bounded sweep retires the lapsed ones that expired first, so one that expired
// later is still there, past its expiry, when its request settles, and what it
// would be held at no longer matters because it no longer counts.
func TestLimitsSettlementReleasesALapsedReservationTheSweepDidNotReach(t *testing.T) {
	f := limCostSeed(t, "settle-lapsed", "0")
	pending, expiry := f.keys()
	// Another request still in flight keeps the keys alive, as one does on a busy
	// owner, so that what a settlement does to the lapsed one can be read.
	live := f.request("0.3")
	limReserve(t, f.limiter, live)
	request := f.request("0.2")
	lease := limReserve(t, f.limiter, request)
	// More lapsed reservations than one page retires, all older than the request's.
	f.lapse(t, 600, "0.0001")
	do(t, f.c, "ZADD", expiry, "XX", strconv.FormatInt(limServerTimeMS(t, f.c)-1000, 10), request.RequestID)

	// A cost of nothing releases the reservation whether or not it had lapsed, and
	// so would not tell the settlement apart from a release: this one costs 0.1.
	lease.SetActualCost("0.1")
	if err := lease.SettleCost(t.Context()); err != nil {
		t.Fatalf("SettleCost: %v", err)
	}
	hash := limHashOrNone(t, f.c, pending)
	if _, kept := hash[request.RequestID]; kept || do(t, f.c, "ZSCORE", expiry, request.RequestID) != nil {
		t.Fatalf("a settlement kept a reservation that had lapsed, at %q", hash[request.RequestID])
	}
	// The sweep retired a page of the older ones, and not the request's own; the
	// request in flight is untouched.
	if left := f.expiries(t); left != 600-256+1 {
		t.Fatalf("%d reservations are left, want the 344 a page of 256 leaves and the one in flight", left)
	}
	if hash[live.RequestID] != "0.3" {
		t.Fatalf("the request in flight holds %q, want its estimate", hash[live.RequestID])
	}
	f.wantConsistent(t)
}

// TestLimitsALapsedReservationOfTheSameRequestIsReservedAfresh proves a request
// reserved again after its first reservation lapsed is not answered as if that one
// still stood. The bounded sweep may not have reached it, so it is retired here.
func TestLimitsALapsedReservationOfTheSameRequestIsReservedAfresh(t *testing.T) {
	f := limCostSeed(t, "lapsed-redelivery", "0")
	pending, _ := f.keys()
	// The day's limit is 1, so the lapsed reservation counted beside the new one
	// would refuse it.
	request := f.request("0.6")
	// The backlog is older than the request's own reservation and fills the page
	// one call retires, so that one is still there, lapsed, when it is asked for.
	f.lapse(t, 600, "0.0001")
	now := limServerTimeMS(t, f.c)
	f.seed(t, now-1000, "0.6", request.RequestID)

	limReserve(t, f.limiter, request)
	if expires := limExpiryScore(t, f.c, f.expiryKey(), request.RequestID); expires <= now {
		t.Fatalf("expires at %d, which has passed: the lapsed reservation was granted again as it stood", expires)
	}
	// Retiring the request's own lapsed reservation is no reason to sweep a second
	// page: more than half of the backlog is left, which one page leaves and two do
	// not.
	if left := f.expiries(t) - 1; left <= 300 {
		t.Fatalf("%d of 600 lapsed reservations are left, want one page of them retired and no more", left)
	}
	if got := limHash(t, f.c, pending)[request.RequestID]; got != "0.6" {
		t.Fatalf("holds %q, want the estimate once", got)
	}
	f.wantConsistent(t)

	// Live again, a second delivery holds it once: the reservation is the same and
	// only what was left of the backlog has been retired meanwhile.
	granted := limExpiryScore(t, f.c, f.expiryKey(), request.RequestID)
	held := limMoney(t, f.reserved(t))
	limReserve(t, f.limiter, request)
	if again := limExpiryScore(t, f.c, f.expiryKey(), request.RequestID); again != granted {
		t.Fatalf("expires at %d after a second delivery, want the granted %d", again, granted)
	}
	if after := limMoney(t, f.reserved(t)); after.Cmp(held) > 0 || after.Cmp(limMoney(t, "0.6")) < 0 {
		t.Fatalf("reserved %s after a second delivery of a reservation held at %s", after.FloatString(12), held.FloatString(12))
	}
	f.wantConsistent(t)
}

func (f *limCostFixture) expiryKey() string {
	_, expiry := f.keys()
	return expiry
}

// TestLimitsReservationKeysThatDisagreeAreDiscarded proves reservations whose two
// keys no longer describe the same leases stop counting. Each is cut off from what
// it needs to lapse or to be released, so it would hold the budget for as long as
// traffic kept its keys alive. Reservations are advisory, so what is lost is the
// protection of the requests in flight and nothing the accrued spend relies on.
func TestLimitsReservationKeysThatDisagreeAreDiscarded(t *testing.T) {
	for _, test := range []struct {
		name string
		seed func(t *testing.T, f *limCostFixture, live int64)
	}{
		{"the set of expiries is gone", func(t *testing.T, f *limCostFixture, live int64) {
			pending, _ := f.keys()
			do(t, f.c, "HSET", pending, "sum", "0.5", uuid.NewString(), "0.5")
			do(t, f.c, "PEXPIRE", pending, "60000")
		}},
		{"one reservation has no expiry", func(t *testing.T, f *limCostFixture, live int64) {
			a, b := uuid.NewString(), uuid.NewString()
			f.seed(t, live, "0.4", a, b)
			do(t, f.c, "ZREM", f.expiryKey(), b)
		}},
		{"the set holds a reservation the hash does not", func(t *testing.T, f *limCostFixture, live int64) {
			f.seed(t, live, "0.5", uuid.NewString())
			do(t, f.c, "ZADD", f.expiryKey(), strconv.FormatInt(live, 10), uuid.NewString())
		}},
		{"only a total remains", func(t *testing.T, f *limCostFixture, live int64) {
			pending, _ := f.keys()
			do(t, f.c, "HSET", pending, "sum", "0.5")
			do(t, f.c, "PEXPIRE", pending, "60000")
		}},
		{"the hash is gone", func(t *testing.T, f *limCostFixture, live int64) {
			f.seed(t, live, "0.5", uuid.NewString())
			pending, _ := f.keys()
			do(t, f.c, "DEL", pending)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := limCostSeed(t, "disagree", "0")
			test.seed(t, f, limServerTimeMS(t, f.c)+60_000)
			// The day's limit is 1, so 0.5 or more still counted would refuse this.
			limReserve(t, f.limiter, f.request("0.6"))
			f.wantReserved(t, "0.6")
			f.wantConsistent(t)
		})
	}
}

// TestLimitsReservingTheSameRequestTwiceHoldsItOnce proves a reservation that is
// delivered again is not counted beside itself, which would refuse a request for
// the estimate it already holds.
func TestLimitsReservingTheSameRequestTwiceHoldsItOnce(t *testing.T) {
	f := limCostSeed(t, "redelivery", "0")
	request := f.request("0.6") // the day's limit is 1, so counting it twice would refuse it
	limReserve(t, f.limiter, request)
	if _, err := f.limiter.Reserve(t.Context(), request); err != nil {
		t.Fatalf("redelivery of a granted reservation: %v", err)
	}
	f.wantReserved(t, "0.6")
}

// TestLimitsAccrualOfAnEarlierWindowStillRemovesTheReservation proves the request
// an accrual accounts for stops being reserved even when the spend it installs
// belongs to a window that is no longer the current one, which is what a snapshot
// applied after midnight UTC for a request observed before it is.
func TestLimitsAccrualOfAnEarlierWindowStillRemovesTheReservation(t *testing.T) {
	f := limCostSeed(t, "stale-accrual", "0")
	request := f.request("0.4")
	limReserve(t, f.limiter, request)
	f.wantReserved(t, "0.4")
	reconciled, _, err := f.limiter.ApplyCostSnapshot(t.Context(), limits.CostSnapshot{
		CostOwnerID: f.owner, DailyWindowID: f.windows.DailyID - 1, DailyAccrued: "0.4",
		MonthlyWindowID: f.windows.MonthlyID - 1, MonthlyAccrued: "0.4", RequestID: request.RequestID,
	})
	if err != nil {
		t.Fatalf("ApplyCostSnapshot: %v", err)
	}
	if reconciled {
		t.Fatal("a snapshot of an earlier window was installed as the current balance")
	}
	f.wantReserved(t, "0")
	if day := limHash(t, f.c, f.day()); day["accrued"] != "0" {
		t.Fatalf("day = %v, want the current balance untouched", day)
	}
}

// limFaulty runs every command against Valkey and then lets a test change what
// the caller hears of it, which is how a command that ran and whose reply was
// lost is made.
type limFaulty struct {
	limits.Commander
	after func(args []string, value any, err error) (any, error)
}

func (f limFaulty) Do(ctx context.Context, args ...string) (any, error) {
	value, err := f.Commander.Do(ctx, args...)
	return f.after(args, value, err)
}

// limScriptKeys is the keys a script call names, whether it sent the script or
// its digest, and nil for a command that is not a script call.
func limScriptKeys(args []string) []string {
	if len(args) < 3 || (args[0] != "EVAL" && args[0] != "EVALSHA") {
		return nil
	}
	count, err := strconv.Atoi(args[2])
	if err != nil || len(args) < 3+count {
		return nil
	}
	return args[3 : 3+count]
}

// limLostReply makes the reply of the first script call that match accepts the one
// the test chooses, once the call has run. A call that did not run, such as one
// the server did not know the script of, is not touched. ran reports whether the
// script did run, so that a test knows it did not lose a reply to nothing.
func limLostReply(match func(keys []string) bool, lost func(value any) (any, error)) (faulty limFaulty, ran *atomic.Bool) {
	ran = new(atomic.Bool)
	return limFaulty{after: func(args []string, value any, err error) (any, error) {
		if err != nil || !match(limScriptKeys(args)) || ran.Swap(true) {
			return value, err
		}
		return lost(value)
	}}, ran
}

func limKeysEndWith(count int, suffix string) func([]string) bool {
	return func(keys []string) bool { return len(keys) == count && strings.HasSuffix(keys[0], suffix) }
}

// TestLimitsReservationWhoseReplyIsLostIsGivenBack proves an estimate that was
// reserved, and whose reply never arrived or could not be read, is released: the
// request is refused as unknown, nothing will settle the reservation, and it would
// otherwise hold the budget until it lapsed.
func TestLimitsReservationWhoseReplyIsLostIsGivenBack(t *testing.T) {
	for _, test := range []struct {
		name string
		lose func(value any) (any, error)
		want func(error) bool
	}{
		{"the reply is lost", func(any) (any, error) {
			return nil, &coordination.CommandError{Cause: context.DeadlineExceeded, Ambiguous: true}
		}, func(err error) bool { var service *limits.ServiceError; return errors.As(err, &service) }},
		{"the reply is unreadable", func(any) (any, error) { return "garbage", nil },
			func(err error) bool { return errors.Is(err, limits.ErrUnexpectedResponse) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := limCostSeed(t, "lost-reply", "0")
			faulty, ran := limLostReply(limKeysEndWith(4, ":cost:day"), test.lose)
			faulty.Commander = f.c
			limiter, err := limits.New(faulty, f.namespace)
			if err != nil {
				t.Fatal(err)
			}
			request := f.request("0.4")
			if lease, err := limiter.Reserve(t.Context(), request); lease != nil || !test.want(err) {
				t.Fatalf("Reserve = %v, %v", lease, err)
			}
			if !ran.Load() {
				t.Fatal("the reservation never ran, so its reply was not lost")
			}
			f.wantReserved(t, "0")
			f.wantConsistent(t)
		})
	}
}

// TestLimitsRateReplyLostAfterTheCostWasReservedGivesTheCostBack proves the same
// of a request the rate script then fails: the cost it reserved is not kept for a
// request that was never admitted.
func TestLimitsRateReplyLostAfterTheCostWasReservedGivesTheCostBack(t *testing.T) {
	f := limCostSeed(t, "lost-rate", "0")
	faulty, ran := limLostReply(limKeysEndWith(2, ":rate"), func(any) (any, error) {
		return nil, &coordination.CommandError{Cause: context.DeadlineExceeded, Ambiguous: true}
	})
	faulty.Commander = f.c
	limiter, err := limits.New(faulty, f.namespace)
	if err != nil {
		t.Fatal(err)
	}
	request := f.request("0.4")
	request.RequestsPerMinute = limPointer(int64(5))
	if lease, err := limiter.Reserve(t.Context(), request); lease != nil || err == nil {
		t.Fatalf("Reserve = %v, %v, want a failure", lease, err)
	}
	if !ran.Load() {
		t.Fatal("the rate script never ran, so its reply was not lost")
	}
	f.wantReserved(t, "0")
}

// limSlow answers every command after latency, and gives up on one whose
// deadline does not leave it that long, as a connection to a distant server does.
type limSlow struct {
	limits.Commander
	latency time.Duration
}

func (s limSlow) Do(ctx context.Context, args ...string) (any, error) {
	timer := time.NewTimer(s.latency)
	defer timer.Stop()
	select {
	case <-timer.C:
		return s.Commander.Do(ctx, args...)
	case <-ctx.Done():
		return nil, &coordination.CommandError{Cause: ctx.Err(), Ambiguous: true}
	}
}

// TestLimitsReleaseOutlastsAServerSlowerThanASettlementMayWait proves a request
// that is admitted through a slow server can give its estimate back. Admission
// tolerates a second a command, so at 150 milliseconds a command it reserves
// everything; the release, whether it is a refund or the settlement of a request
// that cost nothing, then gets as long as every other cleanup and not as long as
// the best-effort replacement of an estimate with a cost, which gives up at 100.
func TestLimitsReleaseOutlastsAServerSlowerThanASettlementMayWait(t *testing.T) {
	f := limCostSeed(t, "slow-release", "0")
	limSettleInMinute(t, f.c, 10*time.Second)
	// A server that has not cached a script yet takes a second round trip to be
	// sent it, which is a cost of its own and not what this proves.
	warm := f.request("0.1")
	warm.RequestsPerMinute = limPointer(int64(1))
	if err := limReserve(t, f.limiter, warm).Refund(t.Context()); err != nil {
		t.Fatalf("Refund: %v", err)
	}
	slow, err := limits.New(limSlow{Commander: f.c, latency: 150 * time.Millisecond}, f.namespace)
	if err != nil {
		t.Fatal(err)
	}
	lookup := limLookup()
	request := func() limits.Request {
		r := f.request("0.4")
		r.LookupID, r.RequestsPerMinute = lookup, limPointer(int64(1))
		return r
	}

	admission, cancel := context.WithTimeout(t.Context(), time.Second)
	lease, err := slow.Reserve(admission, request())
	cancel()
	if err != nil {
		t.Fatalf("Reserve through a slow server: %v", err)
	}
	f.wantReserved(t, "0.4")
	if err := lease.Refund(t.Context()); err != nil {
		t.Fatalf("Refund through a slow server: %v", err)
	}
	f.wantReserved(t, "0")
	f.wantConsistent(t)
	// The request it counted is given back too, so the one the minute allows is free.
	limReserve(t, f.limiter, request())

	// A settlement is the step that may be lost, and is bounded: it is one attempt
	// that gives up well before a release would, and leaves the estimate held.
	f = limCostSeed(t, "slow-settlement", "0")
	slow, err = limits.New(limSlow{Commander: f.c, latency: 150 * time.Millisecond}, f.namespace)
	if err != nil {
		t.Fatal(err)
	}
	lease, err = slow.Reserve(t.Context(), f.request("0.4"))
	if err != nil {
		t.Fatalf("Reserve through a slow server: %v", err)
	}
	lease.SetActualCost("0.1")
	started := time.Now()
	if err := lease.SettleCost(t.Context()); err == nil {
		t.Fatal("a settlement slower than its bound succeeded")
	}
	if elapsed := time.Since(started); elapsed > 220*time.Millisecond {
		t.Fatalf("a settlement waited %v, want one attempt of about 100ms", elapsed)
	}
	f.wantReserved(t, "0.4")

	// A request that cost nothing, such as one whose usage is still to come, is a
	// release and not a settlement: nothing else will give its estimate back.
	lease.SetActualCost("0")
	if err := lease.SettleCost(t.Context()); err != nil {
		t.Fatalf("SettleCost to nothing through a slow server: %v", err)
	}
	f.wantReserved(t, "0")
	f.wantConsistent(t)
}

var limScriptCommandCalls = regexp.MustCompile(`cmdstat_(evalsha|eval):calls=(\d+),`)

// limScriptCalls is how many scripts Valkey has run, by the digest and by the
// source they were sent as.
func limScriptCalls(t *testing.T, c *coordination.Client) (byDigest, bySource int64) {
	t.Helper()
	text, ok := do(t, c, "INFO", "commandstats").(string)
	if !ok {
		t.Fatal("INFO commandstats is not text")
	}
	for _, match := range limScriptCommandCalls.FindAllStringSubmatch(text, -1) {
		calls, err := strconv.ParseInt(match[2], 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		if match[1] == "evalsha" {
			byDigest = calls
		} else {
			bySource = calls
		}
	}
	return byDigest, bySource
}

// TestLimitsScriptsAreSentByDigestAndReloadedWhenTheServerForgetsThem proves a
// script is run by its digest, which is what spares the server parsing it again
// for every call, and that a server that has lost its scripts is given them again
// without the caller seeing an error.
func TestLimitsScriptsAreSentByDigestAndReloadedWhenTheServerForgetsThem(t *testing.T) {
	f := limCostSeed(t, "script-cache", "0")
	do(t, f.c, "SCRIPT", "FLUSH")

	// The server holds nothing, so the first call is refused by digest and then
	// carries the source, which runs it.
	digest, source := limScriptCalls(t, f.c)
	limReserve(t, f.limiter, f.request("0.1"))
	if d, s := limScriptCalls(t, f.c); d > digest+1 || s != source+1 {
		t.Fatalf("after a flush the first call ran %d by digest and %d by source, want it sent by source once", d-digest, s-source)
	}
	// It holds the script now: the same call names it and carries no source.
	digest, source = limScriptCalls(t, f.c)
	limReserve(t, f.limiter, f.request("0.1"))
	if d, s := limScriptCalls(t, f.c); d != digest+1 || s != source {
		t.Fatalf("a cached script ran %d by digest and %d by source, want one by digest", d-digest, s-source)
	}
	f.wantReserved(t, "0.2")
}
