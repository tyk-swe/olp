//go:build integration

package gateway

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/coordination"
	"github.com/tyk-swe/olp/internal/limits"
)

// lookupOutage runs every command against Valkey except the script calls that
// name one lookup, which it fails the way an outage does: refused, before
// anything ran. It is how the key's own budgets are made unreachable after the
// budget group's was reserved, since the two live under different lookups.
type lookupOutage struct {
	limits.Commander
	lookup string
}

func (c lookupOutage) Do(ctx context.Context, args ...string) (any, error) {
	if len(args) > 3 && (args[0] == "EVAL" || args[0] == "EVALSHA") && strings.Contains(args[3], "{"+c.lookup+"}") {
		return nil, &coordination.CommandError{Cause: errors.New("connection reset by peer")}
	}
	return c.Commander.Do(ctx, args...)
}

// TestIntegrationKeyOutageSettlesWhatTheBudgetGroupReserved proves a request the
// key's own limits cannot be consulted for does not keep the budget group's
// reservation, whichever way the outage is decided: failing closed gives it back
// and refuses the request, and failing open admits the request holding it, so that
// it is settled when the request ends. A key's rate limits follow the outage
// policy and its group's cost budget never does, so the group is reserved first.
func TestIntegrationKeyOutageSettlesWhatTheBudgetGroupReserved(t *testing.T) {
	for _, policy := range []limits.OutagePolicy{limits.FailClosed, limits.FailOpen} {
		t.Run(policy.String(), func(t *testing.T) {
			client, namespace := mediaValkey(t)
			rpm, group, daily := int64(100), uuid.NewString(), "10"
			authority := access.Authority{
				ID: uuid.NewString(), LookupID: strings.ReplaceAll(uuid.NewString(), "-", "")[:24],
				Policy:        access.KeyPolicy{RequestsPerMinute: &rpm},
				BudgetGroupID: &group, BudgetGroupDailyCostLimit: &daily,
			}
			limiter, err := limits.New(lookupOutage{Commander: client, lookup: authority.LookupID}, namespace)
			if err != nil {
				t.Fatal(err)
			}
			windows := limits.BudgetWindows(time.Now())
			if _, _, err := limiter.ApplyCostSnapshot(t.Context(), limits.CostSnapshot{
				CostOwnerID: group, DailyWindowID: windows.DailyID, DailyAccrued: "0",
				MonthlyWindowID: windows.MonthlyID, MonthlyAccrued: "0",
			}); err != nil {
				t.Fatal(err)
			}
			admission := NewAdmission(limiter, func() limits.OutagePolicy { return policy }, slog.New(slog.DiscardHandler))
			held := func() string {
				t.Helper()
				total, err := limiter.Reserved(t.Context(), group)
				if err != nil {
					t.Fatal(err)
				}
				return total
			}

			hold := costReservation{amount: "0.25", requestID: uuid.NewString()}
			lease, refusal := admission.reserveKeyCosted(t.Context(), &authority, "openai", 100, time.Minute, hold)
			if policy == limits.FailClosed {
				if lease != nil || refusal == nil || refusal.Status != http.StatusServiceUnavailable || refusal.Code != "distributed_limits_unavailable" {
					t.Fatalf("reserveKeyCosted = %v, %v, want the request refused", lease, refusal)
				}
				if got := held(); got != "0" {
					t.Fatalf("the group still holds %q for a request that was refused", got)
				}
				return
			}
			if refusal != nil {
				t.Fatalf("reserveKeyCosted refused a request that fails open: %v", refusal)
			}
			if lease == nil {
				t.Fatal("the request was admitted without the group's reservation, which nothing can now settle")
			}
			if got := admission.FailOpenTotal(); got != 1 {
				t.Fatalf("fail open total = %d, want the request counted", got)
			}
			if got := held(); got != "0.25" {
				t.Fatalf("the group holds %q for the admitted request, want its estimate", got)
			}
			// The request ends having cost less, which is what the group then holds.
			lease.SetActualCost("0.1")
			settleKey(t.Context(), lease, true, nil, slog.New(slog.DiscardHandler))
			if got := held(); got != "0.1" {
				t.Fatalf("the group holds %q once the request ended, want what it cost", got)
			}
		})
	}
}
