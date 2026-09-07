//! Canonical provider inventory, runtime snapshots, and attempt selection.

pub mod drafts;

pub mod http;

pub mod model;

pub mod repository;

pub mod revisions;

pub mod selection;

#[cfg(test)]
pub mod tests;

pub mod records;

pub(crate) mod queries;
