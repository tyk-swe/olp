use olp_conformance_fixtures::read_json;
use olp_providers::anthropic::ConnectorConfig as AnthropicConnectorConfig;
use olp_providers::gemini::ConnectorConfig as GeminiConnectorConfig;
use olp_providers::openai::ConnectorConfig as OpenAiConnectorConfig;
use serde::Deserialize;

#[derive(Debug, Deserialize)]
struct EndpointCase {
    name: String,
    url: String,
    accepted_at_configuration: bool,
    #[serde(default)]
    requires_resolution_check: bool,
}

#[test]
fn literal_ssrf_fixture_matches_connector_configuration_boundary() {
    let cases: Vec<EndpointCase> = read_json("security/custom-endpoints.json");
    assert!(!cases.is_empty());
    assert!(
        cases.iter().any(|case| case.requires_resolution_check),
        "corpus must retain a DNS-rebinding boundary case"
    );
    for case in cases {
        for (connector, accepted) in [
            (
                "OpenAI",
                OpenAiConnectorConfig::with_base_url(&case.url).is_ok(),
            ),
            (
                "Anthropic",
                AnthropicConnectorConfig::with_base_url(&case.url).is_ok(),
            ),
            (
                "Gemini",
                GeminiConnectorConfig::with_base_url(&case.url).is_ok(),
            ),
        ] {
            assert_eq!(
                accepted, case.accepted_at_configuration,
                "unexpected {connector} endpoint classification for {} ({})",
                case.name, case.url
            );
        }
    }
}
