package runtime

import (
	"errors"
	"testing"
	"time"
)

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
	if manager.Eligibility("credential") != StaleAuthority {
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

func TestIdleReplicasStayFreshWithoutDatabaseClockOrWrites(t *testing.T) {
	progress := replicaProgress{}
	at := time.Now()
	for i := 0; i < 100; i++ {
		now := at.Add(time.Duration(i) * AuthorityStaleAfter)
		confirmed, err := progress.observe(now, 10, 10)
		if err != nil || !confirmed.Equal(now) {
			t.Fatalf("idle checkpoint %d: %v %v", i, confirmed, err)
		}
	}
	if _, err := progress.observe(at.Add(101*AuthorityStaleAfter), 11, 9); !errors.Is(err, ErrStaleAuthority) {
		t.Fatal("replay regression accepted")
	}
}

func TestReplicaCheckpointMemoryIsBoundedUnderFrequentRefresh(t *testing.T) {
	progress := replicaProgress{}
	at := time.Now()
	progress.observe(at, 1, 1)
	for i := 1; i < 10000; i++ {
		progress.observe(at.Add(time.Duration(i)*time.Millisecond), uint64(i+1), 1)
	}
	if len(progress.pending) > int(AuthorityStaleAfter/PollInterval)+1 {
		t.Fatalf("unbounded pending checkpoints: %d", len(progress.pending))
	}
}

func TestPostgresWALPositionsCompareAcrossSegments(t *testing.T) {
	a, err := parseLSN("A/FFFFFFFF")
	if err != nil {
		t.Fatal(err)
	}
	b, err := parseLSN("B/0")
	if err != nil || b <= a {
		t.Fatalf("WAL order: %x %x %v", a, b, err)
	}
	for _, raw := range []string{"", "/1", "1/", "1/100000000", "1/2/3", "-1/0"} {
		if _, err = parseLSN(raw); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
}
