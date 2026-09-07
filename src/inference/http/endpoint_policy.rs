//! Canonical inference endpoint policy.
//!
//! Routing and request-boundary classification consume the single endpoint
//! table in [`registry::ENDPOINTS`].

pub mod classification;

pub mod export;

pub mod registry;

pub mod router;

#[cfg(test)]
pub mod tests;
