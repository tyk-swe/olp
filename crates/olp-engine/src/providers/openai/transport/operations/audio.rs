use crate::domain::{
    canonical::{identity::TransportMode, requests::Operation, results::CanonicalResult},
    ports::{ProviderOutput, ProviderRequest, TransportError},
};
use crate::protocols::openai::audio::{
    TranscriptionResponse, TranscriptionResponseFormat, decode_speech_body,
    decode_transcription_response, encode_speech, encode_transcription,
};
use http::header;
use reqwest::{Response, multipart};

use super::super::{
    Connector,
    errors::*,
    media::*,
    streams::{read_deadline_body, require_content_type},
};
use crate::providers::transport_common::{protocol_body_error, transport_error};

pub(super) async fn execute_speech(
    connector: &Connector,
    request: ProviderRequest,
) -> Result<ProviderOutput, TransportError> {
    // The operation dispatcher routes only speech requests here.
    let Operation::Speech(operation) = &*request.operation else {
        unreachable!("checked by caller")
    };
    let wire = encode_speech(operation, &request.attempt.upstream_model)
        .map_err(|error| protocol_encode_error("speech", error))?;
    let response = connector
        .post_raw_json(&request, "audio/speech", serialize_wire("speech", &wire)?)
        .await?;
    if request.metadata.mode == TransportMode::Streaming {
        require_content_type(&response, "text/event-stream")?;
        return Ok(ProviderOutput::Events(connector.raw_sse_response(response)));
    }
    let content_type = response
        .headers()
        .get(header::CONTENT_TYPE)
        .and_then(|value| value.to_str().ok())
        .and_then(|value| value.split(';').next())
        .map(str::trim)
        .filter(|value| value.starts_with("audio/") || *value == "application/octet-stream")
        .ok_or_else(|| protocol_body_error("OpenAI speech response used an invalid content type"))?
        .to_owned();
    let spool = request.media.as_ref().ok_or_else(|| {
        protocol_body_error("the OpenAI speech response requires a bounded media spool")
    })?;
    let maximum = u64::try_from(connector.config.max_response_bytes).unwrap_or(u64::MAX);
    let artifact = spool_response_body(
        response,
        spool,
        "speech-output.bin".into(),
        Some(content_type),
        maximum,
        connector.config.timeouts.idle,
    )
    .await?;
    Ok(ProviderOutput::Result(Box::new(CanonicalResult::Speech(
        decode_speech_body(crate::protocols::openai::media::BinaryMediaBody { media: artifact }),
    ))))
}

pub(super) async fn execute_transcription(
    connector: &Connector,
    request: ProviderRequest,
) -> Result<ProviderOutput, TransportError> {
    // The operation dispatcher routes only transcription requests here.
    let Operation::Transcription(operation) = &*request.operation else {
        unreachable!("checked by caller")
    };
    let spool = request.media.as_ref().ok_or_else(|| {
        protocol_body_error("OpenAI transcription requires a bounded media spool")
    })?;
    let metadata = bounded_part(spool.as_ref(), &operation.audio, 25 * 1024 * 1024).await?;
    let wire = encode_transcription(operation, &request.attempt.upstream_model, |_| {
        Ok(metadata.clone())
    })
    .map_err(|error| protocol_encode_error("transcription", error))?;
    let opened = spool
        .open(&operation.audio)
        .await
        .map_err(map_spool_error)?;
    let mut form = multipart::Form::new()
        .text("model", wire.model)
        .part("file", multipart_part(opened)?);
    form = add_optional_text(form, "language", wire.language);
    form = add_optional_text(form, "prompt", wire.prompt);
    form = add_optional_text(form, "response_format", wire.response_format.clone());
    form = add_optional_text(
        form,
        "temperature",
        wire.temperature.map(|value| value.to_string()),
    );
    if !wire.include.is_empty() {
        for value in wire.include {
            form = form.text("include[]", value);
        }
    }
    if !wire.timestamp_granularities.is_empty() {
        for value in wire.timestamp_granularities {
            form = form.text("timestamp_granularities[]", value);
        }
    }
    if let Some(value) = wire.chunking_strategy {
        form = form.text("chunking_strategy", value.to_string());
    }
    form = form.text("stream", wire.stream.to_string());
    form = add_extra_fields(form, wire.extra);
    let response = connector
        .post_multipart_raw(&request, "audio/transcriptions", form)
        .await?;
    if request.metadata.mode == TransportMode::Streaming {
        require_content_type(&response, "text/event-stream")?;
        return Ok(ProviderOutput::Events(connector.raw_sse_response(response)));
    }
    let response_format = TranscriptionResponseFormat::parse(wire.response_format.as_deref())
        .map_err(|error| protocol_encode_error("transcription", error))?;
    if response_format.is_text() {
        require_transcription_text_content_type(&response, response_format)?;
    } else {
        require_content_type(&response, "application/json")?;
    }
    let bytes = read_deadline_body(
        response,
        connector.config.timeouts.idle,
        connector.config.max_response_bytes,
    )
    .await?;
    let response = if response_format.is_text() {
        TranscriptionResponse::Text(
            String::from_utf8(bytes)
                .map_err(|error| protocol_decode_error("transcription text", error))?,
        )
    } else {
        parse_wire("transcription", &bytes)?
    };
    Ok(ProviderOutput::Result(Box::new(
        CanonicalResult::Transcription(decode_transcription_response(response)),
    )))
}

fn require_transcription_text_content_type(
    response: &Response,
    format: TranscriptionResponseFormat,
) -> Result<(), TransportError> {
    let actual = response
        .headers()
        .get(header::CONTENT_TYPE)
        .and_then(|value| value.to_str().ok())
        .and_then(|value| value.split(';').next())
        .map(str::trim);
    let valid = match format {
        TranscriptionResponseFormat::Text => actual == Some("text/plain"),
        TranscriptionResponseFormat::Srt => {
            matches!(actual, Some("application/x-subrip" | "text/plain"))
        }
        TranscriptionResponseFormat::Vtt => matches!(actual, Some("text/vtt" | "text/plain")),
        _ => false,
    };
    if valid {
        Ok(())
    } else {
        Err(transport_error(
            crate::domain::ports::TransportPhase::FirstByte,
            crate::domain::ports::AttemptFailureClass::Protocol,
            false,
            "OpenAI transcription response used an invalid text content type",
        ))
    }
}
