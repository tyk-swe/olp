//! Transport-neutral inference application logic.
//!
//! HTTP delivery adapts this module's failures and results to vendor wire
//! surfaces. Provider networking remains in the sibling `providers` module;
//! durable persistence is implemented by `olp-db` behind engine-owned seams.

mod accounting;
pub mod circuit;
mod error;
pub mod events;
mod execution;
pub mod failover;
pub mod limits;
mod media_lifecycle;
mod principal;
pub mod request_metadata;
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
pub use limits::InferenceReservation;
pub use media_lifecycle::CleanupMediaStream;
pub use principal::InferencePrincipal;
pub use service::{InferenceService, SessionGenerationExecution};
