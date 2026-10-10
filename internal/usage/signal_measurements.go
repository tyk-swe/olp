package usage

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/tyk-swe/olp/internal/catalog"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/notifications"
	"github.com/tyk-swe/olp/internal/vendors"
)

var errSignalLimit = errors.New("notification signal scope exceeds its bound")
var errSignalUnavailable = errors.New("notification signal source is unavailable")

func readRuleSignals(ctx context.Context, tx pgx.Tx, rule signalRule, now time.Time, limiter *limits.Limiter, reference *catalog.Signed) ([]notificationSignal, error) {
	var query string
	var args []any
	var circuits map[string]time.Time
	switch rule.event {
	case "budget.exhausted":
		query, args = exhaustedBudgetSignalsSQL, []any{rule.project}
	case "provider.error_rate":
		query, args = providerErrorSignalsSQL, []any{rule.configuration.WindowSeconds}
	case "provider.credential.failing":
		query, args = credentialFailureSignalsSQL, []any{rule.configuration.WindowSeconds}
	case "route.latency":
		query, args = routeLatencySignalsSQL, []any{rule.configuration.WindowSeconds, rule.project, rule.configuration.Metric}
	case "worker.stale":
		query = workerStaleSignalsSQL
	case "runtime.install_failed":
		query = runtimeInstallSignalsSQL
	case "provider.circuit.open", "provider.circuit.closed":
		if limiter == nil {
			return nil, errSignalUnavailable
		}
		var err error
		circuits, err = limiter.Circuits(ctx, now)
		if err != nil {
			return nil, fmt.Errorf("%w: fleet circuit state", errSignalUnavailable)
		}
		query = providerCircuitSubjectsSQL
	case "model.retirement":
		if reference == nil {
			return nil, errSignalUnavailable
		}
		query, args = routedRetirementModelsSQL, []any{rule.project}
	default:
		return nil, fmt.Errorf("unsupported signal event %q", rule.event)
	}
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	signals := []notificationSignal{}
	for rows.Next() {
		if len(signals) == 10000 {
			return nil, errSignalLimit
		}
		signal, err := scanRuleSignal(rows, rule, now, circuits, reference)
		if err != nil {
			return nil, err
		}
		signals = append(signals, signal)
	}
	return signals, rows.Err()
}

func scanRuleSignal(row pgx.Row, rule signalRule, now time.Time, circuits map[string]time.Time, reference *catalog.Signed) (notificationSignal, error) {
	signal := notificationSignal{known: true}
	switch rule.event {
	case "budget.exhausted":
		var kind, id, name, window, accrued, limit string
		var project *string
		var windowID int64
		if err := row.Scan(&kind, &id, &project, &name, &window, &windowID, &accrued, &limit, &signal.known); err != nil {
			return signal, err
		}
		accruedValue, err := decimal.NewFromString(accrued)
		if err != nil || accruedValue.IsNegative() {
			return signal, errors.New("invalid budget accrued signal")
		}
		limitValue, err := decimal.NewFromString(limit)
		if err != nil || !limitValue.IsPositive() {
			return signal, errors.New("invalid budget limit signal")
		}
		signal.subject = kind + ":" + id + ":" + window + ":" + strconv.FormatInt(windowID, 10)
		signal.active = accruedValue.Cmp(limitValue) >= 0
		signal.evidence = map[string]any{"subject_kind": kind, "subject_id": id, "subject_name": name, "project_id": project,
			"window_kind": window, "window_id": windowID, "accrued": accrued, "limit": limit}
	case "provider.error_rate":
		var provider, name string
		var count int64
		var ratio *string
		if err := row.Scan(&provider, &name, &count, &ratio); err != nil {
			return signal, err
		}
		signal.subject = provider
		signal.known = count >= int64(rule.configuration.MinimumSamples) && ratio != nil
		signal.measurement = ratio
		signal.evidence = map[string]any{"provider_id": provider, "provider_name": name, "samples": count,
			"failure_ratio": ratio, "window_seconds": rule.configuration.WindowSeconds}
	case "provider.credential.failing":
		var credential, provider string
		var failures, samples int64
		if err := row.Scan(&credential, &provider, &failures, &samples); err != nil {
			return signal, err
		}
		count := strconv.FormatInt(failures, 10)
		signal.subject = credential
		signal.known = samples >= int64(rule.configuration.MinimumSamples)
		signal.measurement = &count
		signal.evidence = map[string]any{"credential_version_id": credential, "provider_id": provider,
			"authentication_failures": failures, "samples": samples, "window_seconds": rule.configuration.WindowSeconds}
	case "route.latency":
		var route string
		var project *string
		var count int64
		var value *string
		if err := row.Scan(&route, &project, &count, &value); err != nil {
			return signal, err
		}
		if rule.project != nil {
			project = rule.project
		}
		signal.subject = route
		signal.known = count >= int64(rule.configuration.MinimumSamples) && value != nil
		signal.measurement = value
		signal.evidence = map[string]any{"route_slug": route, "project_id": project, "samples": count,
			"metric": rule.configuration.Metric, "p95_ms": value, "window_seconds": rule.configuration.WindowSeconds}
	case "worker.stale":
		var task, age string
		if err := row.Scan(&task, &age); err != nil {
			return signal, err
		}
		seconds, err := decimal.NewFromString(age)
		if err != nil || seconds.IsNegative() {
			return signal, errors.New("invalid worker age signal")
		}
		staleAfter := notifications.WorkerStaleAfter(task)
		signal.subject, signal.known = task, staleAfter > 0
		signal.active = seconds.Cmp(decimal.NewFromInt(staleAfter)) > 0
		signal.evidence = map[string]any{"task": task, "age_seconds": age, "stale_after_seconds": staleAfter}
	case "runtime.install_failed":
		var instance string
		var desired, installed int64
		var failed bool
		if err := row.Scan(&instance, &desired, &installed, &failed); err != nil {
			return signal, err
		}
		signal.subject, signal.active = instance, failed && desired > installed
		signal.evidence = map[string]any{"gateway_instance": instance, "desired_generation": desired, "installed_generation": installed}
	case "provider.circuit.open", "provider.circuit.closed":
		var provider, name string
		if err := row.Scan(&provider, &name); err != nil {
			return signal, err
		}
		until, open := circuits[provider]
		signal.subject, signal.active = provider, open
		signal.evidence = map[string]any{"provider_id": provider, "provider_name": name, "open": open}
		if open {
			signal.evidence["open_until"] = until.UTC().Format(time.RFC3339Nano)
		}
	case "model.retirement":
		var route, provider, kind, upstream string
		var project, vendor *string
		if err := row.Scan(&route, &project, &provider, &kind, &vendor, &upstream); err != nil {
			return signal, err
		}
		vendorID := vendors.DefaultFor(kind)
		if vendor != nil && *vendor != "" {
			vendorID = *vendor
		}
		signal.subject = route + ":" + provider + ":" + upstream
		model, _, found := reference.Lookup(vendorID, upstream)
		signal.known = found && model.Lifecycle != nil && model.Lifecycle.RetiresAt != nil
		signal.evidence = map[string]any{"route_slug": route, "project_id": project, "provider_id": provider, "provider_model": upstream}
		if signal.known {
			retirement, err := time.Parse("2006-01-02", string(*model.Lifecycle.RetiresAt))
			if err != nil {
				return signal, err
			}
			signal.active = !retirement.After(now.AddDate(0, 0, rule.configuration.LeadDays))
			signal.evidence["retires_at"] = model.Lifecycle.RetiresAt
			signal.evidence["replacement"] = model.Lifecycle.Replacement
		}
	}
	return signal, nil
}
