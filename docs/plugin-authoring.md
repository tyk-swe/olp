# Writing a provider plugin

A [provider plugin](plugins.md) supplies a provider profile's authentication and
hosting around one of OLP's built-in dialects. This guide covers writing one with
the Go SDK, `github.com/tyk-swe/olp/sdk/plugin`, and documents the ABI the SDK
implements. The reference plugin in
[`sdk/plugin/reference`](../sdk/plugin/reference/main.go) is a complete,
minimal plugin to start from.

Plugins build with the Go version in OLP's `go.mod`. During 0.x the plugin ABI
carries no compatibility promise
([ADR 0004](adr/0004-no-compatibility-promises-during-0x.md)): build a plugin with
the SDK from the OLP version that will run it, because OLP refuses a module built
for another ABI version at install.

## A minimal plugin

A plugin is a `main` package that registers its implementation from `init` and
has an empty `main`:

```go
package main

import "github.com/tyk-swe/olp/sdk/plugin"

type acme struct{}

func (acme) Manifest() plugin.Manifest {
	return plugin.Manifest{
		Name:    "acme",
		Version: "1.0.0",
		Origins: []string{"https://api.acme.example"},
		Profiles: []plugin.Profile{{
			ID: "acme-chat", Label: "Acme Chat Completions", Dialect: "openai-chat",
			Hosting: plugin.Hosting{
				Address: "https://api.acme.example/v1",
				Headers: map[string]string{"Authorization": "Token {credential}"},
			},
		}},
	}
}

func init() { plugin.Register(acme{}) }

func main() {}
```

Build it as a WASI preview 1 reactor, which OLP initializes once per instance
and then calls:

```sh
GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o acme.wasm .
```

A module built without `-buildmode=c-shared` is a WASI command, which OLP
refuses. Install the result from the console's Plugins page.

## The manifest

OLP calls `Manifest` once, at install, and stores the result with the module's
digest. It never changes for that digest, so bump `Version` and install a new
build to change it.

| Field | Rule |
| --- | --- |
| `Name` | 1–64 lowercase letters, digits and hyphens, starting and ending with a letter or digit. Builds sharing a name are digests of the same plugin. |
| `Version` | 1–64 letters, digits, `.`, `-`, `_` and `+`. |
| `Description` | Optional; at most 500 characters, no control characters. |
| `Origins` | At most 16 distinct `http` or `https` origins in canonical form: lowercase, no path, credentials, query or default port, such as `https://api.acme.example` or `http://127.0.0.1:8080`. These are the only origins the plugin may ever reach, once an owner approves them. |
| `Profiles` | 1–16 profiles, each with an `ID` unique in the plugin (same syntax as `Name`), a `Label` of 1–100 characters, the `Dialect` it serves, its `Hosting` adaptation and, if providers using it authenticate with a grant, its `Grant`. |

A profile serves one of OLP's built-in dialects whose requests and events are
plain HTTP JSON and server-sent events: `openai-chat`, `openai-responses`,
`anthropic-messages` or `gemini-generate-content`. A plugin never defines a
dialect. OLP refuses a manifest that is invalid, names another dialect, or
carries fields it does not know, with the offending field in the problem it
returns.

### Hosting adaptation

A profile's `Hosting` declares where and how the dialect's requests reach the
upstream. OLP runs it for every request of a provider using the profile; no
plugin code runs per request. Providers using the profile authenticate with a
static credential, or with a [grant](#grants), which the adaptation places.

| Field | Rule |
| --- | --- |
| `Address` | The upstream's base URL, which the dialect's paths extend: `https://api.acme.example/v1` receives `/chat/completions`. An `http` or `https` URL at one of the manifest's `Origins`, written the same way, without credentials, query, fragment or placeholders. It becomes the endpoint of every provider using the profile. |
| `Headers` | At most 16 request headers by name. Hop-by-hop, framing, content negotiation, tracing and `X-OLP-` headers are OLP's, and the dialect's semantic headers, such as `Anthropic-Version` or `OpenAI-Beta`, are the provider's. |
| `Query` | At most 16 query parameters of the address by name: 1–128 letters, digits, `.`, `_`, `~` and `-`, other than the dialect's semantic query settings and addressing, such as Gemini's `alt`. |

Header and query values are templates of at most 2048 characters without
control characters. `{credential}` stands for the provider's static credential,
such as `Token {credential}`, or a grant's current access token, and
`{grant.<name>}` for a grant fact the profile declares; braces appear nowhere
else. At least one value must place the credential. OLP refuses a credential it
can't place in a header, such as one containing a line break, before sending
anything, and redacts every value that carries the credential wherever it
records upstream text.

## Grants

Some upstreams authorize an account, often with a refresh token that rotates,
rather than issue an API key. A profile that declares `Grant` authenticates
providers with a grant: upstream authorization the plugin obtains when an
operator signs in, which OLP holds beneath a credential version and places
with the profile's hosting adaptation.

```go
plugin.Profile{
	ID: "acme-account", Label: "Acme with sign-in", Dialect: "openai-chat",
	Grant: &plugin.GrantAuthentication{Facts: []string{"account"}},
	Hosting: plugin.Hosting{
		Address: "https://api.acme.example/v1",
		Headers: map[string]string{"Authorization": "Bearer {credential}", "Acme-Account": "{grant.account}"},
	},
}
```

`Facts` names up to 16 grant facts: non-secret values the plugin reports with
every grant, such as the upstream account, that the hosting templates use as
`{grant.<name>}`. Each is 1–64 lowercase letters, digits and underscores,
starting with a letter. A template may use only the facts its profile declares.

A plugin with such a profile implements `plugin.GrantEnroller`. OLP runs its
steps in control when an operator enrolls a grant from the provider wizard:

```go
func (acme) StartGrant(start plugin.GrantStart) (plugin.GrantAuthorization, error)
func (acme) ExchangeGrant(exchange plugin.GrantExchange) (plugin.Grant, error)
```

- `StartGrant` builds the authorization request the operator opens: its `URL`,
  at one of the plugin's origins, with a fresh `state` and PKCE challenge, and a
  `Session` of at most 16 KiB holding what `ExchangeGrant` needs, such as the
  state and the PKCE verifier. OLP stores the session encrypted and hands it
  back once.
- `ExchangeGrant` receives the session and what the operator pasted back: the
  whole callback URL the upstream redirected the browser to, typically a
  loopback address where nothing listens, or the code the upstream displayed.
  It checks the state, reporting a mismatch as an `*plugin.Error` with code
  `abi.CodeStateMismatch`, exchanges the code with `plugin.Fetch`, and returns
  the `Grant`: its `AccessToken` (at most 16 KiB, sendable in a header), any
  `RefreshToken`, `ExpiresIn` seconds, the observed `Principal` (the upstream
  account, 1–256 characters) and a value for every declared fact.

Report a failure the operator should see, such as the upstream refusing the
code, as an `*plugin.Error` with a code of your own; any other error is reported
as `internal`. OLP redacts the session and the pasted value from what the step
logs; keep tokens the plugin receives out of its log.

The reference plugin's `reference-grant-chat` profile implements this flow
against a fictional authority with the authorization code flow and PKCE.

### Fetch

`plugin.Fetch` sends an HTTP request through OLP, the only way a plugin reaches
the network. OLP grants it to grant enrollment steps, and sends a request only
to one of the plugin's approved origins, over the provider's network path and
egress policy. It sets the framing headers itself; a plugin may not set `Host`,
`Content-Length`, hop-by-hop or `Proxy-` headers. It returns a redirect rather
than following it, and any response the upstream sent, whatever its status.
Request and response bodies are at most 512 KiB. A request OLP refuses or can't
complete fails with an `*plugin.Error`: `origin_not_approved`, `http_failed` or
`invalid_request`.

## What a plugin can reach

OLP runs a plugin confined, on a fresh instance per call, within 64 MiB of
memory and 10 seconds per call. It grants only:

- a clock: `time.Now` reads the host's wall and monotonic clocks;
- randomness: `crypto/rand` reads the host's cryptographic source;
- logging, through `plugin.Log`;
- to grant enrollment steps, HTTP to the plugin's approved origins, through
  `plugin.Fetch`.

There is no filesystem, other network access, environment or argument list.
Sleeping spends the call's time limit, including the time `Fetch` waits, so
avoid it.

## Logging

`plugin.Log` is a `*slog.Logger` whose records go to OLP's log. Attribute values
travel as text, and groups flatten into dotted keys. Anything written to
standard output or standard error is logged a line at a time as well, including
the Go runtime's panic output.

OLP redacts every secret value it handed the call, and bounds a call's output to
16 KiB, and each message or attribute to 2 KiB, so a plugin can log freely
without leaking what OLP gave it or flooding the log.

## Testing

The SDK builds natively too, so ordinary Go tests can call a plugin's methods
directly; outside OLP, `plugin.Log` discards its records and `plugin.Fetch`
fails as unavailable. Build the module for `wasip1` in your tests to check that
it compiles as a reactor.

## ABI reference

The ABI is defined in package
[`sdk/plugin/abi`](../sdk/plugin/abi/abi.go), which OLP and the SDK share. The
SDK implements all of it; this section is for authors of other SDKs.

### Module

A plugin is a WebAssembly module targeting WASI preview 1 as a reactor. OLP
runs its `_initialize` export, if present, when it instantiates the module. It
refuses a module that exports `_start`.

The module exports:

| Export | Signature | Purpose |
| --- | --- | --- |
| `memory` | memory | Linear memory, where every message is exchanged. |
| `olp_abi_version` | `() -> i32` | Returns the ABI version the module targets. This document describes version 1. |
| `olp_alloc` | `(size i32) -> i32` | Returns a buffer of `size` bytes for OLP to write a message into. The buffer belongs to the plugin again once OLP passes it to `olp_call` or returns it from `host_call`. |
| `olp_call` | `(ptr i32, len i32) -> i64` | Serves one request and returns its response, packed. The response must stay valid until the next export call. |

It may import:

| Import | Signature | Purpose |
| --- | --- | --- |
| `wasi_snapshot_preview1.*` | WASI | Clock, randomness, and standard output and error, which OLP logs. There are no files, arguments or environment variables. |
| `olp.host_call` | `(ptr i32, len i32) -> i64` | Uses one OLP capability and returns its response, packed, in a buffer OLP obtained from `olp_alloc`. |

A module importing any other `olp` function is refused as built for another ABI
version, and one importing anything else is refused as invalid. Packed results
carry the buffer's pointer in the high 32 bits and its length in the low 32.

### Messages

Every message is one JSON document of at most 1 MiB. A request names a method
and carries its parameters:

```json
{"method": "manifest", "params": null}
```

A response carries either a result or an error:

```json
{"result": {"name": "acme", "version": "1.0.0", "origins": ["https://api.acme.example"], "profiles": [{"id": "acme-chat", "label": "Acme Chat Completions", "dialect": "openai-chat", "hosting": {"address": "https://api.acme.example/v1", "headers": {"Authorization": "Token {credential}"}}}]}}
{"error": {"code": "unknown_method", "message": "The plugin does not implement sign."}}
```

Error codes `invalid_request`, `unknown_method`, `internal`, `state_mismatch`,
`origin_not_approved` and `http_failed` are shared; a plugin may report codes of
its own.

### Methods

OLP calls, through `olp_call`:

| Method | Parameters | Result |
| --- | --- | --- |
| `manifest` | none | The manifest, as described above. |
| `grant_start` | `{"profile": "…"}` | `{"url": "…", "session": "…"}` |
| `grant_exchange` | `{"profile": "…", "session": "…", "input": "…"}` | `{"access_token": "…", "refresh_token": "…", "expires_in": 3600, "principal": "…", "facts": {"name": "value"}}` |

A profile that authenticates with a grant is declared with
`"grant": {"facts": ["name"]}`.

### Capabilities

The plugin calls, through `host_call`:

| Capability | Parameters | Result |
| --- | --- | --- |
| `log` | `{"level": "debug" \| "info" \| "warn" \| "error", "message": "…", "attrs": {"key": "value"}}` | none |
| `http` | `{"method": "POST", "url": "…", "header": {"Name": ["value"]}, "body": "<base64>"}` | `{"status": 200, "header": {"Name": ["value"]}, "body": "<base64>"}` |

A call may use only the capabilities OLP grants it; any other capability returns
`unknown_method`. OLP grants `http` to `grant_start` and `grant_exchange`.
