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
		Profiles: []plugin.Profile{
			{ID: "acme-chat", Label: "Acme Chat Completions", Dialect: "openai-chat"},
		},
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
| `Profiles` | 1–16 profiles, each with an `ID` unique in the plugin (same syntax as `Name`), a `Label` of 1–100 characters and the `Dialect` it serves. |

A dialect is one of OLP's built-in dialects, as listed in the `dialect` field of
`GET /api/v1/provider-profiles`: for example `openai-chat`, `openai-responses`,
`anthropic-messages`, `gemini-generate-content` or `bedrock-converse`. A plugin
never defines a dialect. OLP refuses a manifest that is invalid, names another
dialect, or carries fields it does not know, with the offending field in the
problem it returns.

## What a plugin can reach

OLP runs a plugin confined, on a fresh instance per call, within 64 MiB of
memory and 10 seconds per call. It grants only:

- a clock: `time.Now` reads the host's wall and monotonic clocks;
- randomness: `crypto/rand` reads the host's cryptographic source;
- logging, through `plugin.Log`.

There is no filesystem, network, environment or argument list. Sleeping spends
the call's time limit, so avoid it.

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
directly; outside OLP, `plugin.Log` discards its records. Build the module for
`wasip1` in your tests to check that it compiles as a reactor.

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
{"result": {"name": "acme", "version": "1.0.0", "origins": [], "profiles": []}}
{"error": {"code": "unknown_method", "message": "The plugin does not implement sign."}}
```

Error codes `invalid_request`, `unknown_method` and `internal` are shared; a
plugin may report codes of its own.

### Methods

OLP calls, through `olp_call`:

| Method | Parameters | Result |
| --- | --- | --- |
| `manifest` | none | The manifest, as described above. |

### Capabilities

The plugin calls, through `host_call`:

| Capability | Parameters | Result |
| --- | --- | --- |
| `log` | `{"level": "debug" \| "info" \| "warn" \| "error", "message": "…", "attrs": {"key": "value"}}` | none |

A call may use only the capabilities OLP grants it; any other capability returns
`unknown_method`.
