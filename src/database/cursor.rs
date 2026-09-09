use base64::Engine as _;
use base64::engine::general_purpose::URL_SAFE_NO_PAD;
use chrono::DateTime;
use chrono::Utc;
use serde::Deserialize;
use serde::Serialize;
use thiserror::Error;
use uuid::Uuid;

use crate::database::error::Error as PersistenceError;

#[derive(Debug, Error)]
pub enum Error {
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
pub struct Timestamp {
    pub at: DateTime<Utc>,
    pub id: Uuid,
}

impl Timestamp {
    pub fn parse(value: &str) -> Result<Self, Error> {
        let bytes = URL_SAFE_NO_PAD
            .decode(value)
            .map_err(|_| Error::InvalidCursor)?;
        let cursor: Self = serde_json::from_slice(&bytes).map_err(|_| Error::InvalidCursor)?;
        if cursor.id.get_version_num() != 7 {
            return Err(Error::InvalidCursor);
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
pub struct Page<T> {
    pub items: Vec<T>,
    pub next_cursor: Option<String>,
}

pub(crate) fn checked_u16<T>(value: T, name: &str) -> Result<u16, Error>
where
    u16: TryFrom<T>,
{
    u16::try_from(value).map_err(|_| invalid_stored(name))
}

pub(crate) fn optional_u16<T>(value: Option<T>, name: &str) -> Result<Option<u16>, Error>
where
    u16: TryFrom<T>,
{
    value
        .map(u16::try_from)
        .transpose()
        .map_err(|_| invalid_stored(name))
}

pub(crate) fn checked_u64<T>(value: T, name: &str) -> Result<u64, Error>
where
    u64: TryFrom<T>,
{
    u64::try_from(value).map_err(|_| invalid_stored(name))
}

pub(crate) fn optional_u64<T>(value: Option<T>, name: &str) -> Result<Option<u64>, Error>
where
    u64: TryFrom<T>,
{
    value
        .map(u64::try_from)
        .transpose()
        .map_err(|_| invalid_stored(name))
}

fn invalid_stored(name: &str) -> Error {
    Error::Invalid(format!("stored {name} is invalid"))
}

pub(crate) fn trimmed_optional(value: Option<String>) -> Option<String> {
    value.map(|value| value.trim().to_owned())
}
