#!/usr/bin/env bash
set -euo pipefail

chart=${1:-deploy/helm}
command -v helm >/dev/null || { echo "helm is required" >&2; exit 1; }
command -v jq >/dev/null || { echo "jq is required" >&2; exit 1; }
command -v docker >/dev/null || { echo "docker is required" >&2; exit 1; }
docker compose version >/dev/null 2>&1 || { echo "Docker Compose is required" >&2; exit 1; }
compose_file="$(dirname "$chart")/compose.yaml"
bootstrap_compose_file="$(dirname "$chart")/compose.bootstrap.yaml"
dockerfile="$(dirname "$chart")/Dockerfile"
compose_secret_helper="$(dirname "$chart")/../scripts/prepare-compose-secrets.sh"
compose_bootstrap_retirement_helper="$(dirname "$chart")/../scripts/retire-compose-bootstrap-secret.sh"

dashboard="$(dirname "$chart")/monitoring/grafana-dashboard.json"
[[ -f $dashboard ]] || {
  echo "Grafana dashboard is missing: $dashboard" >&2
  exit 1
}
[[ -f $compose_file && -f $bootstrap_compose_file && -f $dockerfile && \
  -x $compose_secret_helper && -x $compose_bootstrap_retirement_helper ]] || {
  echo "deployment Compose file, bootstrap overlay, Dockerfile, or secret helper is missing" >&2
  exit 1
}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

legacy_secrets="$work/legacy-compose-secrets"
install -d -m 700 "$legacy_secrets"
printf 'legacy-key-fixture\n' > "$legacy_secrets/olp_key_hash_key"
chmod 600 "$legacy_secrets/olp_key_hash_key"
legacy_auth_hmac_key_checksum=$(sha256sum "$legacy_secrets/olp_key_hash_key")
if OLP_COMPOSE_SECRETS_DIR="$legacy_secrets" \
  "$compose_secret_helper" >"$work/legacy-compose-error" 2>&1; then
  echo "Compose secret preparation replaced a legacy authentication HMAC key" >&2
  exit 1
fi
[[ $(sha256sum "$legacy_secrets/olp_key_hash_key") == "$legacy_auth_hmac_key_checksum" && \
  ! -e "$legacy_secrets/olp_auth_hmac_key" && \
  ! -e "$legacy_secrets/olp_master_key" ]] || {
  echo "Compose legacy authentication HMAC key guard changed secret files" >&2
  exit 1
}
grep -Fq 'move or securely copy the existing bytes' "$work/legacy-compose-error" || {
  echo "Compose legacy authentication HMAC key guard is not actionable" >&2
  exit 1
}

OLP_COMPOSE_SECRETS_DIR="$work/compose-secrets" "$compose_secret_helper" >/dev/null
for secret in olp_master_key olp_auth_hmac_key olp_bootstrap_token; do
  [[ -f "$work/compose-secrets/$secret" ]] || {
    echo "Compose quick-start did not generate $secret" >&2
    exit 1
  }
  [[ $(stat -c '%a' "$work/compose-secrets/$secret") == 600 ]] || {
    echo "Compose quick-start did not secure $secret" >&2
    exit 1
  }
done
master_key_checksum=$(sha256sum "$work/compose-secrets/olp_master_key")
auth_hmac_key_checksum=$(sha256sum "$work/compose-secrets/olp_auth_hmac_key")
OLP_COMPOSE_SECRETS_DIR="$work/compose-secrets" \
  "$compose_bootstrap_retirement_helper" >/dev/null
[[ ! -e "$work/compose-secrets/olp_bootstrap_token" && \
  -f "$work/compose-secrets/.olp_bootstrap_retired" ]] || {
  echo "Compose bootstrap retirement did not remove and retire the token" >&2
  exit 1
}
OLP_COMPOSE_SECRETS_DIR="$work/compose-secrets" "$compose_secret_helper" >/dev/null
[[ ! -e "$work/compose-secrets/olp_bootstrap_token" ]] || {
  echo "Compose secret preparation recreated a retired bootstrap token" >&2
  exit 1
}
[[ $(sha256sum "$work/compose-secrets/olp_master_key") == "$master_key_checksum" && \
  $(sha256sum "$work/compose-secrets/olp_auth_hmac_key") == "$auth_hmac_key_checksum" ]] || {
  echo "Compose bootstrap retirement changed a long-lived key" >&2
  exit 1
}
digest="sha256:$(printf 'a%.0s' {1..64})"
helm lint --strict "$chart"
helm template olp "$chart" --namespace olp --set-string image.digest="$digest" \
  > "$work/manifests.yaml"
helm template olp "$chart" --namespace olp --set-string image.digest="$digest" \
  --set monitoring.enabled=true \
  --set ingress.enabled=true \
  --set ingress.className=nginx \
  --set ingress.host=olp.example.com \
  --set config.trustedProxyCidrs=10.0.0.0/8 \
  > "$work/edge-manifests.yaml"
helm template olp "$chart" --namespace olp --set-string image.digest="$digest" \
  --set mediaSpool.capacityBytes=9007199254740991 \
  > "$work/max-spool-manifests.yaml"
helm template olp "$chart" --namespace olp --set-string image.digest="$digest" \
  --set-string config.valkeySecretName=migration-preflight-valkey \
  --set-string config.valkeySecretKey=migration-preflight-url \
  --show-only templates/migration-job.yaml \
  > "$work/migration-manifest.yaml"
long_fullname="$(printf 'a%.0s' {1..63})"
helm template olp "$chart" --namespace olp --set fullnameOverride="$long_fullname" \
  > "$work/long-name-manifests.yaml"
helm package "$chart" --destination "$work" --version 2.0.0 --app-version 2.0.0 >/dev/null
if helm template invalid "$chart" --set preStopDelaySeconds=300 \
  --set terminationGracePeriodSeconds=300 >/dev/null 2>&1; then
  echo "chart accepted a pre-stop delay without a connection-drain window" >&2
  exit 1
fi
if helm template invalid "$chart" --set ingress.enabled=true \
  --set gateway.enabled=false >/dev/null 2>&1; then
  echo "chart accepted same-origin ingress without a gateway" >&2
  exit 1
fi
if helm template invalid "$chart" --set ingress.enabled=true \
  --set control.service.enabled=false >/dev/null 2>&1; then
  echo "chart accepted same-origin ingress without a control service" >&2
  exit 1
fi
if helm template invalid "$chart" --set mediaSpool.capacityBytes=9007199254740992 \
  >/dev/null 2>&1; then
  echo "chart accepted a media spool capacity beyond exact integer serialization" >&2
  exit 1
fi
if helm template invalid "$chart" --set gateway.httpMaxConnections=0 \
  >/dev/null 2>&1; then
  echo "chart accepted a zero gateway connection cap" >&2
  exit 1
fi

for expected in \
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
  'olp-openllmproxy-control-observability'; do
  grep -Fq "$expected" "$work/manifests.yaml" || {
    echo "rendered Helm contract is missing: $expected" >&2
    exit 1
  }
done

for expected in \
  'name: OLP_VALKEY_URL' \
  'name: "migration-preflight-valkey"' \
  'key: "migration-preflight-url"'; do
  grep -Fq "$expected" "$work/migration-manifest.yaml" || {
    echo "rendered migration Job is missing its Valkey preflight dependency: $expected" >&2
    exit 1
  }
done

grep -Fq 'value: "9007199254740991"' "$work/max-spool-manifests.yaml" || {
  echo "rendered Helm contract did not preserve the maximum exact spool capacity" >&2
  exit 1
}
if rg -q 'name: OLP_TRUSTED_PROXY_CIDRS' "$work/manifests.yaml"; then
  echo "default chart must omit an empty trusted-proxy CIDR environment value" >&2
  exit 1
fi
grep -Fq 'name: OLP_TRUSTED_PROXY_CIDRS' "$work/edge-manifests.yaml" || {
  echo "ingress chart must pass configured trusted-proxy CIDRs to application pods" >&2
  exit 1
}
if grep -Eiq 'value: "?[0-9]+(\.[0-9]+)?e[+-]?[0-9]+"?' \
  "$work/manifests.yaml" "$work/max-spool-manifests.yaml"; then
  echo "rendered media spool capacity used scientific notation" >&2
  exit 1
fi

if awk '/^  name: / && length($2) > 63 { exit 1 }' "$work/long-name-manifests.yaml"; then
  :
else
  echo "chart rendered a Kubernetes resource name longer than 63 characters" >&2
  exit 1
fi
observability_name_count=$(awk '/^  name: .*observability$/ { print $2 }' \
  "$work/long-name-manifests.yaml" | sort -u | wc -l)
[[ $observability_name_count == 2 ]] || {
  echo "long chart names must retain distinct gateway and control observability Services" >&2
  exit 1
}

grep -Fq 'OLP_OBSERVABILITY_LISTEN_ADDR: 0.0.0.0:9090' "$compose_file" || {
  echo "Compose does not start the private observability listener" >&2
  exit 1
}
grep -Fq 'OLP_AUTH_HMAC_KEY_FILE: /run/secrets/olp_auth_hmac_key' "$compose_file" || {
  echo "Compose does not mount the authentication HMAC key" >&2
  exit 1
}
grep -Fq "OLP_HTTP_MAX_CONNECTIONS: \${OLP_HTTP_MAX_CONNECTIONS:-1024}" "$compose_file" || {
  echo "Compose does not expose the public-listener connection cap" >&2
  exit 1
}
if rg -q 'OLP_BOOTSTRAP_TOKEN_FILE|olp_bootstrap_token' "$compose_file"; then
  echo "base Compose configuration must not require the retired bootstrap token" >&2
  exit 1
fi
for expected in 'OLP_BOOTSTRAP_TOKEN_FILE' 'olp_bootstrap_token'; do
  grep -Fq "$expected" "$bootstrap_compose_file" || {
    echo "Compose bootstrap overlay is missing: $expected" >&2
    exit 1
  }
done
grep -Fq 'EXPOSE 8080 9090' "$dockerfile" || {
  echo "image does not declare the observability port" >&2
  exit 1
}
docker compose -f "$compose_file" config > "$work/compose.yaml"
docker compose -f "$compose_file" config --format json > "$work/compose.json"
docker compose -f "$compose_file" -f "$bootstrap_compose_file" config \
  > "$work/compose-bootstrap.yaml"
jq -e '
  .services.migrate.environment.OLP_VALKEY_URL == "redis://valkey:6379" and
  .services.migrate.depends_on.valkey.condition == "service_healthy"
' "$work/compose.json" >/dev/null || {
  echo "Compose migration must wait for and preflight Valkey" >&2
  exit 1
}
if rg -q 'OLP_BOOTSTRAP_TOKEN_FILE|olp_bootstrap_token' "$work/compose.yaml"; then
  echo "rendered base Compose configuration still requires the bootstrap token" >&2
  exit 1
fi
for expected in 'OLP_BOOTSTRAP_TOKEN_FILE' 'olp_bootstrap_token'; do
  grep -Fq "$expected" "$work/compose-bootstrap.yaml" || {
    echo "rendered bootstrap Compose configuration is missing: $expected" >&2
    exit 1
  }
done
if rg -q '(^|[[:space:]])(target: 9090|published: "?9090"?)$' "$work/compose.yaml"; then
  echo "Compose must not host-publish private observability port 9090" >&2
  exit 1
fi

for expected in \
  'kind: Ingress' \
  'ingressClassName: "nginx"' \
  'host: "olp.example.com"' \
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
  'olp_ready{namespace="olp"'; do
  grep -Fq "$expected" "$work/edge-manifests.yaml" || {
    echo "rendered edge/monitoring contract is missing: $expected" >&2
    exit 1
  }
done

if rg -q 'OLPUsage(Events|Persistence|Backlog|Consumer)|olp_usage_(events|persistence|consumer|gateway|stream)' \
  "$work/edge-manifests.yaml"; then
  echo "rendered monitoring contract contains legacy usage-named request metadata telemetry" >&2
  exit 1
fi

if awk '
  /^kind: Ingress$/ { ingress=1 }
  ingress { print }
  ingress && /^---$/ { exit }
' "$work/edge-manifests.yaml" | grep -Fq 'path: /health'; then
  echo "public Ingress must not expose health endpoints" >&2
  exit 1
fi

service_monitor_count=$(grep -c '^kind: ServiceMonitor$' "$work/edge-manifests.yaml")
[[ $service_monitor_count == 2 ]] || {
  echo "monitoring must render exactly one gateway and one control ServiceMonitor" >&2
  exit 1
}
if ! rg -U 'kind: ServiceMonitor[\s\S]*?port: observability[\s\S]*?path: /metrics' \
  "$work/edge-manifests.yaml" >/dev/null; then
  echo "ServiceMonitors must target private observability Services" >&2
  exit 1
fi

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
[[ -n $gateway_service && -n $control_service ]] || {
  echo "same-origin ingress did not bind protocol/control paths to distinct services" >&2
  exit 1
}

[[ -s $work/openllmproxy-2.0.0.tgz ]] || {
  echo "Helm chart package was not produced" >&2
  exit 1
}

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
' "$dashboard" >/dev/null || {
  echo "Grafana dashboard is missing an operational acceptance panel or query" >&2
  exit 1
}

echo "Helm contract verified: digest, drain, spread, private observability, exact media capacity, same-origin edge, monitoring, dashboard, package"
