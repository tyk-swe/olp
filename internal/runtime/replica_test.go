package runtime

import (
	"errors"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/routing"
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
