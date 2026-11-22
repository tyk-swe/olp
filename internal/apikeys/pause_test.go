package apikeys

import (
	"testing"
	"time"
)

func TestPauseAndResumeDecisions(t *testing.T) {
	now := time.Now()
	paused, revoked, expired, future := now, now, now.Add(-time.Minute), now.Add(time.Hour)
	for _, tc := range []struct {
		name    string
		key     storedKey
		change  pauseChange
		wantErr bool
	}{
		{name: "pause active", key: storedKey{}, change: pauseKey},
		{name: "pause already paused", key: storedKey{paused: &paused}, change: pauseKey, wantErr: true},
		{name: "pause revoked", key: storedKey{revoked: &revoked}, change: pauseKey, wantErr: true},
		{name: "resume paused", key: storedKey{paused: &paused, expires: &future}, change: resumeKey},
		{name: "resume not paused", key: storedKey{}, change: resumeKey, wantErr: true},
		{name: "resume expired", key: storedKey{paused: &paused, expires: &expired}, change: resumeKey, wantErr: true},
		{name: "resume revoked wins", key: storedKey{paused: &paused, revoked: &revoked}, change: resumeKey, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.change.decide(tc.key, now); (err != nil) != tc.wantErr {
				t.Fatalf("decision error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestPauseEnforcedRequiresEveryReportedReplica(t *testing.T) {
	seq := int64(42)
	at := int64(42)
	newer := int64(43)
	older := int64(41)
	for _, tc := range []struct {
		name     string
		replicas []pauseReplica
		want     bool
	}{
		{name: "all applied", replicas: []pauseReplica{{Instance: "a", AuthoritySequence: &at}, {Instance: "b", AuthoritySequence: &newer}}, want: true},
		{name: "one lagging", replicas: []pauseReplica{{Instance: "a", AuthoritySequence: &at}, {Instance: "b", AuthoritySequence: &older}}},
		{name: "one unreported", replicas: []pauseReplica{{Instance: "a", AuthoritySequence: &at}, {Instance: "b"}}},
		{name: "none reported", replicas: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := pauseEnforced(&seq, tc.replicas); got != tc.want {
				t.Fatalf("pauseEnforced = %v, want %v", got, tc.want)
			}
		})
	}
	if pauseEnforced(nil, []pauseReplica{{Instance: "a", AuthoritySequence: &at}}) {
		t.Fatal("missing pause generation reported enforced")
	}
}
