//! HTTP authentication overrides are assembled only from decrypted credentials.
use crate::providers::configuration::ProviderConfiguration;
use crate::providers::connectors::configuration::{BorrowedCredential, Error};
use crate::providers::options::TOKEN_LIMIT_ALIASES;
use crate::providers::types::ProviderAuthMode;
use http::{HeaderMap, HeaderName, HeaderValue};

/// Custom authentication values must not be reflected through provider errors,
/// including canonical errors delivered inside a successful HTTP stream.
pub(crate) fn redact_output_errors(
    output: Result<
        crate::inference::transport::ProviderOutput,
        crate::inference::transport::TransportError,
    >,
    custom_authentication: bool,
) -> Result<crate::inference::transport::ProviderOutput, crate::inference::transport::TransportError>
{
    use crate::inference::transport::ProviderOutput;
    use crate::protocols::canonical::events::Kind;
    use futures::StreamExt;
    if !custom_authentication {
        return output;
    }
    let redact_transport = |mut error: crate::inference::transport::TransportError| {
        error.message = "Provider request failed".into();
        error
    };
    let mut failed = false;
    match output.map_err(redact_transport)? {
        ProviderOutput::Events(events) => {
            Ok(ProviderOutput::Events(Box::pin(events.map(move |event| {
                event.map_err(redact_transport).map(|mut event| {
                    if let Kind::Error { error } = &mut event.kind {
                        failed = true;
                        error.message = "Provider request failed".into();
                        error.provider_code = None;
                    } else if failed && let Kind::SourceExtension { extensions } = &mut event.kind {
                        extensions.values.clear();
                    }
                    event
                })
            }))))
        }
        output => Ok(output),
    }
}

pub(crate) fn estimate_tokens(
    operation: &crate::protocols::canonical::requests::Operation,
    kind: crate::providers::runtime_model::ProviderKind,
    options: &crate::providers::options::ConnectionOptions,
) -> i64 {
    use crate::providers::runtime_model::ProviderKind;
    let defaults = &options.parameter_defaults;
    let (tokens, candidates) = match kind {
        ProviderKind::OpenAi | ProviderKind::OpenAiCompatible | ProviderKind::AzureOpenAi => (
            TOKEN_LIMIT_ALIASES
                .iter()
                .find_map(|name| defaults.get(*name)),
            defaults.get("n"),
        ),
        ProviderKind::Anthropic => (defaults.get("max_tokens"), None),
        ProviderKind::Gemini => (
            defaults
                .get("generationConfig")
                .and_then(|value| value.get("maxOutputTokens")),
            defaults
                .get("generationConfig")
                .and_then(|value| value.get("candidateCount")),
        ),
        ProviderKind::VertexAi | ProviderKind::Bedrock => (None, None),
    };
    let count = |value: Option<&serde_json::Value>| {
        value
            .and_then(serde_json::Value::as_u64)
            .and_then(|value| u32::try_from(value).ok())
    };
    crate::limits::admission::estimate_tokens_with_defaults(
        operation,
        count(tokens),
        count(candidates),
    )
}

pub(crate) fn headers(
    config: &ProviderConfiguration,
    credential: BorrowedCredential<'_>,
) -> Result<Option<HeaderMap>, Error> {
    match config.auth_mode {
        ProviderAuthMode::None => Ok(Some(HeaderMap::new())),
        ProviderAuthMode::Headers => {
            let text = crate::providers::connectors::configuration::text_credential(
                credential,
                "Header credential is missing",
            )?;
            let values: std::collections::BTreeMap<String, String> = serde_json::from_str(text)
                .map_err(|_| {
                    Error::credential("Credential must be a JSON object of header values")
                })?;
            if values.is_empty()
                || values
                    .keys()
                    .cloned()
                    .collect::<std::collections::BTreeSet<_>>()
                    != config.options.credential_headers
            {
                return Err(Error::credential(
                    "Credential headers must exactly match the configured names",
                ));
            }
            let mut result = HeaderMap::new();
            for (name, value) in values {
                crate::providers::options::validate_header_name(&name)
                    .map_err(Error::configuration)?;
                let mut value = HeaderValue::from_str(&value).map_err(|_| {
                    Error::credential("Credential contains an invalid header value")
                })?;
                value.set_sensitive(true);
                result.insert(
                    HeaderName::from_bytes(name.as_bytes()).map_err(Error::configuration)?,
                    value,
                );
            }
            Ok(Some(result))
        }
        _ => Ok(None),
    }
}

pub(crate) fn key_text<'a>(
    config: &ProviderConfiguration,
    credential: BorrowedCredential<'a>,
) -> Result<&'a str, Error> {
    if matches!(
        config.auth_mode,
        ProviderAuthMode::None | ProviderAuthMode::Headers
    ) {
        headers(config, credential)?;
        // Existing key wrappers validate construction; this value is never sent.
        Ok("olp-custom-authentication")
    } else {
        crate::providers::connectors::configuration::text_credential(
            credential,
            "Provider credential is missing",
        )
    }
}

pub(crate) fn request_body(
    options: &crate::providers::options::ConnectionOptions,
    path: &str,
    body: Vec<u8>,
) -> Result<Vec<u8>, crate::inference::transport::TransportError> {
    let vendor = options.vendor_id.as_deref();
    if options.parameter_defaults.is_empty()
        && !crate::providers::catalog::chat_completions_only(vendor)
        && vendor != Some("voyage")
    {
        return Ok(body);
    }
    let error = || {
        crate::providers::transport_common::protocol_error("Provider request options are invalid")
    };
    let mut value: serde_json::Value = serde_json::from_slice(&body).map_err(|_| error())?;
    let object = value.as_object_mut().ok_or_else(error)?;
    let explicit_token_limit = TOKEN_LIMIT_ALIASES
        .iter()
        .any(|key| object.get(*key).is_some_and(|value| !value.is_null()));
    for (key, value) in &options.parameter_defaults {
        if key == "response_format"
            && (object.get(key).is_some_and(|value| !value.is_null())
                || !response_format_default_applies(path, value))
        {
            continue;
        }
        if explicit_token_limit && TOKEN_LIMIT_ALIASES.contains(&key.as_str()) {
            continue;
        }

        let generation = matches!(path, "generation" | "chat/completions" | "responses");
        if !generation
            && (TOKEN_LIMIT_ALIASES.contains(&key.as_str())
                || matches!(
                    key.as_str(),
                    "temperature"
                        | "top_p"
                        | "top_k"
                        | "stop"
                        | "stop_sequences"
                        | "seed"
                        | "n"
                        | "parallel_tool_calls"
                        | "generationConfig"
                ))
        {
            continue;
        }
        if path != "embeddings"
            && matches!(
                key.as_str(),
                "dimensions"
                    | "output_dimension"
                    | "output_dtype"
                    | "encoding_format"
                    | "input_type"
                    | "truncation"
            )
        {
            continue;
        }

        let key = match (path, key.as_str()) {
            ("responses", "max_tokens" | "max_completion_tokens") => "max_output_tokens",
            ("chat/completions", "max_output_tokens") => "max_completion_tokens",
            (_, key) => key,
        };
        merge_default(object.entry(key).or_insert(serde_json::Value::Null), value);
    }
    if path == "chat/completions"
        && crate::providers::catalog::chat_completions_only(vendor)
        && let Some(tokens) = object.remove("max_completion_tokens")
    {
        object.insert("max_tokens".into(), tokens);
    }
    if vendor == Some("voyage") {
        if path != "embeddings" {
            return Err(crate::providers::transport_common::protocol_error(
                "Voyage supports embeddings only",
            ));
        }
        if object
            .get("encoding_format")
            .and_then(serde_json::Value::as_str)
            == Some("float")
        {
            object.remove("encoding_format");
        }
        if let Some(dimensions) = object.remove("dimensions") {
            object.insert("output_dimension".into(), dimensions);
        }
        object
            .entry("truncation")
            .or_insert(serde_json::Value::Bool(false));
    }
    serde_json::to_vec(&value).map_err(|_| error())
}

// Response formats share a field name across endpoints, but have different
// shapes and values. Treat an explicit format as one complete choice.
fn response_format_default_applies(path: &str, value: &serde_json::Value) -> bool {
    match path {
        "chat/completions" => value.is_object(),
        "images/generations" => matches!(value.as_str(), Some("url" | "b64_json")),
        "audio/speech" => matches!(
            value.as_str(),
            Some("mp3" | "opus" | "aac" | "flac" | "wav" | "pcm")
        ),
        _ => false,
    }
}

fn merge_default(value: &mut serde_json::Value, default: &serde_json::Value) {
    if value.is_null() {
        *value = default.clone();
        return;
    }
    if let (Some(value), Some(default)) = (value.as_object_mut(), default.as_object()) {
        for (key, default) in default {
            merge_default(value.entry(key).or_insert(serde_json::Value::Null), default);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn token_estimates_use_wire_defaults_and_preserve_explicit_limits() {
        use crate::protocols::canonical::{identity::TransportMode, requests::Operation};
        use crate::providers::runtime_model::ProviderKind;
        let mut generation = crate::providers::openai::certification::probe_generation_request(
            TransportMode::Unary,
            Default::default(),
        );
        generation.parameters.max_output_tokens = Some(0);
        generation.parameters.candidate_count = None;
        let input_tokens =
            crate::limits::admission::estimate_tokens(&Operation::Generation(generation.clone()));
        generation.parameters.max_output_tokens = None;
        for (kind, defaults, output_tokens) in [
            (
                ProviderKind::Anthropic,
                serde_json::json!({"max_tokens":64}),
                64,
            ),
            (
                ProviderKind::OpenAiCompatible,
                serde_json::json!({"max_tokens":8192,"n":3}),
                24576,
            ),
            (
                ProviderKind::OpenAi,
                serde_json::json!({"max_output_tokens":16384}),
                16384,
            ),
            (
                ProviderKind::Gemini,
                serde_json::json!({"generationConfig":{"maxOutputTokens":512,"candidateCount":2}}),
                1024,
            ),
        ] {
            let options = crate::providers::options::ConnectionOptions {
                parameter_defaults: serde_json::from_value(defaults).unwrap(),
                ..Default::default()
            };
            let operation = Operation::Generation(generation.clone());
            assert_eq!(
                estimate_tokens(&operation, kind, &options),
                input_tokens + output_tokens,
                "{kind}"
            );
            let mut explicit = generation.clone();
            explicit.parameters.max_output_tokens = Some(10);
            explicit.parameters.candidate_count = Some(2);
            assert_eq!(
                estimate_tokens(&Operation::Generation(explicit), kind, &options),
                input_tokens + 20
            );
        }
    }
    #[test]
    fn custom_authentication_requires_exact_secret_header_names() {
        let mut config = ProviderConfiguration::new(
            crate::providers::runtime_model::ProviderKind::OpenAiCompatible,
        );
        config.auth_mode = ProviderAuthMode::Headers;
        config
            .options
            .credential_headers
            .insert("authorization".into());
        let supplied = BorrowedCredential::Text(r#"{"authorization":"Bearer private"}"#);
        let parsed = headers(&config, supplied).unwrap().unwrap();
        assert!(parsed.get("authorization").unwrap().is_sensitive());
        assert!(headers(&config, BorrowedCredential::Text(r#"{"host":"attacker"}"#)).is_err());
    }
    #[test]
    fn voyage_maps_dimensions_and_disables_implicit_truncation() {
        let options = crate::providers::options::ConnectionOptions {
            vendor_id: Some("voyage".into()),
            ..Default::default()
        };
        let body = request_body(
            &options,
            "embeddings",
            br#"{"input":["test"],"dimensions":512}"#.to_vec(),
        )
        .unwrap();
        let value: serde_json::Value = serde_json::from_slice(&body).unwrap();
        assert_eq!(value["output_dimension"], 512);
        assert_eq!(value["truncation"], false);
        assert!(value.get("dimensions").is_none());
    }
    #[test]
    fn defaults_preserve_explicit_request_parameters() {
        let mut options = crate::providers::options::ConnectionOptions::default();
        options
            .parameter_defaults
            .insert("temperature".into(), serde_json::json!(0.2));
        let body = request_body(
            &options,
            "chat/completions",
            br#"{"temperature":0.8}"#.to_vec(),
        )
        .unwrap();
        assert_eq!(
            serde_json::from_slice::<serde_json::Value>(&body).unwrap()["temperature"],
            0.8
        );
    }
    #[test]
    fn explicit_token_limit_overrides_defaults_with_a_different_wire_alias() {
        let options = crate::providers::options::ConnectionOptions {
            parameter_defaults: std::collections::BTreeMap::from([(
                "max_tokens".into(),
                serde_json::json!(100),
            )]),
            ..Default::default()
        };
        let wire = request_body(
            &options,
            "chat/completions",
            br#"{"max_completion_tokens":32}"#.to_vec(),
        )
        .unwrap();
        let value: serde_json::Value = serde_json::from_slice(&wire).unwrap();
        assert_eq!(value["max_completion_tokens"], 32);
        assert!(value.get("max_tokens").is_none());
        let wire = request_body(&options, "embeddings", br#"{"input":["text"]}"#.to_vec()).unwrap();
        let value: serde_json::Value = serde_json::from_slice(&wire).unwrap();
        assert!(value.get("max_tokens").is_none());
    }

    #[test]
    fn response_format_defaults_are_scoped_to_compatible_endpoints() {
        for (format, endpoint) in [
            (serde_json::json!({"type":"text"}), "chat/completions"),
            (
                serde_json::json!({"type":"json_schema","json_schema":{"name":"reply","schema":{"type":"object"}}}),
                "chat/completions",
            ),
            (serde_json::json!("b64_json"), "images/generations"),
            (serde_json::json!("url"), "images/generations"),
            (serde_json::json!("mp3"), "audio/speech"),
            (serde_json::json!("wav"), "audio/speech"),
        ] {
            let options = crate::providers::options::ConnectionOptions {
                parameter_defaults: std::collections::BTreeMap::from([(
                    "response_format".into(),
                    format.clone(),
                )]),
                ..Default::default()
            };
            for path in [
                "chat/completions",
                "responses",
                "generation",
                "embeddings",
                "images/generations",
                "audio/speech",
                "moderations",
            ] {
                let wire = request_body(&options, path, b"{}".to_vec()).unwrap();
                let value: serde_json::Value = serde_json::from_slice(&wire).unwrap();
                assert_eq!(
                    value.get("response_format"),
                    (path == endpoint).then_some(&format),
                    "{path}/{format}"
                );
            }
            let explicit = if endpoint == "chat/completions" {
                serde_json::json!({"type":"json_object"})
            } else if endpoint == "audio/speech" {
                serde_json::json!("flac")
            } else {
                serde_json::json!("url")
            };
            let input = serde_json::json!({"response_format":explicit});
            let wire =
                request_body(&options, endpoint, serde_json::to_vec(&input).unwrap()).unwrap();
            assert_eq!(
                serde_json::from_slice::<serde_json::Value>(&wire).unwrap(),
                input
            );
        }
    }

    #[test]
    fn omitted_token_limits_use_an_alias_supported_by_the_selected_endpoint() {
        for alias in TOKEN_LIMIT_ALIASES.iter().copied() {
            for (path, vendor, expected) in [
                ("responses", None, "max_output_tokens"),
                (
                    "chat/completions",
                    None,
                    if alias == "max_tokens" {
                        "max_tokens"
                    } else {
                        "max_completion_tokens"
                    },
                ),
                ("chat/completions", Some("deepseek"), "max_tokens"),
            ] {
                let options = crate::providers::options::ConnectionOptions {
                    vendor_id: vendor.map(str::to_owned),
                    parameter_defaults: std::collections::BTreeMap::from([(
                        alias.into(),
                        serde_json::json!(64),
                    )]),
                    ..Default::default()
                };
                let wire = request_body(&options, path, b"{}".to_vec()).unwrap();
                let value: serde_json::Value = serde_json::from_slice(&wire).unwrap();
                assert_eq!(value, serde_json::json!({expected:64}), "{path}/{alias}");

                let explicit = serde_json::to_vec(&serde_json::json!({expected:32})).unwrap();
                let wire = request_body(&options, path, explicit).unwrap();
                let value: serde_json::Value = serde_json::from_slice(&wire).unwrap();
                assert_eq!(value, serde_json::json!({expected:32}), "{path}/{alias}");
            }
        }
    }
}
