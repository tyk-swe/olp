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
		pause   bool
		wantErr bool
	}{
		{name: "pause active", key: storedKey{}, pause: true},
		{name: "pause already paused", key: storedKey{paused: &paused}, pause: true, wantErr: true},
		{name: "pause revoked", key: storedKey{revoked: &revoked}, pause: true, wantErr: true},
		{name: "resume paused", key: storedKey{paused: &paused, expires: &future}},
		{name: "resume not paused", key: storedKey{}, wantErr: true},
		{name: "resume expired", key: storedKey{paused: &paused, expires: &expired}, wantErr: true},
		{name: "resume revoked wins", key: storedKey{paused: &paused, revoked: &revoked}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			if tc.pause {
				err = pauseDecision(tc.key)
			} else {
				err = resumeDecision(tc.key, now)
			}
			if (err != nil) != tc.wantErr {
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
			if got := pauseEnforced(true, &seq, tc.replicas); got != tc.want {
				t.Fatalf("pauseEnforced = %v, want %v", got, tc.want)
			}
		})
	}
	if pauseEnforced(false, &seq, []pauseReplica{{Instance: "a", AuthoritySequence: &at}}) {
		t.Fatal("unpaused key reported enforced")
	}
	if pauseEnforced(true, nil, []pauseReplica{{Instance: "a", AuthoritySequence: &at}}) {
		t.Fatal("missing pause generation reported enforced")
	}
}
