//! Single conformance-test binary. One binary instead of six keeps link
//! time and target size in check — olp-providers alone added ~220 MB of
//! debug link output when `ssrf` linked it as a standalone binary.

mod corpus;
mod protocols;
mod provider_connectors;
mod routing_retry;
mod selected_operations;
mod ssrf;
mod streaming;
