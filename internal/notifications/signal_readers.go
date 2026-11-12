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

func querySignals(ctx context.Context, tx pgx.Tx, query string, scan func(pgx.Row) (notificationSignal, error), args ...any) ([]notificationSignal, error) {
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	signals, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (notificationSignal, error) { return scan(row) })
	if err != nil {
		return nil, err
	}
	if len(signals) > signalScopeLimit {
		return nil, errSignalLimit
	}
	return signals, nil
}

func budgetExhaustedSignals(ctx context.Context, tx pgx.Tx, rule signalRule, _ signalEnv) ([]notificationSignal, error) {
	return querySignals(ctx, tx, exhaustedBudgetSignalsSQL, scanBudgetExhausted, signalScopeLimit+1, rule.project)
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
		return querySignals(ctx, tx, providerErrorSignalsSQL, func(row pgx.Row) (notificationSignal, error) {
			return scanProviderError(row, c)
		}, signalScopeLimit+1, c.WindowSeconds)
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
		return querySignals(ctx, tx, credentialFailureSignalsSQL, func(row pgx.Row) (notificationSignal, error) {
			return scanCredentialFailure(row, c)
		}, signalScopeLimit+1, c.WindowSeconds)
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
	return querySignals(ctx, tx, workerStaleSignalsSQL, scanWorkerStale, signalScopeLimit+1)
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
	return querySignals(ctx, tx, runtimeInstallSignalsSQL, scanRuntimeInstall, signalScopeLimit+1)
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
	return querySignals(ctx, tx, providerCircuitSubjectsSQL, func(row pgx.Row) (notificationSignal, error) {
		return scanCircuit(row, circuits)
	}, signalScopeLimit+1)
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
		return querySignals(ctx, tx, routedRetirementModelsSQL, func(row pgx.Row) (notificationSignal, error) {
			return scanRetirement(row, c, env.now, env.reference)
		}, signalScopeLimit+1, rule.project)
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

const providerErrorSignalsSQL = `SELECT p.id::text,p.name,count(a.id),
 CASE WHEN count(a.id)>0 THEN count(a.id) FILTER(WHERE a.error_class IS NOT NULL OR a.status_code>=400)::numeric/count(a.id) END::text
 FROM olp.providers p LEFT JOIN olp.attempts a ON a.provider_id=p.id AND a.completed_at>=now()-make_interval(secs=>$2)
 AND NOT(a.status_code IS NULL AND COALESCE(a.error_class,'') IN ('rate_limit','budget'))
 WHERE p.state='active' GROUP BY p.id,p.name ORDER BY p.id LIMIT $1`

const credentialFailureSignalsSQL = `WITH recent AS (
 SELECT a.routing->>'credential_version_id' AS credential_id,a.provider_id,a.completed_at,
 (a.status_code IN (401,403) OR a.error_class='credential') AS failed,
 (a.status_code BETWEEN 200 AND 399 AND a.error_class IS NULL) AS succeeded
 FROM olp.attempts a WHERE a.completed_at>=now()-make_interval(secs=>$2) AND a.routing->>'credential_version_id' IS NOT NULL
 AND NOT(a.status_code IS NULL AND COALESCE(a.error_class,'') IN ('rate_limit','budget'))
 ), successes AS (
 SELECT credential_id,max(completed_at) FILTER(WHERE succeeded) AS last_success FROM recent GROUP BY credential_id
 ) SELECT r.credential_id,r.provider_id::text,count(*) FILTER(WHERE r.failed AND (s.last_success IS NULL OR r.completed_at>s.last_success)),count(*)
 FROM recent r JOIN successes s USING(credential_id) GROUP BY r.credential_id,r.provider_id ORDER BY r.credential_id LIMIT $1`

const workerStaleSignalsSQL = `WITH workers AS (
 SELECT w.task,''::text AS region,w.last_success_at,w.first_seen_at FROM olp.worker_task_health w
 WHERE NOT EXISTS(SELECT 1 FROM olp.regional_worker_task_health r WHERE r.task=w.task AND r.first_seen_at>w.checked_at)
 UNION ALL SELECT task,region,last_success_at,first_seen_at FROM olp.regional_worker_task_health
 ) SELECT task,region,GREATEST(0,EXTRACT(epoch FROM now()-COALESCE(last_success_at,first_seen_at)))::text
 FROM workers ORDER BY task,region LIMIT $1`

const runtimeInstallSignalsSQL = `SELECT gateway_instance,desired_generation,installed_generation,failed FROM olp.runtime_install_status
 WHERE checked_at>=now()-interval '2 minutes' ORDER BY gateway_instance LIMIT $1`

const providerCircuitSubjectsSQL = `SELECT id::text,name FROM olp.providers WHERE state='active' ORDER BY id LIMIT $1`

const routedRetirementModelsSQL = `SELECT DISTINCT rt.slug,rt.project_id::text,p.id::text,p.kind,COALESCE(pr.configuration->>'vendor_id',''),t->>'provider_model'
 FROM olp.routes rt JOIN olp.route_revisions rr ON rr.id=rt.latest_revision_id
 CROSS JOIN LATERAL jsonb_array_elements(rr.targets) t JOIN olp.providers p ON p.id=(t->>'provider_id')::uuid
 JOIN olp.provider_revisions pr ON pr.id=p.active_revision_id
 WHERE rt.state='active' AND ($2::uuid IS NULL OR rt.project_id=$2) ORDER BY rt.slug,p.id::text,t->>'provider_model' LIMIT $1`

const exhaustedBudgetSignalsSQL = `WITH end_users AS (
 SELECT a.id,COALESCE(a.project_id,k.project_id) AS project_id,a.end_user_digest,
 CASE WHEN a.api_key_id IS NULL THEN p.end_user_policy ELSE k.policy->'end_user_policy' END AS policy,p.limit_templates
 FROM olp.end_user_accounts a LEFT JOIN olp.api_keys k ON k.id=a.api_key_id
 LEFT JOIN olp.projects p ON p.id=COALESCE(a.project_id,k.project_id)
 ), subjects AS (
 SELECT 'api_key'::text AS kind,k.id,k.id AS owner_id,k.project_id,k.name,k.effective_limits AS policy
 FROM olp.api_keys_with_limits k WHERE k.revoked_at IS NULL
 UNION ALL SELECT 'budget_group',g.id,g.id,g.project_id,g.name,g.effective_limits FROM olp.budget_groups_with_limits g
 UNION ALL
 SELECT CASE WHEN a.organization_id IS NOT NULL THEN 'organization' WHEN a.project_id IS NOT NULL THEN 'project' ELSE 'installation' END,
 COALESCE(a.organization_id,a.project_id,a.installation_id),a.id,a.project_id,COALESCE(o.name,p.name,'Installation'),
 COALESCE(o.budget_policy,p.budget_policy,i.budget_policy)
 FROM olp.aggregate_budget_accounts a LEFT JOIN olp.organizations o ON o.id=a.organization_id
 LEFT JOIN olp.projects p ON p.id=a.project_id LEFT JOIN olp.installation i ON i.id=a.installation_id WHERE a.api_key_id IS NULL
 UNION ALL SELECT 'end_user',e.id,e.id,e.project_id,e.end_user_digest,
 olp.effective_limits(COALESCE(e.policy->'overrides'->e.end_user_digest,e.policy->'defaults'),e.limit_templates->(e.policy->>'limit_template'))
 FROM end_users e
 ), current AS (
 SELECT s.*,k.window_kind,b.window_id,COALESCE(w.accrued,0) AS accrued,
 s.policy->>CASE k.window_kind WHEN 'day' THEN 'daily_cost_limit' WHEN 'week' THEN 'weekly_cost_limit' ELSE 'monthly_cost_limit' END AS base_limit,
 (k.window_kind<>'week' OR COALESCE(w.weekly_complete,false)) AS known
 FROM subjects s CROSS JOIN unnest(ARRAY['day','week','month']) AS k(window_kind)
 CROSS JOIN LATERAL olp.budget_window(k.window_kind,now()) b
 LEFT JOIN LATERAL (
 SELECT CASE WHEN s.policy->>'cost_basis'='charge' THEN charged ELSE accrued END,weekly_complete FROM olp.api_key_cost_windows WHERE s.kind='api_key' AND api_key_id=s.owner_id AND window_kind=k.window_kind AND window_id=b.window_id
 UNION ALL SELECT CASE WHEN s.policy->>'cost_basis'='charge' THEN charged ELSE accrued END,weekly_complete FROM olp.budget_group_cost_windows WHERE s.kind='budget_group' AND budget_group_id=s.owner_id AND window_kind=k.window_kind AND window_id=b.window_id
 UNION ALL SELECT CASE WHEN s.policy->>'cost_basis'='charge' THEN charged ELSE accrued END,weekly_complete FROM olp.end_user_cost_windows WHERE s.kind='end_user' AND account_id=s.owner_id AND window_kind=k.window_kind AND window_id=b.window_id
 UNION ALL SELECT CASE WHEN s.policy->>'cost_basis'='charge' THEN charged ELSE accrued END,weekly_complete FROM olp.aggregate_cost_windows WHERE s.kind IN ('organization','project','installation') AND account_id=s.owner_id AND window_kind=k.window_kind AND window_id=b.window_id
 ) w ON true WHERE ($2::uuid IS NULL OR s.project_id=$2)
 ) SELECT c.kind,c.id::text,c.project_id::text,c.name,c.window_kind,c.window_id,c.accrued::text,
 (c.base_limit::numeric+olp.active_budget_increase(c.owner_id,c.window_kind,c.window_id,now()))::text,c.known
 FROM current c WHERE c.base_limit IS NOT NULL AND c.base_limit::numeric>0 ORDER BY c.kind,c.id,c.window_kind LIMIT $1`
