//go:build integration

package integration_test

import (
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/codemode"
)

func TestCodeAccountRecoveryRetainsPinAndHonorsAllowanceReset(t *testing.T) {
	f := newCodeFixture(t)
	root, err := f.store.BindConnection(t.Context(), f.route, f.key, codemode.Identity{Conversation: "recover"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	zero := 0.0
	reset := now.Add(time.Hour)
	if err = f.store.ObserveAllowance(t.Context(), f.account, codemode.Allowance{RemainingPercent: &zero, ResetsAt: &reset, ObservedAt: now}); err != nil {
		t.Fatal(err)
	}
	_, err = f.store.Admit(t.Context(), f.input("recover", "", nil))
	codeRefusal(t, err, "code_account_unavailable")
	if err = f.store.ObserveHealth(t.Context(), f.account, "quota_limited"); err != nil {
		t.Fatal(err)
	}
	f.exec(t, `UPDATE olp.code_accounts SET unavailable_until=now()-interval '1 second' WHERE id=$1`, f.account)
	_, err = f.store.Admit(t.Context(), f.input("recover", "", nil))
	codeRefusal(t, err, "code_account_unavailable")
	reset = now.Add(-time.Second)
	if err = f.store.ObserveAllowance(t.Context(), f.account, codemode.Allowance{RemainingPercent: &zero, ResetsAt: &reset, ObservedAt: now.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	permit, err := f.store.Admit(t.Context(), f.input("recover", "", nil))
	if err != nil || permit.Binding.ID != root.Binding.ID || permit.Account.ID != root.Account.ID {
		t.Fatalf("recovered admission changed pin: %v", err)
	}
	if err = f.store.Abort(t.Context(), permit.Attempt.ID); err != nil {
		t.Fatal(err)
	}
	if err = f.store.ObserveHealth(t.Context(), f.account, "unavailable"); err != nil {
		t.Fatal(err)
	}
	_, err = f.store.Admit(t.Context(), f.input("recover", "", nil))
	codeRefusal(t, err, "code_account_unavailable")
	f.exec(t, `UPDATE olp.code_accounts SET unavailable_until=now()-interval '1 second' WHERE id=$1`, f.account)
	permit, err = f.store.Admit(t.Context(), f.input("recover", "", nil))
	if err != nil || permit.Account.ID != root.Account.ID {
		t.Fatalf("temporary outage changed pin: %v", err)
	}
}
