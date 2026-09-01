//! Transport-neutral inference application logic.
//!
//! HTTP delivery adapts this module's failures and results to vendor wire
//! surfaces. Provider networking remains in the sibling `providers` module;
//! durable persistence is implemented by `olp-db` behind engine-owned seams.

pub mod accounting;
pub mod circuit;
pub mod error;
pub mod events;
pub mod execution;
pub mod failover;
pub mod limits;
pub mod media_lifecycle;
pub mod principal;
pub mod request_metadata;
pub mod runtime;
pub mod selection;
pub mod service;
mod telemetry;
pub mod tracing;
