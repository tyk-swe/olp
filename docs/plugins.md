# Provider plugins

A provider plugin is an operator-installed extension that supplies a provider
profile's authentication and hosting around a built-in dialect. It never
defines a dialect: codecs and interaction contracts stay first-party. Plugins
are confined: OLP runs their code on [wazero](https://wazero.io) and grants
every capability they have. [ADR 0005](adr/0005-confined-provider-plugins.md)
records the design; the [authoring guide](plugin-authoring.md) covers writing
one.

This page covers installing, reviewing, approving and uninstalling plugins.
Providers cannot use plugin profiles yet.

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
| `plugin_manifest_invalid` | The manifest is invalid or declares something this OLP does not understand. The problem's `errors` names the field, such as `manifest.origins[0]`. |
| `plugin_dialect_unknown` | A declared profile names a dialect OLP doesn't have, such as `manifest.profiles[0].dialect`. |
| `plugin_timed_out`, `plugin_failed` | The module exceeded its time limit, or trapped, exited or exhausted its memory, while starting or reporting its manifest. |

Uploading a digest that is already installed changes nothing and returns it.
Several digests of the same plugin (the manifest `name`) may be installed side by
side, so a new build can be installed and reviewed before anything moves to it.

## Reviewing and approving

`GET /api/v1/plugins` and the Plugins page show each installed digest with what
its manifest declares:

- **profiles**, each naming the built-in dialect it serves;
- **origins**, the only `scheme://host[:port]` origins the plugin may ever
  reach.

An installed plugin can't be used until an owner approves its origins. Approval
covers exactly the declared origins: `POST /api/v1/plugins/{digest}/approve`
takes the plugin's current ETag in `If-Match` and lists the origins being
approved, and OLP refuses the approval with `plugin_origins_mismatch` unless the
list matches the declaration. A digest's manifest never changes, so an approval
never widens; a new digest of the same plugin needs its own approval.

## Uninstalling

`DELETE /api/v1/plugins/{digest}`, with the current ETag in `If-Match`, deletes
the module and its approval. Installing the same module again starts over, with
approval pending.

## Confinement and limits

Every plugin call runs on a fresh instance of the module, which is discarded
afterwards, within these limits:

| Limit | Value |
| --- | --- |
| Linear memory per instance | 64 MiB |
| Time per call, including instantiation | 10 seconds |
| Message across the ABI | 1 MiB |
| Log output per call | 16 KiB, 2 KiB per message or attribute |

A call that exceeds a limit fails with `plugin_timed_out` or `plugin_failed` and
leaves nothing behind; the OLP process is unaffected.

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

Audit records `plugin.install`, `plugin.approve` and `plugin.uninstall` with the
owner as actor and the digest as resource. A repeated upload of an installed
digest records nothing.

Modules and manifests are stored in PostgreSQL in `olp.plugins`, so database
[backups](operations.md#backup-and-restore) include them.
