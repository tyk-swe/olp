# Signed-request template

This template provides a provider plugin for an upstream that authenticates each
request by a signature rather than a bearer token so the secret never travels.
The operator stores a static credential formatted as `KEY_ID:SECRET`. For every
upstream request, OLP calls `Sign`, which adds `X-Api-Key-Id`, `X-Api-Date` and
`X-Api-Signature`. The signature is the hex HMAC-SHA256, keyed with the secret,
of the canonical request containing the method, the escaped path, the query
sorted by name, the request time and the hex SHA-256 of the body, one per line.

## Adapt it

1. Copy this directory outside the OLP repository and set its module path.
2. Replace `api`, the manifest's `Origins`, `canonicalRequest` and the header
   names with your upstream's documented scheme.
3. Add a `main_test.go` file beside `main.go` that fixes the package `now`
   clock and pins a known-answer signature computed with an independent
   implementation, such as the upstream's own SDK, so any change to the
   signing scheme fails before an upstream refuses it.

This template ships `main.go` without test files. Once you add your unit tests
and verify them, build the WebAssembly reactor.

```sh
GOOS=wasip1 GOARCH=wasm CGO_ENABLED=0 go build -trimpath -buildvcs=false -buildmode=c-shared -o plugin.wasm .
```

## Before you install it

`Sign` runs once per request and never per stream event, after OLP has placed
the request, and it must not change the body or any header it does not add. Any
header `Sign` adds must be one the request does not already carry, and OLP
redacts its values wherever it records upstream text.
