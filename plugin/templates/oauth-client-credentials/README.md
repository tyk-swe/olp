# OAuth 2.0 client credentials template

This template provides a provider plugin for an upstream that accepts
short-lived access tokens issued to OAuth 2.0 clients by the client credentials
grant ([RFC 6749, section 4.4](https://www.rfc-editor.org/rfc/rfc6749#section-4.4)).
During enrollment, the operator creates a client on the authorization server
(`StartGrant` opens that page) and pastes `CLIENT_ID:CLIENT_SECRET`, and the
plugin requests a token with HTTP Basic client authentication. While serving,
gateways send the access token as `Authorization: Bearer`. For refresh, OLP
stores the client credentials encrypted as the grant's refresh token without
ever handing them to a gateway, and runs the grant again before expiry. A
refused client (`invalid_client`) lapses the grant.

## Adapt it

1. Copy this directory outside the OLP repository and set its module path.
2. Replace `api`, `tokenEndpoint`, `clientsPage` and `scope`, along with the
   manifest's `Origins`, with your upstream's values.
3. Rename the plugin and its profile, and choose the dialect your upstream
   speaks.

This template ships `main.go` without test files. Add unit tests beside
`main.go` by substituting the package `fetch` variable, then build the WebAssembly
reactor.

```sh
GOOS=wasip1 GOARCH=wasm CGO_ENABLED=0 go build -trimpath -buildvcs=false -buildmode=c-shared -o plugin.wasm .
```

Building with `-trimpath -buildvcs=false` makes the module's digest
reproducible, which the
[reviewed plugin index](../../../docs/use/plugins.md#reviewed-plugin-index)
requires.

## Before you install it

Every origin the manifest lists is one OLP will let the plugin reach, so list
only the API and the authorization server. Check that the upstream's terms allow
a gateway to hold client credentials, and confirm what the token endpoint
returns for a revoked client so you map it to `abi.CodeInvalidGrant`.
