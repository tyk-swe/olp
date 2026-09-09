#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
version=1.27.0
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
for invalid in 'config.databaseMaxConnections=0' 'config.httpMaxJsonBodyBytes=0' \
  'gateway.replicas=-1' 'ingress.enabled=true,config.trustedProxyCidrs=' \
  'networkPolicy.enabled=true'; do
  if helm template olp deploy/helm --set "$invalid" >/dev/null 2>&1; then
    echo "Expected invalid Helm configuration to fail: $invalid" >&2
    exit 1
  fi
done
echo 'Helm profiles and invalid-input checks passed.'
