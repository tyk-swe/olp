//! Canonical provider inventory, runtime snapshots, and attempt selection.

mod provider;
mod route;
mod selection;
mod snapshot;

pub use provider::{Capability, InvalidProviderKind, Provider, ProviderKind};
pub use route::{Route, RouteValidationError, Target};
pub use selection::{
    AttemptPlan, RoutingError, select_attempts, select_attempts_filtered, weighted_rendezvous_score,
};
pub use snapshot::{RuntimeGeneration, RuntimeSnapshot, SnapshotValidationError};

#[cfg(test)]
mod tests;
