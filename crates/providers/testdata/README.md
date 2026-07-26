# Test fixtures

`vertex/test_only_private_key.pem` is a throwaway keypair generated solely
for unit tests (`crates/providers/src/vertex/tests.rs`). It is not a secret,
grants access to nothing, and is never used outside `cfg(test)`. Secret
scanners flagging it can safely allowlist this path.
