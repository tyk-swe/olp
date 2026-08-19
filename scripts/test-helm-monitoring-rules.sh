#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=scripts/lib/repository-validation.sh
source "$script_dir/lib/repository-validation.sh"

chart=${1:-deploy/helm}
for required_executable in helm jq docker awk mktemp rm chmod grep sed; do
  validation_require_executable "$required_executable"
done
validation_require_directory "$chart"

work=$(mktemp -d)
chmod 777 "$work"
trap 'rm -rf "$work"' EXIT

run_promtool() {
  chmod -R a+rX "$work"
  if command -v promtool >/dev/null 2>&1; then
    promtool "$@"
  else
    docker run --rm --user "$(id -u):$(id -g)" -v "$work:$work" -w "$work" \
      --entrypoint promtool prom/prometheus:latest "$@"
  fi
}

extract_rules() {
  local manifest=$1
  local output=$2
  awk -v RS='---' '
    /kind: PrometheusRule/ {
      match($0, /  groups:[\s\S]*/)
      if (RSTART > 0) {
        groups_block = substr($0, RSTART)
        gsub(/\n  /, "\n", groups_block)
        sub(/^  /, "", groups_block)
        print groups_block
      }
    }
  ' "$manifest" > "$output"
}

# --- Case 0: Conditional Omission Checks ---
helm template olp "$chart" \
  --set monitoring.enabled=true \
  --set monitoring.rules.enabled=false > "$work/no-rules.yaml"
if grep -Fq 'kind: PrometheusRule' "$work/no-rules.yaml"; then
  echo "PrometheusRule rendered when monitoring.rules.enabled=false" >&2
  exit 1
fi

helm template olp "$chart" \
  --set monitoring.enabled=true \
  --set gateway.enabled=false \
  --set control.enabled=false > "$work/no-services.yaml"
if grep -Fq 'kind: PrometheusRule' "$work/no-services.yaml"; then
  echo "PrometheusRule rendered when all components are disabled" >&2
  exit 1
fi

helm template olp "$chart" \
  --set monitoring.enabled=true \
  --set gateway.service.enabled=false \
  --set control.service.enabled=false > "$work/no-svc-enabled.yaml"
if grep -Fq 'kind: PrometheusRule' "$work/no-svc-enabled.yaml"; then
  echo "PrometheusRule rendered when all services are disabled" >&2
  exit 1
fi

# --- Case 1: Gateway + Control (Default Topology) ---
helm template test "$chart" \
  --set monitoring.enabled=true > "$work/manifest-1.yaml"
extract_rules "$work/manifest-1.yaml" "$work/rules-1.yaml"
run_promtool check rules "$work/rules-1.yaml" >/dev/null

cat << 'TESTEOF' > "$work/test-1.yaml"
rule_files:
  - rules-1.yaml
evaluation_interval: 1m
tests:
  - interval: 1m
    input_series:
      - series: 'olp_ready{namespace="default",service="test-openllmproxy-gateway-observability"}'
        values: '1+0x15'
      - series: 'up{namespace="default",service="test-openllmproxy-gateway-observability"}'
        values: '1+0x15'
      - series: 'olp_ready{namespace="default",service="test-openllmproxy-control-observability"}'
        values: '1+0x15'
      - series: 'up{namespace="default",service="test-openllmproxy-control-observability"}'
        values: '1+0x15'
      - series: 'olp_request_metadata_events_dropped_total{namespace="default",service="test-openllmproxy-gateway-observability"}'
        values: '0+0x15'
      - series: 'olp_request_metadata_events_abandoned_total{namespace="default",service="test-openllmproxy-gateway-observability"}'
        values: '0+0x15'
      - series: 'olp_request_metadata_persistence_available{namespace="default",service="test-openllmproxy-gateway-observability"}'
        values: '1+0x15'
      - series: 'olp_request_metadata_events_pending{namespace="default",service="test-openllmproxy-gateway-observability"}'
        values: '0+0x15'
      - series: 'olp_request_metadata_consumer_pending_events{namespace="default",service="test-openllmproxy-gateway-observability"}'
        values: '0+0x15'
      - series: 'olp_request_metadata_consumer_lag_events{namespace="default",service="test-openllmproxy-gateway-observability"}'
        values: '0+0x15'
      - series: 'olp_async_plane_current{namespace="default",service="test-openllmproxy-gateway-observability"}'
        values: '1+0x15'
      - series: 'olp_async_worker_observability_available{namespace="default",service="test-openllmproxy-gateway-observability"}'
        values: '1+0x15'
      - series: 'olp_runtime_outbox_pending_rows{namespace="default",service="test-openllmproxy-gateway-observability"}'
        values: '0+0x15'
      - series: 'olp_runtime_outbox_failed_takeovers_total{namespace="default",service="test-openllmproxy-gateway-observability"}'
        values: '0+0x15'
      - series: 'olp_distributed_limiter_available{namespace="default",service="test-openllmproxy-gateway-observability"}'
        values: '1+0x15'
      - series: 'olp_runtime_generation{namespace="default",service="test-openllmproxy-gateway-observability"}'
        values: '1+0x15'
    alert_rule_test:
      - eval_time: 15m
        alertname: OLPReadinessAbsent
        exp_alerts: []
      - eval_time: 15m
        alertname: OLPRequestMetadataEventsDropped
        exp_alerts: []
      - eval_time: 15m
        alertname: OLPRequestMetadataEventsAbandoned
        exp_alerts: []
      - eval_time: 15m
        alertname: OLPRequestMetadataPersistenceUnavailable
        exp_alerts: []
      - eval_time: 15m
        alertname: OLPRequestMetadataBacklogHigh
        exp_alerts: []
      - eval_time: 15m
        alertname: OLPRequestMetadataConsumerBacklogHigh
        exp_alerts: []
      - eval_time: 15m
        alertname: OLPAsyncPlaneStale
        exp_alerts: []
      - eval_time: 15m
        alertname: OLPAsyncWorkerObservabilityUnavailable
        exp_alerts: []
      - eval_time: 15m
        alertname: OLPRuntimeOutboxBacklogHigh
        exp_alerts: []
      - eval_time: 15m
        alertname: OLPRuntimeOutboxTakeoverBlocked
        exp_alerts: []
      - eval_time: 15m
        alertname: OLPDistributedLimiterUnavailable
        exp_alerts: []
      - eval_time: 15m
        alertname: OLPRuntimeGenerationMissing
        exp_alerts: []

  # Gateway target unready
  - interval: 1m
    input_series:
      - series: 'olp_ready{namespace="default",service="test-openllmproxy-gateway-observability"}'
        values: '0+0x5'
      - series: 'up{namespace="default",service="test-openllmproxy-gateway-observability"}'
        values: '1+0x5'
      - series: 'olp_ready{namespace="default",service="test-openllmproxy-control-observability"}'
        values: '1+0x5'
      - series: 'up{namespace="default",service="test-openllmproxy-control-observability"}'
        values: '1+0x5'
    alert_rule_test:
      - eval_time: 5m
        alertname: OLPReadinessAbsent
        exp_alerts:
          - exp_labels:
              severity: critical
              alertname: OLPReadinessAbsent
              namespace: default
              service: test-openllmproxy-gateway-observability
            exp_annotations:
              summary: OpenLLMProxy readiness is absent
              description: A gateway or control target has been unready or unavailable for five minutes.

  # Control target absent
  - interval: 1m
    input_series:
      - series: 'olp_ready{namespace="default",service="test-openllmproxy-gateway-observability"}'
        values: '1+0x5'
      - series: 'up{namespace="default",service="test-openllmproxy-gateway-observability"}'
        values: '1+0x5'
    alert_rule_test:
      - eval_time: 5m
        alertname: OLPReadinessAbsent
        exp_alerts:
          - exp_labels:
              severity: critical
              alertname: OLPReadinessAbsent
              namespace: default
              service: test-openllmproxy-control-observability
            exp_annotations:
              summary: OpenLLMProxy readiness is absent
              description: A gateway or control target has been unready or unavailable for five minutes.

  # Dropped events counter
  - interval: 1m
    input_series:
      - series: 'olp_request_metadata_events_dropped_total{namespace="default",service="test-openllmproxy-gateway-observability"}'
        values: '0 1 2 3 4 5'
    alert_rule_test:
      - eval_time: 5m
        alertname: OLPRequestMetadataEventsDropped
        exp_alerts:
          - exp_labels:
              severity: critical
              alertname: OLPRequestMetadataEventsDropped
              namespace: default
              service: test-openllmproxy-gateway-observability
            exp_annotations:
              summary: OpenLLMProxy dropped metadata events
              description: Usage and cost completeness is degraded; preserve stream state and reconcile the affected interval.
TESTEOF
run_promtool test rules "$work/test-1.yaml" >/dev/null

# --- Case 2: Gateway Only ---
helm template test "$chart" \
  --set monitoring.enabled=true \
  --set control.enabled=false > "$work/manifest-2.yaml"
extract_rules "$work/manifest-2.yaml" "$work/rules-2.yaml"
run_promtool check rules "$work/rules-2.yaml" >/dev/null

cat << 'TESTEOF' > "$work/test-2.yaml"
rule_files:
  - rules-2.yaml
evaluation_interval: 1m
tests:
  - interval: 1m
    input_series:
      - series: 'olp_ready{namespace="default",service="test-openllmproxy-gateway-observability"}'
        values: '1+0x10'
      - series: 'up{namespace="default",service="test-openllmproxy-gateway-observability"}'
        values: '1+0x10'
      - series: 'olp_request_metadata_events_dropped_total{namespace="default",service="test-openllmproxy-gateway-observability"}'
        values: '0+0x10'
      - series: 'olp_request_metadata_events_abandoned_total{namespace="default",service="test-openllmproxy-gateway-observability"}'
        values: '0+0x10'
      - series: 'olp_request_metadata_persistence_available{namespace="default",service="test-openllmproxy-gateway-observability"}'
        values: '1+0x10'
      - series: 'olp_request_metadata_events_pending{namespace="default",service="test-openllmproxy-gateway-observability"}'
        values: '0+0x10'
      - series: 'olp_request_metadata_consumer_pending_events{namespace="default",service="test-openllmproxy-gateway-observability"}'
        values: '0+0x10'
      - series: 'olp_request_metadata_consumer_lag_events{namespace="default",service="test-openllmproxy-gateway-observability"}'
        values: '0+0x10'
      - series: 'olp_async_plane_current{namespace="default",service="test-openllmproxy-gateway-observability"}'
        values: '1+0x10'
      - series: 'olp_async_worker_observability_available{namespace="default",service="test-openllmproxy-gateway-observability"}'
        values: '1+0x10'
      - series: 'olp_runtime_outbox_pending_rows{namespace="default",service="test-openllmproxy-gateway-observability"}'
        values: '0+0x10'
      - series: 'olp_runtime_outbox_failed_takeovers_total{namespace="default",service="test-openllmproxy-gateway-observability"}'
        values: '0+0x10'
      - series: 'olp_distributed_limiter_available{namespace="default",service="test-openllmproxy-gateway-observability"}'
        values: '1+0x10'
      - series: 'olp_runtime_generation{namespace="default",service="test-openllmproxy-gateway-observability"}'
        values: '1+0x10'
    alert_rule_test:
      - eval_time: 10m
        alertname: OLPReadinessAbsent
        exp_alerts: []
      - eval_time: 10m
        alertname: OLPAsyncPlaneStale
        exp_alerts: []
TESTEOF
run_promtool test rules "$work/test-2.yaml" >/dev/null

# --- Case 3: Control Only ---
helm template test "$chart" \
  --set monitoring.enabled=true \
  --set gateway.enabled=false > "$work/manifest-3.yaml"
extract_rules "$work/manifest-3.yaml" "$work/rules-3.yaml"
run_promtool check rules "$work/rules-3.yaml" >/dev/null

cat << 'TESTEOF' > "$work/test-3.yaml"
rule_files:
  - rules-3.yaml
evaluation_interval: 1m
tests:
  - interval: 1m
    input_series:
      - series: 'olp_ready{namespace="default",service="test-openllmproxy-control-observability"}'
        values: '1+0x10'
      - series: 'up{namespace="default",service="test-openllmproxy-control-observability"}'
        values: '1+0x10'
      - series: 'olp_request_metadata_consumer_pending_events{namespace="default",service="test-openllmproxy-control-observability"}'
        values: '0+0x10'
      - series: 'olp_request_metadata_consumer_lag_events{namespace="default",service="test-openllmproxy-control-observability"}'
        values: '0+0x10'
      - series: 'olp_async_plane_current{namespace="default",service="test-openllmproxy-control-observability"}'
        values: '1+0x10'
      - series: 'olp_async_worker_observability_available{namespace="default",service="test-openllmproxy-control-observability"}'
        values: '1+0x10'
      - series: 'olp_runtime_outbox_pending_rows{namespace="default",service="test-openllmproxy-control-observability"}'
        values: '0+0x10'
      - series: 'olp_runtime_outbox_failed_takeovers_total{namespace="default",service="test-openllmproxy-control-observability"}'
        values: '0+0x10'
      - series: 'olp_distributed_limiter_available{namespace="default",service="test-openllmproxy-control-observability"}'
        values: '1+0x10'
    alert_rule_test:
      - eval_time: 10m
        alertname: OLPReadinessAbsent
        exp_alerts: []
      - eval_time: 10m
        alertname: OLPAsyncPlaneStale
        exp_alerts: []
      - eval_time: 10m
        alertname: OLPDistributedLimiterUnavailable
        exp_alerts: []
TESTEOF
run_promtool test rules "$work/test-3.yaml" >/dev/null

# --- Case 4: Gateway Enabled with Service Disabled ---
helm template test "$chart" \
  --set monitoring.enabled=true \
  --set gateway.enabled=true \
  --set gateway.service.enabled=false \
  --set control.enabled=true \
  --set control.service.enabled=true > "$work/manifest-4.yaml"
extract_rules "$work/manifest-4.yaml" "$work/rules-4.yaml"
run_promtool check rules "$work/rules-4.yaml" >/dev/null

cat << 'TESTEOF' > "$work/test-4.yaml"
rule_files:
  - rules-4.yaml
evaluation_interval: 1m
tests:
  - interval: 1m
    input_series:
      - series: 'olp_ready{namespace="default",service="test-openllmproxy-control-observability"}'
        values: '1+0x10'
      - series: 'up{namespace="default",service="test-openllmproxy-control-observability"}'
        values: '1+0x10'
      - series: 'olp_async_plane_current{namespace="default",service="test-openllmproxy-control-observability"}'
        values: '1+0x10'
      - series: 'olp_async_worker_observability_available{namespace="default",service="test-openllmproxy-control-observability"}'
        values: '1+0x10'
      - series: 'olp_distributed_limiter_available{namespace="default",service="test-openllmproxy-control-observability"}'
        values: '1+0x10'
    alert_rule_test:
      - eval_time: 10m
        alertname: OLPReadinessAbsent
        exp_alerts: []
TESTEOF
run_promtool test rules "$work/test-4.yaml" >/dev/null

# --- Case 5: Control Enabled with Service Disabled ---
helm template test "$chart" \
  --set monitoring.enabled=true \
  --set gateway.enabled=true \
  --set gateway.service.enabled=true \
  --set control.enabled=true \
  --set control.service.enabled=false > "$work/manifest-5.yaml"
extract_rules "$work/manifest-5.yaml" "$work/rules-5.yaml"
run_promtool check rules "$work/rules-5.yaml" >/dev/null

cat << 'TESTEOF' > "$work/test-5.yaml"
rule_files:
  - rules-5.yaml
evaluation_interval: 1m
tests:
  - interval: 1m
    input_series:
      - series: 'olp_ready{namespace="default",service="test-openllmproxy-gateway-observability"}'
        values: '1+0x10'
      - series: 'up{namespace="default",service="test-openllmproxy-gateway-observability"}'
        values: '1+0x10'
      - series: 'olp_request_metadata_persistence_available{namespace="default",service="test-openllmproxy-gateway-observability"}'
        values: '1+0x10'
      - series: 'olp_async_plane_current{namespace="default",service="test-openllmproxy-gateway-observability"}'
        values: '1+0x10'
      - series: 'olp_async_worker_observability_available{namespace="default",service="test-openllmproxy-gateway-observability"}'
        values: '1+0x10'
      - series: 'olp_distributed_limiter_available{namespace="default",service="test-openllmproxy-gateway-observability"}'
        values: '1+0x10'
      - series: 'olp_runtime_generation{namespace="default",service="test-openllmproxy-gateway-observability"}'
        values: '1+0x10'
    alert_rule_test:
      - eval_time: 10m
        alertname: OLPReadinessAbsent
        exp_alerts: []
TESTEOF
run_promtool test rules "$work/test-5.yaml" >/dev/null

# --- Case 6: Workers Disabled ---
helm template test "$chart" \
  --set monitoring.enabled=true \
  --set worker.enabled=false > "$work/manifest-6.yaml"
extract_rules "$work/manifest-6.yaml" "$work/rules-6.yaml"
run_promtool check rules "$work/rules-6.yaml" >/dev/null

cat << 'TESTEOF' > "$work/test-6.yaml"
rule_files:
  - rules-6.yaml
evaluation_interval: 1m
tests:
  - interval: 1m
    input_series:
      - series: 'olp_ready{namespace="default",service="test-openllmproxy-gateway-observability"}'
        values: '1+0x10'
      - series: 'up{namespace="default",service="test-openllmproxy-gateway-observability"}'
        values: '1+0x10'
      - series: 'olp_ready{namespace="default",service="test-openllmproxy-control-observability"}'
        values: '1+0x10'
      - series: 'up{namespace="default",service="test-openllmproxy-control-observability"}'
        values: '1+0x10'
      - series: 'olp_request_metadata_events_dropped_total{namespace="default",service="test-openllmproxy-gateway-observability"}'
        values: '0+0x10'
      - series: 'olp_request_metadata_events_abandoned_total{namespace="default",service="test-openllmproxy-gateway-observability"}'
        values: '0+0x10'
      - series: 'olp_request_metadata_persistence_available{namespace="default",service="test-openllmproxy-gateway-observability"}'
        values: '1+0x10'
      - series: 'olp_request_metadata_events_pending{namespace="default",service="test-openllmproxy-gateway-observability"}'
        values: '0+0x10'
      - series: 'olp_distributed_limiter_available{namespace="default",service="test-openllmproxy-gateway-observability"}'
        values: '1+0x10'
      - series: 'olp_runtime_generation{namespace="default",service="test-openllmproxy-gateway-observability"}'
        values: '1+0x10'
    alert_rule_test:
      - eval_time: 10m
        alertname: OLPReadinessAbsent
        exp_alerts: []
      - eval_time: 10m
        alertname: OLPDistributedLimiterUnavailable
        exp_alerts: []
      - eval_time: 10m
        alertname: OLPRuntimeGenerationMissing
        exp_alerts: []
TESTEOF
run_promtool test rules "$work/test-6.yaml" >/dev/null

# --- Case 7: Zero Worker Replicas ---
helm template test "$chart" \
  --set monitoring.enabled=true \
  --set worker.enabled=true \
  --set worker.replicas=0 > "$work/manifest-7.yaml"
extract_rules "$work/manifest-7.yaml" "$work/rules-7.yaml"
run_promtool check rules "$work/rules-7.yaml" >/dev/null

cat << 'TESTEOF' > "$work/test-7.yaml"
rule_files:
  - rules-7.yaml
evaluation_interval: 1m
tests:
  - interval: 1m
    input_series:
      - series: 'olp_ready{namespace="default",service="test-openllmproxy-gateway-observability"}'
        values: '1+0x10'
      - series: 'up{namespace="default",service="test-openllmproxy-gateway-observability"}'
        values: '1+0x10'
      - series: 'olp_ready{namespace="default",service="test-openllmproxy-control-observability"}'
        values: '1+0x10'
      - series: 'up{namespace="default",service="test-openllmproxy-control-observability"}'
        values: '1+0x10'
      - series: 'olp_request_metadata_events_dropped_total{namespace="default",service="test-openllmproxy-gateway-observability"}'
        values: '0+0x10'
      - series: 'olp_request_metadata_events_abandoned_total{namespace="default",service="test-openllmproxy-gateway-observability"}'
        values: '0+0x10'
      - series: 'olp_request_metadata_persistence_available{namespace="default",service="test-openllmproxy-gateway-observability"}'
        values: '1+0x10'
      - series: 'olp_request_metadata_events_pending{namespace="default",service="test-openllmproxy-gateway-observability"}'
        values: '0+0x10'
      - series: 'olp_distributed_limiter_available{namespace="default",service="test-openllmproxy-gateway-observability"}'
        values: '1+0x10'
      - series: 'olp_runtime_generation{namespace="default",service="test-openllmproxy-gateway-observability"}'
        values: '1+0x10'
    alert_rule_test:
      - eval_time: 10m
        alertname: OLPReadinessAbsent
        exp_alerts: []
      - eval_time: 10m
        alertname: OLPDistributedLimiterUnavailable
        exp_alerts: []
      - eval_time: 10m
        alertname: OLPRuntimeGenerationMissing
        exp_alerts: []
TESTEOF
run_promtool test rules "$work/test-7.yaml" >/dev/null

# --- Case 8: Custom Namespace ---
helm template test "$chart" --namespace custom-ns \
  --set monitoring.enabled=true > "$work/manifest-8.yaml"
extract_rules "$work/manifest-8.yaml" "$work/rules-8.yaml"
run_promtool check rules "$work/rules-8.yaml" >/dev/null

cat << 'TESTEOF' > "$work/test-8.yaml"
rule_files:
  - rules-8.yaml
evaluation_interval: 1m
tests:
  # Unready series in default namespace does NOT fire alert in custom-ns
  - interval: 1m
    input_series:
      - series: 'olp_ready{namespace="default",service="test-openllmproxy-gateway-observability"}'
        values: '0+0x10'
      - series: 'olp_ready{namespace="custom-ns",service="test-openllmproxy-gateway-observability"}'
        values: '1+0x10'
      - series: 'up{namespace="custom-ns",service="test-openllmproxy-gateway-observability"}'
        values: '1+0x10'
      - series: 'olp_ready{namespace="custom-ns",service="test-openllmproxy-control-observability"}'
        values: '1+0x10'
      - series: 'up{namespace="custom-ns",service="test-openllmproxy-control-observability"}'
        values: '1+0x10'
      - series: 'olp_request_metadata_events_dropped_total{namespace="custom-ns",service="test-openllmproxy-gateway-observability"}'
        values: '0+0x10'
      - series: 'olp_request_metadata_events_abandoned_total{namespace="custom-ns",service="test-openllmproxy-gateway-observability"}'
        values: '0+0x10'
      - series: 'olp_request_metadata_persistence_available{namespace="custom-ns",service="test-openllmproxy-gateway-observability"}'
        values: '1+0x10'
      - series: 'olp_request_metadata_events_pending{namespace="custom-ns",service="test-openllmproxy-gateway-observability"}'
        values: '0+0x10'
      - series: 'olp_request_metadata_consumer_pending_events{namespace="custom-ns",service="test-openllmproxy-gateway-observability"}'
        values: '0+0x10'
      - series: 'olp_request_metadata_consumer_lag_events{namespace="custom-ns",service="test-openllmproxy-gateway-observability"}'
        values: '0+0x10'
      - series: 'olp_async_plane_current{namespace="custom-ns",service="test-openllmproxy-gateway-observability"}'
        values: '1+0x10'
      - series: 'olp_async_worker_observability_available{namespace="custom-ns",service="test-openllmproxy-gateway-observability"}'
        values: '1+0x10'
      - series: 'olp_runtime_outbox_pending_rows{namespace="custom-ns",service="test-openllmproxy-gateway-observability"}'
        values: '0+0x10'
      - series: 'olp_runtime_outbox_failed_takeovers_total{namespace="custom-ns",service="test-openllmproxy-gateway-observability"}'
        values: '0+0x10'
      - series: 'olp_distributed_limiter_available{namespace="custom-ns",service="test-openllmproxy-gateway-observability"}'
        values: '1+0x10'
      - series: 'olp_runtime_generation{namespace="custom-ns",service="test-openllmproxy-gateway-observability"}'
        values: '1+0x10'
    alert_rule_test:
      - eval_time: 10m
        alertname: OLPReadinessAbsent
        exp_alerts: []
TESTEOF
run_promtool test rules "$work/test-8.yaml" >/dev/null

# --- Case 9: Two Differently Named Releases in One Namespace ---
helm template release-b "$chart" --namespace olp \
  --set monitoring.enabled=true > "$work/manifest-9b.yaml"
extract_rules "$work/manifest-9b.yaml" "$work/rules-9b.yaml"
run_promtool check rules "$work/rules-9b.yaml" >/dev/null

cat << 'TESTEOF' > "$work/test-9.yaml"
rule_files:
  - rules-9b.yaml
evaluation_interval: 1m
tests:
  # Subcase A: Release A is failing, Release B is healthy -> zero alerts for Release B
  - interval: 1m
    input_series:
      - series: 'olp_ready{namespace="olp",service="release-a-openllmproxy-gateway-observability"}'
        values: '0+0x10'
      - series: 'up{namespace="olp",service="release-a-openllmproxy-gateway-observability"}'
        values: '0+0x10'
      - series: 'olp_request_metadata_events_dropped_total{namespace="olp",service="release-a-openllmproxy-gateway-observability"}'
        values: '0 10 20 30 40 50 60 70 80 90 100'
      - series: 'olp_async_plane_current{namespace="olp",service="release-a-openllmproxy-gateway-observability"}'
        values: '0+0x10'
      - series: 'olp_runtime_generation{namespace="olp",service="release-a-openllmproxy-gateway-observability"}'
        values: '0+0x10'
      - series: 'olp_ready{namespace="olp",service="release-b-openllmproxy-gateway-observability"}'
        values: '1+0x10'
      - series: 'up{namespace="olp",service="release-b-openllmproxy-gateway-observability"}'
        values: '1+0x10'
      - series: 'olp_ready{namespace="olp",service="release-b-openllmproxy-control-observability"}'
        values: '1+0x10'
      - series: 'up{namespace="olp",service="release-b-openllmproxy-control-observability"}'
        values: '1+0x10'
      - series: 'olp_request_metadata_events_dropped_total{namespace="olp",service="release-b-openllmproxy-gateway-observability"}'
        values: '0+0x10'
      - series: 'olp_request_metadata_events_abandoned_total{namespace="olp",service="release-b-openllmproxy-gateway-observability"}'
        values: '0+0x10'
      - series: 'olp_request_metadata_persistence_available{namespace="olp",service="release-b-openllmproxy-gateway-observability"}'
        values: '1+0x10'
      - series: 'olp_request_metadata_events_pending{namespace="olp",service="release-b-openllmproxy-gateway-observability"}'
        values: '0+0x10'
      - series: 'olp_request_metadata_consumer_pending_events{namespace="olp",service="release-b-openllmproxy-gateway-observability"}'
        values: '0+0x10'
      - series: 'olp_request_metadata_consumer_lag_events{namespace="olp",service="release-b-openllmproxy-gateway-observability"}'
        values: '0+0x10'
      - series: 'olp_async_plane_current{namespace="olp",service="release-b-openllmproxy-gateway-observability"}'
        values: '1+0x10'
      - series: 'olp_async_worker_observability_available{namespace="olp",service="release-b-openllmproxy-gateway-observability"}'
        values: '1+0x10'
      - series: 'olp_runtime_outbox_pending_rows{namespace="olp",service="release-b-openllmproxy-gateway-observability"}'
        values: '0+0x10'
      - series: 'olp_runtime_outbox_failed_takeovers_total{namespace="olp",service="release-b-openllmproxy-gateway-observability"}'
        values: '0+0x10'
      - series: 'olp_distributed_limiter_available{namespace="olp",service="release-b-openllmproxy-gateway-observability"}'
        values: '1+0x10'
      - series: 'olp_runtime_generation{namespace="olp",service="release-b-openllmproxy-gateway-observability"}'
        values: '1+0x10'
    alert_rule_test:
      - eval_time: 10m
        alertname: OLPReadinessAbsent
        exp_alerts: []
      - eval_time: 10m
        alertname: OLPRequestMetadataEventsDropped
        exp_alerts: []
      - eval_time: 10m
        alertname: OLPAsyncPlaneStale
        exp_alerts: []
      - eval_time: 10m
        alertname: OLPRuntimeGenerationMissing
        exp_alerts: []

  # Subcase B: Release A is healthy, Release B is failing -> Release A cannot mask Release B
  - interval: 1m
    input_series:
      - series: 'olp_async_plane_current{namespace="olp",service="release-a-openllmproxy-gateway-observability"}'
        values: '1+0x10'
      - series: 'olp_async_plane_current{namespace="olp",service="release-b-openllmproxy-gateway-observability"}'
        values: '0+0x10'
    alert_rule_test:
      - eval_time: 5m
        alertname: OLPAsyncPlaneStale
        exp_alerts:
          - exp_labels:
              severity: critical
              alertname: OLPAsyncPlaneStale
            exp_annotations:
              summary: OpenLLMProxy asynchronous worker plane is stale
              description: No replica has advanced every worker responsibility within its documented heartbeat window.
TESTEOF
run_promtool test rules "$work/test-9.yaml" >/dev/null

# --- Case 10: Two Releases Visible to One Prometheus Instance ---
helm template release-a "$chart" --namespace ns-a \
  --set monitoring.enabled=true > "$work/manifest-10a.yaml"
extract_rules "$work/manifest-10a.yaml" "$work/rules-10a.yaml"
sed -i 's/name: openllmproxy.rules/name: release_a.rules/' "$work/rules-10a.yaml"

helm template release-b "$chart" --namespace ns-b \
  --set monitoring.enabled=true > "$work/manifest-10b.yaml"
extract_rules "$work/manifest-10b.yaml" "$work/rules-10b.yaml"
sed -i 's/name: openllmproxy.rules/name: release_b.rules/' "$work/rules-10b.yaml"

run_promtool check rules "$work/rules-10a.yaml" "$work/rules-10b.yaml" >/dev/null

cat << 'TESTEOF' > "$work/test-10.yaml"
rule_files:
  - rules-10a.yaml
  - rules-10b.yaml
evaluation_interval: 1m
tests:
  - interval: 1m
    input_series:
      - series: 'olp_ready{namespace="ns-a",service="release-a-openllmproxy-gateway-observability"}'
        values: '0+0x10'
      - series: 'up{namespace="ns-a",service="release-a-openllmproxy-gateway-observability"}'
        values: '1+0x10'
      - series: 'olp_ready{namespace="ns-a",service="release-a-openllmproxy-control-observability"}'
        values: '1+0x10'
      - series: 'up{namespace="ns-a",service="release-a-openllmproxy-control-observability"}'
        values: '1+0x10'
      - series: 'olp_ready{namespace="ns-b",service="release-b-openllmproxy-gateway-observability"}'
        values: '1+0x10'
      - series: 'up{namespace="ns-b",service="release-b-openllmproxy-gateway-observability"}'
        values: '1+0x10'
      - series: 'olp_ready{namespace="ns-b",service="release-b-openllmproxy-control-observability"}'
        values: '1+0x10'
      - series: 'up{namespace="ns-b",service="release-b-openllmproxy-control-observability"}'
        values: '1+0x10'
      - series: 'olp_request_metadata_events_dropped_total{namespace="ns-b",service="release-b-openllmproxy-gateway-observability"}'
        values: '0+0x10'
      - series: 'olp_request_metadata_events_abandoned_total{namespace="ns-b",service="release-b-openllmproxy-gateway-observability"}'
        values: '0+0x10'
      - series: 'olp_request_metadata_persistence_available{namespace="ns-b",service="release-b-openllmproxy-gateway-observability"}'
        values: '1+0x10'
      - series: 'olp_request_metadata_events_pending{namespace="ns-b",service="release-b-openllmproxy-gateway-observability"}'
        values: '0+0x10'
      - series: 'olp_request_metadata_consumer_pending_events{namespace="ns-b",service="release-b-openllmproxy-gateway-observability"}'
        values: '0+0x10'
      - series: 'olp_request_metadata_consumer_lag_events{namespace="ns-b",service="release-b-openllmproxy-gateway-observability"}'
        values: '0+0x10'
      - series: 'olp_async_plane_current{namespace="ns-b",service="release-b-openllmproxy-gateway-observability"}'
        values: '1+0x10'
      - series: 'olp_async_worker_observability_available{namespace="ns-b",service="release-b-openllmproxy-gateway-observability"}'
        values: '1+0x10'
      - series: 'olp_runtime_outbox_pending_rows{namespace="ns-b",service="release-b-openllmproxy-gateway-observability"}'
        values: '0+0x10'
      - series: 'olp_runtime_outbox_failed_takeovers_total{namespace="ns-b",service="release-b-openllmproxy-gateway-observability"}'
        values: '0+0x10'
      - series: 'olp_distributed_limiter_available{namespace="ns-b",service="release-b-openllmproxy-gateway-observability"}'
        values: '1+0x10'
      - series: 'olp_runtime_generation{namespace="ns-b",service="release-b-openllmproxy-gateway-observability"}'
        values: '1+0x10'
    alert_rule_test:
      - eval_time: 5m
        alertname: OLPReadinessAbsent
        exp_alerts:
          - exp_labels:
              severity: critical
              alertname: OLPReadinessAbsent
              namespace: ns-a
              service: release-a-openllmproxy-gateway-observability
            exp_annotations:
              summary: OpenLLMProxy readiness is absent
              description: A gateway or control target has been unready or unavailable for five minutes.
TESTEOF
run_promtool test rules "$work/test-10.yaml" >/dev/null

echo "Helm monitoring rule matrix verified: 10 topology and release-isolation scenarios passed Promtool evaluation"
