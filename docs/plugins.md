# Provider plugins

A provider plugin is an operator-installed extension that supplies a provider
profile's authentication and hosting around a built-in dialect. It never
defines a dialect: codecs and interaction contracts stay first-party. Plugins
are confined: OLP runs their code on [wazero](https://wazero.io) and grants
every capability they have, unless a deployment enables the experimental
[unconfined tier](#unconfined-plugins-experimental).
[ADR 0005](adr/0005-confined-provider-plugins.md) records the design; the
[authoring guide](plugin-authoring.md) covers writing one.

This page covers installing, reviewing and approving plugins, creating
providers from their profiles, enrolling grants, permitting unconfined
plugins, and uninstalling them.

## Upstream terms of use

Some upstreams, including Anthropic and Google, prohibit third-party use of
their subscription sign-in. OLP stays neutral: it ships only the SDK and a
reference plugin against a fictional upstream, and **whether a plugin may be
used with an upstream is the operator's responsibility** under that upstream's
terms of use.

## Installing

An owner installs a plugin from the console's **Plugins** page, or with
`POST /api/v1/plugins` carrying the module as an `application/wasm` body. A
module is at most 32 MiB, and an installation holds at most 64 plugin digests.
Installation needs an owner signed in with a user session, like management-token
administration; management tokens cannot install plugins.

OLP stores the module by the SHA-256 digest of its bytes, which identifies the
plugin from then on. Before storing it, OLP instantiates the module within the
plugin limits and reads the manifest it declares. It refuses the upload with a
`422` problem naming the reason:

| Code | Reason |
| --- | --- |
| `plugin_module_invalid` | Not WebAssembly, not a plugin, a WASI command instead of a reactor, or it imports something OLP does not provide. |
| `plugin_abi_unsupported` | Built for another plugin ABI version. During 0.x the ABI carries no compatibility promise ([ADR 0004](adr/0004-no-compatibility-promises-during-0x.md)); rebuild the plugin with the SDK of this OLP version. |
| `plugin_manifest_invalid` | The manifest is invalid or declares something this OLP does not understand. The problem's `errors` names the field, such as `manifest.origins[0]` or `manifest.profiles[0].hosting.headers.Authorization`. |
| `plugin_dialect_unknown` | A declared profile names a dialect plugin profiles can't serve, such as `manifest.profiles[0].dialect`. |
| `plugin_timed_out`, `plugin_failed` | The module exceeded its time limit, or trapped, exited or exhausted its memory, while starting or reporting its manifest. |

Uploading a digest that is already installed changes nothing and returns it.
Several digests of the same plugin (the manifest `name`) may be installed side by
side, so a new build can be installed and reviewed before anything moves to it.

## Reviewing and approving

`GET /api/v1/plugins` and the Plugins page show each installed digest with what
its manifest declares:

- **profiles**, each naming the built-in dialect it serves, whether providers
  using it authenticate with a static credential or a
  [grant](#grant-enrollment), the options its providers set, and its hosting
  adaptation: the address its requests go to, at one of the plugin's origins,
  the headers and query parameters it declares, any model listing and failure
  classification, any envelope and rewrites of the dialect's bodies, and
  whether its upstream serves only streaming requests. A profile whose
  manifest entry has `signing: true` also runs the plugin's signing hook on
  every request;
- **origins**, the only `scheme://host[:port]` origins the plugin may ever
  reach.

An installed plugin can't be used until an owner approves its origins. Approval
covers exactly the declared origins: `POST /api/v1/plugins/{digest}/approve`
takes the plugin's current ETag in `If-Match` and lists the origins being
approved, and OLP refuses the approval with `plugin_origins_mismatch` unless the
list matches the declaration. A digest's manifest never changes, so an approval
never widens; a new digest of the same plugin needs its own approval.

## Providers from plugin profiles

The profiles of approved plugins appear in `GET /api/v1/provider-profiles` under
the `plugin` provider kind. A plugin profile's `revision` is the digest of the
plugin module that supplies it, and its `plugin` names that plugin and version.
In the console's provider wizard, choose **Provider plugin**, then the profile
and plugin build.

A provider of the `plugin` kind pins the digest as its profile revision and
authenticates as its profile declares: with a static credential, or with a
[grant](#grant-enrollment) (`"auth_mode": "grant"`):

```json
{
  "kind": "plugin",
  "auth_mode": "static_credential",
  "profile_id": "reference-chat",
  "profile_revision": "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
}
```

Pinning needs an installed plugin whose origins an owner approved:
`plugin_not_installed`, `plugin_not_approved` and `plugin_profile_unknown`
refuse the draft otherwise. The provider's endpoint is the profile's address,
with the provider's options in place, which OLP sets.

OLP runs the profile's hosting adaptation itself. It fills the declared headers
and query parameters from the static credential, or from a grant's access token
and grant facts, and the provider's [options](#options), such as
`Authorization: Token {credential}`, and sends the request over OLP's own
transport, with the provider's network options and the
egress policy. The credential is stored like any credential version, and every
value that carries it is redacted wherever upstream text is recorded.

A profile may also declare an envelope and rewrites for an upstream that speaks
the dialect inside a JSON object of its own, or needs request members set or
removed. OLP applies exactly what the profile declares, around its built-in
codecs: it rewrites the prepared dialect request, which the route inspector's
effective request shows, wraps it in the envelope, such as
`{"model": "…", "request": {…}}`, and unwraps each successful response and
stream event, such as `{"response": {…}}`. Codecs, content policy and usage
accounting see only plain dialect bodies. The
[authoring guide](plugin-authoring.md#envelopes-and-rewrites) describes the
declarations.

A Responses profile may also force streaming, for an upstream that accepts only
streaming requests. OLP sends every request as a streaming one and aggregates
the stream into the non-streaming result for callers that did not ask to
stream, within the gateway's response size limit; their usage and prices are
those of the equivalent streaming request. See
[Upstreams that serve only streams](plugin-authoring.md#upstreams-that-serve-only-streams).

No built-in kind's defaults apply to a plugin provider:

- **Endpoint:** the profile's address, with the provider's options in place.
- **Discovery:** only the profile's declared model listing, if it declares one.
  The probe and discover flows then list the upstream's models, following its
  pages, as for built-in kinds, and the catalogue entry reports
  `model_discovery: true`. Without one, declare models, such as the wizard's
  probe model; each is certified individually.
- **API-key header:** only the headers and query parameters the profile
  declares carry the credential.
- **Vendor prices:** a plugin provider has no vendor, so no list price applies.
  Its attempts are unpriced until a pricing revision carries a price scoped to
  that provider (`provider_kind: "plugin"` with its `provider_id`); a plugin
  price must name its provider.

A profile may also declare how its upstream's failures are classified. The
first declared rule matching a failure decides whether it is a credential
failure, a rate limit, retryable or terminal, which governs
[failover and cooldown](provider-routing.md) as it does for built-in classes;
for example, a `400` carrying the upstream's own quota code can cool the slot
and fail over like a `429`. The built-in rules classify every failure no rule
matches. The declarations apply to gateway attempts and probes alike.

A plugin profile that changes only authorization, address and declared headers
reports `strict: true` in the catalogue and serves strict routes. A
[signing hook](#signing-hooks) changes only authorization, so a profile with one
is strict too. An envelope or any rewrite changes the dialect's bodies, and
forced streaming changes how non-streaming requests reach the upstream, so such
a profile reports `strict: false` and serves only
[transformed routes](provider-routing.md#route-fidelity): validating or
activating a strict route with a target using it fails with
`422 target_capability`, telling you to declare the route transformed.

Moving a provider to another installed build of its plugin is an ordinary draft
change: choose the other digest, certify and activate. The new revision's diff
reports `plugin_changed`, and certification from the previous build does not
carry over.

A gateway serving from [mounted connectors](configuration.md#mounted-connectors)
mounts a plugin provider's static credential with `credential_file`. The
mounted `configuration` must match the published revision, including its
`profile_revision`, endpoint and plugin options. A mounted gateway refuses a provider that
authenticates with a grant.

### Options

A plugin profile may declare options: non-secret settings each provider using
it sets, such as an account ID, a region or a project. The profile's catalogue
entry describes them in `options_schema`, the JSON Schema of the provider's
values, and the provider wizard shows them in the Connection stage once the
profile is chosen. A provider sets them in `options.plugin_options`:

```json
{
  "kind": "plugin",
  "auth_mode": "static_credential",
  "profile_id": "reference-workspace-chat",
  "profile_revision": "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
  "options": { "plugin_options": { "workspace": "acme" } }
}
```

OLP validates the values against the declaration when the draft is saved and
refuses a missing required option, an undeclared one, or a value outside its
enum or pattern with `validation_failed`, naming the option as
`configuration.options.plugin_options.<name>`. An empty value leaves an option
unset. The hosting adaptation places the values in the address, headers,
query parameters and envelope fields it declares, and every call OLP makes to
the plugin on behalf of the provider, such as its signing hook or its
[grant enrollment](#grant-enrollment) steps, carries them.

Options are part of the provider's configuration like any other setting:
changing them is a draft change whose certification starts over, the revision
diff reports `plugin_options_changed`, and configuration exports and imports
carry them. They are not secret.

### Signing hooks

A profile may declare a signing hook for an upstream whose keys must become
signatures or timestamped tokens. The hook is the only plugin code that runs
per request: once per upstream request, never per stream event, after hosting
has placed the request and its body is final. It receives the finished request,
the static credential or the grant's access token and the provider's
[options](#options), and returns headers to add, which OLP redacts like the
credential. Gateways run it for traffic, and control for probes and
certification. A signing profile need not place the credential at all.

A hook that fails, exceeds the plugin limits or returns a header OLP refuses
fails the attempt before anything is sent: the attempt records a credential
failure, the slot cools down, and the route fails over. A probe reports
`credential_invalid` with the reason.

Each process keeps the modules of the plugins it signs with compiled, by digest,
with a bounded pool of instances, so no request compiles or instantiates
anything. The first request a process signs with a plugin build loads the
module from the database and compiles it, which takes a few seconds for a
typical Go plugin. After that, signing adds about a millisecond per request for
a small body, growing with the body's size.

## Grant enrollment

Some upstreams, such as subscription backends, authorize an account rather than
issue an API key, and rotate that authorization. A profile that declares a
grant authenticates providers with one: rotating upstream authorization the
plugin obtains when an operator signs in to the upstream account, which OLP
holds beneath an ordinary, immutable credential version
([ADR 0006](adr/0006-grants-beneath-immutable-credentials.md)).

In the provider wizard's Connection stage, choosing such a profile replaces the
credential field with grant enrollment. After saving the draft:

1. OLP shows the authorization URL the plugin builds, with its state and PKCE
   challenge, at one of the plugin's approved origins.
2. The operator opens it, in any browser, and signs in upstream.
3. The operator pastes back what the upstream returns: the whole callback URL
   the browser was sent to (loopback paste-back), which may not load, or the
   code the upstream displays (out-of-band code).
4. The plugin exchanges it for a grant. OLP creates a credential version,
   stages it on the draft's default credential slot like a rotated credential,
   and the wizard tests the connection with it.

Through the management API, which needs the `configure` scope:

| Operation | Request |
| --- | --- |
| Start | `POST /api/v1/providers/{id}/grant-enrollments` with the draft's ETag in `If-Match`; returns the enrollment's `id`, `authorization_url` and `expires_at`. |
| Continue | `POST /api/v1/providers/{id}/grant-enrollments/{enrollment_id}/continue` with `{"input": "<callback URL or code>"}`; returns the new `credential_id`, `credential_version` and observed `principal`. |
| Cancel | `DELETE /api/v1/providers/{id}/grant-enrollments/{enrollment_id}` |

A grant enrollment lasts 10 minutes. Its session state, such as the PKCE
verifier, is stored encrypted in the database, so any control replica can
continue it. Only the principal that started it continues or cancels it, and it
is continued once, whatever the outcome. Continuing fails with:

| Code | Reason |
| --- | --- |
| `grant_state_mismatch` | What was pasted back answers another sign-in than this enrollment's. |
| `grant_enrollment_expired` (`410`) | The enrollment expired. |
| `grant_enrollment_used` (`409`) | The enrollment was already continued. |
| `grant_enrollment_stale` (`409`) | The provider's plugin profile or credential slot changed meanwhile. |
| `grant_enrollment_failed` | The plugin or the upstream refused, such as an authorization code that expired; the detail carries the plugin's reason. |
| `plugin_*` | The plugin is no longer usable, or exceeded its limits. |

In each case, start another grant enrollment. The worker's maintenance purges
expired enrollments: their session state as they expire, and their record an
hour later.

The credential version records the plugin digest, the observed principal (the
upstream account the plugin reports) and the grant facts, which
`GET /api/v1/providers/{id}/credentials` shows under `grant`, with the access
token's expiry. The grant itself, its access token and refresh token, is
encrypted beneath the version and never leaves OLP. Gateways receive only the
access token, through the credential source; the refresh token is stored under
a secret purpose that gateway code never reads. The access token serves until it
expires. A pasted credential can't be staged for a provider that authenticates
with a grant, and activation refuses a credential slot whose version doesn't
match the provider's authentication.

## Uninstalling

`DELETE /api/v1/plugins/{digest}`, with the current ETag in `If-Match`, deletes
the module and its approval. Installing the same module again starts over, with
approval pending. Uninstalling an unconfined plugin withdraws its permission;
its executable stays in the image.

A plugin stays installed while a provider pins it: uninstalling is refused with
`409 plugin_pinned`, naming the providers, while any provider's draft or any of
its published revisions pins the digest. Published revisions never change, so a
digest that a provider has published stays installed, which keeps every
revision restorable and its retained resources servable.

## Confinement and limits

Plugin calls run on instances of the module within these limits:

| Limit | Value |
| --- | --- |
| Linear memory per instance | 64 MiB |
| Instances of a module at once | 4 |
| Time per call, including waiting for and instantiating an instance | 10 seconds |
| Message from the plugin | 1 MiB |
| Log output per call | 16 KiB, 2 KiB per message or attribute |

A call takes an idle instance, or instantiates the module when none is idle,
and returns it for later calls. A call that exceeds a limit fails with
`plugin_timed_out` or `plugin_failed`, and its instance is discarded, so it
leaves nothing behind; the OLP process is unaffected.

Installing reads a manifest once, on wazero's interpreter, which is ready
soonest. Signing hooks run on modules compiled to machine code, which take
longer to prepare but then sign about ten times faster; `BenchmarkSign` in
`internal/plugins` measures both.

The runtime grants a plugin a clock, randomness and logging, and nothing else:
no filesystem, environment or arguments. Grant enrollment steps are also granted
HTTP, which reaches only the plugin's approved origins, over the provider's
network path (its proxy, trust roots and network credential) and the egress
policy; a redirect comes back to the plugin rather than being followed. What a
plugin logs, including its standard output and standard error, reaches OLP's
log with the secret values of the call redacted, attributed by `plugin_digest`
and `plugin_method`.

## Unconfined plugins (experimental)

Some plugins need what confinement withholds, such as a native library or an
upstream's own client. A deployment may enable the experimental unconfined
tier for them. An unconfined plugin is a native executable in the deployment's
image, which OLP runs as a subprocess with the operating system privileges of
its own processes, speaking the same ABI over standard input and output (see
the [authoring guide](plugin-authoring.md#unconfined-plugins)). Nothing
confines it: it receives the credentials and requests OLP hands it, can read
whatever OLP's processes can, and reaches any network outside the egress
policy. Enable the tier only for executables you trust as much as OLP itself.

### Enabling the tier

Only a deployment setting enables the tier, never the management API:
`OLP_UNCONFINED_PLUGIN_DIR` (flag `--unconfined-plugin-dir`, Helm
`config.unconfinedPluginDir`, Compose `OLP_UNCONFINED_PLUGIN_DIR`) names the
absolute directory of the image that holds the executables. Set it for every
process: control reviews and permits plugins and runs them for probes and
certification, and gateways run them for traffic.

The release image holds no plugins, so build an image that adds them. It is
distroless, so build each plugin as a static executable:

```dockerfile
FROM ghcr.io/tyk-swe/olp:0.1.0
COPY --chmod=0555 acme /opt/olp/plugins/acme
```

OLP lists the regular files in the directory that have an execute permission
and a name of 1–128 letters, digits, dots, underscores and hyphens, starting
with a letter or digit.

### Reviewing and permitting

The Plugins page's **Unconfined plugins** section shows whether the deployment
enables the tier. Where it does, an owner sees each executable by name, digest
and size, reviews one, which runs it to read the manifest it declares, and
permits it after acknowledging the high-risk warning and confirming their
identity.

| Operation | Purpose |
| --- | --- |
| `GET /api/v1/unconfined-plugins` | Lists the executables, without running them, and whether each build is permitted; `503 unconfined_plugin_dir_unreadable` when OLP can't read the directory. |
| `GET /api/v1/unconfined-plugins/{executable}` | Runs the executable and returns its digest and manifest. |
| `POST /api/v1/unconfined-plugins/{executable}/permit` | Permits the build with the digest the owner reviewed: `{"digest": "…", "acknowledge_risk": true}`. |

Reviewing refuses an executable as installing refuses a module, with a `422`
problem: `plugin_executable_invalid` when it can't be started or doesn't speak
the ABI over standard input and output, and otherwise the codes listed under
[Installing](#installing). Permitting takes:

- an owner with a user session, as every plugin change does;
- `acknowledge_risk: true`, the owner's acknowledgement that the plugin runs
  unconfined;
- a recent authentication for the `plugin_permit` purpose, by password
  (`POST /api/v1/profile/reauthenticate`) or linked OIDC identity, within the
  last five minutes, which permitting spends; `428 reauthentication_required`
  otherwise;
- the reviewed build: OLP runs the executable again and refuses with
  `409 plugin_executable_changed` if its digest differs from the one reviewed.

A permitted build is an installed plugin, approved in the same step: it
appears in `GET /api/v1/plugins` with its `executable`, its profiles appear in
the catalogue with `plugin.unconfined: true`, and provider revisions pin its
digest as they pin a module's. A new build of the executable has a new digest,
which an owner permits again.

### Running

A process starts an unconfined plugin's executable for the plugin's first
call, with no arguments and an empty environment, in the unconfined plugin
directory, after checking that the executable still has the permitted digest
(`plugin_executable_changed` otherwise). One subprocess then serves every call
the process makes to the plugin, concurrently, and keeps running between them.
Every call the confined runtime serves works through it, such as a profile's
signing hook or a grant enrollment step.

- A call is answered within 10 seconds, including starting the subprocess, or
  fails with `plugin_timed_out`. A plugin that leaves a call unanswered that
  long may be stuck, so OLP stops it, and the calls in flight on it fail with
  `plugin_failed`. This holds for a call whose caller gave up, such as a
  request that ended: OLP tells the plugin, which still answers it.
- A subprocess that exits fails its calls in flight with `plugin_failed`,
  naming its exit status, and so does one that writes a message over 1 MiB or
  anything else the ABI doesn't allow.
- OLP starts a stopped plugin again for its next call.
- OLP bounds no unconfined plugin's memory.

What the plugin logs for a call is attributed to it and redacted of the call's
secret values, within the same bounds as a confined plugin's. Its standard
error, and records it logs without a call, are logged a line at a time with
the plugin's digest, redacted of the secret values of every call in flight.

### Without the tier

A deployment that doesn't enable the tier, including one whose setting was
removed after plugins were permitted:

- lists no unconfined plugin or executable: `GET /api/v1/unconfined-plugins`
  and its paths answer `404 unconfined_plugins_disabled`, and the console shows
  the tier disabled;
- offers no unconfined plugin's profiles, and refuses to pin one with
  `422 plugin_unconfined_disabled`;
- refuses to activate a provider revision that pins one, with
  `422 plugin_unconfined_disabled`;
- serves none of their targets: a gateway plans them ineligible with reason
  `plugin_unconfined_disabled`, and a route with no other eligible target
  answers `503`.

Permissions stay recorded, so enabling the tier again restores them.

## Permissions and audit

| Operation | Who |
| --- | --- |
| List and read plugins | Any role, and management tokens with `read` |
| Install, approve, uninstall | An owner with a user session |
| List, review and permit unconfined plugins | An owner with a user session; permitting also takes a recent authentication |
| Create and change plugin providers | The `configure` scope, as for any provider |
| Enroll grants | The `configure` scope |

Audit records `plugin.install`, `plugin.approve`, `plugin.permit` and
`plugin.uninstall` with the owner as actor and the digest as resource. A
repeated upload of an installed digest records nothing. It records
`provider.grant.enroll` for every continuation that reaches the plugin: a
success with the new credential version as resource, a failure with the
provider. Audit never records what was pasted back or obtained.

Modules and manifests are stored in PostgreSQL in `olp.plugins`, so database
[backups](operations.md#backup-and-restore) include them. Unconfined plugins'
executables are not: they belong to the deployment's image.
