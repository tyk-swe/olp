package notifications

import (
	"maps"
	"slices"

	"github.com/tyk-swe/olp/internal/management/contract"
)

type eventSpec struct {
	subject       subjectShape
	projectScoped bool
	configuration configurationSpec
	evaluate      evaluator
}

var (
	cooldownOnly   = configurationSpec{fields: []string{"cooldown_seconds"}}
	measuredFields = []string{"window_seconds", "minimum_samples", "threshold", "recovery_threshold", "cooldown_seconds"}
)

var events = map[contract.NotificationEvent]eventSpec{
	contract.NotificationEventBudgetThreshold:     {subject: budgetSubject},
	contract.NotificationEventProviderGrantLapsed: {},
	contract.NotificationEventKeyExpiring:         {subject: keySubject, configuration: configurationSpec{fields: []string{"lead_time_seconds"}}},
	contract.NotificationEventReportSpend: {
		projectScoped: true,
		configuration: configurationSpec{fields: []string{"period"}},
		evaluate:      claimSpendReport,
	},
	contract.NotificationEventBudgetExhausted: {
		projectScoped: true,
		configuration: cooldownOnly,
		evaluate:      signals(signalSource{read: budgetExhaustedSignals, recoversMissing: true}),
	},
	contract.NotificationEventProviderErrorRate: {
		configuration: configurationSpec{fields: measuredFields, threshold: &thresholdSpec{trigger: "0.1", recovery: "0.05", check: atMostOne}},
		evaluate:      signals(signalSource{read: providerErrorSignals, recoversMissing: true}),
	},
	contract.NotificationEventProviderCredentialFailing: {
		configuration: configurationSpec{fields: measuredFields, threshold: &thresholdSpec{trigger: "3", recovery: "0", minimumSamples: 3, check: failureCount}},
		evaluate:      signals(signalSource{read: credentialFailureSignals}),
	},
	contract.NotificationEventRouteLatency: {
		projectScoped: true,
		configuration: configurationSpec{fields: append(slices.Clone(measuredFields), "metric"), threshold: &thresholdSpec{trigger: "1000", recovery: "800"}},
		evaluate:      signals(signalSource{read: routeLatencySignals, recoversMissing: true}),
	},
	contract.NotificationEventModelRetirement: {
		projectScoped: true,
		configuration: configurationSpec{fields: []string{"lead_days", "cooldown_seconds"}},
		evaluate:      signals(signalSource{read: retirementSignals, recoversMissing: true}),
	},
	contract.NotificationEventProviderCircuitOpen: {
		configuration: cooldownOnly,
		evaluate:      signals(signalSource{read: circuitSignals, recoversMissing: true, recoveredAs: contract.NotificationEventProviderCircuitClosed}),
	},
	contract.NotificationEventProviderCircuitClosed: {
		configuration: cooldownOnly,
		evaluate:      signals(signalSource{read: circuitSignals, recoversMissing: true, recoveryOnly: true}),
	},
	contract.NotificationEventRuntimeInstallFailed: {
		configuration: cooldownOnly,
		evaluate:      signals(signalSource{read: runtimeInstallSignals}),
	},
	contract.NotificationEventWorkerStale: {
		configuration: cooldownOnly,
		evaluate:      signals(signalSource{read: workerStaleSignals}),
	},
}

var evaluatedEvents = slices.Sorted(func(yield func(string) bool) {
	for event, spec := range maps.All(events) {
		if spec.evaluate != nil && !yield(string(event)) {
			return
		}
	}
})
