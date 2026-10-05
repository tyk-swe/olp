# Development signing key

`dev-2026a.seed` is the base64 Ed25519 seed of the **development** signing key.
It is public on purpose and protects nothing: it lets local builds and CI tests
sign the reference catalog and the plugin index after an edit
(`make catalog-sign`).

Only builds without the `release` build tag trust this key. Release builds trust
only the release keys in `internal/signing/keys.go`, and the release workflow
re-signs both documents with the release key, which only CI holds. See
[docs/catalog.md](../../../docs/catalog.md#signing-keys).
