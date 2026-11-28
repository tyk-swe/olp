# Runtime

## Runtime and immutable sources

Published releases freeze configuration and credential bytes. A `routing.View`
keeps published maps private and returns detached selected values while reusing
compiled contracts. Serving never copies complete snapshots per request. Live
authority refresh progresses independently with monotonic freshness and
generation fences. Historical remote-secret lookup occurs outside short
authorized database transactions, followed by revocation revalidation.

Worker health belongs to `platform/workerhealth`. Reporters receive their task
and region explicitly and can write through a caller-owned transaction.
Application shutdown waits for every registered worker, including workflow
recovery, before closing its dependencies.

Routing has no runtime-manager or database dependency. `Snapshot.Validate`
checks configuration without replacing compiled contracts, and
`Snapshot.Compile` publishes a complete set of derived contracts only after
every target succeeds. Runtime owns installation and live credentials.
Management boundaries translate routing validation failures to HTTP problems.

`Snapshot.Clone` and `connectors.Config.Clone` give new owners independent
mutable configuration, while immutable plugin profiles may be shared. Connector
configuration constructs serving identities for every operation. Provider
management reads health statistics from observability, which the gateway can
report without depending on provider management.

`routing/revision.go` owns pure revision decoding. `routing/release_pin.go`
verifies exact historical provider, route, target, slot and credential identity
for retained and media work. Ambiguous migration backfills remain null and
refuse rather than substituting current configuration. The runtime credential
source owns current and historical resolution, while grant eligibility remains
live. `resources.Resolver` returns the proved historical target as one value.
`providerinvoke` binds that target to current credentials, network secrets and
its connection scope. Resource revision and release-snapshot decoders remain
separate, as do consumer-specific eligibility and timeout rules. Caller
credentials use ephemeral connections.

OIF sources retain immutable JSON spans, presence and numeric provenance without
provider or storage dependencies. `wire` owns neutral envelopes and format
names, and codecs own parsing, validation and encoding. Request transformations
return a new request or an immediate error and preserve the original source.
Decoders reject duplicate keys, invalid UTF-8 and surrogates, and accessors copy
or expose explicit immutable views. Protocol owners validate bounded native
events and terminal proof, and local projection cannot invent provider
completion.

## Process modes

`all` serves both the management API and the inference endpoints. `control`
serves management only. `gateway` serves inference only and needs the database
and `OLP_AUTH_HMAC_KEY_FILE` for authority. Database-encrypted provider
credentials also require `OLP_MASTER_KEY_FILE`. A gateway using
`OLP_CONNECTOR_CONFIG_FILE` may omit the master key for mounted default-slot
credentials, though enabled named pools require it. `worker` publishes no public
listener and runs only the accounting, media reconciliation and
[recovery plane](../operate/operations.md#replicated-worker-health), which `all`
also runs. An installation that serves traffic with `gateway` and `control`
needs at least one `worker` replica for accounting, budget reconciliation,
retention, media job polling and plugin grant refresh to run. Gateway readiness
on the private `GET /health/ready` listener fails while the key-authority
snapshot is missing or stale.

`delivery.Delivery` (`internal/usage/delivery`) owns one producer identity,
bounded intake, durable registration, writer, loss reporting and accounting
readiness. Startup registers it before spending producers run. Shutdown drains
requests and probes while runtime authority remains available, then closes and
flushes delivery before stopping consumers. Failed producer drains mark the
epoch unclean.

## Runtime publication and authority

Activation validates and records one immutable numbered runtime release with a
SHA-256 digest. Gateways install only complete valid releases with decryptable
credentials, and a failed installation keeps the previous release.

Each route pins strict or transformed fidelity. Strict targets require admitted
operation contracts and refuse redaction, while transformed routes translate and
may redact. New revisions switch fidelity, while retained resources keep
creation contracts. See
[route fidelity](../use/provider-routing.md#route-fidelity).

Key and credential authority refreshes independently every five seconds and is
stale after 60 seconds from the start of its last successful read in monotonic
time. New work then receives `503 authority_unavailable`. Admitted ordinary
streams may finish, while realtime rechecks. Revoked or lapsed credential
versions remain denied even under historical releases, with no attempt spent for
skipped slots. Plan skip reasons use `credential_revoked`, `credential_lapsed`,
`credential_stale_authority` or `credential_external_secret_unavailable`, with
network, session and media-specific equivalents.
