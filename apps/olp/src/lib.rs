//! Axum delivery adapter for management, inference, the static operator
//! console, and the separately bound private observability surface.
//!
//! Top-level modules follow runtime ownership: process bootstrap, public HTTP
//! policy, inference delivery, management delivery, observability, and the
//! embedded console. `apps/olp/AGENTS.md` carries the detailed map.

// HTTP surfaces: inference gateway, management API, operations reads, OIDC,
// playground, embedded console, private observability listener, composition.
pub mod bootstrap;
pub mod console;
pub mod gateway;
pub mod management;
pub mod observability;
pub mod public_http;

#[cfg(test)]
mod tests;
