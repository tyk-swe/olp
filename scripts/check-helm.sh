#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
version=1.27.0
bash scripts/check-helm-egress.sh
helm lint deploy/helm
helm lint deploy/helm -f deploy/helm/values.production.yaml
helm template olp deploy/helm --kube-version "$version" >/dev/null
helm template olp deploy/helm --kube-version "$version" -f deploy/helm/values.production.yaml |
  docker run --rm -i ghcr.io/yannh/kubeconform@sha256:faffaf43f95aa6425306e1ab8d6fcad72acb9049158f38e574c085ea1ec0f64e \
    -strict -summary -kubernetes-version "$version" -skip ServiceMonitor,PrometheusRule
for mode in gateway control worker; do
  helm template olp deploy/helm --kube-version "$version" \
    --set gateway.enabled=false,control.enabled=false,worker.enabled=false \
    --set "$mode.enabled=true" >/dev/null
done
helm template olp deploy/helm --set-string image.digest="sha256:$(printf '%064d' 0)" >/dev/null
unconfined=$(helm template olp deploy/helm --set config.unconfinedPluginDir=/opt/olp/plugins | grep -c 'OLP_UNCONFINED_PLUGIN_DIR' || true)
if [ "$unconfined" != 3 ] || helm template olp deploy/helm | grep -q OLP_UNCONFINED_PLUGIN_DIR; then
  echo "Expected OLP_UNCONFINED_PLUGIN_DIR on every component only when config.unconfinedPluginDir is set" >&2
  exit 1
fi
management_network=$(helm template olp deploy/helm --set config.managementAllowedCidrs=192.0.2.0/24 | grep -c 'OLP_MANAGEMENT_ALLOWED_CIDRS' || true)
if [ "$management_network" != 1 ] || helm template olp deploy/helm | grep -q OLP_MANAGEMENT_ALLOWED_CIDRS; then
  echo "Expected OLP_MANAGEMENT_ALLOWED_CIDRS on control only when configured" >&2
  exit 1
fi
runtime_role=$(helm template olp deploy/helm -f deploy/helm/values.production.yaml | grep -c 'OLP_RUNTIME_ROLE' || true)
if [ "$runtime_role" != 1 ] || helm template olp deploy/helm | grep -q OLP_RUNTIME_ROLE; then
  echo "Expected OLP_RUNTIME_ROLE on the migration job only when migration.runtimeRole is set" >&2
  exit 1
fi
for mode in gateway control worker; do
  if helm template olp deploy/helm --set monitoring.enabled=true --set "$mode.replicas=0" |
    grep -q "absent_over_time(olp_ready{[^}]*service=\"olp-openllmproxy-$mode-observability\""; then
    echo "Expected no readiness-absence alert for $mode scaled to zero replicas" >&2
    exit 1
  fi
done
if ! helm template olp deploy/helm --set monitoring.enabled=true |
  grep -q 'absent_over_time(olp_ready{[^}]*service="olp-openllmproxy-worker-observability"'; then
  echo "Expected a worker readiness-absence alert by default" >&2
  exit 1
fi
metrics=$(grep -ohE 'olp_[a-z0-9_]+' deploy/helm/templates/monitoring.yaml deploy/monitoring/grafana-dashboard.json |
  sed -E 's/_(bucket|sum|count)$//' | sort -u)
for metric in $metrics; do
  if ! grep -rqF --include='*.go' --exclude='*_test.go' "$metric" internal; then
    echo "Monitoring queries $metric, which no OLP process emits" >&2
    exit 1
  fi
done
for invalid in 'config.databaseMaxConnections=0' 'config.httpMaxJsonBodyBytes=0' \
  'gateway.replicas=-1' 'ingress.enabled=true,config.trustedProxyCidrs=' \
  'networkPolicy.enabled=true' 'config.unconfinedPluginDir=plugins' \
  'migration.databaseSecretName=olp-postgresql-migration'; do
  if helm template olp deploy/helm --set "$invalid" >/dev/null 2>&1; then
    echo "Expected invalid Helm configuration to fail: $invalid" >&2
    exit 1
  fi
done
echo 'Helm profiles and invalid-input checks passed.'
