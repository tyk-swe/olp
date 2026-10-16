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

// A cost-only request never asks the rate script for anything.

// Both keys outlive the newest reservation, which lasts its lease and the
// default grace, and not by more than a second.

// 0.1 accrued and 0.8 in flight leave room for 0.1 and nothing more.

// In-flight reservations are what stands in the way, so waiting helps.

// The balance is spent to the unit, and one more unit is refused.

// A request that could never fit, however much is released, waits for the
// window and not for the second.

// 0.5 of 1 is spent, so 0.6 does not fit although the budget is not used up.

// Once the day's spend reaches its limit the budget is used up, whatever the
// request would cost, and an unpriced request is refused the same way.

// The day has room for plenty; the month, holding 3 of 5, has room for 2.

// A key limited by one window ignores the other window's limit.

// Replays and smaller estimates retain the larger bound without adding it.

// The original handle settles the grown reservation under the same identity.

// A request that never dispatched owes nothing.

// Settlement replaces the estimate with what the request cost.

// Spending nothing releases the reservation.

// Settling a reservation that is gone does nothing and creates nothing, which
// is what keeps a late settlement from counting spend that accounting has.

// A lease with no actual cost recorded settles nothing.

// Granted for the lease and its grace, which together outlast the settlement,
// whose own grace begins when it is made.

// Granted for a moment and a long grace: settling later would put the expiry
// after the one admission set, which is the backstop and must stand.

// The spend and the removal of what held its place arrive as one step, so
// the budget never counts the request twice, and the other request stays.

// Delivered again, which accounting does, it removes nothing more.

// A settlement that arrives after accrual cannot bring the request back.

// An accrual that names no request removes none.

// Accrual never lowers a balance, replayed or not.

// Reading the total retires nothing; the next reservation does.

// Accounting that arrives after a reservation lapsed finds nothing and may
// not drive the total below what is still reserved.

// A lapsed reservation is not revived by a settlement either.

// Reservations are advisory and derived, so damage to them is discarded and
// the request that found it is judged as if none were held.

// A total that is not a number cannot be trusted to describe the leases beside
// it, and is not read as zero: they go with it, or what they hold would stop
// counting while still being kept.

// Whatever the reservations look like, accrued spend is installed: it is the
// authority, and a damaged advisory key must not stop PostgreSQL's totals.

// Accrued state keeps the fail-closed rule: the balances are never guessed.

// The long reservation lasts fifteen minutes, and the keys a second more.

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
			held.SetActualCost("0.1", "0.1")
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

// It is still judged on what has accrued.

// The request window is spent, so the rate script refuses the second request
// after the cost script has already reserved for it.

// Both halves of the lease are given back together.

// Sixty-four replicas admit at once against a balance of 0.25 spent and a
// limit of 1: the script decides each one against the others, so exactly
// floor((1 - 0.25) / 0.3) = 2 are admitted and the rest are refused.

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
					e.lease.SetActualCost(actual, actual)
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

// Retired, the backlog leaves room for 0.6 beside the 0.2 in flight; counted,
// it does not. The refusal is the transient kind: this request would fit.

// Later calls retire the same page each, never more, and the balance is never
// read as less than the live reservation holds.

// Calls that follow drain the rest, and the total is then what is live.

// Another request still in flight keeps the keys alive, as one does on a busy
// owner, so that what a settlement does to the lapsed one can be read.

// More lapsed reservations than one page retires, all older than the request's.

// A cost of nothing releases the reservation whether or not it had lapsed, and
// so would not tell the settlement apart from a release: this one costs 0.1.

// The sweep retired a page of the older ones, and not the request's own; the
// request in flight is untouched.

// The day's limit is 1, so the lapsed reservation counted beside the new one
// would refuse it.

// The backlog is older than the request's own reservation and fills the page
// one call retires, so that one is still there, lapsed, when it is asked for.

// Retiring the request's own lapsed reservation is no reason to sweep a second
// page: more than half of the backlog is left, which one page leaves and two do
// not.

// Live again, a second delivery holds it once: the reservation is the same and
// only what was left of the backlog has been retired meanwhile.

func (f *limCostFixture) expiryKey() string {
	_, expiry := f.keys()
	return expiry
}

// The day's limit is 1, so 0.5 or more still counted would refuse this.

// the day's limit is 1, so counting it twice would refuse it

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
	lease.SetActualCost("0.1", "0.1")
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
	lease.SetActualCost("0", "0")
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

// The server holds nothing, so the first call is refused by digest and then
// carries the source, which runs it.

// It holds the script now: the same call names it and carries no source.
