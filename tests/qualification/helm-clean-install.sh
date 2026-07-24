#!/usr/bin/env bash
set -euo pipefail

if [[ ${1:-} == --help || ${1:-} == -h ]]; then
  echo "usage: tests/qualification/helm-clean-install.sh" >&2
  exit 0
fi
[[ $# -eq 0 ]] || exit 2
for command in docker kind kubectl helm curl jq openssl; do
  command -v "$command" >/dev/null || { echo "required command is unavailable: $command" >&2; exit 1; }
done

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
work=$(mktemp -d)
cluster="olp-qualification-$$"
namespace=olp-qualification
image=${OLP_QUALIFICATION_IMAGE:-openllmproxy:qualification}
chart=${OLP_QUALIFICATION_CHART:-"$root/deploy/helm"}
[[ -f $chart || -d $chart ]] || { echo "qualification chart is unavailable: $chart" >&2; exit 1; }
kind_image=${OLP_QUALIFICATION_KIND_IMAGE:-kindest/node:v1.34.0@sha256:7416a61b42b1662ca6ca89f02028ac133a309a2a30ba309614e8ec94d976dc5a}
control_pid=
gateway_pid=
cleanup() {
  local status=$?
  trap - EXIT INT TERM
  [[ -z $control_pid ]] || kill "$control_pid" 2>/dev/null || true
  [[ -z $gateway_pid ]] || kill "$gateway_pid" 2>/dev/null || true
  [[ -z $control_pid ]] || wait "$control_pid" 2>/dev/null || true
  [[ -z $gateway_pid ]] || wait "$gateway_pid" 2>/dev/null || true
  kind delete cluster --name "$cluster" >/dev/null 2>&1 || (( status != 0 ))
  rm -rf "$work"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

if [[ -z ${OLP_QUALIFICATION_IMAGE:-} ]]; then
  docker build --file "$root/deploy/Dockerfile" --tag "$image" "$root"
fi
kind create cluster --name "$cluster" --image "$kind_image" --wait 120s
kind load docker-image --name "$cluster" "$image"
kubectl create namespace "$namespace"
master=$(openssl rand -base64 32)
auth=$(openssl rand -base64 32)
bootstrap=$(openssl rand -base64 32)
kubectl -n "$namespace" create secret generic olp-master-key --from-literal=key="$master"
kubectl -n "$namespace" create secret generic olp-auth-hmac-key --from-literal=key="$auth"
kubectl -n "$namespace" create secret generic olp-bootstrap-token --from-literal=token="$bootstrap"
kubectl -n "$namespace" create secret generic olp-postgresql \
  --from-literal=url='postgres://olp:olp@postgresql:5432/olp'
kubectl -n "$namespace" create secret generic olp-valkey --from-literal=url='redis://valkey:6379'
kubectl -n "$namespace" apply -f - <<'EOF'
apiVersion: apps/v1
kind: Deployment
metadata: { name: postgresql }
spec:
  replicas: 1
  selector: { matchLabels: { app: postgresql } }
  template:
    metadata: { labels: { app: postgresql } }
    spec:
      containers:
        - name: postgresql
          image: postgres:18-alpine@sha256:9a8afca54e7861fd90fab5fdf4c42477a6b1cb7d293595148e674e0a3181de15
          env:
            - { name: POSTGRES_DB, value: olp }
            - { name: POSTGRES_USER, value: olp }
            - { name: POSTGRES_PASSWORD, value: olp }
          readinessProbe: { exec: { command: [pg_isready, -U, olp, -d, olp] }, periodSeconds: 2 }
---
apiVersion: v1
kind: Service
metadata: { name: postgresql }
spec: { selector: { app: postgresql }, ports: [{ port: 5432 }] }
---
apiVersion: apps/v1
kind: Deployment
metadata: { name: valkey }
spec:
  replicas: 1
  selector: { matchLabels: { app: valkey } }
  template:
    metadata: { labels: { app: valkey } }
    spec:
      containers:
        - name: valkey
          image: valkey/valkey:9.1-alpine@sha256:a35428eba9043cc0b79dbe54100f0c92784f2de00ad09b01182bfb1c5c83d1bd
          args: [valkey-server, --save, "", --appendonly, "no"]
          readinessProbe: { exec: { command: [valkey-cli, ping] }, periodSeconds: 2 }
---
apiVersion: v1
kind: Service
metadata: { name: valkey }
spec: { selector: { app: valkey }, ports: [{ port: 6379 }] }
EOF
kubectl -n "$namespace" rollout status deployment/postgresql --timeout=120s
kubectl -n "$namespace" rollout status deployment/valkey --timeout=120s

cat >"$work/values.yaml" <<EOF
image:
  repository: ${image%:*}
  tag: ${image##*:}
  pullPolicy: Never
gateway:
  replicas: 1
  resources: { requests: { cpu: 10m, memory: 64Mi }, limits: { cpu: "1", memory: 512Mi } }
control:
  resources: { requests: { cpu: 10m, memory: 64Mi }, limits: { cpu: "1", memory: 512Mi } }
worker:
  resources: { requests: { cpu: 10m, memory: 64Mi }, limits: { cpu: "1", memory: 512Mi } }
migration:
  resources: { requests: { cpu: 10m, memory: 64Mi }, limits: { cpu: "1", memory: 512Mi } }
config:
  bootstrapTokenSecretName: olp-bootstrap-token
  publicOrigin: http://127.0.0.1:28081
EOF
helm upgrade --install olp "$chart" --namespace "$namespace" \
  --values "$work/values.yaml" --timeout 10m --debug >"$work/helm.log" 2>&1
# The migration hook completes before Helm returns. A fresh gateway cannot be
# ready until setup publishes the first runtime, so wait for the management
# and worker workloads first and require the gateway after key creation.
for component in control worker; do
  kubectl -n "$namespace" wait --for=condition=Available deployment \
    --selector="app.kubernetes.io/instance=olp,app.kubernetes.io/component=$component" \
    --timeout=180s
done
control_service=$(kubectl -n "$namespace" get service \
  -l 'app.kubernetes.io/instance=olp,app.kubernetes.io/component=control' \
  -o jsonpath='{.items[0].metadata.name}')
gateway_service=$(kubectl -n "$namespace" get service \
  -l 'app.kubernetes.io/instance=olp,app.kubernetes.io/component=gateway' \
  -o jsonpath='{.items[0].metadata.name}')
kubectl -n "$namespace" port-forward "service/$control_service" 28081:80 >"$work/control-port.log" 2>&1 &
control_pid=$!
kubectl -n "$namespace" port-forward "service/$gateway_service" 28082:80 >"$work/gateway-port.log" 2>&1 &
gateway_pid=$!
for port in 28081 28082; do
  for _ in $(seq 1 100); do
    (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null && { exec 3>&-; break; }
    sleep 0.1
  done
done

origin=http://127.0.0.1:28081
status=$(curl -sS -D "$work/setup.headers" -o "$work/setup.json" -w '%{http_code}' \
  -X POST "$origin/api/v1/setup" -H "Origin: $origin" \
  -H "X-OLP-Setup-Token: $bootstrap" -H 'Content-Type: application/json' \
  --data '{"email":"owner@helm.qualification.test","password":"correct horse battery staple","display_name":"Owner","installation_name":"Helm qualification"}')
[[ $status == 201 ]] || { cat "$work/setup.json" >&2; exit 1; }
csrf=$(jq -er .csrf_token "$work/setup.json")
session=$(awk 'BEGIN{IGNORECASE=1} /^set-cookie: __Host-olp_session=/{sub(/^set-cookie: /,""); sub(/;.*/,""); gsub("\r",""); print}' "$work/setup.headers")
csrf_cookie=$(awk 'BEGIN{IGNORECASE=1} /^set-cookie: __Host-olp_csrf=/{sub(/^set-cookie: /,""); sub(/;.*/,""); gsub("\r",""); print}' "$work/setup.headers")
status=$(curl -sS -o "$work/key.json" -w '%{http_code}' -X POST \
  "$origin/api/v1/api-keys" -H "Origin: $origin" -H "Cookie: $session; $csrf_cookie" \
  -H "x-csrf-token: $csrf" -H 'Idempotency-Key: helm-qualification-key-0001' \
  -H 'Content-Type: application/json' --data '{"name":"Helm qualification","scopes":["models_read"]}')
[[ $status == 201 ]] || { cat "$work/key.json" >&2; exit 1; }
key=$(jq -er .secret "$work/key.json")
for _ in $(seq 1 100); do
  status=$(curl -sS -o "$work/models.json" -w '%{http_code}' \
    -H "Authorization: Bearer $key" http://127.0.0.1:28082/openai/v1/models || true)
  [[ $status == 200 ]] && break
  sleep 0.05
done
[[ $status == 200 ]] || { cat "$work/models.json" >&2; exit 1; }
kubectl -n "$namespace" wait --for=condition=Available deployment \
  --selector=app.kubernetes.io/instance=olp --timeout=180s

helm upgrade olp "$chart" --namespace "$namespace" \
  --values "$work/values.yaml" --set-string config.bootstrapTokenSecretName= \
  --wait --timeout 10m --debug >>"$work/helm.log" 2>&1
kubectl -n "$namespace" delete secret olp-bootstrap-token
kill "$control_pid" 2>/dev/null || true
kill "$gateway_pid" 2>/dev/null || true
wait "$control_pid" 2>/dev/null || true
wait "$gateway_pid" 2>/dev/null || true
kubectl -n "$namespace" port-forward "service/$control_service" 28081:80 \
  >"$work/control-retired-port.log" 2>&1 &
control_pid=$!
kubectl -n "$namespace" port-forward "service/$gateway_service" 28082:80 \
  >"$work/gateway-retired-port.log" 2>&1 &
gateway_pid=$!
for port in 28081 28082; do
  for _ in $(seq 1 100); do
    (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null && { exec 3>&-; break; }
    sleep 0.1
  done
done
status=$(curl -sS -o "$work/session.json" -w '%{http_code}' \
  "$origin/api/v1/sessions/current" -H "Origin: $origin" \
  -H "Cookie: $session; $csrf_cookie")
[[ $status == 200 ]] || { cat "$work/session.json" >&2; exit 1; }
for _ in $(seq 1 100); do
  status=$(curl -sS -o "$work/models.json" -w '%{http_code}' \
    -H "Authorization: Bearer $key" http://127.0.0.1:28082/openai/v1/models || true)
  [[ $status == 200 ]] && break
  sleep 0.05
done
[[ $status == 200 ]] || { cat "$work/models.json" >&2; exit 1; }
jq -e '.object == "list"' "$work/models.json" >/dev/null
echo "Helm clean install passed: migration hook, ready workloads, setup, control and gateway requests"
