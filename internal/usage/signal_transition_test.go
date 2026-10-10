package usage

import (
	"math"
	"testing"
	"time"
)

func TestSignalTransitionPreservesIncidentOrderingAndCooldown(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	cooldown := 15 * time.Minute
	first, err := transitionSignal(false, 0, nil, true, now, cooldown)
	if err != nil || !first.active || !first.trigger || first.recover || first.incident != 1 || first.lastTriggerAt == nil || !first.lastTriggerAt.Equal(now) {
		t.Fatalf("first trigger: %+v, %v", first, err)
	}
	repeated, err := transitionSignal(true, 1, first.lastTriggerAt, true, now.Add(time.Minute), cooldown)
	if err != nil || repeated.trigger || repeated.recover || repeated.incident != 1 || !repeated.lastTriggerAt.Equal(now) {
		t.Fatalf("cooldown: %+v, %v", repeated, err)
	}
	reminder, err := transitionSignal(true, 1, first.lastTriggerAt, true, now.Add(cooldown), cooldown)
	if err != nil || !reminder.trigger || reminder.recover || reminder.incident != 1 || !reminder.lastTriggerAt.Equal(now.Add(cooldown)) {
		t.Fatalf("reminder: %+v, %v", reminder, err)
	}
	recovery, err := transitionSignal(true, 1, first.lastTriggerAt, false, now.Add(2*time.Minute), cooldown)
	if err != nil || recovery.active || recovery.trigger || !recovery.recover || recovery.incident != 1 || !recovery.lastTriggerAt.Equal(now) {
		t.Fatalf("recovery: %+v, %v", recovery, err)
	}
	reopened, err := transitionSignal(false, 1, recovery.lastTriggerAt, true, now.Add(3*time.Minute), cooldown)
	if err != nil || !reopened.active || reopened.trigger || reopened.recover || reopened.incident != 2 {
		t.Fatalf("reopen within cooldown: %+v, %v", reopened, err)
	}
	later, err := transitionSignal(true, 2, reopened.lastTriggerAt, true, now.Add(cooldown), cooldown)
	if err != nil || !later.trigger || later.incident != 2 {
		t.Fatalf("deferred trigger: %+v, %v", later, err)
	}
	quiet, err := transitionSignal(false, 2, later.lastTriggerAt, false, now.Add(2*cooldown), cooldown)
	if err != nil || quiet.active || quiet.trigger || quiet.recover || quiet.incident != 2 {
		t.Fatalf("inactive: %+v, %v", quiet, err)
	}
	if _, err := transitionSignal(false, math.MaxInt64, nil, true, now, cooldown); err == nil {
		t.Fatal("incident overflow was accepted")
	}
}
