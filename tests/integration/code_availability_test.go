//go:build integration

package integration_test

import (
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/codemode"
)

func TestCodeAccountRecoveryRetainsPinAndHonorsAllowanceReset(t *testing.T) {
	f := newCodeFixture(t)
	root, err := f.store.BindConnection(t.Context(), f.route, f.key, codemode.Identity{Conversation: "recover"}, "", []string{f.provider})
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

func TestCodeAccountEligibilityReflectsProviderState(t *testing.T) {
	f := newCodeFixture(t)
	root, err := f.store.BindConnection(t.Context(), f.route, f.key, codemode.Identity{Conversation: "provider-state"}, "", []string{f.provider})
	if err != nil {
		t.Fatal(err)
	}
	if !codeObservedAccount(t, f).Eligible {
		t.Fatal("account with a draft provider is ineligible")
	}
	f.exec(t, `UPDATE olp.providers SET state='disabled' WHERE id=$1`, f.provider)
	if account := codeObservedAccount(t, f); account.Eligible || !account.Enabled || account.GrantState != "current" {
		t.Fatalf("disabled provider eligibility: %+v", account)
	}
	input := map[string]any{"project_id": f.project, "provider_id": f.provider, "credential_id": f.credential, "name": "Fixture subscription", "enabled": true, "models": []string{"native-model"}}
	updated := f.h.want(f.owner, "PUT", "/api/v1/code/accounts/"+f.account, input, etagHeader(f.accountRecord), 200)
	if updated["eligible"] != false {
		t.Fatal("account write reports a disabled provider as eligible")
	}
	_, err = f.store.Admit(t.Context(), f.input("provider-state", "", nil))
	codeRefusal(t, err, "code_account_unavailable")
	f.exec(t, `UPDATE olp.providers SET state='draft' WHERE id=$1`, f.provider)
	if !codeObservedAccount(t, f).Eligible {
		t.Fatal("account eligibility did not recover with its provider")
	}
	permit, err := f.store.Admit(t.Context(), f.input("provider-state", "", nil))
	if err != nil || permit.Binding.ID != root.Binding.ID || permit.Account.ID != root.Account.ID {
		t.Fatalf("provider recovery changed the account pin: %v", err)
	}
	if err := f.store.Abort(t.Context(), permit.Attempt.ID); err != nil {
		t.Fatal(err)
	}
}
