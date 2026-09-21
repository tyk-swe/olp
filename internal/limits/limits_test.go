package limits

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/coordination"
)

// reply builds the array shape GLIDE hands back for a Lua table: integers
// arrive as int64 and strings as string.
func reply(values ...any) []any { return values }

func TestParseReservationRejectsMalformedResponses(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		value any
	}{
		{"unknown version", reply(int64(2), int64(1), "ok", int64(0), int64(1), int64(0))},
		{"granted with retry hint", reply(int64(1), int64(1), "ok", int64(1), int64(1), int64(0))},
		{"rejection without retry hint", reply(int64(1), int64(0), "rpm", int64(0), int64(1), int64(0))},
		{"retry beyond the window", reply(int64(1), int64(0), "rpm", int64(60_001), int64(1), int64(0))},
		{"rejection without a window", reply(int64(1), int64(0), "rpm", int64(1), int64(0), int64(0))},
		{"rejection that leased concurrency", reply(int64(1), int64(0), "rpm", int64(1), int64(1), int64(1))},
		{"unknown dimension", reply(int64(1), int64(0), "unknown", int64(1), int64(1), int64(0))},
		{"malformed state with a retry hint", reply(int64(1), int64(-1), "malformed_rate_state", int64(1), int64(1), int64(0))},
		{"script failure with a window", reply(int64(1), int64(-1), "invalid_arguments", int64(0), int64(1), int64(0))},
		{"counter beyond the Lua range", reply(int64(1), int64(1), "ok", int64(0), maxLuaInteger+1, int64(0))},
		{"negative window", reply(int64(1), int64(1), "ok", int64(0), int64(-1), int64(0))},
		{"string where an integer belongs", reply("1", int64(1), "ok", int64(0), int64(1), int64(0))},
		{"integer where a string belongs", reply(int64(1), int64(1), int64(0), int64(0), int64(1), int64(0))},
		{"short array", reply(int64(1), int64(1), "ok", int64(0), int64(1))},
		{"empty array", reply()},
		{"nil array", []any(nil)},
		{"not an array", int64(1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := parseReservation(test.value); !errors.Is(err, ErrUnexpectedResponse) {
				t.Fatalf("parseReservation(%v) error = %v, want ErrUnexpectedResponse", test.value, err)
			}
		})
	}
}

func TestParseReservationAcceptsTheScriptContract(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		value any
		want  scriptResult
	}{
		{
			"granted without concurrency",
			reply(int64(1), int64(1), "ok", int64(0), int64(29_823_060), int64(0)),
			scriptResult{kind: resultGranted, windowID: 29_823_060},
		},
		{
			"granted with a concurrency lease",
			reply(int64(1), int64(1), "ok", int64(0), int64(29_823_060), int64(1_789_383_605_000)),
			scriptResult{kind: resultGranted, windowID: 29_823_060, leaseExpiresAtMS: 1_789_383_605_000},
		},
		{
			"requests exhausted",
			reply(int64(1), int64(0), "rpm", int64(60_000), int64(29_823_060), int64(0)),
			scriptResult{kind: resultRejected, dimension: DimensionRequests, retryAfterMS: 60_000},
		},
		{
			"tokens exhausted",
			reply(int64(1), int64(0), "tpm", int64(1), int64(29_823_060), int64(0)),
			scriptResult{kind: resultRejected, dimension: DimensionTokens, retryAfterMS: 1},
		},
		{
			"concurrency exhausted beyond a minute",
			reply(int64(1), int64(0), "concurrency", int64(120_000), int64(29_823_060), int64(0)),
			scriptResult{kind: resultRejected, dimension: DimensionConcurrency, retryAfterMS: 120_000},
		},
		{
			"malformed concurrency state",
			reply(int64(1), int64(-1), "malformed_concurrency_state", int64(0), int64(29_823_060), int64(0)),
			scriptResult{kind: resultMalformed},
		},
		{
			"invalid server time",
			reply(int64(1), int64(-1), "invalid_server_time", int64(0), int64(0), int64(0)),
			scriptResult{kind: resultScriptFailure},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseReservation(test.value)
			if err != nil {
				t.Fatalf("parseReservation() error = %v", err)
			}
			if got != test.want {
				t.Fatalf("parseReservation() = %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestParseCostReservationRejectsMalformedResponses(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		value any
	}{
		{"unknown version", reply(int64(2), int64(1), "ok", int64(0), int64(20_000), int64(24_000))},
		{"missing day window", reply(int64(1), int64(1), "ok", int64(0), int64(0), int64(24_000))},
		{"missing month window", reply(int64(1), int64(1), "ok", int64(0), int64(20_000), int64(0))},
		{"granted with a retry hint", reply(int64(1), int64(1), "ok", int64(1), int64(20_000), int64(24_000))},
		{"daily retry beyond a day", reply(int64(1), int64(0), "daily_cost", dayMS+1, int64(20_000), int64(24_000))},
		{"monthly retry beyond a month", reply(int64(1), int64(0), "monthly_cost", maxMonthMS+1, int64(20_000), int64(24_000))},
		{"rejection without a retry hint", reply(int64(1), int64(0), "daily_cost", int64(0), int64(20_000), int64(24_000))},
		{"uninitialized with a retry hint", reply(int64(1), int64(-1), "uninitialized_daily_cost_state", int64(1), int64(20_000), int64(24_000))},
		{"unknown reason", reply(int64(1), int64(-1), "exploded", int64(0), int64(20_000), int64(24_000))},
		{"short array", reply(int64(1), int64(1), "ok", int64(0), int64(20_000))},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := parseCostReservation(test.value); !errors.Is(err, ErrUnexpectedResponse) {
				t.Fatalf("parseCostReservation(%v) error = %v, want ErrUnexpectedResponse", test.value, err)
			}
		})
	}
}

func TestParseCostReservationAcceptsTheScriptContract(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		value any
		want  scriptResult
	}{
		{
			"granted",
			reply(int64(1), int64(1), "ok", int64(0), int64(20_000), int64(24_000)),
			scriptResult{kind: resultGranted},
		},
		{
			"daily budget exhausted",
			reply(int64(1), int64(0), "daily_cost", dayMS, int64(20_000), int64(24_000)),
			scriptResult{kind: resultRejected, dimension: DimensionDailyCost, retryAfterMS: dayMS},
		},
		{
			"monthly budget exhausted",
			reply(int64(1), int64(0), "monthly_cost", maxMonthMS, int64(20_000), int64(24_000)),
			scriptResult{kind: resultRejected, dimension: DimensionMonthlyCost, retryAfterMS: maxMonthMS},
		},
		{
			"monthly state awaits reconciliation",
			reply(int64(1), int64(-1), "uninitialized_monthly_cost_state", int64(0), int64(20_000), int64(24_000)),
			scriptResult{kind: resultUninitialized},
		},
		{
			"daily state is malformed",
			reply(int64(1), int64(-1), "malformed_daily_cost_state", int64(0), int64(20_000), int64(24_000)),
			scriptResult{kind: resultMalformed},
		},
		{
			"invalid arguments",
			reply(int64(1), int64(-1), "invalid_arguments", int64(0), int64(0), int64(0)),
			scriptResult{kind: resultScriptFailure},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseCostReservation(test.value)
			if err != nil {
				t.Fatalf("parseCostReservation() error = %v", err)
			}
			if got != test.want {
				t.Fatalf("parseCostReservation() = %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestParseReconciliation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		value       any
		wantDaily   bool
		wantMonthly bool
		wantErr     error
	}{
		{"nothing reconciled", reply(int64(1), int64(1), "ok", int64(0), int64(0)), false, false, nil},
		{"both windows", reply(int64(1), int64(1), "ok", int64(1), int64(1)), true, true, nil},
		{"only the month", reply(int64(1), int64(1), "ok", int64(0), int64(1)), false, true, nil},
		{"malformed day", reply(int64(1), int64(-1), "malformed_daily_cost_state", int64(0), int64(0)), false, false, ErrMalformedState},
		{"malformed with a count", reply(int64(1), int64(-1), "malformed_daily_cost_state", int64(1), int64(0)), false, false, ErrUnexpectedResponse},
		{"count out of range", reply(int64(1), int64(1), "ok", int64(2), int64(0)), false, false, ErrUnexpectedResponse},
		{"unknown version", reply(int64(2), int64(1), "ok", int64(0), int64(0)), false, false, ErrUnexpectedResponse},
		{"six fields", reply(int64(1), int64(1), "ok", int64(0), int64(0), int64(0)), false, false, ErrUnexpectedResponse},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			daily, monthly, err := parseReconciliation(test.value)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("parseReconciliation() error = %v, want %v", err, test.wantErr)
			}
			if daily != test.wantDaily || monthly != test.wantMonthly {
				t.Fatalf("parseReconciliation() = (%t, %t), want (%t, %t)", daily, monthly, test.wantDaily, test.wantMonthly)
			}
		})
	}
}

func TestKeysShareOneClusterHashTag(t *testing.T) {
	t.Parallel()
	limiter := &Limiter{namespace: "olp:go:v1:0192cf87d4ab7f2ea8b1c2d3e4f50607:limits"}
	const apiKeyID = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607"
	first := limiter.keysFor("lookup_one_abc", apiKeyID)
	second := limiter.keysFor("lookup_two_abc", apiKeyID)

	if got, want := first.rate, "olp:go:v1:0192cf87d4ab7f2ea8b1c2d3e4f50607:limits:{lookup_one_abc}:rate"; got != want {
		t.Fatalf("rate key = %q, want %q", got, want)
	}
	if got, want := first.concurrency, "olp:go:v1:0192cf87d4ab7f2ea8b1c2d3e4f50607:limits:{lookup_one_abc}:concurrency:v2"; got != want {
		t.Fatalf("concurrency key = %q, want %q", got, want)
	}
	if got, want := first.dailyCost, "olp:go:v1:0192cf87d4ab7f2ea8b1c2d3e4f50607:limits:{0192cf87d4ab7f2ea8b1c2d3e4f50607}:cost:day"; got != want {
		t.Fatalf("daily cost key = %q, want %q", got, want)
	}
	if got, want := first.monthlyCost, strings.TrimSuffix(first.dailyCost, "day")+"month"; got != want {
		t.Fatalf("monthly cost key = %q, want %q", got, want)
	}
	// The rate and concurrency keys of one lookup must share a hash tag so a
	// single script can touch both, and both cost keys must follow the API key
	// no matter which lookup spends from it.
	if hashTag(first.rate) != hashTag(first.concurrency) {
		t.Fatalf("rate %q and concurrency %q do not share a hash tag", first.rate, first.concurrency)
	}
	if hashTag(first.rate) == hashTag(second.rate) {
		t.Fatalf("distinct lookups share the hash tag %q", hashTag(first.rate))
	}
	if first.dailyCost != second.dailyCost || first.monthlyCost != second.monthlyCost {
		t.Fatalf("cost keys differ between lookups of one API key: %+v vs %+v", first, second)
	}
	// A counter must never be split per dimension: one hash holds them all.
	if strings.Contains(first.rate, ":rpm:") || strings.Contains(first.rate, ":tpm:") {
		t.Fatalf("rate key %q is split per dimension", first.rate)
	}
	if got, want := limiter.cooldownKey("slot:0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607"),
		"olp:go:v1:0192cf87d4ab7f2ea8b1c2d3e4f50607:limits:provider-cooldown:slot:0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607"; got != want {
		t.Fatalf("cooldown key = %q, want %q", got, want)
	}
}

// hashTag extracts what Valkey Cluster uses to place a key.
func hashTag(key string) string {
	open := strings.IndexByte(key, '{')
	if open < 0 {
		return ""
	}
	closed := strings.IndexByte(key[open:], '}')
	if closed < 0 {
		return ""
	}
	return key[open+1 : open+closed]
}

func TestNamespacesCannotOverrideTheClusterHashTag(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		namespace string
		wantErr   bool
	}{
		{"typical", "olp:go:v1:0192cf87d4ab7f2ea8b1c2d3e4f50607:limits", false},
		{"punctuation", "olp-go_v1:limits", false},
		{"empty", "", true},
		{"too long", strings.Repeat("n", 129), true},
		{"opens a hash tag", "olp:{limits}", true},
		{"whitespace", "olp limits", true},
		{"newline", "olp\nlimits", true},
		{"non ascii", "olp:limité", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			limiter, err := New(&coordination.Client{}, test.namespace)
			if test.wantErr {
				if err == nil {
					t.Fatalf("New(%q) succeeded, want an error", test.namespace)
				}
				return
			}
			if err != nil {
				t.Fatalf("New(%q) error = %v", test.namespace, err)
			}
			if limiter == nil {
				t.Fatal("New() returned no limiter")
			}
		})
	}
	if _, err := New(nil, "limits"); err == nil {
		t.Fatal("New(nil) succeeded, want an error")
	}
}

func TestRequestValidationRejectsBypassableLimits(t *testing.T) {
	t.Parallel()
	const key = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607"
	base := func() Request {
		return Request{
			CostOwnerID:       key,
			LookupID:          "lookup_one_abc",
			RequestsPerMinute: pointer(int64(10)),
			TokensPerMinute:   pointer(int64(1000)),
			MaxConcurrency:    pointer(int64(10)),
			RequestedTokens:   5,
			LeaseTTL:          5 * time.Second,
		}
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("the base request is invalid: %v", err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*Request)
	}{
		{"zero requests per minute", func(r *Request) { r.RequestsPerMinute = pointer(int64(0)) }},
		{"negative tokens per minute", func(r *Request) { r.TokensPerMinute = pointer(int64(-1)) }},
		{"negative requested tokens", func(r *Request) { r.RequestedTokens = -1 }},
		{"no tokens requested under a token limit", func(r *Request) { r.RequestedTokens = 0 }},
		{"concurrency beyond the Lua range", func(r *Request) { r.MaxConcurrency = pointer(maxLuaInteger + 1) }},
		{"requested tokens beyond the Lua range", func(r *Request) { r.RequestedTokens = maxLuaInteger + 1 }},
		{"zero lease TTL", func(r *Request) { r.LeaseTTL = 0 }},
		{"sub-millisecond lease TTL", func(r *Request) { r.LeaseTTL = 999 * time.Microsecond }},
		{"lookup with a hash tag", func(r *Request) { r.LookupID = "bad}{slot" }},
		{"lookup too short", func(r *Request) { r.LookupID = "short" }},
		{"lookup too long", func(r *Request) { r.LookupID = strings.Repeat("a", 41) }},
		{"lookup with a dash", func(r *Request) { r.LookupID = "lookup-one-abc" }},
		{"API key that is not a UUID", func(r *Request) { r.CostOwnerID = "not-a-uuid" }},
		{"zero daily cost limit", func(r *Request) { r.DailyCostLimit = pointer("0.000") }},
		{"negative monthly cost limit", func(r *Request) { r.MonthlyCostLimit = pointer("-1") }},
		{"cost limit in scientific notation", func(r *Request) { r.DailyCostLimit = pointer("1e3") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			request := base()
			test.mutate(&request)
			err := request.Validate()
			var invalid *InvalidRequestError
			if !errors.As(err, &invalid) {
				t.Fatalf("Validate() error = %v, want InvalidRequestError", err)
			}
		})
	}
	// The provider lookups this package builds must pass its own validation.
	for _, lookup := range []string{
		ConnectionLookup("0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607"),
		SlotLookup("0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607"),
	} {
		request := base()
		request.LookupID = lookup
		if err := request.Validate(); err != nil {
			t.Fatalf("Validate() rejected the generated lookup %q: %v", lookup, err)
		}
	}
}

func TestRequestReportsWhichBudgetsApply(t *testing.T) {
	t.Parallel()
	var none Request
	if none.HasHardLimits() || none.HasCostBudget() {
		t.Fatal("an empty request reports budgets it does not have")
	}
	concurrency := Request{MaxConcurrency: pointer(int64(1))}
	if !concurrency.HasHardLimits() || concurrency.HasCostBudget() {
		t.Fatal("a concurrency limit was not reported as a hard limit only")
	}
	monthly := Request{MonthlyCostLimit: pointer("10")}
	if !monthly.HasCostBudget() || !monthly.HasHardLimits() {
		t.Fatal("a cost budget must also count as a hard limit")
	}
}

func TestLookupsAndCooldownScopes(t *testing.T) {
	t.Parallel()
	const provider = "0192CF87-D4AB-7F2E-A8B1-C2D3E4F50607"
	const credential = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50608"
	if got, want := ConnectionLookup(provider), "pc_0192cf87d4ab7f2ea8b1c2d3e4f50607"; got != want {
		t.Fatalf("ConnectionLookup() = %q, want %q", got, want)
	}
	if got, want := SlotLookup(provider), "ps_0192cf87d4ab7f2ea8b1c2d3e4f50607"; got != want {
		t.Fatalf("SlotLookup() = %q, want %q", got, want)
	}
	// An absent credential version still names a stable scope, and an
	// upper-case identifier must address the same key as a lower-case one.
	if got, want := CredentialScope(provider, nil),
		"0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607:00000000-0000-0000-0000-000000000000"; got != want {
		t.Fatalf("CredentialScope() = %q, want %q", got, want)
	}
	if got, want := CredentialScope(provider, pointer(credential)),
		"0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607:0192cf87-d4ab-7f2e-a8b1-c2d3e4f50608"; got != want {
		t.Fatalf("CredentialScope() = %q, want %q", got, want)
	}
	if got, want := SlotScope(provider), "slot:0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607"; got != want {
		t.Fatalf("SlotScope() = %q, want %q", got, want)
	}
}

func TestBudgetWindowsEndOnFixedUTCBoundaries(t *testing.T) {
	t.Parallel()
	// A leap day whose month also ends that midnight: the day and month windows
	// must both close at 2028-03-01, not a day or a month later.
	windows := BudgetWindows(time.Date(2028, time.February, 29, 23, 59, 59, 999_999_999, time.UTC))
	march := time.Date(2028, time.March, 1, 0, 0, 0, 0, time.UTC)
	if !windows.DailyEnd.Equal(march) || !windows.MonthlyEnd.Equal(march) {
		t.Fatalf("windows end at %v and %v, want both at %v", windows.DailyEnd, windows.MonthlyEnd, march)
	}
	if !windows.DailyStart.Equal(time.Date(2028, time.February, 29, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("daily window starts at %v", windows.DailyStart)
	}
	if !windows.MonthlyStart.Equal(time.Date(2028, time.February, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("monthly window starts at %v", windows.MonthlyStart)
	}
	if got, want := windows.MonthlyID, int64(2028*12+1); got != want {
		t.Fatalf("monthly window ID = %d, want %d", got, want)
	}
	// 2028-02-29 is the 21243rd whole UTC day since the epoch, which is the
	// identifier Valkey's own day arithmetic derives from its clock.
	if got, want := windows.DailyID, int64(21_243); got != want {
		t.Fatalf("daily window ID = %d, want %d", got, want)
	}
	// A local-zone instant names the UTC window it falls in, not the local day.
	newYork := time.FixedZone("EST", -5*3600)
	local := BudgetWindows(time.Date(2028, time.February, 29, 20, 0, 0, 0, newYork))
	if local.DailyID != BudgetWindows(time.Date(2028, time.March, 1, 1, 0, 0, 0, time.UTC)).DailyID {
		t.Fatalf("a local instant resolved to daily window %d", local.DailyID)
	}
	if local.MonthlyID != int64(2028*12+2) {
		t.Fatalf("a local instant resolved to monthly window %d", local.MonthlyID)
	}
	// December must roll the year over rather than name a thirteenth month.
	december := BudgetWindows(time.Date(2026, time.December, 31, 12, 0, 0, 0, time.UTC))
	if !december.MonthlyEnd.Equal(time.Date(2027, time.January, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("December's month ends at %v", december.MonthlyEnd)
	}
	if got, want := december.MonthlyID, int64(2026*12+11); got != want {
		t.Fatalf("December's monthly window ID = %d, want %d", got, want)
	}
}

func TestDecimalValidation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		value   string
		limit   bool
		accrued bool
	}{
		{"1", true, true},
		{"0", false, true},
		{"0.000000000000", false, true},
		{"10.5", true, true},
		{"999999999999.999999999999", true, true},
		{"1000000000000", false, true},
		{"0.0000000000001", false, false},
		{"", false, false},
		{".5", false, false},
		{"5.", false, false},
		{"-1", false, false},
		{"+1", false, false},
		{"1e3", false, false},
		{" 1", false, false},
		{"1 ", false, false},
		{"1.2.3", false, false},
		{"NaN", false, false},
		{strings.Repeat("9", 97), false, false},
	} {
		t.Run(test.value, func(t *testing.T) {
			t.Parallel()
			if got := ValidCostLimit(test.value); got != test.limit {
				t.Fatalf("ValidCostLimit(%q) = %t, want %t", test.value, got, test.limit)
			}
			if got := validDecimal(test.value); got != test.accrued {
				t.Fatalf("validDecimal(%q) = %t, want %t", test.value, got, test.accrued)
			}
		})
	}
}

func TestCostSnapshotValidation(t *testing.T) {
	t.Parallel()
	base := CostSnapshot{
		CostOwnerID:      "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607",
		DailyWindowID:    20_000,
		DailyAccrued:     "1.500000000000",
		MonthlyWindowID:  24_000,
		MonthlyAccrued:   "12.000000000000",
		UnpricedAttempts: 3,
	}
	if err := base.validate(); err != nil {
		t.Fatalf("the base snapshot is invalid: %v", err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*CostSnapshot)
	}{
		{"key is not a UUID", func(s *CostSnapshot) { s.CostOwnerID = "key" }},
		{"negative daily window", func(s *CostSnapshot) { s.DailyWindowID = -1 }},
		{"monthly window beyond the Lua range", func(s *CostSnapshot) { s.MonthlyWindowID = maxLuaInteger + 1 }},
		{"daily accrued is not a decimal", func(s *CostSnapshot) { s.DailyAccrued = "1,5" }},
		{"monthly accrued is negative", func(s *CostSnapshot) { s.MonthlyAccrued = "-0.5" }},
		{"unpriced attempts are negative", func(s *CostSnapshot) { s.UnpricedAttempts = -1 }},
		{"unpriced attempts beyond the Lua range", func(s *CostSnapshot) { s.UnpricedAttempts = maxLuaInteger + 1 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			snapshot := base
			test.mutate(&snapshot)
			if err := snapshot.validate(); err == nil {
				t.Fatal("validate() accepted a snapshot Valkey cannot store exactly")
			}
		})
	}
}

func TestOutcomeAndPolicyNames(t *testing.T) {
	t.Parallel()
	for outcome, want := range map[Outcome]string{
		OutcomeSuccess: "success",
		OutcomeFailure: "failure",
		OutcomeSkipped: "skipped",
		Outcome(9):     "unknown",
	} {
		if got := outcome.String(); got != want {
			t.Fatalf("Outcome(%d).String() = %q, want %q", int(outcome), got, want)
		}
	}
	if got, want := FailOpen.String(), "fail_open"; got != want {
		t.Fatalf("FailOpen.String() = %q, want %q", got, want)
	}
	if got, want := FailClosed.String(), "fail_closed"; got != want {
		t.Fatalf("FailClosed.String() = %q, want %q", got, want)
	}
	// A policy that was never loaded must reject rather than admit.
	var zero OutagePolicy
	if zero != FailClosed {
		t.Fatal("the zero outage policy is not fail-closed")
	}
}

func pointer[T any](value T) *T { return &value }
