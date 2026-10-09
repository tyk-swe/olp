//go:build integration

package usage

import (
	"strings"
	"testing"
	"time"
)

func TestIntegrationEndUserDigestsSurviveAccountingAndRetention(t *testing.T) {
	pool := notificationPool(t)
	owner, key := seedParityOwner(t, pool)
	provider := seedParityProvider(t, pool, owner)
	now := time.Now().UTC().Truncate(time.Hour)
	observed := now.Add(-2 * time.Hour)
	input := int64(10)
	for _, digest := range []string{strings.Repeat("ab", 32), strings.Repeat("cd", 32), ""} {
		event := parityEvent(t, key, provider, observed, []parityCase{{model: "model", usage: AttemptUsage{Observed: true, Complete: true, InputTokens: &input}}})
		event.EndUserDigest = digest
		result, _ := persistParity(t, pool, event)
		if result.Outcome != PersistOutcomePersisted {
			t.Fatalf("event outcome = %v", result.Outcome)
		}
		var stored string
		if err := pool.QueryRow(t.Context(), "SELECT end_user_digest FROM olp.requests WHERE id=$1", event.RequestID).Scan(&stored); err != nil || stored != digest {
			t.Fatalf("request digest = %q: %v", stored, err)
		}
	}
	filters := Filters{Start: observed.Add(-time.Hour), End: now, AllProjects: true}
	check := func() {
		t.Helper()
		report, err := ReadBreakdown(t.Context(), pool, filters, DimensionEndUser, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(report.Items) != 3 {
			t.Fatalf("end users merged: %+v", report.Items)
		}
		for _, item := range report.Items {
			if item.RequestCount != 1 || (item.Dimension != "unidentified" && len(item.Dimension) != 64) {
				t.Fatalf("invalid end-user rollup: %+v", item)
			}
		}
	}
	check()
	var rolled, expired int64
	if err := pool.QueryRow(t.Context(), rollupSQL, now, int64(100)).Scan(&rolled, &expired); err != nil || rolled != 3 || expired != 3 {
		t.Fatalf("rolled rows = %d: %v", rolled, err)
	}
	check()
}
