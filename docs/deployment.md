# Production deployment

The bundled Helm chart deploys one immutable image in gateway, control,
worker, and migration modes. This guide covers production topology;
[`operations.md`](operations.md) covers monitoring, recovery, upgrades, and
incidents.

## Prerequisites and secrets

Use Kubernetes 1.27+, PostgreSQL 18, and durable Valkey 9.1. Pin an approved
OCI image digest; do not deploy a mutable development tag. Create these
Secrets before installing (names and keys are configurable through `config`):

| Purpose | Default Secret/key |
|---|---|
| PostgreSQL URL | `olp-postgresql` / `url` |
| Valkey URL | `olp-valkey` / `url` |
| Master keyring | `olp-master-key` / `key` |
| Authentication HMAC key | `olp-auth-hmac-key` / `key` |

Installations using `olp-key-hash-key` must copy the exact bytes to the new
HMAC Secret before upgrading; follow
[`operations.md#naming-migration-prerequisites`](operations.md#naming-migration-prerequisites).
New installations also need a 32-byte base64 bootstrap-token Secret mounted
only into control pods. Keep all secret values out of values files and shell
history; the chart schema validates configured names and keys.

### Shared Valkey and workers

The PostgreSQL installation UUID supplies the Valkey namespace, so independent
installations may share a logical database without key collisions. A restored
database retains its identity and is a replacement, not a clone; use a fresh
Valkey database for rehearsals and never run source and restore together.

The chart defaults to one worker for a small footprint. Production should use
three replicas, a PodDisruptionBudget, and failure-domain spreading. Workers
consume work concurrently; PostgreSQL advisory locking serializes runtime
outbox publication and Valkey consumer groups reclaim metadata ownership. The
worker Deployment uses `Recreate`: mixed-version workers are not supported
through the namespace transition.

## Release artifacts and verification

Published releases use two public GHCR packages: `ghcr.io/tyk-swe/olp` for
the multi-architecture image and `ghcr.io/tyk-swe/charts/openllmproxy` for the
Helm chart. Image tags `2.2.0`, `2.2`, and `latest` identify the same index at
publication. The versioned tag supports the Compose quick start and `latest`
is a convenience alias; production installations must pin the index digest.
The chart is selected independently with `--version 2.2.0`.

GitHub creates the first version of each GHCR package as private. On the first
release, a maintainer must open the package settings for both `olp` and
`charts/openllmproxy`, change their visibility to **Public**, and rerun the
failed release jobs. The workflow creates no GitHub Release until fresh,
unauthenticated runners pull the image, render the chart, and verify both
signatures. Publishing with the repository `GITHUB_TOKEN` and the OCI source
label links the packages to this repository, but does not make them public.

The `v2.2.0` image and chart digests below are deliberately marked for
replacement because they do not exist until the release workflow publishes
them. After publication, resolve the versioned references:

```console
docker buildx imagetools inspect ghcr.io/tyk-swe/olp:2.2.0
helm pull oci://ghcr.io/tyk-swe/charts/openllmproxy --version 2.2.0
```

Both commands report a `Digest:`. Replace each placeholder with the matching
registry-reported digest, then verify the exact OCI artifacts with cosign:

```console
cosign verify \
  --certificate-identity 'https://github.com/tyk-swe/olp/.github/workflows/release.yml@refs/tags/v2.2.0' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
  'ghcr.io/tyk-swe/olp@sha256:REPLACE_WITH_V2_2_0_INDEX_DIGEST'

cosign verify \
  --certificate-identity 'https://github.com/tyk-swe/olp/.github/workflows/release.yml@refs/tags/v2.2.0' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
  'ghcr.io/tyk-swe/charts/openllmproxy@sha256:REPLACE_WITH_V2_2_0_CHART_DIGEST'
```

The chart digest is the OCI manifest digest reported by `helm push`, not the
SHA-256 checksum of the downloaded `.tgz` release asset.

## Edge routing

Route the shared origin as follows, preserving prefixes, streaming, and client
disconnects:

| Prefix | Service |
|---|---|
| `/v1`, `/openai`, `/anthropic`, `/gemini` | gateway |
| `/api`, `/`, and console deep links | control |

Example values:

```yaml
image:
  repository: ghcr.io/tyk-swe/olp
  digest: sha256:REPLACE_WITH_V2_2_0_INDEX_DIGEST
config:
  publicOrigin: https://olp.example.com
  localLoginEnabled: true
  trustedProxyCidrs: 10.0.0.0/8
  bootstrapTokenSecretName: olp-bootstrap-token
  bootstrapTokenSecretKey: token
ingress:
  enabled: true
  className: nginx
  host: olp.example.com
  tls:
    enabled: true
    secretName: olp-tls
```

`config.publicOrigin` and `ingress.host` must identify the same trusted
origin. Disable local login only after OIDC is verified. For Gateway API or a
mesh, leave chart Ingress disabled and reproduce the same routing table.
Disable buffering for SSE and do not lower request-size or idle-timeout
bounds.

## Observability and capacity

`OLP_OBSERVABILITY_LISTEN_ADDR` exposes only `/health/live`, `/health/ready`,
and `/metrics` on the pod network. The chart creates internal
`*-observability` ClusterIP Services on port 9090; the public Ingress has no
health or metrics route. Add an installation-specific NetworkPolicy for the
kubelet and Prometheus topology.

Per-pod TCP caps default to 16,384 gateway and 1,024 control connections.
In-flight work is separate: gateway/control inference pools default to 256
and management pools to 32. Each permit lasts through streaming completion or
cancellation; a full pool returns HTTP 503 with `Retry-After: 1` instead of
queueing. Size limits from CPU, memory, provider connections, and stream
duration.

## Install and verify

Render the exact configuration before applying it:

```console
helm lint --strict deploy/helm
helm template olp deploy/helm --namespace olp \
  --set-string image.digest=sha256:REPLACE_WITH_V2_2_0_INDEX_DIGEST \
  --set ingress.enabled=true --set ingress.className=nginx \
  --set ingress.host=olp.example.com \
  --set-string config.trustedProxyCidrs=10.0.0.0/8 \
  --set config.publicOrigin=https://olp.example.com
```

Install with approved values and at least a 20-minute timeout:

```console
helm upgrade --install olp \
  oci://ghcr.io/tyk-swe/charts/openllmproxy --version 2.2.0 \
  --namespace olp --create-namespace \
  --set-string image.digest=sha256:REPLACE_WITH_V2_2_0_INDEX_DIGEST \
  --values production-values.yaml --timeout 20m --wait
```

## Readiness checks

Before issuing a proxy key or sending traffic, require a successful migration
Job, ready pods, runtime-generation convergence, and healthy observability
targets. With replicated workers also require all four
`olp_worker_task_healthy` series, zero request-metadata pending/lag, and zero
runtime-outbox pending/claimed rows. Continue with the monitoring and recovery
checks in [`operations.md`](operations.md).
