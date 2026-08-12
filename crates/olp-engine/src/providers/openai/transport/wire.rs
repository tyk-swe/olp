use crate::domain::{AttemptFailureClass, ProviderKind, TransportError, TransportPhase};
use http::header;
use reqwest::Response;

use super::errors::transport_error;

const UTF8_BOM: &[u8; 3] = b"\xef\xbb\xbf";

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(super) enum WireProfile {
    Strict,
    Compatible,
}

impl WireProfile {
    #[must_use]
    pub(super) const fn for_provider(kind: ProviderKind) -> Self {
        if matches!(kind, ProviderKind::OpenAiCompatible) {
            Self::Compatible
        } else {
            Self::Strict
        }
    }

    #[must_use]
    pub(super) const fn is_compatible(self) -> bool {
        matches!(self, Self::Compatible)
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(super) enum StreamingBodyKind {
    EventStream,
    UnaryJson,
    Sniff,
}

pub(super) fn require_json_response(
    response: &Response,
    profile: WireProfile,
) -> Result<(), TransportError> {
    match success_content_type(response, profile) {
        SuccessContentType::Json | SuccessContentType::Generic if profile.is_compatible() => Ok(()),
        SuccessContentType::Json => Ok(()),
        _ => Err(content_type_error("application/json")),
    }
}

pub(super) fn streaming_body_kind(
    response: &Response,
    profile: WireProfile,
    unary_chat_fallback: bool,
) -> Result<StreamingBodyKind, TransportError> {
    match success_content_type(response, profile) {
        SuccessContentType::EventStream => Ok(StreamingBodyKind::EventStream),
        SuccessContentType::Json if profile.is_compatible() && unary_chat_fallback => {
            Ok(StreamingBodyKind::UnaryJson)
        }
        SuccessContentType::Generic if profile.is_compatible() => Ok(StreamingBodyKind::Sniff),
        _ => Err(content_type_error("text/event-stream")),
    }
}

#[must_use]
pub(super) fn strip_json_bom(body: &[u8], profile: WireProfile) -> &[u8] {
    if profile.is_compatible() {
        body.strip_prefix(UTF8_BOM).unwrap_or(body)
    } else {
        body
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum SuccessContentType {
    Json,
    EventStream,
    Generic,
    Other,
}

fn success_content_type(response: &Response, profile: WireProfile) -> SuccessContentType {
    let Some(value) = response.headers().get(header::CONTENT_TYPE) else {
        return SuccessContentType::Generic;
    };
    let Ok(value) = value.to_str() else {
        return SuccessContentType::Other;
    };
    let essence = value.split(';').next().unwrap_or_default().trim();
    if essence.eq_ignore_ascii_case("application/json")
        || (profile.is_compatible() && structured_json_essence(essence))
    {
        SuccessContentType::Json
    } else if essence.eq_ignore_ascii_case("text/event-stream") {
        SuccessContentType::EventStream
    } else if profile.is_compatible() && generic_essence(essence) {
        SuccessContentType::Generic
    } else {
        SuccessContentType::Other
    }
}

fn structured_json_essence(essence: &str) -> bool {
    let Some((kind, subtype)) = essence.split_once('/') else {
        return false;
    };
    valid_media_token(kind)
        && valid_media_token(subtype)
        && subtype
            .get(subtype.len().saturating_sub(5)..)
            .is_some_and(|suffix| suffix.eq_ignore_ascii_case("+json"))
}

fn valid_media_token(value: &str) -> bool {
    !value.is_empty()
        && value.bytes().all(|byte| {
            byte.is_ascii_alphanumeric()
                || matches!(
                    byte,
                    b'!' | b'#'
                        | b'$'
                        | b'%'
                        | b'&'
                        | b'\''
                        | b'*'
                        | b'+'
                        | b'-'
                        | b'.'
                        | b'^'
                        | b'_'
                        | b'`'
                        | b'|'
                        | b'~'
                )
        })
}

fn generic_essence(essence: &str) -> bool {
    essence.is_empty()
        || essence.eq_ignore_ascii_case("application/octet-stream")
        || essence.eq_ignore_ascii_case("binary/octet-stream")
        || essence.eq_ignore_ascii_case("text/plain")
}

fn content_type_error(expected: &'static str) -> TransportError {
    transport_error(
        TransportPhase::FirstByte,
        AttemptFailureClass::Protocol,
        false,
        format!("OpenAI response must use content type {expected}"),
    )
}

pub(super) enum BomChunk<'a> {
    Pending,
    Rejected,
    Borrowed(&'a [u8]),
    Buffered(Vec<u8>),
}

/// Removes exactly one leading UTF-8 BOM without assuming that transport
/// chunks align with the three-byte marker.
pub(super) struct StreamingBom {
    strict: bool,
    removed: bool,
    decided: bool,
    prefix: Vec<u8>,
}

impl StreamingBom {
    #[must_use]
    pub(super) fn new(profile: WireProfile) -> Self {
        Self {
            strict: !profile.is_compatible(),
            removed: false,
            decided: false,
            prefix: Vec::with_capacity(UTF8_BOM.len()),
        }
    }

    pub(super) fn push<'a>(&mut self, bytes: &'a [u8]) -> BomChunk<'a> {
        if self.decided {
            return BomChunk::Borrowed(bytes);
        }

        let needed = UTF8_BOM.len().saturating_sub(self.prefix.len());
        let taken = needed.min(bytes.len());
        self.prefix.extend_from_slice(&bytes[..taken]);
        let matches_bom_prefix = UTF8_BOM.starts_with(&self.prefix);
        if matches_bom_prefix && self.prefix.len() < UTF8_BOM.len() {
            return BomChunk::Pending;
        }

        self.decided = true;
        let remaining = &bytes[taken..];
        if self.prefix == UTF8_BOM {
            self.prefix.clear();
            if self.strict || self.removed {
                return BomChunk::Rejected;
            }
            self.removed = true;
            self.decided = false;
            return if remaining.is_empty() {
                BomChunk::Pending
            } else {
                self.push(remaining)
            };
        }

        let mut buffered = std::mem::take(&mut self.prefix);
        buffered.extend_from_slice(remaining);
        BomChunk::Buffered(buffered)
    }

    pub(super) fn finish(&mut self) -> Option<Vec<u8>> {
        if self.decided || self.prefix.is_empty() {
            return None;
        }
        self.decided = true;
        Some(std::mem::take(&mut self.prefix))
    }
}
