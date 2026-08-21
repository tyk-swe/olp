use super::*;

#[test]
fn management_metadata_exposes_presets_only_on_the_compatible_kind() {
    let responses: Vec<_> = provider_kind_specs()
        .iter()
        .map(provider_kind_capability_response)
        .collect();
    let compatible = responses
        .iter()
        .find(|response| response.kind == ProviderKind::OpenAiCompatible)
        .expect("compatible provider kind must be present");
    assert_eq!(
        compatible
            .presets
            .iter()
            .map(|preset| preset.id.as_str())
            .collect::<Vec<_>>(),
        [
            "groq",
            "mistral_ai",
            "together_ai",
            "xai",
            "cerebras",
            "openrouter"
        ]
    );
    assert!(compatible.presets.iter().all(|preset| {
        preset.endpoint.starts_with("https://")
            && preset.documentation_url.starts_with("https://")
            && preset.auth_mode == ProviderAuthMode::ApiKey
    }));
    assert!(
        responses
            .iter()
            .filter(|response| response.kind != ProviderKind::OpenAiCompatible)
            .all(|response| response.presets.is_empty())
    );
}
