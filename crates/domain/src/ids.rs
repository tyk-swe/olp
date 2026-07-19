use std::{fmt, str::FromStr, time::Duration};

use serde::{Deserialize, Serialize};
use thiserror::Error;
use uuid::Uuid;

macro_rules! uuid_id {
    ($name:ident) => {
        #[derive(
            Clone, Copy, Debug, Deserialize, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize,
        )]
        #[serde(transparent)]
        pub struct $name(Uuid);

        impl $name {
            #[must_use]
            pub fn new() -> Self {
                Self(Uuid::now_v7())
            }

            #[must_use]
            pub const fn from_uuid(value: Uuid) -> Self {
                Self(value)
            }

            #[must_use]
            pub const fn as_uuid(self) -> Uuid {
                self.0
            }
        }

        impl Default for $name {
            fn default() -> Self {
                Self::new()
            }
        }

        impl fmt::Display for $name {
            fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
                self.0.fmt(formatter)
            }
        }

        impl FromStr for $name {
            type Err = uuid::Error;

            fn from_str(value: &str) -> Result<Self, Self::Err> {
                Uuid::parse_str(value).map(Self)
            }
        }
    };
}

uuid_id!(ApiKeyId);
uuid_id!(AttemptId);
uuid_id!(CredentialVersionId);
uuid_id!(ProviderId);
uuid_id!(RequestId);
uuid_id!(RouteId);
uuid_id!(RuntimeGenerationId);
uuid_id!(TargetId);

#[derive(Clone, Debug, Deserialize, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize)]
#[serde(try_from = "String", into = "String")]
pub struct RouteSlug(String);

impl RouteSlug {
    pub const MAX_LENGTH: usize = 63;

    pub fn parse(value: impl Into<String>) -> Result<Self, RouteSlugError> {
        let value = value.into();
        if value.is_empty() {
            return Err(RouteSlugError::Empty);
        }
        if value.len() > Self::MAX_LENGTH {
            return Err(RouteSlugError::TooLong {
                length: value.len(),
                maximum: Self::MAX_LENGTH,
            });
        }

        let bytes = value.as_bytes();
        let starts_and_ends_with_alphanumeric =
            bytes.first().is_some_and(u8::is_ascii_lowercase_or_digit)
                && bytes.last().is_some_and(u8::is_ascii_lowercase_or_digit);
        let valid_characters = bytes
            .iter()
            .all(|byte| byte.is_ascii_lowercase_or_digit() || *byte == b'-');
        let valid_separators = !value.contains("--");

        if !starts_and_ends_with_alphanumeric || !valid_characters || !valid_separators {
            return Err(RouteSlugError::InvalidFormat);
        }

        Ok(Self(value))
    }

    #[must_use]
    pub fn as_str(&self) -> &str {
        &self.0
    }
}

trait AsciiSlugByte {
    fn is_ascii_lowercase_or_digit(&self) -> bool;
}

impl AsciiSlugByte for u8 {
    fn is_ascii_lowercase_or_digit(&self) -> bool {
        self.is_ascii_lowercase() || self.is_ascii_digit()
    }
}

impl fmt::Display for RouteSlug {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        self.0.fmt(formatter)
    }
}

impl FromStr for RouteSlug {
    type Err = RouteSlugError;

    fn from_str(value: &str) -> Result<Self, Self::Err> {
        Self::parse(value)
    }
}

impl TryFrom<String> for RouteSlug {
    type Error = RouteSlugError;

    fn try_from(value: String) -> Result<Self, Self::Error> {
        Self::parse(value)
    }
}

impl From<RouteSlug> for String {
    fn from(value: RouteSlug) -> Self {
        value.0
    }
}

#[derive(Clone, Debug, Error, Eq, PartialEq)]
pub enum RouteSlugError {
    #[error("route slug cannot be empty")]
    Empty,
    #[error("route slug is {length} bytes; the maximum is {maximum}")]
    TooLong { length: usize, maximum: usize },
    #[error("route slug must contain lowercase ASCII letters, digits, and single internal hyphens")]
    InvalidFormat,
}

#[derive(Clone, Copy, Debug, Deserialize, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize)]
#[serde(transparent)]
pub struct DurationMs(u64);

impl DurationMs {
    #[must_use]
    pub const fn new(milliseconds: u64) -> Self {
        Self(milliseconds)
    }

    #[must_use]
    pub const fn get(self) -> u64 {
        self.0
    }

    #[must_use]
    pub const fn is_zero(self) -> bool {
        self.0 == 0
    }

    #[must_use]
    pub fn as_duration(self) -> Duration {
        Duration::from_millis(self.0)
    }
}

impl From<DurationMs> for Duration {
    fn from(value: DurationMs) -> Self {
        value.as_duration()
    }
}

impl From<Duration> for DurationMs {
    fn from(value: Duration) -> Self {
        Self(value.as_millis().try_into().unwrap_or(u64::MAX))
    }
}

#[derive(Clone, Debug, Deserialize, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize)]
#[serde(try_from = "String", into = "String")]
pub struct ApiKeyLookupId(String);

impl ApiKeyLookupId {
    pub const MAX_LENGTH: usize = 40;

    pub fn parse(value: impl Into<String>) -> Result<Self, ApiKeyLookupIdError> {
        let value = value.into();
        if !(8..=Self::MAX_LENGTH).contains(&value.len())
            || !value
                .bytes()
                .all(|byte| byte.is_ascii_alphanumeric() || byte == b'_')
        {
            return Err(ApiKeyLookupIdError);
        }
        Ok(Self(value))
    }

    #[must_use]
    pub fn as_str(&self) -> &str {
        &self.0
    }
}

impl fmt::Display for ApiKeyLookupId {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        self.0.fmt(formatter)
    }
}

impl TryFrom<String> for ApiKeyLookupId {
    type Error = ApiKeyLookupIdError;

    fn try_from(value: String) -> Result<Self, Self::Error> {
        Self::parse(value)
    }
}

impl From<ApiKeyLookupId> for String {
    fn from(value: ApiKeyLookupId) -> Self {
        value.0
    }
}

#[derive(Clone, Copy, Debug, Error, Eq, PartialEq)]
#[error("API key lookup ID must be 8-40 ASCII letters, digits, or underscores")]
pub struct ApiKeyLookupIdError;
