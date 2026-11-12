package notifications

import (
	"cmp"
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
		reading: observation(known, accruedValue.Cmp(limitValue) >= 0),
		evidence: map[string]any{"subject_kind": kind, "subject_id": id, "subject_name": name, "project_id": project,
			"window_kind": window, "window_id": windowID, "accrued": accrued, "limit": limit},
	}, nil
}

func providerErrorSignals(c measuredConfig) signalReader {
	return func(ctx context.Context, tx pgx.Tx, _ signalRule, _ signalEnv) ([]notificationSignal, error) {
		return querySignals(ctx, tx, providerErrorSignalsSQL, []any{c.WindowSeconds}, func(row pgx.Row) (notificationSignal, error) {
			return scanProviderError(row, c)
		})
	}
}

func scanProviderError(row pgx.Row, c measuredConfig) (notificationSignal, error) {
	var provider, name string
	var count int64
	var ratio *string
	if err := row.Scan(&provider, &name, &count, &ratio); err != nil {
		return notificationSignal{}, err
	}
	reading, err := c.measure(count, ratio)
	return notificationSignal{
		subject: provider,
		reading: reading,
		evidence: map[string]any{"provider_id": provider, "provider_name": name, "samples": count,
			"failure_ratio": ratio, "window_seconds": c.WindowSeconds},
	}, err
}

func credentialFailureSignals(c measuredConfig) signalReader {
	return func(ctx context.Context, tx pgx.Tx, _ signalRule, _ signalEnv) ([]notificationSignal, error) {
		return querySignals(ctx, tx, credentialFailureSignalsSQL, []any{c.WindowSeconds}, func(row pgx.Row) (notificationSignal, error) {
			return scanCredentialFailure(row, c)
		})
	}
}

func scanCredentialFailure(row pgx.Row, c measuredConfig) (notificationSignal, error) {
	var credential, provider string
	var failures, samples int64
	if err := row.Scan(&credential, &provider, &failures, &samples); err != nil {
		return notificationSignal{}, err
	}
	reading, err := c.measure(samples, new(strconv.FormatInt(failures, 10)))
	return notificationSignal{
		subject: credential,
		reading: reading,
		evidence: map[string]any{"credential_version_id": credential, "provider_id": provider,
			"authentication_failures": failures, "samples": samples, "window_seconds": c.WindowSeconds},
	}, err
}

func (c measuredConfig) measure(samples int64, value *string) (reading, error) {
	if samples < int64(c.MinimumSamples) || value == nil {
		return unknown{}, nil
	}
	parsed, err := boundedDecimal(*value, 64, -24)
	if err != nil {
		return unknown{}, fmt.Errorf("signal measurement is invalid: %w", err)
	}
	return measured{value: parsed, band: c.band}, nil
}

func routeLatencySignals(c latencyConfig) signalReader {
	return func(ctx context.Context, tx pgx.Tx, rule signalRule, _ signalEnv) ([]notificationSignal, error) {
		observations, err := reports.ReadRouteLatency(ctx, tx, c.WindowSeconds, rule.project, c.Metric == firstTokenLatency)
		if err != nil {
			return nil, err
		}
		if len(observations) > signalScopeLimit {
			return nil, errSignalLimit
		}
		signals := make([]notificationSignal, 0, len(observations))
		for _, observation := range observations {
			signal, err := latencySignal(rule, c, observation)
			if err != nil {
				return nil, err
			}
			signals = append(signals, signal)
		}
		return signals, nil
	}
}

func latencySignal(rule signalRule, c latencyConfig, observation reports.RouteLatencyObservation) (notificationSignal, error) {
	reading, err := c.measure(observation.Samples, observation.Milliseconds)
	return notificationSignal{
		subject: observation.Route,
		reading: reading,
		evidence: map[string]any{"route_slug": observation.Route, "project_id": cmp.Or(rule.project, observation.ProjectID), "samples": observation.Samples,
			"metric": c.Metric, "p95_ms": observation.Milliseconds, "window_seconds": c.WindowSeconds},
	}, err
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
	staleAfter := known.StaleAfter()
	reading := observation(recognized, seconds.Cmp(decimal.NewFromInt(staleAfter)) > 0)
	if region == "" {
		return notificationSignal{subject: task, reading: reading,
			evidence: map[string]any{"task": task, "age_seconds": age, "stale_after_seconds": staleAfter}}, nil
	}
	return notificationSignal{subject: "region:" + region + ":" + task, reading: reading,
		evidence: map[string]any{"task": task, "region": region, "age_seconds": age, "stale_after_seconds": staleAfter}}, nil
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
		reading:  observed{active: failed && desired > installed},
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
	if !open {
		return notificationSignal{subject: provider, reading: observed{active: false},
			evidence: map[string]any{"provider_id": provider, "provider_name": name, "open": false}}, nil
	}
	return notificationSignal{subject: provider, reading: observed{active: true},
		evidence: map[string]any{"provider_id": provider, "provider_name": name, "open": true, "open_until": until.UTC().Format(time.RFC3339Nano)}}, nil
}

func retirementSignals(c retirementConfig) signalReader {
	return func(ctx context.Context, tx pgx.Tx, rule signalRule, env signalEnv) ([]notificationSignal, error) {
		if env.reference == nil {
			return nil, errSignalUnavailable
		}
		return querySignals(ctx, tx, routedRetirementModelsSQL, []any{rule.project}, func(row pgx.Row) (notificationSignal, error) {
			return scanRetirement(row, c, env.now, env.reference)
		})
	}
}

func scanRetirement(row pgx.Row, c retirementConfig, now time.Time, reference *catalog.Signed) (notificationSignal, error) {
	var route, provider, kind, upstream string
	var project *string
	var vendor string
	if err := row.Scan(&route, &project, &provider, &kind, &vendor, &upstream); err != nil {
		return notificationSignal{}, err
	}
	model, _, found := reference.Lookup(cmp.Or(vendor, vendors.DefaultFor(kind)), upstream)
	subject := route + ":" + provider + ":" + upstream
	if !found || model.Lifecycle == nil || model.Lifecycle.RetiresAt == nil {
		return notificationSignal{subject: subject, reading: unknown{}}, nil
	}
	retirement, err := time.Parse(time.DateOnly, string(*model.Lifecycle.RetiresAt))
	if err != nil {
		return notificationSignal{}, err
	}
	return notificationSignal{
		subject: subject,
		reading: observed{active: !retirement.After(now.AddDate(0, 0, c.LeadDays))},
		evidence: map[string]any{"route_slug": route, "project_id": project, "provider_id": provider, "provider_model": upstream,
			"retires_at": model.Lifecycle.RetiresAt, "replacement": model.Lifecycle.Replacement},
	}, nil
}
