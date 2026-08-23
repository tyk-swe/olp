//! Canonical provider inventory, runtime snapshots, and attempt selection.

#[cfg(any(test, feature = "test-util"))]
pub mod fixtures;
pub mod provider;
pub mod route;
pub mod selection;
pub mod snapshot;

#[cfg(test)]
mod tests;
