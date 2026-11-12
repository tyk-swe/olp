package notifications

import (
	"encoding/json"
	"slices"

	"github.com/tyk-swe/olp/internal/management/contract"
)

type eventSpec struct {
	subject       subjectShape
	projectScoped bool
	configuration ruleConfiguration
}

type ruleConfiguration interface {
	canonical(raw json.RawMessage) (json.RawMessage, error)
}

type evaluable interface {
	ruleConfiguration
	evaluator(raw json.RawMessage) (evaluator, error)
}

type configResolver[C any] struct {
	resolve func(json.RawMessage) (C, error)
}

type evaluatedResolver[C any] struct {
	configResolver[C]
	evaluate func(C) evaluator
}

func stored[C any](resolve func(json.RawMessage) (C, error)) ruleConfiguration {
	return configResolver[C]{resolve}
}

func evaluated[C any](resolve func(json.RawMessage) (C, error), evaluate func(C) evaluator) evaluable {
	return evaluatedResolver[C]{configResolver[C]{resolve}, evaluate}
}

func (s configResolver[C]) canonical(raw json.RawMessage) (json.RawMessage, error) {
	config, err := s.resolve(raw)
	if err != nil {
		return nil, err
	}
	return json.Marshal(config)
}

func (e evaluatedResolver[C]) evaluator(raw json.RawMessage) (evaluator, error) {
	config, err := e.resolve(raw)
	if err != nil {
		return nil, err
	}
	return e.evaluate(config), nil
}

func cooldownSignals(source signalSource) evaluable {
	return evaluated(resolveCooldown, func(c cooldownConfig) evaluator { return signals(source, c.CooldownSeconds) })
}

var events = map[contract.NotificationEvent]eventSpec{
	contract.NotificationEventBudgetThreshold:     {subject: budgetSubject, configuration: stored(resolveNothing)},
	contract.NotificationEventProviderGrantLapsed: {configuration: stored(resolveNothing)},
	contract.NotificationEventKeyExpiring:         {subject: keySubject, configuration: stored(resolveLeadTime)},
	contract.NotificationEventReportSpend: {
		projectScoped: true,
		configuration: evaluated(resolveSpendReport, func(c spendReportConfig) evaluator { return c.claim }),
	},
	contract.NotificationEventBudgetExhausted: {
		projectScoped: true,
		configuration: cooldownSignals(signalSource{read: budgetExhaustedSignals, recoversMissing: true, announce: triggersAndRecoveries{}}),
	},
	contract.NotificationEventProviderErrorRate: {
		configuration: evaluated(errorRateDefaults.resolve, func(c measuredConfig) evaluator {
			return signals(signalSource{read: providerErrorSignals(c), recoversMissing: true, announce: triggersAndRecoveries{}}, c.CooldownSeconds)
		}),
	},
	contract.NotificationEventProviderCredentialFailing: {
		configuration: evaluated(credentialDefaults.resolve, func(c measuredConfig) evaluator {
			return signals(signalSource{read: credentialFailureSignals(c), announce: triggersAndRecoveries{}}, c.CooldownSeconds)
		}),
	},
	contract.NotificationEventRouteLatency: {
		projectScoped: true,
		configuration: evaluated(resolveLatency, func(c latencyConfig) evaluator {
			return signals(signalSource{read: routeLatencySignals(c), recoversMissing: true, announce: triggersAndRecoveries{}}, c.CooldownSeconds)
		}),
	},
	contract.NotificationEventModelRetirement: {
		projectScoped: true,
		configuration: evaluated(resolveRetirement, func(c retirementConfig) evaluator {
			return signals(signalSource{read: retirementSignals(c), recoversMissing: true, announce: triggersAndRecoveries{}}, c.CooldownSeconds)
		}),
	},
	contract.NotificationEventProviderCircuitOpen: {
		configuration: cooldownSignals(signalSource{read: circuitSignals, recoversMissing: true, announce: recoveriesAs{recovery: contract.NotificationEventProviderCircuitClosed}}),
	},
	contract.NotificationEventProviderCircuitClosed: {
		configuration: cooldownSignals(signalSource{read: circuitSignals, recoversMissing: true, announce: recoveriesOnly{}}),
	},
	contract.NotificationEventRuntimeInstallFailed: {
		configuration: cooldownSignals(signalSource{read: runtimeInstallSignals, announce: triggersAndRecoveries{}}),
	},
	contract.NotificationEventWorkerStale: {
		configuration: cooldownSignals(signalSource{read: workerStaleSignals, announce: triggersAndRecoveries{}}),
	},
}

var evaluatedEvents = slices.Sorted(func(yield func(string) bool) {
	for event, spec := range events {
		if _, ok := spec.configuration.(evaluable); ok && !yield(string(event)) {
			return
		}
	}
})
