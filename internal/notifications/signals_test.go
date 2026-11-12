package notifications

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/management/contract"
)

type signalStep struct {
	at                         time.Duration
	value                      string
	active, trigger, recovered bool
	incident                   int64
}

func TestMeasuredSignalHysteresisAndCooldown(t *testing.T) {
	t.Parallel()
	latency := func(raw json.RawMessage) (measuredConfig, error) {
		configuration, err := resolveLatency(raw)
		return configuration.measuredConfig, err
	}
	for _, test := range []struct {
		event   contract.NotificationEvent
		resolve func(json.RawMessage) (measuredConfig, error)
		steps   []signalStep
	}{
		{contract.NotificationEventProviderErrorRate, errorRateDefaults.resolve, []signalStep{
			{at: 0, value: "0.09"},
			{at: time.Minute, value: "0.1", active: true, trigger: true, incident: 1},
			{at: 2 * time.Minute, value: "0.06", active: true, incident: 1},
			{at: 3 * time.Minute, value: "0.4", active: true, incident: 1},
			{at: 16 * time.Minute, value: "0.4", active: true, trigger: true, incident: 1},
			{at: 17 * time.Minute, value: "0.05", recovered: true, incident: 1},
			{at: 18 * time.Minute, value: "0.07", incident: 1},
			{at: 40 * time.Minute, value: "0.2", active: true, trigger: true, incident: 2},
		}},
		{contract.NotificationEventProviderCredentialFailing, credentialDefaults.resolve, []signalStep{
			{at: 0, value: "2"},
			{at: time.Minute, value: "3", active: true, trigger: true, incident: 1},
			{at: 2 * time.Minute, value: "1", active: true, incident: 1},
			{at: 3 * time.Minute, value: "0", recovered: true, incident: 1},
			{at: 4 * time.Minute, value: "3", incident: 1},
			{at: 5 * time.Minute, value: "0", incident: 1},
			{at: 6 * time.Minute, value: "2", incident: 1},
			{at: 16 * time.Minute, value: "3", active: true, trigger: true, incident: 2},
			{at: 17 * time.Minute, value: "0", recovered: true, incident: 2},
		}},
		{contract.NotificationEventRouteLatency, latency, []signalStep{
			{at: 0, value: "999.9"},
			{at: time.Minute, value: "1000", active: true, trigger: true, incident: 1},
			{at: 2 * time.Minute, value: "800.1", active: true, incident: 1},
			{at: 3 * time.Minute, value: "800", recovered: true, incident: 1},
			{at: 4 * time.Minute, value: "900", incident: 1},
		}},
	} {
		t.Run(string(test.event), func(t *testing.T) {
			t.Parallel()
			configuration := resolved(t, test.resolve)
			cooldown := time.Duration(configuration.CooldownSeconds) * time.Second
			start := time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC)
			state := signalState{}
			for _, step := range test.steps {
				r, err := configuration.measure(int64(configuration.MinimumSamples), &step.value)
				if err != nil {
					t.Fatal(err)
				}
				next, err := transitionSignal(state, r, triggersAndRecoveries{}, start.Add(step.at), cooldown)
				if err != nil {
					t.Fatal(err)
				}
				if next.state.active != step.active || (next.change == triggered) != step.trigger || (next.change == recovered) != step.recovered || next.state.incident != step.incident {
					t.Fatalf("at %s value %s: got %+v, want %+v", step.at, step.value, next, step)
				}
				state = next.state
			}
		})
	}
}

func TestRecoveryOnlySignalsAnnounceEveryRecovery(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC)
	state := signalState{}
	for _, step := range []struct {
		at                time.Duration
		active, recovered bool
		incident          int64
	}{
		{at: 0, active: true, incident: 1},
		{at: time.Minute, recovered: true, incident: 1},
		{at: 2 * time.Minute, active: true, incident: 2},
		{at: 3 * time.Minute, recovered: true, incident: 2},
	} {
		next, err := transitionSignal(state, observed{active: step.active}, recoveriesOnly{}, start.Add(step.at), 15*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if next.state.active != step.active || (next.change == recovered) != step.recovered || next.state.incident != step.incident {
			t.Fatalf("at %s: got %+v, want %+v", step.at, next, step)
		}
		state = next.state
	}
}
