package gateway

import (
	"testing"
	"time"
)

func TestResponseLocalRefusalReleasesHalfOpenProbeWithoutPenalizingProvider(t *testing.T) {
	now := time.Now()
	health := newHealthTracker(func() time.Time { return now })
	for range circuitFailures {
		health.record("provider", AttemptFact{Class: classConnect})
	}
	now = now.Add(circuitOpenFor)
	if probed, granted := health.claim("provider"); !probed || !granted {
		t.Fatal("expired circuit did not admit its probe")
	}
	health.record("provider", AttemptFact{Class: classAmbiguous, localRefusal: true})
	stats := health.ProviderHealth(time.Hour)["provider"]
	if stats.Attempts != circuitFailures+1 || stats.TransportErrors != circuitFailures || stats.Successes != 0 {
		t.Fatalf("local persistence refusal charged provider health: %+v", stats)
	}
	if probed, granted := health.claim("provider"); !probed || !granted {
		t.Fatal("local refusal retained or penalized the half-open probe")
	}
	health.releaseProbe("provider")
}

func TestResponseNativeProtocolFailureStillPenalizesHalfOpenProvider(t *testing.T) {
	now := time.Now()
	health := newHealthTracker(func() time.Time { return now })
	for range circuitFailures {
		health.record("provider", AttemptFact{Class: classConnect})
	}
	now = now.Add(circuitOpenFor)
	if _, granted := health.claim("provider"); !granted {
		t.Fatal("expired circuit did not admit its probe")
	}
	health.record("provider", AttemptFact{Class: classProtocol})
	stats := health.ProviderHealth(time.Hour)["provider"]
	if stats.TransportErrors != circuitFailures+1 || stats.Successes != 0 || !health.open("provider") {
		t.Fatalf("native protocol failure lost its provider evidence: %+v", stats)
	}
	if _, granted := health.claim("provider"); granted {
		t.Fatal("failed native probe did not reopen the endpoint circuit")
	}
}

func TestResponseCompleteNativeSuccessClosesCircuitDespiteLocalRefusal(t *testing.T) {
	now := time.Now()
	health := newHealthTracker(func() time.Time { return now })
	for range circuitFailures {
		health.record("provider", AttemptFact{Class: classConnect})
	}
	now = now.Add(circuitOpenFor)
	if _, granted := health.claim("provider"); !granted {
		t.Fatal("expired circuit did not admit its probe")
	}
	health.record("provider", AttemptFact{Class: classSuccess, localRefusal: true, UsageComplete: true})
	stats := health.ProviderHealth(time.Hour)["provider"]
	if stats.Successes != 1 || stats.TransportErrors != circuitFailures || health.open("provider") {
		t.Fatalf("local projection refusal hid native completion: %+v", stats)
	}
	if probed, granted := health.claim("provider"); probed || !granted {
		t.Fatal("native success did not return the provider to ordinary admission")
	}
}
