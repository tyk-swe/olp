use crate::{error::Error as PersistenceError, runtime::compiler::RuntimeCompileError};
use thiserror::Error;

#[derive(Debug, Error)]
pub enum Error {
    #[error(transparent)]
    Persistence(#[from] PersistenceError),
    #[error("stored encrypted credential is malformed")]
    InvalidCredential,
    #[error("provider does not exist")]
    ProviderNotFound,
    #[error("provider cannot be activated without a credential and enabled model")]
    ProviderIncomplete,
    #[error("provider ETag does not match")]
    PreconditionFailed,
    #[error("configuration resource does not exist")]
    NotFound,
    #[error("configuration resource is in use")]
    InUse,
    #[error("configuration mutation is invalid: {0}")]
    Invalid(String),
    #[error("provider revision diff exceeds the {maximum} {dimension} per-revision server limit")]
    ProviderRevisionDiffTooLarge {
        dimension: &'static str,
        maximum: usize,
    },
    #[error("route draft does not exist")]
    RouteNotFound,
    #[error("route draft is invalid: {0}")]
    InvalidRoute(String),
    #[error(transparent)]
    RuntimeCompile(#[from] RuntimeCompileError),
    #[error("this idempotency key has already been used")]
    IdempotencyConflict,
    #[error("an operation with this idempotency key is still in progress")]
    IdempotencyInProgress,
}

impl From<sqlx::Error> for Error {
    fn from(error: sqlx::Error) -> Self {
        Self::Persistence(PersistenceError::Database(error))
    }
}
