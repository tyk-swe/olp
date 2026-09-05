use std::time::Duration;

use futures::{StreamExt as _, stream};
use uuid::Uuid;
use zeroize::Zeroizing;

use crate::{
    domain::{
        canonical::{
            events::{Event, Kind},
            identity::{OperationKind, Surface, TransportMode},
            requests::SourceExtensions,
            results::CanonicalResult,
        },
        ports::{ProviderOutput, ProviderRequest, ProviderTransport},
        provider::ProviderAuthMode,
        routing::provider::ProviderKind,
    },
    providers::{
        connector::ResponseLimits,
        factory::{
            assembly::Factory,
            certification::{
                certifiable_capabilities, execute_native_capability_probe, native_probe_operation,
                supports,
            },
            configuration::{
                Config, Credential, CredentialKind, connector_configuration_with_policy,
            },
            overrides::Registry,
        },
        http_egress::EgressPolicy,
        openai::{
            ApiKey, ConnectorConfig as OpenAiConnectorConfig,
            certification::{CompatibleCapability, CompatibleCapabilityCertificationError},
            transport::Connector,
        },
    },
};

#[test]
fn public_factory_covers_every_provider_authentication_pairing() {
    let cases = [
        (Config::OpenAi { endpoint: None }, CredentialKind::ApiKey),
        (
            Config::OpenAiCompatible {
                endpoint: "https://provider.example.test/v1".to_owned(),
            },
            CredentialKind::ApiKey,
        ),
        (
            Config::Anthropic {
                endpoint: None,
                api_version: None,
            },
            CredentialKind::ApiKey,
        ),
        (Config::Gemini { endpoint: None }, CredentialKind::ApiKey),
        (
            Config::VertexAi {
                project: "project".to_owned(),
                location: "us-central1".to_owned(),
                probe_model: "model".to_owned(),
                auth_mode: ProviderAuthMode::ApplicationDefault,
            },
            CredentialKind::None,
        ),
        (
            Config::VertexAi {
                project: "project".to_owned(),
                location: "us-central1".to_owned(),
                probe_model: "model".to_owned(),
                auth_mode: ProviderAuthMode::ServiceAccount,
            },
            CredentialKind::ServiceAccountJson,
        ),
        (
            Config::Bedrock {
                region: "us-east-1".to_owned(),
                auth_mode: ProviderAuthMode::DefaultChain,
            },
            CredentialKind::None,
        ),
        (
            Config::Bedrock {
                region: "us-east-1".to_owned(),
                auth_mode: ProviderAuthMode::Static,
            },
            CredentialKind::AwsStatic,
        ),
        (
            Config::AzureOpenAi {
                endpoint: "https://resource.openai.azure.com".to_owned(),
                deployment: "deployment".to_owned(),
                api_version: "2025-04-01-preview".to_owned(),
            },
            CredentialKind::ApiKey,
        ),
    ];

    for (config, expected) in cases {
        assert_eq!(Factory::credential_kind(&config).unwrap(), expected);
    }
}

#[test]
fn semantic_credentials_are_redacted_and_mismatches_are_rejected() {
    let credential = Credential::ApiKey(Zeroizing::new("very-secret".to_owned()));
    let debug = format!("{credential:?}");
    assert!(debug.contains("[REDACTED]"));
    assert!(!debug.contains("very-secret"));

    let config = Config::Bedrock {
        region: "us-east-1".to_owned(),
        auth_mode: ProviderAuthMode::Static,
    };
    let error = Factory::validate_credential(&config, &credential).unwrap_err();
    assert_eq!(
        error.to_string(),
        "provider credential does not match its authentication mode"
    );
    assert!(!error.to_string().contains("very-secret"));
}

#[test]
fn certification_matrix_excludes_unprovable_compatible_tuples() {
    assert!(supports(
        ProviderKind::OpenAiCompatible,
        OperationKind::Generation,
        Surface::OpenAi,
        TransportMode::Streaming,
    ));
    assert!(supports(
        ProviderKind::OpenAiCompatible,
        OperationKind::Moderation,
        Surface::OpenAi,
        TransportMode::Unary,
    ));
    assert!(!supports(
        ProviderKind::OpenAiCompatible,
        OperationKind::Generation,
        Surface::Anthropic,
        TransportMode::Unary,
    ));
    assert!(!supports(
        ProviderKind::OpenAiCompatible,
        OperationKind::ImageGeneration,
        Surface::OpenAi,
        TransportMode::Unary,
    ));
    assert!(!supports(
        ProviderKind::AzureOpenAi,
        OperationKind::ImageGeneration,
        Surface::OpenAi,
        TransportMode::Unary,
    ));
}

#[test]
fn certifiable_capability_options_are_closed_per_provider_kind() {
    for (kind, expected_count) in [
        (ProviderKind::OpenAi, 25),
        (ProviderKind::OpenAiCompatible, 5),
        (ProviderKind::AzureOpenAi, 11),
        (ProviderKind::Anthropic, 9),
        (ProviderKind::Gemini, 9),
        (ProviderKind::VertexAi, 9),
        (ProviderKind::Bedrock, 9),
    ] {
        let capabilities = certifiable_capabilities(kind).collect::<Vec<_>>();
        assert_eq!(capabilities.len(), expected_count, "{kind:?}");
        assert!(
            capabilities
                .iter()
                .all(|(operation, surface, mode)| { supports(kind, *operation, *surface, *mode) })
        );
    }
}

#[test]
fn certification_probe_override_is_available_for_native_and_compatible_providers() {
    let registry = Registry::default();
    let provider_id = Uuid::from_u128(1);
    registry.register(
        provider_id,
        Connector::new(
            OpenAiConnectorConfig::default(),
            ApiKey::new("sk-test-key").unwrap(),
        ),
    );

    assert!(registry.get(provider_id, ProviderKind::OpenAi).is_some());
    assert!(
        registry
            .get(provider_id, ProviderKind::OpenAiCompatible)
            .is_some()
    );
    assert!(
        registry
            .get(provider_id, ProviderKind::AzureOpenAi)
            .is_none()
    );
}

#[test]
fn bedrock_static_credential_validation_accepts_bytes() {
    let config = Config::Bedrock {
        region: "us-east-1".to_owned(),
        auth_mode: ProviderAuthMode::Static,
    };
    let credential = Credential::AwsStatic(Zeroizing::new(
        br#"{"access_key_id":"ABCDEFGHIJKLMNOP","secret_access_key":"abcdefghijklmnop"}"#.to_vec(),
    ));
    assert!(Factory::validate_credential(&config, &credential).is_ok());
}

struct ExactNativeProbeTransport {
    expected_model: &'static str,
    expected_kind: ProviderKind,
    calls: std::sync::atomic::AtomicUsize,
}

struct OpenTerminalNativeProbeTransport;

impl ProviderTransport for OpenTerminalNativeProbeTransport {
    fn execute<'a>(
        &'a self,
        _request: ProviderRequest,
    ) -> crate::domain::ports::BoxFuture<
        'a,
        Result<ProviderOutput, crate::domain::ports::TransportError>,
    > {
        Box::pin(async {
            let events = stream::iter([Ok::<_, crate::domain::ports::TransportError>(Event::new(
                0,
                Kind::Done,
            ))])
            .chain(stream::pending());
            Ok(ProviderOutput::Events(Box::pin(events)))
        })
    }
}

impl ProviderTransport for ExactNativeProbeTransport {
    fn execute<'a>(
        &'a self,
        request: ProviderRequest,
    ) -> crate::domain::ports::BoxFuture<
        'a,
        Result<ProviderOutput, crate::domain::ports::TransportError>,
    > {
        assert_eq!(request.attempt.upstream_model, self.expected_model);
        assert_eq!(request.attempt.provider_kind, self.expected_kind);
        assert_eq!(request.metadata.surface, Surface::Gemini);
        assert_eq!(request.metadata.operation, OperationKind::TokenCount);
        self.calls.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
        Box::pin(async {
            Ok(ProviderOutput::Result(Box::new(
                CanonicalResult::TokenCount(crate::domain::canonical::results::TokenCountResult {
                    input_tokens: 3,
                    extensions: SourceExtensions::default(),
                }),
            )))
        })
    }
}

#[tokio::test]
async fn native_certification_executes_the_exact_model_and_tuple() {
    let transport = ExactNativeProbeTransport {
        expected_model: "exact-model-v2",
        expected_kind: ProviderKind::OpenAi,
        calls: std::sync::atomic::AtomicUsize::new(0),
    };
    execute_native_capability_probe(
        &transport,
        ProviderKind::OpenAi,
        "exact-model-v2",
        CompatibleCapability {
            operation: OperationKind::TokenCount,
            surface: Surface::Gemini,
            mode: TransportMode::Unary,
        },
    )
    .await
    .unwrap();
    assert_eq!(transport.calls.load(std::sync::atomic::Ordering::SeqCst), 1);

    assert!(matches!(
        native_probe_operation(
            ProviderKind::Anthropic,
            CompatibleCapability {
                operation: OperationKind::Embeddings,
                surface: Surface::OpenAi,
                mode: TransportMode::Unary,
            },
        ),
        Err(CompatibleCapabilityCertificationError::Unsupported)
    ));
}

#[tokio::test]
async fn native_streaming_certification_stops_at_terminal_event() {
    tokio::time::timeout(
        Duration::from_secs(1),
        execute_native_capability_probe(
            &OpenTerminalNativeProbeTransport,
            ProviderKind::Anthropic,
            "exact-model-v2",
            CompatibleCapability {
                operation: OperationKind::Generation,
                surface: Surface::Anthropic,
                mode: TransportMode::Streaming,
            },
        ),
    )
    .await
    .unwrap()
    .unwrap();
}

fn private_http_policy() -> EgressPolicy {
    EgressPolicy::new(
        vec!["10.0.0.0/8".parse().unwrap()],
        vec!["10.1.2.3".to_owned()],
    )
}

#[test]
fn default_policy_rejects_private_plain_http_endpoints_that_an_allowlist_accepts() {
    let compat = Config::OpenAiCompatible {
        endpoint: "http://10.1.2.3:8000/v1".to_owned(),
    };
    assert!(Factory::validate(&compat, &EgressPolicy::default()).is_err());
    Factory::validate(&compat, &private_http_policy()).unwrap();

    let cidr_only = EgressPolicy::new(vec!["10.0.0.0/8".parse().unwrap()], vec![]);
    assert!(Factory::validate(&compat, &cidr_only).is_err());
    let host_only = EgressPolicy::new(vec![], vec!["10.1.2.3".to_owned()]);
    assert!(Factory::validate(&compat, &host_only).is_err());
    let other_host = Config::OpenAiCompatible {
        endpoint: "http://10.1.2.4:8000/v1".to_owned(),
    };
    assert!(Factory::validate(&other_host, &private_http_policy()).is_err());

    let azure = Config::AzureOpenAi {
        endpoint: "http://10.1.2.3".to_owned(),
        deployment: "deployment".to_owned(),
        api_version: "2024-10-21".to_owned(),
    };
    assert!(Factory::validate(&azure, &EgressPolicy::default()).is_err());
    Factory::validate(&azure, &private_http_policy()).unwrap();

    let loopback = Config::OpenAiCompatible {
        endpoint: "http://127.0.0.1:9/v1".to_owned(),
    };
    assert!(Factory::validate(&loopback, &EgressPolicy::default()).is_err());
    Factory::validate(&loopback, &EgressPolicy::unsafe_test_targets()).unwrap();
}

#[tokio::test]
async fn allowlisted_private_endpoints_assemble_transports_without_network_io() {
    let compat = Config::OpenAiCompatible {
        endpoint: "http://10.1.2.3:8000/v1".to_owned(),
    };
    let credential = Credential::ApiKey(Zeroizing::new("sk-test".to_owned()));
    assert!(
        Factory::create(
            compat.clone(),
            Credential::ApiKey(Zeroizing::new("sk-test".to_owned())),
            &EgressPolicy::default(),
            ResponseLimits::default(),
        )
        .await
        .is_err()
    );
    Factory::transport(
        compat,
        credential,
        &private_http_policy(),
        ResponseLimits::default(),
    )
    .await
    .unwrap();

    let azure = Config::AzureOpenAi {
        endpoint: "http://10.1.2.3".to_owned(),
        deployment: "deployment".to_owned(),
        api_version: "2024-10-21".to_owned(),
    };
    Factory::transport(
        azure,
        Credential::ApiKey(Zeroizing::new("azure-secret".to_owned())),
        &private_http_policy(),
        ResponseLimits::default(),
    )
    .await
    .unwrap();
}

#[tokio::test]
async fn response_limits_reach_every_http_connector_and_reject_zero() {
    let limits = ResponseLimits {
        max_response_bytes: 4 * 1024 * 1024,
        max_event_bytes: 256 * 1024,
    };
    for config in [
        Config::OpenAi { endpoint: None },
        Config::Anthropic {
            endpoint: None,
            api_version: None,
        },
        Config::Gemini { endpoint: None },
        Config::AzureOpenAi {
            endpoint: "https://example.openai.azure.com".to_owned(),
            deployment: "deployment".to_owned(),
            api_version: "2024-10-21".to_owned(),
        },
        Config::VertexAi {
            project: "project".to_owned(),
            location: "us-central1".to_owned(),
            probe_model: "model".to_owned(),
            auth_mode: ProviderAuthMode::ApplicationDefault,
        },
    ] {
        let configuration =
            connector_configuration_with_policy(&config, &EgressPolicy::default(), limits).unwrap();
        assert_eq!(configuration.response_limits(), Some(limits), "{config:?}");
        let zero = ResponseLimits {
            max_event_bytes: 0,
            ..limits
        };
        assert!(
            connector_configuration_with_policy(&config, &EgressPolicy::default(), zero).is_err(),
            "{config:?}"
        );
    }
    let bedrock = Config::Bedrock {
        region: "us-east-1".to_owned(),
        auth_mode: ProviderAuthMode::DefaultChain,
    };
    let configuration =
        connector_configuration_with_policy(&bedrock, &EgressPolicy::default(), limits).unwrap();
    assert_eq!(configuration.response_limits(), None);
}
