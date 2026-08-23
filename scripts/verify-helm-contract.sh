#!/usr/bin/env bash
set -euo pipefail

chart=${1:-deploy/helm}
deploy_dir=$(dirname "$chart")
compose_file="$deploy_dir/compose.yaml"
bootstrap_compose_file="$deploy_dir/compose.bootstrap.yaml"
dockerfile="$deploy_dir/Dockerfile"
dashboard="$deploy_dir/monitoring/grafana-dashboard.json"

fail() { echo "$1" >&2; exit 1; }

for required in helm jq docker rg; do
  command -v "$required" >/dev/null || fail "$required is required"
done
docker compose version >/dev/null 2>&1 || fail "Docker Compose is required"

# require_in FILE MESSAGE NEEDLE...: every fixed-string NEEDLE must appear in FILE.
require_in() {
  local file=$1 message=$2 needle
  shift 2
  for needle in "$@"; do
    grep -Fq -- "$needle" "$file" || fail "$message: $needle"
  done
}

# forbid_in REGEX MESSAGE FILE...: grep exit 1 is a clean no-match; 0 means the
# forbidden content is present; anything else is a scan failure and aborts.
forbid_in() {
  local regex=$1 message=$2 status=0
  shift 2
  grep -Eq -- "$regex" "$@" || status=$?
  case $status in
    0) fail "$message" ;;
    1) ;;
    *) exit "$status" ;;
  esac
}

# reject_values MESSAGE --set...: the chart schema must refuse these values.
reject_values() {
  local message=$1
  shift
  if helm template invalid "$chart" "$@" >/dev/null 2>&1; then
    fail "$message"
  fi
}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

digest="sha256:$(printf 'a%.0s' {1..64})"
render() { helm template olp "$chart" --namespace olp --set-string image.digest="$digest" "$@"; }

helm lint --strict "$chart"
render > "$work/manifests.yaml"
render --set monitoring.enabled=true \
  --set ingress.enabled=true \
  --set ingress.className=nginx \
  --set ingress.host=olp.example.com \
  --set config.trustedProxyCidrs=10.0.0.0/8 \
  > "$work/edge-manifests.yaml"
render --set mediaSpool.capacityBytes=9007199254740991 > "$work/max-spool-manifests.yaml"
render --set worker.replicas=3 --set worker.podDisruptionBudget.minAvailable=2 \
  > "$work/worker-ha-manifests.yaml"
render --set-string config.valkeySecretName=migration-preflight-valkey \
  --set-string config.valkeySecretKey=migration-preflight-url \
  --show-only templates/migration-job.yaml \
  > "$work/migration-manifest.yaml"
long_fullname="$(printf 'a%.0s' {1..63})"
helm template olp "$chart" --namespace olp --set fullnameOverride="$long_fullname" \
  > "$work/long-name-manifests.yaml"
helm package "$chart" --destination "$work" --version 2.0.0 --app-version 2.0.0 >/dev/null
[[ -s $work/openllmproxy-2.0.0.tgz ]] || fail "Helm chart package was not produced"

reject_values "chart accepted a pre-stop delay without a connection-drain window" \
  --set preStopDelaySeconds=300 --set terminationGracePeriodSeconds=300
reject_values "chart accepted same-origin ingress without a gateway" \
  --set ingress.enabled=true --set gateway.enabled=false
reject_values "chart accepted same-origin ingress without a control service" \
  --set ingress.enabled=true --set control.service.enabled=false
reject_values "chart accepted a media spool capacity beyond exact integer serialization" \
  --set mediaSpool.capacityBytes=9007199254740992
reject_values "chart accepted a zero gateway connection cap" \
  --set gateway.httpMaxConnections=0

require_in "$work/manifests.yaml" "rendered Helm contract is missing" \
  "ghcr.io/tyk-swe/olp@$digest" \
  'terminationGracePeriodSeconds: 300' \
  '/usr/local/bin/olp' \
  'internal-pre-stop' \
  'topologySpreadConstraints:' \
  'name: media-spool' \
  'sizeLimit: "2Gi"' \
  'containerPort: 9090' \
  'name: observability' \
  'name: OLP_AUTH_HMAC_KEY_FILE' \
  'value: /run/secrets/auth-hmac-key/key' \
  'name: OLP_HTTP_MAX_CONNECTIONS' \
  'value: "16384"' \
  'value: "1073741824"' \
  'olp-openllmproxy-gateway-observability' \
  'olp-openllmproxy-control-observability'
require_in "$work/migration-manifest.yaml" \
  "rendered migration Job is missing its Valkey preflight dependency" \
  'name: OLP_VALKEY_URL' \
  'name: "migration-preflight-valkey"' \
  'key: "migration-preflight-url"'
require_in "$work/max-spool-manifests.yaml" \
  "rendered Helm contract did not preserve the maximum exact spool capacity" \
  'value: "9007199254740991"'
forbid_in 'value: "?[0-9]+(\.[0-9]+)?[eE][+-]?[0-9]+"?' \
  "rendered media spool capacity used scientific notation" \
  "$work/manifests.yaml" "$work/max-spool-manifests.yaml"

awk -v RS='---' '/kind: Deployment/ && /name: olp-openllmproxy-worker/ { print }' \
  "$work/worker-ha-manifests.yaml" > "$work/worker-deployment.yaml"
awk -v RS='---' '/kind: PodDisruptionBudget/ && /name: olp-openllmproxy-worker/ { print }' \
  "$work/worker-ha-manifests.yaml" > "$work/worker-pdb.yaml"
require_in "$work/worker-deployment.yaml" "rendered worker Deployment contract is missing" \
  'replicas: 3' 'type: Recreate' 'topologyKey: "kubernetes.io/hostname"'
require_in "$work/worker-pdb.yaml" "rendered worker PodDisruptionBudget is missing" \
  'minAvailable: 2'

forbid_in 'name: OLP_TRUSTED_PROXY_CIDRS' \
  "default chart must omit an empty trusted-proxy CIDR environment value" \
  "$work/manifests.yaml"
require_in "$work/edge-manifests.yaml" \
  "ingress chart must pass configured trusted-proxy CIDRs to application pods" \
  'name: OLP_TRUSTED_PROXY_CIDRS'

awk '/^  name: / && length($2) > 63 { exit 1 }' "$work/long-name-manifests.yaml" ||
  fail "chart rendered a Kubernetes resource name longer than 63 characters"
observability_name_count=$(awk '/^  name: .*observability$/ { print $2 }' \
  "$work/long-name-manifests.yaml" | sort -u | wc -l)
[[ $observability_name_count == 2 ]] ||
  fail "long chart names must retain distinct gateway and control observability Services"

require_in "$compose_file" "Compose contract is missing" \
  'OLP_OBSERVABILITY_LISTEN_ADDR: 0.0.0.0:9090' \
  'OLP_AUTH_HMAC_KEY_FILE: /run/secrets/olp_auth_hmac_key' \
  "OLP_HTTP_MAX_CONNECTIONS: \${OLP_HTTP_MAX_CONNECTIONS:-1024}"
forbid_in 'OLP_BOOTSTRAP_TOKEN_FILE|olp_bootstrap_token' \
  "base Compose configuration must not require the retired bootstrap token" \
  "$compose_file"
require_in "$bootstrap_compose_file" "Compose bootstrap overlay is missing" \
  'OLP_BOOTSTRAP_TOKEN_FILE' 'olp_bootstrap_token'
require_in "$dockerfile" "image does not declare the observability port" 'EXPOSE 8080 9090'

docker compose -f "$compose_file" config > "$work/compose.yaml"
docker compose -f "$compose_file" config --format json > "$work/compose.json"
docker compose -f "$compose_file" -f "$bootstrap_compose_file" config \
  > "$work/compose-bootstrap.yaml"
jq -e '
  .services.migrate.environment.OLP_VALKEY_URL == "redis://valkey:6379" and
  .services.migrate.depends_on.valkey.condition == "service_healthy"
' "$work/compose.json" >/dev/null || fail "Compose migration must wait for and preflight Valkey"
forbid_in 'OLP_BOOTSTRAP_TOKEN_FILE|olp_bootstrap_token' \
  "rendered base Compose configuration still requires the bootstrap token" \
  "$work/compose.yaml"
require_in "$work/compose-bootstrap.yaml" "rendered bootstrap Compose configuration is missing" \
  'OLP_BOOTSTRAP_TOKEN_FILE' 'olp_bootstrap_token'
forbid_in '(^|[[:space:]])(target: 9090|published: "?9090"?)$' \
  "Compose must not host-publish private observability port 9090" \
  "$work/compose.yaml"

require_in "$work/edge-manifests.yaml" "rendered edge/monitoring contract is missing" \
  'kind: Ingress' \
  'ingressClassName: "nginx"' \
  'host: "olp.example.com"' \
  'path: /v1' \
  'path: /openai' \
  'path: /anthropic' \
  'path: /gemini' \
  'path: /api' \
  'path: /' \
  'alert: OLPReadinessAbsent' \
  'alert: OLPRequestMetadataEventsDropped' \
  'alert: OLPRequestMetadataEventsAbandoned' \
  'alert: OLPRequestMetadataPersistenceUnavailable' \
  'alert: OLPRequestMetadataBacklogHigh' \
  'alert: OLPRequestMetadataConsumerBacklogHigh' \
  'olp_request_metadata_events_pending' \
  'olp_ready{namespace="olp"'
forbid_in 'OLPUsage(Events|Persistence|Backlog|Consumer)|olp_usage_(events|persistence|consumer|gateway|stream)' \
  "rendered monitoring contract contains legacy usage-named request metadata telemetry" \
  "$work/edge-manifests.yaml"

rendered_ingress=$(awk '
  /^kind: Ingress$/ { ingress=1 }
  ingress { print }
  ingress && /^---$/ { exit }
' "$work/edge-manifests.yaml")
[[ $rendered_ingress != *'path: /health'* ]] || fail "public Ingress must not expose health endpoints"

service_monitor_count=$(grep -c '^kind: ServiceMonitor$' "$work/edge-manifests.yaml")
[[ $service_monitor_count == 2 ]] ||
  fail "monitoring must render exactly one gateway and one control ServiceMonitor"
rg -qU 'kind: ServiceMonitor[\s\S]*?port: observability[\s\S]*?path: /metrics' \
  "$work/edge-manifests.yaml" || fail "ServiceMonitors must target private observability Services"

gateway_service=$(awk '
  /^kind: Ingress$/ { in_ingress=1 }
  in_ingress && /path: \/openai/ { in_openai=1 }
  in_openai && /name: .*gateway/ { print; exit }
' "$work/edge-manifests.yaml")
control_service=$(awk '
  /^kind: Ingress$/ { in_ingress=1 }
  in_ingress && /path: \/api/ { in_api=1 }
  in_api && /name: .*control/ { print; exit }
' "$work/edge-manifests.yaml")
[[ -n $gateway_service && -n $control_service ]] ||
  fail "same-origin ingress did not bind protocol/control paths to distinct services"

jq -e '
  ([.panels[].title] | index("Ready targets") != null) and
  ([.panels[].title] | index("Request success (5m)") != null) and
  ([.panels[].title] | index("Request latency p95 / p99 (5m)") != null) and
  ([.panels[].title] | index("Provider success and latency (15m)") != null) and
  ([.panels[].title] | index("Upstream cancellations (5m)") != null) and
  ([.panels[].title] | index("Gateway memory working set") != null) and
  ([.panels[].targets[].expr] | tostring | contains("olp_ready")) and
  ([.panels[].targets[].expr] | tostring | contains("olp_request_success_ratio_5m")) and
  ([.panels[].targets[].expr] | tostring | contains("olp_request_latency_seconds")) and
  ([.panels[].targets[].expr] | tostring | contains("olp_provider_health")) and
  ([.panels[].targets[].expr] | tostring | contains("olp_upstream_cancellations_5m")) and
  ([.panels[].targets[].expr] | tostring | contains("container_memory_working_set_bytes"))
' "$dashboard" >/dev/null || fail "Grafana dashboard is missing an operational acceptance panel or query"

echo "Helm contract verified: digest, drain, spread, private observability, exact media capacity, same-origin edge, monitoring, dashboard, package"
