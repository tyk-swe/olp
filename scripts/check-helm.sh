#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
awk '
  /^helm (install|upgrade)[[:space:]]/ {
    command = $0
    while (command ~ /\\$/ && getline > 0) {
      sub(/\\$/, "", command)
      command = command " " $0
    }
    if (command ~ /^helm install[[:space:]]/) {
      install = 1
      if (command ~ /[[:space:]]--(wait(-for-jobs)?|atomic|rollback-on-failure)([[:space:]=]|$)/) {
        print "First install must return the setup notes without waiting for workloads" > "/dev/stderr"
        failed = 1
      }
    } else {
      upgrade = 1
      if (command ~ /[[:space:]](--install|-i)([[:space:]=]|$)/ ||
          command !~ /[[:space:]]--wait(=(true|watcher|legacy))?([[:space:]]|$)/) {
        print "Use a separate upgrade command with --wait and without --install" > "/dev/stderr"
        failed = 1
      }
    }
  }
  END {
    if (!install || !upgrade) {
      print "Document both first install and upgrade commands" > "/dev/stderr"
      failed = 1
    }
    exit failed
  }' docs/operate/deployment.md
version=1.27.0
bash scripts/check-helm-egress.sh
helm lint deploy/helm
helm lint deploy/helm -f deploy/helm/values.production.yaml
helm template olp deploy/helm --kube-version "$version" >/dev/null
helm template olp deploy/helm --set-string config.bootstrapTokenSecretKey=yes |
  awk '
    /^kind: Secret$/ { secret = 1 }
    /^---$/ { secret = 0 }
    secret && /^  "yes":/ { data_key = 1 }
    /- key: "yes"$/ { volume_key = 1 }
    END {
      if (!data_key || !volume_key) {
        print "Expected the bootstrap Secret data and the volume item to use the same string key" > "/dev/stderr"
        exit 1
      }
    }'
helm template olp deploy/helm --kube-version "$version" \
  --set-json 'extraEnv=[{"name":"OLP_VAULT_ROLE","value":"olp"},{"name":"OLP_VAULT_JWT_FILE","value":"/run/identity/token"}]' \
  --set-json 'extraVolumes=[{"name":"workload-identity","projected":{"sources":[{"serviceAccountToken":{"path":"token","audience":"vault","expirationSeconds":3600}}]}}]' \
  --set-json 'extraVolumeMounts=[{"name":"workload-identity","mountPath":"/run/identity","readOnly":true}]' |
  docker run --rm -i ghcr.io/yannh/kubeconform@sha256:faffaf43f95aa6425306e1ab8d6fcad72acb9049158f38e574c085ea1ec0f64e \
    -strict -summary -kubernetes-version "$version" -skip ServiceMonitor,PrometheusRule
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
zones=$(helm template olp deploy/helm -f deploy/helm/values.production.yaml | grep -c 'topologyKey: topology.kubernetes.io/zone' || true)
if [ "$zones" != 3 ] || helm template olp deploy/helm | grep -q 'topology.kubernetes.io/zone'; then
  echo "Expected a zone spread on every production component only when topologySpread.zone is enabled" >&2
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
for migration in 'migration.databaseSecretKey=migration' \
  'migration.databaseSecretName=olp-postgresql,migration.databaseSecretKey=migration'; do
  runtime_role=$(helm template olp deploy/helm --set "$migration" --set migration.runtimeRole=olp_runtime |
    grep -c 'OLP_RUNTIME_ROLE' || true)
  if [ "$runtime_role" != 1 ]; then
    echo "Expected a runtime grant role for a separate migration key" >&2
    exit 1
  fi
done
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
helm template olp deploy/helm \
  --set monitoring.enabled=true,monitoring.rules.enabled=true,gateway.service.enabled=false,control.service.enabled=false |
  awk '
    /^kind: / { kind = $2 }
    /^  name: olp-openllmproxy-(gateway|control)-observability$/ && kind == "Service" { services++ }
    /^  name: olp-openllmproxy-(gateway|control)$/ && kind == "ServiceMonitor" { monitors++ }
    /absent_over_time\(olp_ready\{[^}]*service="olp-openllmproxy-gateway-observability"/ { gateway_alert = 1 }
    /absent_over_time\(olp_ready\{[^}]*service="olp-openllmproxy-control-observability"/ { control_alert = 1 }
    END {
      if (services != 2 || monitors != 2 || !gateway_alert || !control_alert) {
        print "Expected gateway and control monitoring with their traffic Services disabled" > "/dev/stderr"
        exit 1
      }
    }'
helm template olp deploy/helm --show-only templates/workloads.yaml \
  --set tracing.endpoint=https://collector.invalid/v1/traces,tracing.headersSecretName=collector-headers |
  awk '
    /^  name: olp-openllmproxy-(gateway|control|worker)$/ { component = $2 }
    /name: OLP_(OTLP|TRACE)_|name: otlp-headers|secretName: "collector-headers"/ {
      if (component == "olp-openllmproxy-worker") worker_tracing = 1
      if (/name: OLP_OTLP_TRACES_ENDPOINT/) endpoints++
      if (/secretName: "collector-headers"/) secrets++
    }
    END {
      if (worker_tracing || endpoints != 2 || secrets != 2) {
        print "Expected tracing configuration and collector credentials on gateway and control only" > "/dev/stderr"
        exit 1
      }
    }'
metrics=$(grep -ohE 'olp_[a-z0-9_]+' deploy/helm/templates/monitoring.yaml deploy/monitoring/grafana-dashboard.json |
  sed -E 's/_(bucket|sum|count)$//' | sort -u)
for metric in $metrics; do
  if ! grep -rqF --include='*.go' --exclude='*_test.go' "$metric" internal cmd; then
    echo "Monitoring queries $metric, which no OLP process emits" >&2
    exit 1
  fi
done
if helm template olp deploy/helm | grep -q 'name: OLP_DATABASE_READ_URL'; then
  echo 'Expected the default chart to omit the optional read-replica URL' >&2
  exit 1
fi
if ! helm template olp deploy/helm --set config.databaseReadSecretName=olp-replica,config.databaseReadSecretKey=readonly |
  grep -q 'name: OLP_DATABASE_READ_URL'; then
  echo 'Expected the configured read-replica URL in workload environments' >&2
  exit 1
fi
if ! helm template olp deploy/helm --set config.region=us-west-2 | grep -q 'name: OLP_REGION'; then
  echo 'Expected the regional environment in workloads' >&2
  exit 1
fi
for invalid in 'config.region=Invalid' 'config.databaseMaxConnections=0' 'config.httpMaxJsonBodyBytes=0' \
  'config.databaseReadSecretName=olp-replica,config.databaseReadSecretKey=' \
  'gateway.replicas=-1' 'ingress.enabled=true,config.trustedProxyCidrs=' \
  'networkPolicy.enabled=true' 'config.unconfinedPluginDir=plugins' \
  'migration.databaseSecretName=olp-postgresql-migration' \
  'migration.databaseSecretKey=migration' \
  'migration.databaseSecretName=olp-postgresql,migration.databaseSecretKey=migration'; do
  if helm template olp deploy/helm --set "$invalid" >/dev/null 2>&1; then
    echo "Expected invalid Helm configuration to fail: $invalid" >&2
    exit 1
  fi
done
ingress_gateway=$(helm template olp deploy/helm --set ingress.enabled=true,config.trustedProxyCidrs=10.0.0.0/8 \
  --show-only templates/ingress.yaml | awk '/- path:/ { path = $3 } /name: .*-gateway$/ { print path }' | sort)
documented_gateway=$(grep -E '^\| .* \| gateway \|$' docs/operate/deployment.md | grep -oE '`/[^`]*`' | tr -d '`' | sort)
for prefix in /mcp /a2a; do
  if ! grep -qx "$prefix" <<<"$documented_gateway"; then
    echo "Expected the deployment guide to route $prefix to gateway" >&2
    exit 1
  fi
done
if [ "$ingress_gateway" != "$documented_gateway" ]; then
  echo 'Expected the chart Ingress gateway prefixes to match the deployment guide' >&2
  diff <(echo "$ingress_gateway") <(echo "$documented_gateway") >&2 || true
  exit 1
fi
echo 'Helm profiles and invalid-input checks passed.'
