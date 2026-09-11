use crate::inference::transport::AttemptFailureClass;
use crate::inference::transport::ProviderOutput;
use crate::inference::transport::ProviderRequest;
use crate::inference::transport::TransportError;
use crate::inference::transport::TransportPhase;
use crate::protocols::canonical::identity::Surface;
use crate::protocols::canonical::identity::TransportMode;
use crate::protocols::canonical::requests::OPENAI_ENDPOINT_EXTENSION;
use crate::protocols::canonical::requests::Operation;
use crate::protocols::openai::chat::CompletionRequest;
use crate::protocols::openai::chat::encode;
use crate::protocols::openai::responses::request::encode_response_create;
use ::http::HeaderMap;
use tokio::time::Instant;
use tokio::time::timeout;

use crate::providers::openai::transport::Connector;
use crate::providers::openai::transport::errors::*;
use crate::providers::openai::transport::media::*;
use crate::providers::transport_common::insert_json_request_headers;
use crate::providers::transport_common::transport_error;
use crate::providers::transport_io::bounded_duration;

pub(crate) async fn execute(
    connector: &Connector,
    request: ProviderRequest,
) -> Result<ProviderOutput, TransportError> {
    // The operation dispatcher routes only generation requests here.
    let Operation::Generation(generation) = &*request.operation else {
        unreachable!("checked by caller")
    };
    let responses_endpoint = generation
        .extensions
        .values
        .get(OPENAI_ENDPOINT_EXTENSION)
        .and_then(serde_json::Value::as_str)
        .is_some_and(|endpoint| endpoint == "responses");
    generation
        .extensions
        .ensure_representable_on(Surface::OpenAi)
        .map_err(|error| {
            transport_error(
                TransportPhase::Connect,
                AttemptFailureClass::Protocol,
                false,
                error.to_string(),
            )
        })?;

    let streaming = request.metadata.mode == TransportMode::Streaming;
    let body = encode_generation_body(&request, generation, responses_endpoint, streaming).await?;
    let body = crate::providers::http_options::request_body(
        &connector.options,
        if responses_endpoint {
            "responses"
        } else {
            "chat/completions"
        },
        body,
    )?;

    let started = Instant::now();
    let attempt_deadline = started + request.attempt.timeout.as_duration();
    let connect_timeout = bounded_duration(
        connector.config.timeouts.connect,
        remaining(attempt_deadline, TransportPhase::Connect)?,
    );
    // Resolution is validated and pinned before any credential is copied
    // into an HTTP header or request object.
    let client = connector
        .config
        .endpoint
        .pinned_client(connect_timeout)
        .await
        .map_err(map_endpoint_error)?;
    let url = connector
        .config
        .endpoint
        .resource_url(if responses_endpoint {
            "responses"
        } else {
            "chat/completions"
        })
        .map_err(map_endpoint_error)?;

    let first_byte_deadline = Instant::now() + connector.config.timeouts.first_byte;
    let headers = generation_headers(connector, &request, streaming)?;

    let send_wait =
        remaining_until(first_byte_deadline, attempt_deadline).ok_or_else(first_byte_timeout)?;
    let response = timeout(
        send_wait,
        client.post(url).headers(headers).body(body).send(),
    )
    .await
    .map_err(|_| first_byte_timeout())?
    .map_err(map_send_error)?;

    if !response.status().is_success() {
        return Err(connector
            .map_error_response(response, attempt_deadline)
            .await);
    }

    let events = if streaming {
        connector.streaming_response(
            response,
            first_byte_deadline,
            attempt_deadline,
            responses_endpoint,
        )
    } else {
        connector
            .unary_response(
                response,
                first_byte_deadline,
                attempt_deadline,
                responses_endpoint,
            )
            .await
    }?;
    Ok(ProviderOutput::Events(events))
}

fn require_stream_usage(request: &mut CompletionRequest) -> Result<(), TransportError> {
    let options = request
        .extra
        .entry("stream_options".to_owned())
        .or_insert_with(|| serde_json::json!({}));
    let Some(options) = options.as_object_mut() else {
        return Err(transport_error(
            TransportPhase::Connect,
            AttemptFailureClass::Protocol,
            false,
            "OpenAI stream_options extension must be an object",
        ));
    };
    options.insert("include_usage".to_owned(), serde_json::Value::Bool(true));
    Ok(())
}

async fn encode_generation_body(
    request: &ProviderRequest,
    generation: &crate::protocols::canonical::requests::GenerationRequest,
    responses_endpoint: bool,
    streaming: bool,
) -> Result<Vec<u8>, TransportError> {
    if responses_endpoint {
        let mut wire = encode_response_create(generation, &request.attempt.upstream_model)
            .map_err(|error| protocol_encode_error("Responses", error))?;
        hydrate_responses_media(
            &mut wire.input,
            request.media.as_ref(),
            request.max_inline_media_bytes,
        )
        .await?;
        serialize_wire("Responses", &wire)
    } else {
        let mut wire = encode::chat_completion(generation, &request.attempt.upstream_model)
            .map_err(|error| protocol_encode_error("chat", error))?;
        if streaming {
            require_stream_usage(&mut wire)?;
        }
        hydrate_chat_media(
            &mut wire,
            request.media.as_ref(),
            request.max_inline_media_bytes,
        )
        .await?;
        serialize_wire("chat", &wire)
    }
}

fn generation_headers(
    connector: &Connector,
    request: &ProviderRequest,
    streaming: bool,
) -> Result<HeaderMap, TransportError> {
    let mut headers = HeaderMap::new();
    connector.attach_auth(&mut headers)?;
    insert_json_request_headers(&mut headers, request, streaming)?;
    Ok(headers)
}
