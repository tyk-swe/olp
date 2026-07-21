use base64::{Engine as _, engine::general_purpose::URL_SAFE_NO_PAD};
use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use thiserror::Error;
use uuid::Uuid;

use crate::PersistenceError;

#[derive(Debug, Error)]
pub enum OperationsError {
    #[error(transparent)]
    Persistence(#[from] PersistenceError),
    #[error("database operation failed")]
    Database(#[from] sqlx::Error),
    #[error("cursor is invalid")]
    InvalidCursor,
    #[error("resource was not found")]
    NotFound,
    #[error("the resource changed; refresh and retry")]
    PreconditionFailed,
    #[error("idempotency key has already been used for this operation")]
    IdempotencyConflict,
    #[error("an operation with this idempotency key is still in progress")]
    IdempotencyInProgress,
    #[error("operation input is invalid: {0}")]
    Invalid(String),
}

#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
pub struct TimestampCursor {
    pub at: DateTime<Utc>,
    pub id: Uuid,
}

impl TimestampCursor {
    pub fn parse(value: &str) -> Result<Self, OperationsError> {
        let bytes = URL_SAFE_NO_PAD
            .decode(value)
            .map_err(|_| OperationsError::InvalidCursor)?;
        let cursor: Self =
            serde_json::from_slice(&bytes).map_err(|_| OperationsError::InvalidCursor)?;
        if cursor.id.get_version_num() != 7 {
            return Err(OperationsError::InvalidCursor);
        }
        Ok(cursor)
    }

    #[must_use]
    pub fn encode(&self) -> String {
        URL_SAFE_NO_PAD
            .encode(serde_json::to_vec(self).expect("timestamp cursor serialization cannot fail"))
    }
}

#[derive(Clone, Debug)]
pub struct OperationsPage<T> {
    pub items: Vec<T>,
    pub next_cursor: Option<String>,
}

pub(super) fn checked_u16(value: i16, name: &str) -> Result<u16, OperationsError> {
    u16::try_from(value).map_err(|_| OperationsError::Invalid(format!("stored {name} is invalid")))
}

pub(super) fn optional_u16(value: Option<i32>, name: &str) -> Result<Option<u16>, OperationsError> {
    value
        .map(u16::try_from)
        .transpose()
        .map_err(|_| OperationsError::Invalid(format!("stored {name} is invalid")))
}

pub(crate) fn checked_u64(value: i64, name: &str) -> Result<u64, OperationsError> {
    u64::try_from(value).map_err(|_| OperationsError::Invalid(format!("stored {name} is invalid")))
}

pub(super) fn optional_u64(value: Option<i64>, name: &str) -> Result<Option<u64>, OperationsError> {
    value
        .map(u64::try_from)
        .transpose()
        .map_err(|_| OperationsError::Invalid(format!("stored {name} is invalid")))
}

pub(super) fn optional_i32_u64(
    value: Option<i32>,
    name: &str,
) -> Result<Option<u64>, OperationsError> {
    value
        .map(u64::try_from)
        .transpose()
        .map_err(|_| OperationsError::Invalid(format!("stored {name} is invalid")))
}

pub(crate) fn trimmed_optional(value: Option<String>) -> Option<String> {
    value.map(|value| value.trim().to_owned())
}
