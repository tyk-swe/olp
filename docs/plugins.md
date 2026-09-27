# Provider plugins

A provider plugin is an operator-installed extension that supplies a provider
profile's authentication and hosting around a built-in dialect. It never
defines a dialect: codecs and interaction contracts stay first-party. Plugins
are confined: OLP runs their code on [wazero](https://wazero.io) and grants
every capability they have. [ADR 0005](adr/0005-confined-provider-plugins.md)
records the design; the [authoring guide](plugin-authoring.md) covers writing
one.

This page covers installing, reviewing and approving plugins, creating
providers from their profiles, and uninstalling them.

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

- **profiles**, each naming the built-in dialect it serves and its hosting
  adaptation: the address its requests go to, at one of the plugin's origins,
  and the headers and query parameters it declares. A profile whose manifest
  entry has `signing: true` also runs the plugin's signing hook on every
  request;
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
authenticates with a static credential:

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
which OLP sets.

OLP runs the profile's hosting adaptation itself. It fills the declared headers
and query parameters from the static credential, such as
`Authorization: Token {credential}`, and sends the request over OLP's own
transport, with the provider's network options and the egress policy. The
credential is stored like any credential version, and every value that carries
it is redacted wherever upstream text is recorded.

### Signing hooks

A profile may declare a signing hook for an upstream whose keys must become
signatures or timestamped tokens. The hook is the only plugin code that runs
per request: once per upstream request, never per stream event, after hosting
has placed the request and its body is final. It receives the finished request
and the static credential, and returns headers to add, which OLP redacts like
the credential. Gateways run it for traffic, and control for probes and
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

No built-in kind's defaults apply to a plugin provider:

- **Endpoint:** the profile's address.
- **Discovery:** there is no upstream model listing. Declare models, such as
  the wizard's probe model; each is certified individually.
- **API-key header:** only the headers and query parameters the profile
  declares carry the credential.
- **Vendor prices:** a plugin provider has no vendor, so no list price applies.
  Its attempts are unpriced until a pricing revision carries a price scoped to
  that provider (`provider_kind: "plugin"` with its `provider_id`); a plugin
  price must name its provider.

A plugin profile changes only authorization, address and declared headers, so
its catalogue entry reports `strict: true` and it serves strict routes. A
signing hook changes only authorization, so a profile with one is strict too.

Moving a provider to another installed build of its plugin is an ordinary draft
change: choose the other digest, certify and activate. The new revision's diff
reports `plugin_changed`, and certification from the previous build does not
carry over.

A gateway serving from [mounted connectors](configuration.md#mounted-connectors)
mounts a plugin provider's static credential with `credential_file`. The
mounted `configuration` must match the published revision, including its
`profile_revision` and endpoint.

## Uninstalling

`DELETE /api/v1/plugins/{digest}`, with the current ETag in `If-Match`, deletes
the module and its approval. Installing the same module again starts over, with
approval pending.

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

The runtime currently grants a plugin a clock, randomness and logging, and
nothing else: no filesystem, network, environment or arguments. What a plugin
logs, including its standard output and standard error, reaches OLP's log with
the secret values of the call redacted, attributed by `plugin_digest` and
`plugin_method`.

## Permissions and audit

| Operation | Who |
| --- | --- |
| List and read plugins | Any role, and management tokens with `read` |
| Install, approve, uninstall | An owner with a user session |
| Create and change plugin providers | The `configure` scope, as for any provider |

Audit records `plugin.install`, `plugin.approve` and `plugin.uninstall` with the
owner as actor and the digest as resource. A repeated upload of an installed
digest records nothing.

Modules and manifests are stored in PostgreSQL in `olp.plugins`, so database
[backups](operations.md#backup-and-restore) include them.
