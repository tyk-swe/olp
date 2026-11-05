# Signed-request template

A provider plugin for an upstream that authenticates each request by a
signature rather than a bearer token, so the secret never travels.

- **Credential.** The operator stores a static credential `KEY_ID:SECRET`.
- **Signing.** For every upstream request OLP calls `Sign`, which adds
  `X-Api-Key-Id`, `X-Api-Date` and `X-Api-Signature`: the hex HMAC-SHA256,
  keyed with the secret, of the canonical request. That is the method, the
  escaped path, the query sorted by name, the request time and the hex SHA-256
  of the body, one per line.
- **Evidence.** `main_test.go` pins a signature computed independently of this
  code, so a change to the scheme fails before an upstream refuses it.

## Adapt it

1. Copy this directory outside the OLP repository and set its module path.
2. Replace `api`, the manifest's `Origins`, and `canonicalRequest` and the
   header names with your upstream's documented scheme.
3. Recompute the known answer with an independent implementation, such as the
   upstream's own SDK, and update the test.

```sh
go test .
GOOS=wasip1 GOARCH=wasm CGO_ENABLED=0 go build -trimpath -buildvcs=false -buildmode=c-shared -o plugin.wasm .
```

## Before you install it

- `Sign` runs once per request, never per stream event, after OLP placed the
  request; it must not change the body or any header it does not add.
- A header `Sign` adds must be one the request lacks; OLP redacts its values
  wherever it records upstream text.
