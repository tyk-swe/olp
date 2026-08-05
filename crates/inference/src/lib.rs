//! Transport-neutral inference application logic.
//!
//! HTTP delivery adapts this crate's failures and results to vendor wire
//! surfaces. Provider networking and persistence remain in their designated
//! infrastructure crates.

mod accounting;
pub mod circuit;
mod error;
pub mod events;
mod execution;
pub mod failover;
pub mod limits;
pub mod runtime;
pub mod selection;
mod service;
mod telemetry;

pub use accounting::{RequestAccountingGuard, RequestOutcome, UsageCapture};
pub use error::{InferenceError, InferenceErrorKind};
pub use execution::{
    CompletedEventExecution, RequestAdmission, RequiredTarget, RoutedEventExecution,
    RoutedUnaryFinalizer, RoutedUnaryResult,
};
pub use limits::{InferencePrincipal, InferenceReservation};
pub use service::{InferenceService, SessionGenerationExecution};
