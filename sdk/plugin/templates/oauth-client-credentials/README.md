# OAuth 2.0 client credentials template

A provider plugin for an upstream that accepts short-lived access tokens issued
to OAuth 2.0 clients by the client credentials grant
([RFC 6749, section 4.4](https://www.rfc-editor.org/rfc/rfc6749#section-4.4)).

- **Enrollment.** The operator creates a client on the authorization server
  (`StartGrant` opens that page) and pastes `CLIENT_ID:CLIENT_SECRET`. The
  plugin requests a token with HTTP Basic client authentication.
- **Serving.** Gateways send the access token as `Authorization: Bearer`.
- **Refresh.** The client credentials are the grant's refresh token, which OLP
  stores encrypted and never hands to a gateway; refreshing runs the grant
  again. A refused client (`invalid_client`) lapses the grant.

## Adapt it

1. Copy this directory outside the OLP repository and set its module path.
2. Replace `api`, `tokenEndpoint`, `clientsPage` and `scope`, and the manifest's
   `Origins`, with your upstream's.
3. Rename the plugin and its profile, and choose the dialect your upstream
   speaks.

```sh
go test .
GOOS=wasip1 GOARCH=wasm CGO_ENABLED=0 go build -trimpath -buildvcs=false -buildmode=c-shared -o plugin.wasm .
```

Building with `-trimpath -buildvcs=false` makes the module's digest
reproducible, which the [reviewed plugin index](../../../../docs/plugins.md#reviewed-plugin-index)
requires.

## Before you install it

- Every origin the manifest lists is one OLP will let the plugin reach; list
  only the API and the authorization server.
- Check the upstream's terms allow a gateway to hold client credentials.
- Confirm what the token endpoint returns for a revoked client, and map it to
  `abi.CodeInvalidGrant`.
