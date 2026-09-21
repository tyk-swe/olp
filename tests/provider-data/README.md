# Test fixtures

`vertex/test_only_private_key.pem` is a throwaway keypair retained from the Rust
connector tests. It grants no access and is not referenced by current Go tests.
Secret scanners may allowlist this fixture path; never use it as an application
credential.
