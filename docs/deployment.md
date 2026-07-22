# Production deployment

This document covers production deployment with the bundled Helm chart:
required infrastructure and secrets, edge routing rules, installation, and the
checks that must pass before the deployment receives traffic.

## Contents

- [Prerequisites and secrets](#prerequisites-and-secrets)
- [Edge routing](#edge-routing)
- [Install and verify](#install-and-verify)
- [Readiness checks](#readiness-checks)

## Prerequisites and secrets

The Helm chart requires Kubernetes 1.27 or newer, PostgreSQL 18, and durable
Valkey 9.1. It runs one immutable image in `gateway`, `control`, `worker`, and
migration modes. Pin an approved image by `image.digest`; do not use a mutable
development tag. Commands below run from the repository root.

Create the four long-lived Kubernetes Secrets selected by `config.*SecretName`
before installation. The defaults are:

| Purpose | Secret | Key |
|---|---|---|
| PostgreSQL URL | `olp-postgresql` | `url` |
| Valkey URL | `olp-valkey` | `url` |
| Master key | `olp-master-key` | `key` |
| Authentication HMAC key | `olp-auth-hmac-key` | `key` |

Existing installations that still use `olp-key-hash-key` must copy those exact
bytes to the new Secret and update the chart values before upgrading. Never
generate a replacement. Follow the byte-preserving procedure in the
[upgrade runbook](operations.md#naming-migration-prerequisites).

For a new installation, also create a 32-byte base64 bootstrap token Secret
and set `config.bootstrapTokenSecretName` and `config.bootstrapTokenSecretKey`.
Helm mounts it only into control pods; it is required until the first owner has
been created and is never exposed to gateway pods.

Keep secret values out of values files and shell history. Override names and
keys through `config` when required; `deploy/helm/values.yaml` documents the
fields and `values.schema.json` validates them.

## Edge routing

The console and management API share an origin; vendor SDK traffic terminates
at gateway pods. The optional Ingress preserves paths and applies this routing:

| Prefix | Service |
|---|---|
| `/openai`, `/anthropic`, `/gemini` | gateway |
| `/api`, `/` and console deep links | control |

Enable it only when the selected controller is trusted to terminate TLS:

```yaml
image:
  repository: ghcr.io/tyk-swe/olp
  digest: sha256:REPLACE_WITH_APPROVED_INDEX_DIGEST

config:
  publicOrigin: https://olp.example.com
  # Set false only after OIDC login is configured and verified.
  localLoginEnabled: true
  # CIDRs of the ingress/controller peers that append X-Forwarded-For.
  trustedProxyCidrs: 10.0.0.0/8
  bootstrapTokenSecretName: olp-bootstrap-token
  bootstrapTokenSecretKey: token

gateway:
  # Per-pod TCP admission cap with headroom for long-lived HTTP/1.1 streams
  # and concurrent unary traffic.
  httpMaxConnections: 16384

ingress:
  enabled: true
  className: nginx
  host: olp.example.com
  annotations: {}
  tls:
    enabled: true
    secretName: olp-tls
```

`config.publicOrigin` and `ingress.host` must identify the same trusted origin.
Set `config.localLoginEnabled: false` only after OIDC login is configured and
verified; the public capability endpoint then removes the password form.
The chart has no gateway catch-all and refuses to render the Ingress unless the
gateway and control Services are enabled. It also refuses to render an Ingress
without `config.trustedProxyCidrs`: public login, invitation, and OIDC limits
use the connection peer unless that peer is explicitly trusted to supply
`X-Forwarded-For`.

For Gateway API, a service mesh, or an external Ingress, leave
`ingress.enabled: false` and reproduce the table above. Preserve the Host,
scheme, path, streaming behavior, and client disconnects. Do not strip the
vendor or `/api` prefixes. Disable buffering for SSE and set request-size and
idle-timeout limits no lower than the application's bounded limits and longest
approved route deadline.

### Observability listener

Observability is a separate listener. `OLP_OBSERVABILITY_LISTEN_ADDR` defaults
to `127.0.0.1:9090` and exposes only `/health/live`, `/health/ready`, and
`/metrics`; the public listener returns 404 for all three paths. The chart
binds it to the pod network and creates internal ClusterIP
`*-observability` Services on port 9090. Kubelet probes use the matching
container port and optional ServiceMonitors select those Services; the bundled
Ingress intentionally has no health or metrics route.

The chart does not install a generic NetworkPolicy because the correct policy
depends on the kubelet, Prometheus, and CNI topology. Restrict access to the
internal observability Services with an installation-specific policy when your
network policy provider supports it.

### Connection capacity

`gateway.httpMaxConnections` and `control.httpMaxConnections` configure the
per-pod public-listener connection caps. Each proxied HTTP/1.1 SSE stream holds
one connection permit for its lifetime, so size the gateway value above the
largest expected per-pod stream count with room for unary requests. The chart's
gateway default is 16,384 connections per pod; the control-plane default
remains 1,024.

## Install and verify

Render and review the exact digest before applying:

```console
helm lint --strict deploy/helm
helm template olp deploy/helm --namespace olp \
  --set-string image.digest=sha256:REPLACE_WITH_APPROVED_INDEX_DIGEST \
  --set ingress.enabled=true \
  --set ingress.className=nginx \
  --set ingress.host=olp.example.com \
  --set-string config.trustedProxyCidrs=10.0.0.0/8 \
  --set config.publicOrigin=https://olp.example.com
```

Install the OCI chart with the version, image digest, and production values
approved for the deployment:

```console
helm upgrade --install olp \
  oci://ghcr.io/tyk-swe/charts/openllmproxy \
  --version 2.0.0 \
  --namespace olp \
  --create-namespace \
  --set-string image.digest=sha256:REPLACE_WITH_APPROVED_INDEX_DIGEST \
  --values production-values.yaml \
  --timeout 20m \
  --wait
```

The explicit timeout covers the chart's ten-minute migration deadline plus a
five-minute graceful pod drain and rollout headroom. Do not lower it below
those configured bounds.

## Readiness checks

Before issuing a proxy key or routing a client, require a successful migration
Job, ready pods, runtime-generation convergence, and—when monitoring is
enabled—healthy gateway and control ServiceMonitor targets.

Once the deployment is serving, continue with the monitoring, backup, and
upgrade procedures in the [operations runbook](operations.md).
