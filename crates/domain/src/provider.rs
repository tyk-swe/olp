//! Closed provider and configuration policy values shared across application layers.

use std::{fmt, str::FromStr};

use serde::{Deserialize, Serialize};
use thiserror::Error;
use utoipa::ToSchema;

#[derive(Clone, Debug, Error, Eq, PartialEq)]
#[error("invalid {kind} value: {value}")]
pub struct ClosedSetParseError {
    kind: &'static str,
    value: String,
}

macro_rules! closed_set {
    ($name:ident, $kind:literal, {$($variant:ident => $wire:literal),+ $(,)?}) => {
        #[derive(Clone, Copy, Debug, Deserialize, Eq, Hash, PartialEq, Serialize, ToSchema)]
        pub enum $name {
            $(
                #[serde(rename = $wire)]
                #[schema(rename = $wire)]
                $variant
            ),+
        }

        impl $name {
            #[must_use]
            pub const fn as_str(self) -> &'static str {
                match self {
                    $(Self::$variant => $wire),+
                }
            }
        }

        impl AsRef<str> for $name {
            fn as_ref(&self) -> &str {
                self.as_str()
            }
        }

        impl fmt::Display for $name {
            fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
                formatter.write_str(self.as_str())
            }
        }

        impl FromStr for $name {
            type Err = ClosedSetParseError;

            fn from_str(value: &str) -> Result<Self, Self::Err> {
                match value {
                    $($wire => Ok(Self::$variant),)+
                    _ => Err(ClosedSetParseError {
                        kind: $kind,
                        value: value.to_owned(),
                    }),
                }
            }
        }
    };
}

closed_set!(ProviderState, "provider state", {
    Draft => "draft",
    Active => "active",
    Disabled => "disabled",
});

closed_set!(RouteDraftState, "route draft state", {
    Draft => "draft",
    Validated => "validated",
});

closed_set!(CapabilitySource, "capability source", {
    Declared => "declared",
    Probed => "probed",
    Certified => "certified",
});

closed_set!(ProviderAuthMode, "provider authentication mode", {
    ApiKey => "api_key",
    ApplicationDefault => "adc",
    ServiceAccount => "service_account",
    DefaultChain => "default_chain",
    Static => "static",
});
