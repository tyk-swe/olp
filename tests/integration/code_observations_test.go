//go:build integration

package integration_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/codemode"
)

func codeObservedWindow(id, kind string, used float64, reset *time.Time, observed time.Time) codemode.AllowanceWindow {
	return codemode.AllowanceWindow{LimitID: id, Window: kind, UsedPercent: used, RemainingPercent: max(0, 100-used), ResetsAt: reset, ObservedAt: observed}
}

func codeObservedAccount(t *testing.T, f *codeFixture) codemode.Account {
	t.Helper()
	response := f.h.want(f.owner, "GET", "/api/v1/code/accounts?project_id="+f.project, nil, nil, 200)
	accounts := codePublicDecode[[]codemode.Account](t, response["items"])
	if len(accounts) != 1 {
		t.Fatalf("accounts: %+v", accounts)
	}
	return accounts[0]
}

func TestCodeObservationWindowsMergeAndGateUntilIndependentResets(t *testing.T) {
	f := newCodeFixture(t)
	ctx := t.Context()
	root, err := f.store.BindConnection(ctx, f.route, f.key, codemode.Identity{Conversation: "observation-root"}, "")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond).Add(-time.Minute)
	primaryReset, secondaryReset := now.Add(2*time.Hour), now.Add(time.Hour)
	observe := func(at time.Time, windows ...codemode.AllowanceWindow) {
		t.Helper()
		if err := f.store.ObserveAllowance(ctx, f.account, codemode.Allowance{ObservedAt: at, Windows: windows}); err != nil {
			t.Fatal(err)
		}
	}
	refuse := func() {
		t.Helper()
		_, err := f.store.Admit(ctx, f.input("observation-root", "", nil))
		codeRefusal(t, err, "code_account_unavailable")
		if codeObservedAccount(t, f).Eligible {
			t.Fatal("management eligibility disagrees with admission")
		}
	}
	observe(now, codeObservedWindow("codex", "primary", 20, &primaryReset, now), codeObservedWindow("codex", "secondary", 100, &secondaryReset, now))
	refuse()
	later := now.Add(3 * time.Second)
	observe(later, codeObservedWindow("codex", "primary", 0, &primaryReset, later))
	stale := now.Add(-time.Second)
	observe(stale, codeObservedWindow("codex", "secondary", 0, &secondaryReset, stale))
	refuse()
	account := codeObservedAccount(t, f)
	if len(account.Allowance.Windows) != 2 || account.Allowance.Windows[1].UsedPercent != 100 || *account.Allowance.RemainingPercent != 100 {
		t.Fatalf("partial or stale update replaced a window: %+v", account.Allowance)
	}
	// This event is newer for secondary but older than the latest primary.
	reset := time.Now().UTC().Add(-time.Second)
	at := now.Add(time.Second)
	observe(at, codeObservedWindow("codex", "secondary", 100, &reset, at))
	if !codeObservedAccount(t, f).Eligible {
		t.Fatal("independent reset did not restore eligibility")
	}
	permit, err := f.store.Admit(ctx, f.input("observation-root", "", nil))
	if err != nil || permit.Binding.ID != root.Binding.ID || permit.Account.ID != root.Account.ID {
		t.Fatalf("reset changed the pinned account: %+v %v", permit, err)
	}
	if err = f.store.Abort(ctx, permit.Attempt.ID); err != nil {
		t.Fatal(err)
	}
	at = now.Add(4 * time.Second)
	observe(at, codeObservedWindow("codex_other", "secondary", 101, nil, at))
	refuse()
	balance := "0"
	if err = f.store.ObserveAllowance(ctx, f.account, codemode.Allowance{ObservedAt: later, Credits: &codemode.Credits{HasCredits: false, Unlimited: false, Balance: &balance, ObservedAt: later}}); err != nil {
		t.Fatal(err)
	}
	refuse()
	at = now.Add(5 * time.Second)
	observe(at, codeObservedWindow("codex_other", "secondary", 0, nil, at))
	account = codeObservedAccount(t, f)
	if !account.Eligible || len(account.Allowance.Windows) != 3 || account.Allowance.Credits == nil || account.Allowance.Credits.HasCredits {
		t.Fatal("credits invented an account-wide exhaustion or windows were lost")
	}
	olderCredit := &codemode.Credits{HasCredits: true, Unlimited: true, ObservedAt: stale}
	if err = f.store.ObserveAllowance(ctx, f.account, codemode.Allowance{ObservedAt: stale, Credits: olderCredit}); err != nil {
		t.Fatal(err)
	}
	if codeObservedAccount(t, f).Allowance.Credits.Unlimited {
		t.Fatal("stale credits replaced a newer observation")
	}
}

func TestCodeObservationConcurrentWindowUpdatesRetainNewestPerIdentity(t *testing.T) {
	f := newCodeFixture(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	var wg sync.WaitGroup
	errors := make(chan error, 24)
	for i := range 12 {
		for version := range 2 {
			wg.Go(func() {
				at := now.Add(time.Duration(version) * time.Second)
				w := codeObservedWindow(fmt.Sprintf("codex_%d", i), "primary", float64(version*100), nil, at)
				errors <- f.store.ObserveAllowance(t.Context(), f.account, codemode.Allowance{ObservedAt: at, Windows: []codemode.AllowanceWindow{w}})
			})
		}
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	account := codeObservedAccount(t, f)
	if account.Eligible || len(account.Allowance.Windows) != 12 {
		t.Fatalf("lost concurrent observations: %+v", account.Allowance)
	}
	for _, window := range account.Allowance.Windows {
		if window.UsedPercent != 100 || !window.ObservedAt.Equal(now.Add(time.Second)) {
			t.Fatalf("stale concurrent window won: %+v", window)
		}
	}
}

func TestCodeObservationOutcomesRemainDistinctFromUsageUncertainty(t *testing.T) {
	f := newCodeFixture(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, status := range []int{401, 429, 500, 200} {
		permit, err := f.store.Admit(t.Context(), f.input(fmt.Sprintf("outcome-%d", status), "", nil))
		if err != nil {
			t.Fatal(err)
		}
		id := permit.Attempt.ID
		if err = f.store.MarkDispatched(t.Context(), id); err != nil {
			t.Fatal(err)
		}
		observe := func(o codemode.Outcome) {
			t.Helper()
			if err := f.store.ObserveOutcome(t.Context(), id, o); err != nil {
				t.Fatal(err)
			}
		}
		observe(codemode.Outcome{Origin: "upstream", Kind: "headers", UpstreamStatus: &status, ObservedAt: now})
		wantOrigin, wantKind := "upstream", "rejected"
		if status == 200 {
			wantOrigin, wantKind = "gateway", "interrupted"
			observe(codemode.Outcome{Origin: wantOrigin, Kind: wantKind, ObservedAt: now.Add(time.Second)})
		} else {
			observe(codemode.Outcome{Origin: wantOrigin, Kind: wantKind, UpstreamStatus: &status, ObservedAt: now.Add(time.Second)})
		}
		observe(codemode.Outcome{Origin: "gateway", Kind: "interrupted", ObservedAt: now.Add(2 * time.Second)})
		observe(codemode.Outcome{Origin: "upstream", Kind: "headers", UpstreamStatus: &status, ObservedAt: now.Add(3 * time.Second)})
		if err = f.store.Settle(t.Context(), id, codemode.Usage{}); err != nil {
			t.Fatal(err)
		}
		response := f.h.want(f.owner, "GET", "/api/v1/code/attempts?project_id="+f.project+"&binding_id="+permit.Binding.ID, nil, nil, 200)
		attempts := codePublicDecode[[]codemode.Attempt](t, response["items"])
		if len(attempts) != 1 {
			t.Fatal("missing outcome")
		}
		a := attempts[0]
		if a.UpstreamStatus == nil || *a.UpstreamStatus != status || a.OutcomeOrigin == nil || *a.OutcomeOrigin != wantOrigin || a.Outcome == nil || *a.Outcome != wantKind || a.State != "uncertain" || a.ReportedTokens != nil {
			t.Fatalf("outcome and consumption conflated: %+v", a)
		}
		if a.OutcomeObservedAt == nil || !a.OutcomeObservedAt.Equal(now.Add(time.Second)) {
			t.Fatal("late cleanup replaced the outcome")
		}
		if err = f.store.ObserveOutcome(t.Context(), id, codemode.Outcome{Origin: "upstream", Kind: "secret-canary", ObservedAt: now}); err == nil {
			t.Fatal("free text outcome accepted")
		}
		if _, err = f.h.Pool.Exec(t.Context(), `UPDATE olp.code_attempts SET outcome='secret-canary' WHERE id=$1`, id); err == nil {
			t.Fatal("database accepted unbounded outcome")
		}
		encoded, _ := json.Marshal(response)
		if strings.Contains(string(encoded), "secret-canary") {
			t.Fatal("diagnostics retained raw provider error text")
		}
	}
}

func TestCodeObservationCompletionPreservesStatusAndUnknownUsage(t *testing.T) {
	f := newCodeFixture(t)
	ctx := t.Context()
	permit, err := f.store.Admit(ctx, f.input("completion-metadata", "", nil))
	if err != nil {
		t.Fatal(err)
	}
	id := permit.Attempt.ID
	if err = f.store.MarkDispatched(ctx, id); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	status := 200
	for _, outcome := range []codemode.Outcome{
		{Origin: "upstream", Kind: "headers", UpstreamStatus: &status, ObservedAt: now},
		{Origin: "gateway", Kind: "interrupted", ObservedAt: now.Add(2 * time.Second)},
		{Origin: "upstream", Kind: "completed", ObservedAt: now.Add(time.Second)},
	} {
		if err = f.store.ObserveOutcome(ctx, id, outcome); err != nil {
			t.Fatal(err)
		}
	}
	var kind string
	if err = f.h.Pool.QueryRow(ctx, `SELECT outcome FROM olp.code_attempts WHERE id=$1`, id).Scan(&kind); err != nil || kind != "interrupted" {
		t.Fatalf("stale completion replaced the newer observation: %s %v", kind, err)
	}
	for _, outcome := range []codemode.Outcome{
		{Origin: "upstream", Kind: "completed", ObservedAt: now.Add(3 * time.Second)},
		{Origin: "gateway", Kind: "interrupted", ObservedAt: now.Add(4 * time.Second)},
	} {
		if err = f.store.ObserveOutcome(ctx, id, outcome); err != nil {
			t.Fatal(err)
		}
	}
	if err = f.store.Settle(ctx, id, codemode.Usage{}); err != nil {
		t.Fatal(err)
	}
	response := f.h.want(f.owner, "GET", "/api/v1/code/attempts?project_id="+f.project+"&binding_id="+permit.Binding.ID, nil, nil, 200)
	attempts := codePublicDecode[[]codemode.Attempt](t, response["items"])
	if len(attempts) != 1 {
		t.Fatal("missing completed observation")
	}
	a := attempts[0]
	if a.UpstreamStatus == nil || *a.UpstreamStatus != 200 || a.OutcomeOrigin == nil || *a.OutcomeOrigin != "upstream" || a.Outcome == nil || *a.Outcome != "completed" || a.State != "uncertain" || a.ReportedTokens != nil {
		t.Fatalf("completion inferred consumption or dropped response status: %+v", a)
	}
}

func TestCodeObservationEqualTimestampPromotesHeadersToTerminal(t *testing.T) {
	f := newCodeFixture(t)
	ctx := t.Context()
	permit, err := f.store.Admit(ctx, f.input("outcome-precision", "", nil))
	if err != nil {
		t.Fatal(err)
	}
	id := permit.Attempt.ID
	if err = f.store.MarkDispatched(ctx, id); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	status := 429
	for _, kind := range []string{"headers", "rejected", "headers"} {
		if err = f.store.ObserveOutcome(ctx, id, codemode.Outcome{Origin: "upstream", Kind: kind, UpstreamStatus: &status, ObservedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	var kind string
	if err = f.h.Pool.QueryRow(ctx, `SELECT outcome FROM olp.code_attempts WHERE id=$1`, id).Scan(&kind); err != nil || kind != "rejected" {
		t.Fatalf("timestamp precision lost the terminal outcome: %s %v", kind, err)
	}
}
