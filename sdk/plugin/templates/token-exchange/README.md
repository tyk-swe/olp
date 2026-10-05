# Token exchange template

A provider plugin for an upstream that takes short-lived bearer tokens an
authorization server issues in exchange for a long-lived API key, as OAuth 2.0
token exchange ([RFC 8693](https://www.rfc-editor.org/rfc/rfc8693)) describes
and cloud IAM services commonly do.

- **Enrollment.** The operator creates an API key (`StartGrant` opens that
  page) and pastes it. The plugin exchanges it for an access token.
- **Serving.** Gateways send the access token as `Authorization: Bearer`.
- **Refresh.** The API key is the grant's refresh token, which OLP stores
  encrypted and never hands to a gateway; refreshing exchanges it again. A
  refused key (`invalid_grant`) lapses the grant.
- **Principal.** The grant's principal is the account the authorization server
  reports, or a digest of the key that reveals nothing of it.

## Adapt it

1. Copy this directory outside the OLP repository and set its module path.
2. Replace `api`, `tokenEndpoint`, `keysPage`, `subjectTokenType` and the
   manifest's `Origins` with your upstream's. Some IAM services use their own
   grant type and field names instead of RFC 8693's; follow their
   documentation.

```sh
go test .
GOOS=wasip1 GOARCH=wasm CGO_ENABLED=0 go build -trimpath -buildvcs=false -buildmode=c-shared -o plugin.wasm .
```

## Before you install it

- List only the API and the authorization server as origins.
- Confirm the token's lifetime: OLP refreshes ahead of `expires_in`.
