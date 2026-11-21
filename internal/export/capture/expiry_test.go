package capture

import (
	"encoding/json"
	"math/big"
	"testing"
	"testing/synctest"
	"time"
)

func TestCachedCapturePoliciesExpireWithoutRefresh(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		deadline := time.Now().Add(time.Second)
		route := "temporary"
		project := "project"
		policy := func(id string, route *string, expiry *time.Time) active {
			return active{
				Policy:    Policy{ID: id, Settings: Settings{ProjectID: &project, Route: route, ExpiresAt: expiry, MaxBytes: 1024, Include: []Include{Input}}},
				ratio:     big.NewRat(1, 1),
				redaction: unredacted{},
			}
		}
		manager := NewManager(nil, nil, "", nil, nil)
		manager.cache.Store(&snapshot{at: time.Now(), policies: []active{policy("temporary", &route, &deadline), policy("always-on", nil, nil)}})
		assertCapture(t, manager, project, route, "temporary")
		inFlight := manager.Resolve(project, route, "", "", "in-flight")
		if inFlight == nil {
			t.Fatal("no collector for the in-flight request")
		}
		inFlight.CaptureDocument(map[string]json.RawMessage{"input": json.RawMessage(`"collected before expiry"`)})

		time.Sleep(time.Until(deadline))
		if inFlight.Finish(t.Context(), Identity{ProjectID: project, Outcome: "success", Mode: Unary}, time.Now()) || len(manager.queue) != 0 {
			t.Fatal("payload collected before the deadline was queued by an expired collector")
		}
		if dropped := manager.dropped.Load(); dropped != 0 {
			t.Fatalf("planned expiry counted %d dropped captures", dropped)
		}
		assertCapture(t, manager, project, route, "always-on")
	})
}

func assertCapture(t *testing.T, manager *Manager, project, route, wantPolicy string) {
	t.Helper()
	collector := manager.Resolve(project, route, "", "", "request")
	if collector == nil {
		t.Fatalf("no collector for route %s", route)
	}
	collector.CaptureDocument(map[string]json.RawMessage{"input": json.RawMessage(`"sample"`)})
	if !collector.Finish(t.Context(), Identity{ProjectID: project, Outcome: "success", Mode: Unary}, time.Now()) {
		t.Fatal("eligible request was not captured")
	}
	if got := (<-manager.queue).policyID; got != wantPolicy {
		t.Fatalf("queued capture for policy %s, want %s", got, wantPolicy)
	}
}
