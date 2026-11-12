package notifications

import (
	"encoding/json"
	"maps"
	"slices"

	"github.com/tyk-swe/olp/internal/management/contract"
)

type eventSpec struct {
	subject       subjectShape
	projectScoped bool
	configure     func(json.RawMessage) (configured, error)
}

type configured struct {
	canonical json.RawMessage
	evaluate  evaluator
}

func evaluated[C any](resolve func(json.RawMessage) (C, error), evaluate func(C) evaluator) func(json.RawMessage) (configured, error) {
	return func(raw json.RawMessage) (configured, error) {
		config, err := resolve(raw)
		if err != nil {
			return configured{}, err
		}
		canonical, err := json.Marshal(config)
		return configured{canonical: canonical, evaluate: evaluate(config)}, err
	}
}

func stored[C any](resolve func(json.RawMessage) (C, error)) func(json.RawMessage) (configured, error) {
	return evaluated(resolve, func(C) evaluator { return nil })
}

func cooldownSignals(source signalSource) func(json.RawMessage) (configured, error) {
	return evaluated(resolveCooldown, func(c cooldownConfig) evaluator { return signals(source, c.CooldownSeconds) })
}

var events = map[contract.NotificationEvent]eventSpec{
	contract.NotificationEventBudgetThreshold:     {subject: budgetSubject, configure: stored(resolveNothing)},
	contract.NotificationEventProviderGrantLapsed: {configure: stored(resolveNothing)},
	contract.NotificationEventKeyExpiring:         {subject: keySubject, configure: stored(resolveLeadTime)},
	contract.NotificationEventReportSpend: {
		projectScoped: true,
		configure:     evaluated(resolveSpendReport, func(c spendReportConfig) evaluator { return c.claim }),
	},
	contract.NotificationEventBudgetExhausted: {
		projectScoped: true,
		configure:     cooldownSignals(signalSource{read: budgetExhaustedSignals, recoversMissing: true, announce: triggersAndRecoveries{}}),
	},
	contract.NotificationEventProviderErrorRate: {
		configure: evaluated(errorRateDefaults.resolve, func(c measuredConfig) evaluator {
			return signals(signalSource{read: providerErrorSignals(c), recoversMissing: true, announce: triggersAndRecoveries{}}, c.CooldownSeconds)
		}),
	},
	contract.NotificationEventProviderCredentialFailing: {
		configure: evaluated(credentialDefaults.resolve, func(c measuredConfig) evaluator {
			return signals(signalSource{read: credentialFailureSignals(c), announce: triggersAndRecoveries{}}, c.CooldownSeconds)
		}),
	},
	contract.NotificationEventRouteLatency: {
		projectScoped: true,
		configure: evaluated(resolveLatency, func(c latencyConfig) evaluator {
			return signals(signalSource{read: routeLatencySignals(c), recoversMissing: true, announce: triggersAndRecoveries{}}, c.CooldownSeconds)
		}),
	},
	contract.NotificationEventModelRetirement: {
		projectScoped: true,
		configure: evaluated(resolveRetirement, func(c retirementConfig) evaluator {
			return signals(signalSource{read: retirementSignals(c), recoversMissing: true, announce: triggersAndRecoveries{}}, c.CooldownSeconds)
		}),
	},
	contract.NotificationEventProviderCircuitOpen: {
		configure: cooldownSignals(signalSource{read: circuitSignals, recoversMissing: true, announce: recoveriesAs{recovery: contract.NotificationEventProviderCircuitClosed}}),
	},
	contract.NotificationEventProviderCircuitClosed: {
		configure: cooldownSignals(signalSource{read: circuitSignals, recoversMissing: true, announce: recoveriesOnly{}}),
	},
	contract.NotificationEventRuntimeInstallFailed: {
		configure: cooldownSignals(signalSource{read: runtimeInstallSignals, announce: triggersAndRecoveries{}}),
	},
	contract.NotificationEventWorkerStale: {
		configure: cooldownSignals(signalSource{read: workerStaleSignals, announce: triggersAndRecoveries{}}),
	},
}

var evaluatedEvents = slices.Sorted(func(yield func(string) bool) {
	for event, spec := range maps.All(events) {
		if defaults, err := spec.configure(nil); err == nil && defaults.evaluate != nil && !yield(string(event)) {
			return
		}
	}
})
