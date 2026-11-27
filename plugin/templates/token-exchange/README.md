# Token exchange template

This template provides a provider plugin for an upstream that takes short-lived
bearer tokens issued by an authorization server in exchange for a long-lived API
key, as OAuth 2.0 token exchange
([RFC 8693](https://www.rfc-editor.org/rfc/rfc8693)) describes and cloud IAM
services commonly do. During enrollment, the operator creates an API key
(`StartGrant` opens that page) and pastes it so the plugin can exchange it for
an access token. While serving, gateways send the access token as
`Authorization: Bearer`. For refresh, OLP stores the API key encrypted as the
grant's refresh token without ever handing it to a gateway, and exchanges it
again before expiry. A refused key (`invalid_grant`) lapses the grant. The
grant's principal is the account the authorization server reports, or a digest
of the key that reveals nothing of it.

## Adapt it

1. Copy this directory outside the OLP repository and set its module path.
2. Replace `api`, `tokenEndpoint`, `keysPage`, `subjectTokenType` and the
   manifest's `Origins` with your upstream's values. Some IAM services use their
   own grant type and field names instead of RFC 8693's, so follow their
   documentation.

This template ships `main.go` without test files. Add unit tests beside
`main.go` by substituting the package `fetch` variable, then build the WebAssembly
reactor.

```sh
GOOS=wasip1 GOARCH=wasm CGO_ENABLED=0 go build -trimpath -buildvcs=false -buildmode=c-shared -o plugin.wasm .
```

## Before you install it

List only the API and the authorization server as origins, and confirm the
token's lifetime because OLP refreshes ahead of `expires_in`.
