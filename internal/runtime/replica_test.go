package runtime

import (
	"errors"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/routing"
)

func TestAuthorityFreshnessUsesInjectedClock(t *testing.T) {
	now := time.Unix(0, 0)
	for _, test := range []struct {
		name        string
		age         time.Duration
		stale       bool
		eligibility routing.Eligibility
		budgetError error
	}{
		{name: "fresh", eligibility: routing.Eligible},
		{name: "at limit", age: AuthorityStaleAfter, eligibility: routing.Eligible},
		{name: "stale", age: AuthorityStaleAfter + time.Nanosecond, stale: true, eligibility: routing.StaleAuthority, budgetError: ErrStaleAuthority},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager := NewManager(nil, ManagerConfig{Now: func() time.Time { return now }})
			at, err := manager.authorityReadTime(t.Context())
			if err != nil || !at.Equal(now) {
				t.Fatalf("authority read time = %v, %v, want %v", at, err, now)
			}
			manager.authority = authorityState{loaded: true, readAt: at.Add(-test.age)}
			if got := manager.Authority().Stale; got != test.stale {
				t.Errorf("authority stale = %v, want %v", got, test.stale)
			}
			if got := manager.Eligibility("credential"); got != test.eligibility {
				t.Errorf("eligibility = %v, want %v", got, test.eligibility)
			}
			if _, err := manager.BudgetAuthority(nil); !errors.Is(err, test.budgetError) {
				t.Errorf("budget authority error = %v, want %v", err, test.budgetError)
			}
		})
	}
}

func TestReplicaLagConsumesTheAuthorityAgeBudget(t *testing.T) {
	at := time.Now()
	progress := replicaProgress{}
	confirmed, err := progress.observe(at, 100, 100)
	if err != nil || !confirmed.Equal(at) {
		t.Fatalf("initial checkpoint: %v %v", confirmed, err)
	}
	confirmed, err = progress.observe(at.Add(30*time.Second), 200, 100)
	if err != nil || !confirmed.Equal(at) {
		t.Fatalf("lag reset authority age: %v %v", confirmed, err)
	}
	manager := authorityManager(confirmed, nil)
	manager.authority.readAt = confirmed.Add(-AuthorityStaleAfter - time.Second)
	if manager.Eligibility("credential") != routing.StaleAuthority {
		t.Fatal("lagged authority vouched for credentials")
	}
	if _, err = progress.observe(at.Add(AuthorityStaleAfter+time.Second), 300, 100); !errors.Is(err, ErrStaleAuthority) {
		t.Fatalf("stale replica: %v", err)
	}
	confirmed, err = progress.observe(at.Add(AuthorityStaleAfter+2*time.Second), 400, 200)
	if err != nil || !confirmed.Equal(at.Add(30*time.Second)) {
		t.Fatalf("replay did not inherit its original checkpoint age: %v %v", confirmed, err)
	}
	if _, err = progress.observe(at.Add(2*AuthorityStaleAfter), 500, 200); !errors.Is(err, ErrStaleAuthority) {
		t.Fatalf("stalled replay remained fresh: %v", err)
	}
}
