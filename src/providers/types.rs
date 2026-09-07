//! Closed provider and configuration policy values shared across application layers.

use thiserror::Error;

#[derive(Clone, Debug, Error, Eq, PartialEq)]
#[error("invalid {kind} value: {value}")]
pub struct ClosedSetParseError {
    kind: &'static str,
    value: String,
}

impl ClosedSetParseError {
    fn new(kind: &'static str, value: &str) -> Self {
        Self {
            kind,
            value: value.to_owned(),
        }
    }
}

closed_string_enum! {
    pub enum ProviderState {
        Draft => "draft",
        Active => "active",
        Disabled => "disabled",
    }
    parse_error ClosedSetParseError => |value| ClosedSetParseError::new("provider state", value);
}

closed_string_enum! {
    pub enum RouteDraftState {
        Draft => "draft",
        Validated => "validated",
    }
    parse_error ClosedSetParseError => |value| ClosedSetParseError::new("route draft state", value);
}

closed_string_enum! {
    pub enum CapabilitySource {
        Declared => "declared",
        Certified => "certified",
    }
    parse_error ClosedSetParseError => |value| ClosedSetParseError::new("capability source", value);
}

closed_string_enum! {
    pub enum ProviderAuthMode {
        ApiKey => "api_key",
        ApplicationDefault => "adc",
        ServiceAccount => "service_account",
        DefaultChain => "default_chain",
        Static => "static",
    }
    parse_error ClosedSetParseError => |value| {
        ClosedSetParseError::new("provider authentication mode", value)
    };
}
