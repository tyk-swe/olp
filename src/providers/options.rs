//! Revisioned connection options and model facts. No secrets belong in these types.
use serde::{Deserialize, Serialize};
use std::collections::{BTreeMap, BTreeSet};
use utoipa::ToSchema;

#[derive(Clone, Debug, Default, Deserialize, Serialize, ToSchema, Eq, PartialEq)]
#[serde(default, deny_unknown_fields)]
pub struct ConnectionOptions {
    pub vendor_id: Option<String>,
    pub limits: Option<ConnectionLimits>,
    /// Header names whose values are supplied in the encrypted credential JSON.
    pub credential_headers: BTreeSet<String>,
    pub parameter_defaults: BTreeMap<String, serde_json::Value>,
    pub models: BTreeMap<String, ModelMetadata>,
}

#[derive(Clone, Debug, Default, Deserialize, Serialize, ToSchema, Eq, PartialEq)]
#[serde(default, deny_unknown_fields)]
pub struct ModelMetadata {
    pub canonical_model: Option<String>,
    pub input_modalities: BTreeSet<String>,
    pub output_modalities: BTreeSet<String>,
    pub context_length: Option<u64>,
    pub max_output_tokens: Option<u64>,
    /// None means unknown, an empty set means no optional parameters are supported.
    pub supported_parameters: Option<BTreeSet<String>>,
    pub quantization: Option<String>,
    pub region: Option<String>,
    pub data_collection: Option<bool>,
    pub zero_data_retention: Option<bool>,
    pub deployment: Option<String>,
    pub source: Option<String>,
    pub observed_at: Option<chrono::DateTime<chrono::Utc>>,
}

/// Wire names for the one output-token ceiling across OpenAI-family endpoints.
pub(crate) const TOKEN_LIMIT_ALIASES: &[&str] =
    &["max_tokens", "max_completion_tokens", "max_output_tokens"];

impl ConnectionOptions {
    pub fn validate(
        &self,
        kind: crate::providers::runtime_model::ProviderKind,
    ) -> Result<(), String> {
        if let Some(id) = &self.vendor_id {
            let vendor = crate::providers::catalog::vendor(id).ok_or("Unknown provider vendor")?;
            if vendor.connector != kind {
                return Err("Vendor does not support this connector".into());
            }
        }
        if self.credential_headers.len() > 16
            || self.models.len() > 2_000
            || self.parameter_defaults.len() > 64
        {
            return Err("Connection options exceed the configured collection limits".into());
        }
        if let Some(limits) = &self.limits {
            limits.validate()?;
        }
        if !self.parameter_defaults.is_empty() && sdk_connector(kind) {
            return Err("Parameter defaults require a native HTTP connector".into());
        }
        for name in &self.credential_headers {
            validate_header_name(name)?;
        }
        if TOKEN_LIMIT_ALIASES
            .iter()
            .filter(|key| self.parameter_defaults.contains_key(**key))
            .count()
            > 1
        {
            return Err("Configure one token-limit default".into());
        }
        for (name, value) in &self.parameter_defaults {
            validate_parameter_default(name, value)?;
            if self.vendor_id.as_deref() == Some("cohere")
                && crate::providers::catalog::COHERE_UNSUPPORTED_PARAMETERS.contains(&name.as_str())
            {
                return Err(format!("Cohere does not support parameter default: {name}"));
            }
            if matches!(
                name.as_str(),
                "model"
                    | "messages"
                    | "input"
                    | "contents"
                    | "stream"
                    | "tools"
                    | "provider"
                    | "routing"
            ) || name.starts_with('/')
                || value.to_string().len() > 8_192
            {
                return Err(format!("Invalid parameter default: {name}"));
            }
        }
        for (model, metadata) in &self.models {
            if metadata.deployment.is_some() && sdk_connector(kind) {
                return Err("Deployment overrides require a native HTTP connector".into());
            }
            if model.is_empty()
                || model.len() > 200
                || metadata.context_length == Some(0)
                || metadata.max_output_tokens == Some(0)
            {
                return Err("Invalid model metadata".into());
            }
            if metadata
                .deployment
                .as_ref()
                .is_some_and(|s| s.is_empty() || s.len() > 200 || s.chars().any(char::is_control))
            {
                return Err("Invalid deployment override".into());
            }
            if metadata
                .supported_parameters
                .as_ref()
                .is_some_and(|parameters| parameters.len() > 128)
            {
                return Err("Too many supported parameters".into());
            }
            if (metadata.data_collection.is_some() || metadata.zero_data_retention.is_some())
                && (metadata.source.as_ref().is_none_or(|s| s.trim().is_empty())
                    || metadata.observed_at.is_none())
            {
                return Err("Privacy declarations require a source and observation time".into());
            }
        }
        if serde_json::to_vec(self)
            .map_err(|_| "Invalid connection options")?
            .len()
            > 1_048_576
        {
            return Err("Connection options exceed one MiB".into());
        }
        Ok(())
    }
}

/// SDK-backed connectors build their own request bodies, so wire-level
/// defaults and deployment overrides cannot be honoured.
fn sdk_connector(kind: crate::providers::runtime_model::ProviderKind) -> bool {
    matches!(
        kind,
        crate::providers::runtime_model::ProviderKind::Bedrock
            | crate::providers::runtime_model::ProviderKind::VertexAi
    )
}

pub fn validate_header_name(name: &str) -> Result<(), String> {
    let parsed = http::HeaderName::from_bytes(name.as_bytes())
        .map_err(|_| "Invalid credential header name")?;
    if parsed.as_str() != name
        || name.starts_with("x-olp-")
        || matches!(
            name,
            "host"
                | "content-length"
                | "transfer-encoding"
                | "connection"
                | "cookie"
                | "proxy-authorization"
                | "proxy-connection"
                | "upgrade"
                | "te"
                | "trailer"
                | "content-type"
                | "accept"
                | "traceparent"
                | "tracestate"
        )
    {
        return Err("Credential header name is reserved or is not lowercase".into());
    }
    Ok(())
}

#[derive(Clone, Debug, Default, Deserialize, Serialize, ToSchema, Eq, PartialEq)]
#[serde(default, deny_unknown_fields)]
pub struct ConnectionLimits {
    pub requests_per_minute: Option<u32>,
    pub tokens_per_minute: Option<u64>,
    pub max_concurrency: Option<u32>,
}
impl ConnectionLimits {
    pub(crate) fn validate(&self) -> Result<(), String> {
        if self
            .requests_per_minute
            .is_some_and(|n| n == 0 || n > i32::MAX as u32)
            || self
                .tokens_per_minute
                .is_some_and(|n| n == 0 || n > (1_u64 << 53) - 1)
            || self
                .max_concurrency
                .is_some_and(|n| n == 0 || n > i32::MAX as u32)
        {
            return Err("Invalid connection quota".into());
        }
        Ok(())
    }
}

fn validate_parameter_default(name: &str, value: &serde_json::Value) -> Result<(), String> {
    let valid = match name {
        "temperature" => value.as_f64().is_some_and(|n| (0.0..=2.0).contains(&n)),
        "top_p" | "topP" => value.as_f64().is_some_and(|n| (0.0..=1.0).contains(&n)),
        "max_tokens"
        | "max_completion_tokens"
        | "max_output_tokens"
        | "maxOutputTokens"
        | "n"
        | "candidateCount"
        | "dimensions"
        | "output_dimension" => value
            .as_u64()
            .is_some_and(|n| n > 0 && n <= u32::MAX.into()),
        "top_k" | "topK" => value.as_u64().is_some_and(|n| n <= u32::MAX.into()),
        "seed" => value.as_i64().is_some(),
        "parallel_tool_calls" | "truncation" => value.is_boolean(),
        "stop" => {
            value.is_string()
                || value
                    .as_array()
                    .is_some_and(|values| values.iter().all(serde_json::Value::is_string))
        }
        "stop_sequences" | "stopSequences" => value
            .as_array()
            .is_some_and(|values| values.iter().all(serde_json::Value::is_string)),
        "generationConfig" => {
            let values = value
                .as_object()
                .ok_or("generationConfig defaults must be an object")?;
            for (name, value) in values {
                validate_parameter_default(name, value)?;
            }
            true
        }
        _ => true,
    };
    if valid {
        Ok(())
    } else {
        Err(format!("Invalid parameter default: {name}"))
    }
}

#[cfg(test)]
mod default_tests {
    use super::*;
    #[test]
    fn invalid_defaults_are_rejected_even_when_probe_parameters_would_override_them() {
        for (name, value) in [
            ("temperature", serde_json::json!(-1)),
            ("max_tokens", serde_json::json!(0)),
            ("top_p", serde_json::json!("bad")),
            ("truncation", serde_json::json!("true")),
        ] {
            let options = ConnectionOptions {
                parameter_defaults: BTreeMap::from([(name.into(), value)]),
                ..Default::default()
            };
            assert!(
                options
                    .validate(crate::providers::runtime_model::ProviderKind::OpenAiCompatible)
                    .is_err()
            );
        }
    }
}
