# Test fixtures

`vertex/test_only_private_key.pem` is a throwaway keypair generated for
unit tests in `crates/olp-engine/src/providers/vertex/tests.rs`. It is not a
secret, grants no access, and is never used outside `cfg(test)`; secret
scanners may allowlist this path.
