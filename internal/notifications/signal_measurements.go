package notifications

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/tyk-swe/olp/internal/catalog"
	"github.com/tyk-swe/olp/internal/observability/workerhealth"
	"github.com/tyk-swe/olp/internal/usage/reports"
	"github.com/tyk-swe/olp/internal/vendors"
)

const signalScopeLimit = 10000

var errSignalLimit = errors.New("notification signal scope exceeds its bound")
var errSignalUnavailable = errors.New("notification signal source is unavailable")

func querySignals(ctx context.Context, tx pgx.Tx, query string, args []any, scan func(pgx.Row) (notificationSignal, error)) ([]notificationSignal, error) {
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	signals := []notificationSignal{}
	for rows.Next() {
		if len(signals) == signalScopeLimit {
			return nil, errSignalLimit
		}
		signal, err := scan(rows)
		if err != nil {
			return nil, err
		}
		signals = append(signals, signal)
	}
	return signals, rows.Err()
}

func budgetExhaustedSignals(ctx context.Context, tx pgx.Tx, rule signalRule, _ signalEnv) ([]notificationSignal, error) {
	return querySignals(ctx, tx, exhaustedBudgetSignalsSQL, []any{rule.project}, scanBudgetExhausted)
}

func scanBudgetExhausted(row pgx.Row) (notificationSignal, error) {
	var kind, id, name, window, accrued, limit string
	var project *string
	var windowID int64
	var known bool
	if err := row.Scan(&kind, &id, &project, &name, &window, &windowID, &accrued, &limit, &known); err != nil {
		return notificationSignal{}, err
	}
	accruedValue, err := decimal.NewFromString(accrued)
	if err != nil || accruedValue.IsNegative() {
		return notificationSignal{}, errors.New("invalid budget accrued signal")
	}
	limitValue, err := decimal.NewFromString(limit)
	if err != nil || !limitValue.IsPositive() {
		return notificationSignal{}, errors.New("invalid budget limit signal")
	}
	return notificationSignal{
		subject: kind + ":" + id + ":" + window + ":" + strconv.FormatInt(windowID, 10),
		known:   known,
		active:  accruedValue.Cmp(limitValue) >= 0,
		evidence: map[string]any{"subject_kind": kind, "subject_id": id, "subject_name": name, "project_id": project,
			"window_kind": window, "window_id": windowID, "accrued": accrued, "limit": limit},
	}, nil
}

func providerErrorSignals(ctx context.Context, tx pgx.Tx, rule signalRule, _ signalEnv) ([]notificationSignal, error) {
	return querySignals(ctx, tx, providerErrorSignalsSQL, []any{rule.configuration.WindowSeconds}, func(row pgx.Row) (notificationSignal, error) {
		return scanProviderError(row, rule.configuration)
	})
}

func scanProviderError(row pgx.Row, configuration ruleConfiguration) (notificationSignal, error) {
	var provider, name string
	var count int64
	var ratio *string
	if err := row.Scan(&provider, &name, &count, &ratio); err != nil {
		return notificationSignal{}, err
	}
	return notificationSignal{
		subject:     provider,
		known:       count >= int64(configuration.MinimumSamples) && ratio != nil,
		measurement: ratio,
		evidence: map[string]any{"provider_id": provider, "provider_name": name, "samples": count,
			"failure_ratio": ratio, "window_seconds": configuration.WindowSeconds},
	}, nil
}

func credentialFailureSignals(ctx context.Context, tx pgx.Tx, rule signalRule, _ signalEnv) ([]notificationSignal, error) {
	return querySignals(ctx, tx, credentialFailureSignalsSQL, []any{rule.configuration.WindowSeconds}, func(row pgx.Row) (notificationSignal, error) {
		return scanCredentialFailure(row, rule.configuration)
	})
}

func scanCredentialFailure(row pgx.Row, configuration ruleConfiguration) (notificationSignal, error) {
	var credential, provider string
	var failures, samples int64
	if err := row.Scan(&credential, &provider, &failures, &samples); err != nil {
		return notificationSignal{}, err
	}
	count := strconv.FormatInt(failures, 10)
	return notificationSignal{
		subject:     credential,
		known:       samples >= int64(configuration.MinimumSamples),
		measurement: &count,
		evidence: map[string]any{"credential_version_id": credential, "provider_id": provider,
			"authentication_failures": failures, "samples": samples, "window_seconds": configuration.WindowSeconds},
	}, nil
}

func routeLatencySignals(ctx context.Context, tx pgx.Tx, rule signalRule, _ signalEnv) ([]notificationSignal, error) {
	observations, err := reports.ReadRouteLatency(ctx, tx, rule.configuration.WindowSeconds, rule.project, rule.configuration.Metric == "ttft")
	if err != nil {
		return nil, err
	}
	if len(observations) > signalScopeLimit {
		return nil, errSignalLimit
	}
	signals := make([]notificationSignal, 0, len(observations))
	for _, observation := range observations {
		signals = append(signals, latencySignal(rule, observation))
	}
	return signals, nil
}

func latencySignal(rule signalRule, observation reports.RouteLatencyObservation) notificationSignal {
	project := observation.ProjectID
	if rule.project != nil {
		project = rule.project
	}
	return notificationSignal{
		subject:     observation.Route,
		known:       observation.Samples >= int64(rule.configuration.MinimumSamples) && observation.Milliseconds != nil,
		measurement: observation.Milliseconds,
		evidence: map[string]any{"route_slug": observation.Route, "project_id": project, "samples": observation.Samples,
			"metric": rule.configuration.Metric, "p95_ms": observation.Milliseconds, "window_seconds": rule.configuration.WindowSeconds},
	}
}

func workerStaleSignals(ctx context.Context, tx pgx.Tx, _ signalRule, _ signalEnv) ([]notificationSignal, error) {
	return querySignals(ctx, tx, workerStaleSignalsSQL, nil, scanWorkerStale)
}

func scanWorkerStale(row pgx.Row) (notificationSignal, error) {
	var task, region, age string
	if err := row.Scan(&task, &region, &age); err != nil {
		return notificationSignal{}, err
	}
	seconds, err := decimal.NewFromString(age)
	if err != nil || seconds.IsNegative() {
		return notificationSignal{}, errors.New("invalid worker age signal")
	}
	known, recognized := workerhealth.Parse(task)
	staleAfter := int64(0)
	if recognized {
		staleAfter = known.StaleAfter()
	}
	signal := notificationSignal{
		subject:  task,
		known:    recognized,
		active:   seconds.Cmp(decimal.NewFromInt(staleAfter)) > 0,
		evidence: map[string]any{"task": task, "age_seconds": age, "stale_after_seconds": staleAfter},
	}
	if region == "" {
		return signal, nil
	}
	signal.subject = "region:" + region + ":" + task
	signal.evidence["region"] = region
	return signal, nil
}

func runtimeInstallSignals(ctx context.Context, tx pgx.Tx, _ signalRule, _ signalEnv) ([]notificationSignal, error) {
	return querySignals(ctx, tx, runtimeInstallSignalsSQL, nil, scanRuntimeInstall)
}

func scanRuntimeInstall(row pgx.Row) (notificationSignal, error) {
	var instance string
	var desired, installed int64
	var failed bool
	if err := row.Scan(&instance, &desired, &installed, &failed); err != nil {
		return notificationSignal{}, err
	}
	return notificationSignal{
		subject:  instance,
		known:    true,
		active:   failed && desired > installed,
		evidence: map[string]any{"gateway_instance": instance, "desired_generation": desired, "installed_generation": installed},
	}, nil
}

func circuitSignals(ctx context.Context, tx pgx.Tx, _ signalRule, env signalEnv) ([]notificationSignal, error) {
	if env.fleet == nil {
		return nil, errSignalUnavailable
	}
	circuits, err := env.fleet.Circuits(ctx, env.now)
	if err != nil {
		return nil, fmt.Errorf("%w: fleet circuit state", errSignalUnavailable)
	}
	return querySignals(ctx, tx, providerCircuitSubjectsSQL, nil, func(row pgx.Row) (notificationSignal, error) {
		return scanCircuit(row, circuits)
	})
}

func scanCircuit(row pgx.Row, circuits map[string]time.Time) (notificationSignal, error) {
	var provider, name string
	if err := row.Scan(&provider, &name); err != nil {
		return notificationSignal{}, err
	}
	until, open := circuits[provider]
	signal := notificationSignal{
		subject:  provider,
		known:    true,
		active:   open,
		evidence: map[string]any{"provider_id": provider, "provider_name": name, "open": open},
	}
	if open {
		signal.evidence["open_until"] = until.UTC().Format(time.RFC3339Nano)
	}
	return signal, nil
}

func retirementSignals(ctx context.Context, tx pgx.Tx, rule signalRule, env signalEnv) ([]notificationSignal, error) {
	if env.reference == nil {
		return nil, errSignalUnavailable
	}
	return querySignals(ctx, tx, routedRetirementModelsSQL, []any{rule.project}, func(row pgx.Row) (notificationSignal, error) {
		return scanRetirement(row, rule.configuration, env.now, env.reference)
	})
}

func scanRetirement(row pgx.Row, configuration ruleConfiguration, now time.Time, reference *catalog.Signed) (notificationSignal, error) {
	var route, provider, kind, upstream string
	var project, vendor *string
	if err := row.Scan(&route, &project, &provider, &kind, &vendor, &upstream); err != nil {
		return notificationSignal{}, err
	}
	vendorID := vendors.DefaultFor(kind)
	if vendor != nil && *vendor != "" {
		vendorID = *vendor
	}
	model, _, found := reference.Lookup(vendorID, upstream)
	signal := notificationSignal{
		subject:  route + ":" + provider + ":" + upstream,
		known:    found && model.Lifecycle != nil && model.Lifecycle.RetiresAt != nil,
		evidence: map[string]any{"route_slug": route, "project_id": project, "provider_id": provider, "provider_model": upstream},
	}
	if !signal.known {
		return signal, nil
	}
	retirement, err := time.Parse(time.DateOnly, string(*model.Lifecycle.RetiresAt))
	if err != nil {
		return notificationSignal{}, err
	}
	signal.active = !retirement.After(now.AddDate(0, 0, configuration.LeadDays))
	signal.evidence["retires_at"] = model.Lifecycle.RetiresAt
	signal.evidence["replacement"] = model.Lifecycle.Replacement
	return signal, nil
}
