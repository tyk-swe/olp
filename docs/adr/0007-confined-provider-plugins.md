# Extend providers through confined plugins that never define dialects

Subscription and non-standard providers need authentication and hosting that
OpenLLMProxy cannot anticipate: custom OAuth and device flows, token exchanges,
onboarding calls, envelopes and per-request signatures. An owner installs a
provider plugin by uploading a WASM module and manifest, which OLP stores by
digest; provider revisions pin that digest, so upgrading a plugin is an ordinary
new revision. A plugin declares profiles that name a built-in dialect with its
hosting adaptation and authentication. Providers using them have their own
`plugin` provider kind, so no built-in kind's endpoints, discovery or vendor
list prices apply. A plugin never defines a dialect: OIF codecs and interaction
contracts stay first-party. This reverses the earlier rule that the connector
registry loads no executable configuration.

Plugins are confined by default. They run on wazero behind an OLP-owned,
versioned ABI with a Go SDK, and the host grants every capability: HTTP only to
origins declared in the manifest and approved at install, over the provider's
network path and egress policy; the plugin's own grant; clock, randomness and
redacted logging. Plugin code runs for grant enrollment steps, refresh and an
optional per-request signing hook, never per stream event. Hosting adaptation is
declarative and host-run (address, headers, envelope, declared body rewrites,
failure classification and model discovery), so what the operator approves is
exactly what runs and no sandbox sits on the event path.

An experimental unconfined tier serves plugins that need native privileges. An
unconfined plugin is a native executable in the image speaking the same ABI over
stdio. Only a deployment setting enables the tier, never the management API, and
an owner then grants each plugin after a high-risk warning. It may carry a
target's HTTP and SSE traffic itself, seeing all caller content; any failure it
does not report as not sent is an unknown upstream outcome, which blocks
failover.

A profile with no envelope, no body rewrites and OLP's own transport changes
only authorization, address and declared headers, so it may serve strict
routes. Any envelope, rewrite or plugin-carried traffic makes it transformed
only. The plugin ABI falls under ADR 0004, and OLP refuses a plugin built for
another ABI version at install rather than at request time. OLP ships only the
SDK and a reference plugin against a fake OAuth provider, and upstream terms of
use remain the operator's responsibility.

## Considered Options

- Trusted in-process or subprocess plugins by default: they could bypass egress
  policy, the secret authority and redaction while holding refresh tokens.
- Embedded JavaScript: easier to author, but without a hard memory limit.
- A declarative manifest alone: it cannot express Codex's non-standard device
  login or Gemini Code Assist onboarding.
- Plugin code per request and stream event: sandbox cost on every event, and
  declared rewrites could not be enforced.
- Extism: ready-made multi-language kits, but its Go host SDK has had no release
  since March 2025, and OLP needs its own host functions for egress anyway.
