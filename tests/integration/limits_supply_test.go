//go:build integration

package integration_test

import (
	"reflect"
	"strconv"
	"testing"

	"github.com/tyk-swe/olp/internal/limits"
)

func TestLimitsSupplyIncludesPendingCostWithoutChangingState(t *testing.T) {
	for _, window := range []string{"daily", "monthly"} {
		t.Run(window, func(t *testing.T) {
			f := limCostSeed(t, "supply-"+window, "0.2")
			request := f.request("0.8")
			probe := limits.CapProbe{OwnerID: f.owner}
			if window == "daily" {
				request.DailyCostLimit, request.MonthlyCostLimit = limPointer("1"), nil
				probe.DailyCostLimit = request.DailyCostLimit
			} else {
				request.DailyCostLimit, request.MonthlyCostLimit = nil, limPointer("1")
				probe.MonthlyCostLimit = request.MonthlyCostLimit
			}
			pending, expiry := f.keys()
			day, month := limCostKeys(f.namespace, f.owner)
			// A read must also leave balances without a TTL untouched.
			do(t, f.c, "PERSIST", day)
			do(t, f.c, "PERSIST", month)
			state := func() []any {
				return []any{do(t, f.c, "HGETALL", day), do(t, f.c, "HGETALL", month),
					do(t, f.c, "HGETALL", pending), do(t, f.c, "ZRANGE", expiry, "0", "-1", "WITHSCORES")}
			}
			wantSupply := func(want limits.CapState) {
				t.Helper()
				before := state()
				_, caps := f.limiter.Supply(t.Context(), nil, []limits.CapProbe{probe})
				if len(caps) != 1 || caps[0] != want {
					t.Fatalf("cap states=%v, want %v", caps, want)
				}
				if after := state(); !reflect.DeepEqual(before, after) {
					t.Fatalf("Supply changed state: before=%v after=%v", before, after)
				}
				if limInt(t, f.c, "PTTL", day) != -1 || limInt(t, f.c, "PTTL", month) != -1 {
					t.Fatal("Supply changed a balance TTL")
				}
			}
			wantSupply(limits.CapAvailable)
			limReserve(t, f.limiter, request)
			// Reservation admission can add balance TTLs; remove them again.
			do(t, f.c, "PERSIST", day)
			do(t, f.c, "PERSIST", month)
			wantSupply(limits.CapExhausted)
			// Expired holds stop counting without a read retiring them.
			do(t, f.c, "ZADD", expiry, strconv.FormatInt(limServerTimeMS(t, f.c)-1, 10), request.RequestID)
			wantSupply(limits.CapAvailable)
			f.accrue(t, "", "1")
			do(t, f.c, "PERSIST", day)
			do(t, f.c, "PERSIST", month)
			wantSupply(limits.CapExhausted)
		})
	}
}
