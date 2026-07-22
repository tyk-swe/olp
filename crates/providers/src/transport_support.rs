//! Provider-neutral transport security and diagnostic helpers.

use std::collections::BTreeMap;

use http::{HeaderValue, StatusCode, header::InvalidHeaderValue};
use olp_domain::{
    AttemptFailureClass, SourceExtensions, Surface, TransportError, TransportPhase,
};
use zeroize::Zeroizing;

const MAX_UPSTREAM_ERROR_CHARS: usize = 512;

pub(crate) fn secret_header(
    prefix: &[u8],
    secret: &str,
) -> Result<HeaderValue, InvalidHeaderValue> {
    let mut value = Zeroizing::new(Vec::with_capacity(prefix.len().saturating_add(secret.len())));
    value.extend_from_slice(prefix);
    value.extend_from_slice(secret.as_bytes());
    HeaderValue::from_bytes(value.as_slice())
}

pub(crate) fn safe_upstream_error_message(
    provider: &str,
    status: StatusCode,
    body: &[u8],
    secrets: &[&str],
) -> String {
    let message = serde_json::from_slice::<serde_json::Value>(body)
        .ok()
        .and_then(|value| {
            value
                .pointer("/error/message")
                .and_then(serde_json::Value::as_str)
                .map(str::to_owned)
        })
        .map(|mut message| {
            let mut secrets = secrets
                .iter()
                .copied()
                .filter(|secret| !secret.is_empty())
                .collect::<Vec<_>>();
            secrets.sort_unstable_by(|left, right| {
                right
                    .len()
                    .cmp(&left.len())
                    .then_with(|| left.cmp(right))
            });
            secrets.dedup();
            for secret in secrets {
                message = message.replace(secret, "[REDACTED]");
            }
            message
                .chars()
                .map(|character| {
                    if character.is_control() {
                        ' '
                    } else {
                        character
                    }
                })
                .take(MAX_UPSTREAM_ERROR_CHARS)
                .collect::<String>()
                .trim()
                .to_owned()
        });
    match message {
        Some(message) if !message.is_empty() => {
            format!("{provider} returned HTTP {status}: {message}")
        }
        _ => format!("{provider} returned HTTP {status}"),
    }
}

pub(crate) fn source_extensions(
    surface: Surface,
    values: BTreeMap<String, serde_json::Value>,
) -> SourceExtensions {
    let values = values
        .into_iter()
        .map(|(key, value)| {
            let escaped = key.replace('~', "~0").replace('/', "~1");
            (format!("/{escaped}"), value)
        })
        .collect();
    SourceExtensions::new(surface, values)
}

pub(crate) fn map_send_error(provider: &str, error: reqwest::Error) -> TransportError {
    let (phase, class, message) = if error.is_connect() {
        (
            TransportPhase::Connect,
            if error.is_timeout() {
                AttemptFailureClass::Timeout
            } else {
                AttemptFailureClass::Connect
            },
            format!("{provider} connection failed"),
        )
    } else if error.is_timeout() {
        (
            TransportPhase::FirstByte,
            AttemptFailureClass::Timeout,
            format!("{provider} first-byte deadline elapsed"),
        )
    } else {
        (
            TransportPhase::FirstByte,
            AttemptFailureClass::Connect,
            format!("{provider} request failed before response headers"),
        )
    };
    transport_error(phase, class, false, message)
}

pub(crate) fn transport_error(
    phase: TransportPhase,
    class: AttemptFailureClass,
    response_committed: bool,
    message: impl Into<String>,
) -> TransportError {
    TransportError {
        phase,
        class,
        response_committed,
        message: message.into(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn upstream_messages_are_redacted_and_bounded() {
        let secret = "top-secret";
        let body = serde_json::json!({
            "error": { "message": format!("prefix\n{secret} {}", "x".repeat(600)) }
        });
        let message = safe_upstream_error_message(
            "Provider",
            StatusCode::BAD_REQUEST,
            body.to_string().as_bytes(),
            &[secret],
        );

        assert!(!message.contains(secret));
        assert!(!message.contains('\n'));
        assert!(message.contains("[REDACTED]"));
        assert!(message.chars().count() <= 512 + "Provider returned HTTP 400 Bad Request: ".len());
    }

    #[test]
    fn source_extension_keys_use_json_pointer_escaping() {
        let extensions = source_extensions(
            Surface::OpenAi,
            BTreeMap::from([("a/b~c".into(), serde_json::Value::Bool(true))]),
        );

        assert_eq!(
            extensions.values.get("/a~1b~0c"),
            Some(&serde_json::Value::Bool(true))
        );
    }
}
