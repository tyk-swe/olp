use std::{fmt::Display, str::FromStr};

use olp_domain::{
    CapabilitySource, OperationKind, ProviderAuthMode, ProviderKind, ProviderState,
    RouteDraftState, Surface, TransportMode,
};
use serde::{Serialize, de::DeserializeOwned};

fn assert_codec<T>(values: &[T])
where
    T: Copy + Display + DeserializeOwned + Eq + std::fmt::Debug + FromStr + Serialize,
    T::Err: std::fmt::Debug,
{
    for value in values {
        let name = value.to_string();
        assert_eq!(name.parse::<T>().unwrap(), *value);
        assert_eq!(serde_json::to_value(value).unwrap(), name);
        assert_eq!(serde_json::from_value::<T>(name.into()).unwrap(), *value);
    }
}

#[test]
fn canonical_enum_text_and_serde_codecs_agree() {
    assert_codec(&Surface::ALL);
    assert_codec(&TransportMode::ALL);
    assert_codec(&OperationKind::ALL);
    assert_codec(&ProviderKind::ALL);
    assert_codec(&[
        ProviderState::Draft,
        ProviderState::Active,
        ProviderState::Disabled,
    ]);
    assert_codec(&[RouteDraftState::Draft, RouteDraftState::Validated]);
    assert_codec(&[
        CapabilitySource::Declared,
        CapabilitySource::Probed,
        CapabilitySource::Certified,
    ]);
    assert_codec(&[
        ProviderAuthMode::ApiKey,
        ProviderAuthMode::ApplicationDefault,
        ProviderAuthMode::ServiceAccount,
        ProviderAuthMode::DefaultChain,
        ProviderAuthMode::Static,
    ]);
}

#[test]
fn canonical_enum_codecs_reject_noncanonical_names() {
    for value in ["", "OpenAi", "openai", "open-ai", " open_ai"] {
        assert!(value.parse::<Surface>().is_err());
        assert!(value.parse::<ProviderKind>().is_err());
    }
    for value in ["", "Streaming", "stream", "streaming "] {
        assert!(value.parse::<TransportMode>().is_err());
    }
    for value in ["", "Generation", "chat", "video-create"] {
        assert!(value.parse::<OperationKind>().is_err());
    }
    for value in ["", "Active", "enabled", "active "] {
        assert!(value.parse::<ProviderState>().is_err());
    }
    for value in ["", "Validated", "pending", "validated "] {
        assert!(value.parse::<RouteDraftState>().is_err());
    }
    for value in ["", "Certified", "probe", "certified "] {
        assert!(value.parse::<CapabilitySource>().is_err());
    }
    for value in ["", "api-key", "application_default", "static "] {
        assert!(value.parse::<ProviderAuthMode>().is_err());
    }
}
