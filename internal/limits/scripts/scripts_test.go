package scripts

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/coordination"
	"github.com/tyk-swe/olp/internal/limits"
)

type scripted struct {
	mu     sync.Mutex
	asked  [][]string
	answer func(args []string) (any, error)
}

func (c *scripted) Do(_ context.Context, args ...string) (any, error) {
	c.mu.Lock()
	c.asked = append(c.asked, slices.Clone(args))
	c.mu.Unlock()
	return c.answer(args)
}

func (c *scripted) commands() [][]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.asked)
}

type pipelinedScripted struct{ *scripted }

func (c *pipelinedScripted) Pipeline(ctx context.Context, commands ...[]string) ([]any, error) {
	replies := make([]any, len(commands))
	for i, command := range commands {
		value, err := c.Do(ctx, command...)
		if err != nil {
			replies[i] = err
		} else {
			replies[i] = value
		}
	}
	return replies, nil
}

const (
	rateReplyVersion = int64(2)
	testWindow       = int64(29_823_060)
	owner            = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607"
)

type allowance struct{ requestLimit, requestRemaining, tokenLimit, tokenRemaining, resetMS int64 }

func headReply(status int64, detail string, retry, window, expiry int64) []any {
	return []any{rateReplyVersion, status, detail, retry, window, expiry}
}

func rateReply(status int64, detail string, retry, window, expiry int64, a allowance) []any {
	return append(headReply(status, detail, retry, window, expiry), a.requestLimit, a.requestRemaining, a.tokenLimit, a.tokenRemaining, a.resetMS)
}

func TestReserveRateRejectsMalformedResponses(t *testing.T) {
	t.Parallel()
	both := allowance{requestLimit: 10, requestRemaining: 4, tokenLimit: 1000, tokenRemaining: 900, resetMS: 30_000}
	requestsOnly := allowance{requestLimit: 10, requestRemaining: 4, resetMS: 30_000}
	granted := func(mutate func(items []any)) []any {
		items := rateReply(1, "ok", 0, testWindow, 0, both)
		mutate(items)
		return items
	}
	rejected := func(detail string, retry int64, a allowance) []any {
		return rateReply(0, detail, retry, testWindow, 0, a)
	}
	reserving := func(t *testing.T, value any) (RateGrant, error) {
		runner, err := NewRunner(&scripted{answer: func([]string) (any, error) { return value, nil }}, "olp:test")
		if err != nil {
			t.Fatal(err)
		}
		return runner.ReserveRate(t.Context(), RateReservation{Lookup: "lookup_one_abc", LeaseID: "lease", RequestsPerMinute: 10, TokensPerMinute: 1000, Tokens: 5, TTL: time.Second, Stated: true})
	}
	if grant, err := reserving(t, granted(func([]any) {})); err != nil || grant.Rate.RequestRemaining != 4 || grant.Rate.TokenRemaining != 900 {
		t.Fatalf("a well-formed grant = %+v, %v", grant, err)
	}
	for _, test := range []struct {
		name  string
		value any
	}{
		{"the version before the allowance", []any{int64(1), int64(1), "ok", int64(0), testWindow, int64(0)}},
		{"the version before the allowance with the allowance", granted(func(items []any) { items[0] = int64(1) })},
		{"a later version", granted(func(items []any) { items[0] = int64(3) })},
		{"granted with retry hint", granted(func(items []any) { items[3] = int64(1) })},
		{"granted without a window", granted(func(items []any) { items[4] = int64(0) })},
		{"rejection without retry hint", rejected("rpm", 0, allowance{requestLimit: 10, resetMS: 0})},
		{"retry beyond the window", rejected("rpm", 60_001, allowance{requestLimit: 10, resetMS: 60_001})},
		{"rejection without a window", rateReply(0, "rpm", 1, 0, 0, allowance{requestLimit: 10, resetMS: 1})},
		{"rejection that leased concurrency", rateReply(0, "rpm", 1, testWindow, 1, allowance{requestLimit: 10, resetMS: 1})},
		{"unknown dimension", rejected("unknown", 1, allowance{requestLimit: 10, resetMS: 1})},
		{"unknown status", rateReply(2, "ok", 0, testWindow, 0, both)},
		{"malformed state with a retry hint", headReply(-1, "malformed_rate_state", 1, testWindow, 0)},
		{"malformed state without a window", headReply(-1, "malformed_rate_state", 0, 0, 0)},
		{"malformed state that states an allowance", rateReply(-1, "malformed_rate_state", 0, testWindow, 0, both)},
		{"malformed state that states an empty allowance", rateReply(-1, "malformed_rate_state", 0, testWindow, 0, allowance{})},
		{"script failure with a window", headReply(-1, "invalid_arguments", 0, testWindow, 0)},
		{"script failure that states an allowance", rateReply(-1, "invalid_server_time", 0, 0, 0, allowance{resetMS: 1})},
		{"script failure that states an empty allowance", rateReply(-1, "invalid_arguments", 0, 0, 0, allowance{})},
		{"unknown failure", headReply(-1, "unknown", 0, 0, 0)},
		{"a grant that does not state the allowance it was asked for", headReply(1, "ok", 0, testWindow, 0)},
		{"a rejection that does not state the allowance it was asked for", headReply(0, "rpm", 30_000, testWindow, 0)},
		{"counter beyond the Lua range", granted(func(items []any) { items[4] = limits.MaxCounter + 1 })},
		{"negative window", granted(func(items []any) { items[4] = int64(-1) })},
		{"a window that never ends", rateReply(1, "ok", 0, limits.MaxCounter/60_000, 0, both)},
		{"remaining beyond the Lua range", granted(func(items []any) { items[7] = limits.MaxCounter + 1 })},
		{"negative remaining", granted(func(items []any) { items[7] = int64(-1) })},
		{"negative limit", granted(func(items []any) { items[6] = int64(-1) })},
		{"string where an integer belongs", granted(func(items []any) { items[0] = "2" })},
		{"string where the limit belongs", granted(func(items []any) { items[6] = "10" })},
		{"integer where a string belongs", granted(func(items []any) { items[2] = int64(0) })},
		{"request remaining above its limit", granted(func(items []any) { items[7] = int64(11) })},
		{"token remaining above its limit", granted(func(items []any) { items[9] = int64(1001) })},
		{"request remaining of an unlimited dimension", rateReply(1, "ok", 0, testWindow, 0, allowance{tokenLimit: 1000, requestRemaining: 1, tokenRemaining: 900, resetMS: 30_000})},
		{"token remaining of an unlimited dimension", rateReply(1, "ok", 0, testWindow, 0, allowance{requestLimit: 10, requestRemaining: 4, tokenRemaining: 1, resetMS: 30_000})},
		{"a granted request that left a request limit untouched", granted(func(items []any) { items[7] = int64(10) })},
		{"a granted request that left a token limit untouched", granted(func(items []any) { items[9] = int64(1000) })},
		{"a window that is over", granted(func(items []any) { items[10] = int64(0) })},
		{"a window beyond a minute", granted(func(items []any) { items[10] = int64(60_001) })},
		{"a window of nothing limited", rateReply(1, "ok", 0, testWindow, 0, allowance{resetMS: 30_000})},
		{"requests exhausted with requests left", rejected("rpm", 30_000, both)},
		{"requests exhausted without a request limit", rejected("rpm", 30_000, allowance{tokenLimit: 1000, tokenRemaining: 900, resetMS: 30_000})},
		{"tokens exhausted without a token limit", rejected("tpm", 30_000, requestsOnly)},
		{"requests exhausted with another retry hint", rejected("rpm", 29_000, allowance{requestLimit: 10, resetMS: 30_000})},
		{"tokens exhausted with another retry hint", rejected("tpm", 29_000, both)},
		{"short array", rateReply(1, "ok", 0, testWindow, 0, both)[:10]},
		{"long array", append(rateReply(1, "ok", 0, testWindow, 0, both), int64(0))},
		{"empty array", []any{}},
		{"nil array", []any(nil)},
		{"not an array", int64(1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := reserving(t, test.value); !errors.Is(err, limits.ErrUnexpectedResponse) {
				t.Fatalf("ReserveRate(%v) error = %v, want ErrUnexpectedResponse", test.value, err)
			}
		})
	}
}

func TestSupplyLoadsMissingScriptsForServerAndDriverErrors(t *testing.T) {
	for name, refusal := range map[string]error{
		"server": errors.New("NOSCRIPT No matching script. Please use EVAL."),
		"driver": errors.New("An error was signalled by the server: - NoScriptError: No matching script."),
	} {
		t.Run(name, func(t *testing.T) {
			client := &pipelinedScripted{&scripted{answer: func(command []string) (any, error) {
				if command[0] == "EVALSHA" {
					return nil, refusal
				}
				return []any{int64(1), int64(1), "ok", int64(0), int64(20_000), int64(24_000)}, nil
			}}}
			runner, err := NewRunner(client, "test")
			if err != nil {
				t.Fatal(err)
			}
			answer := runner.Supply(t.Context(), SupplyQuery{Caps: []CostCaps{{Owner: limits.Owner{ID: owner, Kind: limits.OwnerSupply, Basis: limits.BasisCost}, Daily: "1"}}})
			if len(answer.Caps) != 1 || answer.Caps[0] != nil {
				t.Fatalf("available cap with an uncached script = %v", answer.Caps)
			}
			if commands := client.commands(); len(commands) != 2 || commands[0][0] != "EVALSHA" || commands[1][0] != "EVAL" {
				t.Fatalf("missing script was not loaded: %v", commands)
			}
		})
	}
}

func TestMissingScriptIsLoadedOnlyWhenTheServerSaidSo(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]struct {
		refusal error
		sent    int
	}{
		"a missing script": {&coordination.CommandError{Cause: errors.New("NOSCRIPT No matching script.")}, 2},
		"a lost reply":     {&coordination.CommandError{Cause: errors.New("NOSCRIPT No matching script."), Ambiguous: true}, 1},
		"a refusal":        {&coordination.CommandError{Cause: errors.New("connection refused")}, 1},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			client := &scripted{answer: func(command []string) (any, error) {
				if command[0] == "EVALSHA" {
					return nil, test.refusal
				}
				return []any{int64(1), int64(2), int64(3)}, nil
			}}
			runner, err := NewRunner(client, "test")
			if err != nil {
				t.Fatal(err)
			}
			_, _ = runner.Usage(t.Context(), "lookup_one_abc")
			if sent := len(client.commands()); sent != test.sent {
				t.Fatalf("sent %d commands, want %d", sent, test.sent)
			}
		})
	}
}

func TestSnapshotsInstallOnTheirOwnBasis(t *testing.T) {
	t.Parallel()
	server := &scripted{answer: func([]string) (any, error) { return []any{int64(1), int64(1), "ok", int64(1), int64(1)}, nil }}
	runner, err := NewRunner(server, "olp:test")
	if err != nil {
		t.Fatal(err)
	}
	snapshot := limits.Snapshot{
		Basis: limits.BasisCharge, OwnerID: owner,
		Windows:      limits.Windows{DailyID: 1, MonthlyID: 2},
		DailyAccrued: "0.5", MonthlyAccrued: "0.6",
		RequestID: "0192CF87-D4AB-7F2E-A8B1-C2D3E4F50608",
	}
	installed, err := runner.InstallSnapshot(t.Context(), snapshot, nil)
	if err != nil || !installed.Daily || !installed.Monthly {
		t.Fatalf("InstallSnapshot = %+v, %v", installed, err)
	}
	joined := ""
	for _, command := range server.commands() {
		joined += strings.Join(command, " ")
	}
	if !strings.Contains(joined, ":charge:day") || strings.Contains(joined, ":cost:day") {
		t.Fatalf("the snapshot installed on %q, want the charge counters", joined)
	}
}

func TestVerifyCostRefusesUnknownAccountingState(t *testing.T) {
	caps := CostCaps{Owner: limits.Owner{ID: owner, Basis: limits.BasisCharge}, Daily: "1", Durable: true}
	for _, test := range []struct {
		name   string
		status int64
		detail string
		retry  int64
		want   error
	}{
		{"missing projection", -1, "uninitialized_durable_cost_state", 0, ErrDurableMissing},
		{"missing projection is uninitialized cost", -1, "uninitialized_durable_cost_state", 0, limits.ErrUninitializedCost},
		{"unresolved exposure", -1, "unsettled_durable_cost_state", 0, limits.ErrUninitializedCost},
		{"malformed projection", -1, "malformed_durable_cost_state", 0, limits.ErrMalformedState},
		{"unknown response", -1, "unknown", 0, limits.ErrUnexpectedResponse},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &scripted{answer: func([]string) (any, error) {
				return []any{int64(1), test.status, test.detail, test.retry, int64(1), int64(2)}, nil
			}}
			runner, err := NewRunner(client, "limits")
			if err != nil {
				t.Fatal(err)
			}
			if err := runner.VerifyCost(t.Context(), caps); !errors.Is(err, test.want) {
				t.Fatalf("unknown charge accounting admitted: %v, want %v", err, test.want)
			}
		})
	}
	unsettled := &scripted{answer: func([]string) (any, error) {
		return []any{int64(1), int64(-1), "unsettled_durable_cost_state", int64(0), int64(1), int64(2)}, nil
	}}
	runner, err := NewRunner(unsettled, "limits")
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.VerifyCost(t.Context(), caps); errors.Is(err, ErrDurableMissing) {
		t.Fatalf("an unresolved overrun asked for a projection rebuild: %v", err)
	}
}

func TestVerifyCostRetainsRefusedBudgetBasisAndRetry(t *testing.T) {
	client := &scripted{answer: func([]string) (any, error) {
		return []any{int64(1), int64(0), "daily_cost", int64(250), int64(1), int64(2)}, nil
	}}
	runner, err := NewRunner(client, "limits")
	if err != nil {
		t.Fatal(err)
	}
	refusal := runner.VerifyCost(t.Context(), CostCaps{Owner: limits.Owner{ID: owner, Basis: limits.BasisCharge}, Daily: "1", Durable: true})
	var exceeded *limits.ExceededError
	if !errors.As(refusal, &exceeded) || exceeded.Basis != limits.BasisCharge || exceeded.Dimension != limits.DimensionDailyCost || exceeded.RetryAfter != 250*time.Millisecond {
		t.Fatalf("refusal lost the enforced charge budget or retry guidance: %v", refusal)
	}
	for _, command := range client.commands() {
		if !slices.Contains(command, "verify") || !slices.ContainsFunc(command, func(arg string) bool { return strings.HasSuffix(arg, ":durable") }) {
			t.Fatalf("verification did not read the durable projection read-only: %v", command)
		}
	}
}
