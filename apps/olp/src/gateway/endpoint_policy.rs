//! Canonical inference endpoint policy.
//!
//! Routing and request-boundary classification consume the single endpoint
//! table in [`registry::ENDPOINTS`].

pub(crate) mod classification;
mod registry;
pub(super) mod router;

#[cfg(test)]
mod tests;
