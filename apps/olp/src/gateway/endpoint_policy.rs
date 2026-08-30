//! Canonical inference endpoint policy.
//!
//! Routing and request-boundary classification consume the single endpoint
//! table in [`registry::ENDPOINTS`].

pub(crate) mod classification;
pub(crate) mod export;
mod registry;
pub(super) mod router;

#[cfg(test)]
mod tests;
