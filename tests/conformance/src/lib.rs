//! Reference harness support for the framework-independent fixture corpus.

use std::path::{Path, PathBuf};

/// Maximum size of any individual fixture accepted by the reference harness.
pub const MAX_FIXTURE_BYTES: u64 = 64 * 1024;

/// Returns the repository-owned fixture corpus directory.
#[must_use]
pub fn fixture_root() -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR")).join("../fixtures")
}

/// Reads one bounded fixture relative to [`fixture_root`].
///
/// # Panics
///
/// Panics when the fixture cannot be inspected or read, or exceeds the corpus
/// size limit. These conditions are repository errors rather than runtime
/// input errors.
#[must_use]
pub fn read_fixture(relative: impl AsRef<Path>) -> Vec<u8> {
    let path = fixture_root().join(relative);
    let metadata = std::fs::metadata(&path)
        .unwrap_or_else(|error| panic!("failed to inspect fixture {}: {error}", path.display()));
    assert!(
        metadata.len() <= MAX_FIXTURE_BYTES,
        "fixture {} is {} bytes; maximum is {MAX_FIXTURE_BYTES}",
        path.display(),
        metadata.len()
    );
    std::fs::read(&path)
        .unwrap_or_else(|error| panic!("failed to read fixture {}: {error}", path.display()))
}

/// Reads and deserializes one JSON fixture.
///
/// # Panics
///
/// Panics for the same repository errors as [`read_fixture`] or when JSON does
/// not match the requested fixture type.
#[must_use]
pub fn read_json<T: serde::de::DeserializeOwned>(relative: impl AsRef<Path>) -> T {
    let relative = relative.as_ref();
    serde_json::from_slice(&read_fixture(relative)).unwrap_or_else(|error| {
        panic!(
            "failed to decode JSON fixture {}: {error}",
            relative.display()
        )
    })
}
