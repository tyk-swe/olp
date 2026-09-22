# Native configuration editor qualification

This record covers the configuration-editor slice of #217, implemented as #223.
It does not qualify the operation playgrounds, the final evidence presentation,
all native provider interactions, or the whole-spec G1–G7 gates.

The provider editor owns one immutable native draft. Connection fields,
versioned profile selection, semantic headers/query settings, operation controls,
native defaults, serving bindings, network settings and advanced JSON all read
and change that document. Profile capabilities and default schemas come from
`GET /api/v3/provider-profiles`; connection and binding fields come from the
published management schemas. Registered compatible profiles therefore require
no profile-name switch in the editor.

The JSON boundary retains numeric source tokens with the platform JSON reviver
source context, rejects duplicate decoded member names before parsing, and
records source member order independently of JavaScript object enumeration.
Serialization emits the recorded order and numeric tokens. Native configuration
objects are opaque to generic query-cache structural sharing, keeping arbitrary
schema keys such as `__proto__` inert. Both native form fields and advanced JSON
retain partial invalid input and block saving it; null is an explicit native
value and removal is a separate action. Arrays and schema objects replace as
whole values.

The generated API client obtains JSON response source as text before decoding.
This covers responses without `Content-Length`, for which the upstream client
library's default path otherwise calls ordinary `JSON.parse` directly. Explicit
text, binary and streaming response callers retain their selected transport.
No response headers or body lengths are fabricated. Native request bodies use
the same ordered serializer, including configuration export/import drafts.

The authoritative backend source and revision/runtime storage are qualified
separately in [native configuration storage](native-configuration-storage.md).
The editor does not recover precision already lost by older stored writes.

## Public browser assertions

`console/tests/gateway/provider-configuration.spec.ts` runs against the real Go
control API in both the packaged console and Vite, using fresh PostgreSQL
databases and a local deterministic provider. Its independent literal corpus
includes `9007199254740993`, `-0`, `1e-1000`, a long decimal, null, false, zero,
empty strings and arrays, ordered arrays, unknown nested blocks, and schema
properties ordered `"10"`, `"2"`, `"__proto__"`.

The journey asserts literal source in the actual browser PATCH request and the
public GET response after editing unrelated name/network/binding fields, saving,
reloading and reading an activated provider revision. It also verifies:

- Duplicate decoded JSON keys stay invalid and cannot be saved.
- Null and removal produce different authoritative documents.
- Selecting a profile retains incompatible legacy defaults until explicit
  removal; selecting legacy retains profile settings until an explicit choice.
- A concurrent API write returns 412 without replacing the local draft; explicit
  reload discards local changes and releases the activation guard.
- Creating a network credential returns metadata, clears the write-only input,
  retains the local profile/default draft, and advances its ETag. Selection,
  removal and revocation expose no stored secret.
- Metadata reads, editing, credential storage and saves make zero upstream
  requests before an operator explicitly requests certification/probing.
- Provider certification/activation and a saved, validated strict route
  migration succeed. An existing omitted fidelity contract stays omitted on an
  unrelated save; new route forms select strict by default.

The existing OpenAI-compatible onboarding journey explicitly chooses legacy
fidelity for its historical workload. Generated-field unit tests also exercise
explicit legacy, strict and transformed create/update payloads.

## Validation and screenshots

Qualification uses Node.js 26 and Playwright Chromium 153. Both expose the
required standard JSON source-context and raw-JSON facilities. Unsupported
browsers fail visibly instead of silently rounding native values. The parser
bounds input at 4 MiB and nesting depth 128 and evaluates no code.

Functional source is `001e4681`, including configuration implementation
`d7b9a55e`, response/ETag corrections `93bb7dc8`, and integrated backend
`ac9083a3`. Qualification was run on 2026-09-22. The reviewed screenshots
focus on editor elements; unrelated sticky navigation is made static during
capture so it cannot obscure the fields. Shared services were left running; only this
run's databases and browser processes were removed. No paid provider calls or
production writes were made.

- `make build` and `make check`: passed, including Go vet/tests, formatting,
  Svelte/TypeScript/ESLint, 630 console tests and 19 script tests at `93bb7dc8`. The final draft-status
  display correction passed another console build, type/lint check and 52
  focused provider/draft tests.
- Packaged and Vite native configuration journeys: all four passed on final functional source (40.6s). The final focused
  screenshot capture reran both packaged cases successfully (21.2s).
- Existing packaged legacy onboarding, including unary and streaming inference:
  passed (33.5s).
- Current release inventories regenerated without changes; frozen references
  remain untouched.
- The chunked-response regression failed on the original decoder and passes at
  the real generated-client boundary after the fix.
- [Native defaults and synchronized JSON](console-native-defaults.png).
- [Network configuration and write-only credential metadata](console-network-configuration.png).
- [Strict route fidelity](console-route-fidelity.png).
