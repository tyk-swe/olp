//! Transport-neutral inference application logic.
//!
//! HTTP delivery adapts this module's failures and results to vendor wire
//! surfaces. Provider networking remains in the sibling `providers` module;
//! request lifetime, attempts, cancellation, and completion live here.

pub mod circuit;

pub mod error;

pub mod events;

pub mod execution;

pub mod failover;

pub mod http;

pub mod playground;

pub mod principal;

pub mod selection;

pub mod executor;
pub mod lifecycle;

pub mod telemetry;

pub mod tracing;

pub mod transport;

pub mod video;
