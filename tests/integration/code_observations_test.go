//go:build integration

package integration_test

import (
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

// This event is newer for secondary but older than the latest primary.
